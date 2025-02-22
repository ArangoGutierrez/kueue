/*
Copyright 2022 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package capacity

import (
	"context"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kueue "sigs.k8s.io/kueue/api/v1alpha1"
	"sigs.k8s.io/kueue/pkg/workload"
)

const workloadCapacityKey = "spec.assignedCapacity"

type Cache struct {
	sync.Mutex

	client           client.Client
	capacities       map[string]*Capacity
	cohorts          map[string]*Cohort
	assumedWorkloads map[string]string
}

func NewCache(client client.Client) *Cache {
	return &Cache{
		client:           client,
		capacities:       make(map[string]*Capacity),
		cohorts:          make(map[string]*Cohort),
		assumedWorkloads: make(map[string]string),
	}
}

type Resources map[corev1.ResourceName]map[string]int64

// Cohort is a set of Capacities that can borrow resources from each other.
type Cohort struct {
	Name    string
	members map[*Capacity]struct{}

	// These fields are only populated for a snapshot.
	RequestableResources Resources
	UsedResources        Resources
}

func newCohort(name string, cap int) *Cohort {
	return &Cohort{
		Name:    name,
		members: make(map[*Capacity]struct{}, cap),
	}
}

// Capacity is the internal implementation of kueue.Capacity
type Capacity struct {
	Name                 string
	Cohort               *Cohort
	RequestableResources map[corev1.ResourceName][]kueue.ResourceFlavor
	UsedResources        Resources
	Workloads            map[string]*workload.Info
}

func NewCapacity(cap *kueue.Capacity) *Capacity {
	c := &Capacity{
		Name:                 cap.Name,
		RequestableResources: resourcesByName(cap.Spec.RequestableResources),
		UsedResources:        make(Resources, len(cap.Spec.RequestableResources)),
		Workloads:            map[string]*workload.Info{},
	}

	for _, r := range cap.Spec.RequestableResources {
		if len(r.Flavors) == 0 {
			continue
		}

		ts := make(map[string]int64, len(r.Flavors))
		for _, t := range r.Flavors {
			ts[t.Name] = 0
		}
		c.UsedResources[r.Name] = ts
	}
	return c
}

func (c *Capacity) addWorkload(w *kueue.QueuedWorkload) error {
	k := workload.Key(w)
	if _, exist := c.Workloads[k]; exist {
		return fmt.Errorf("workload already exists in capacity")
	}
	wi := workload.NewInfo(w)
	c.Workloads[k] = wi
	c.updateWorkloadUsage(wi, 1)
	return nil
}

func (c *Capacity) deleteWorkload(w *kueue.QueuedWorkload) {
	k := workload.Key(w)
	wi, exist := c.Workloads[k]
	if !exist {
		return
	}
	c.updateWorkloadUsage(wi, -1)
	delete(c.Workloads, k)
	return
}

func (c *Capacity) updateWorkloadUsage(wi *workload.Info, m int64) {
	for _, ps := range wi.TotalRequests {
		for wlRes, wlResFlv := range ps.Flavors {
			v, wlResExist := ps.Requests[wlRes]
			capResFlv, capResExist := c.UsedResources[wlRes]
			if capResExist && wlResExist {
				if _, capFlvExist := capResFlv[wlResFlv]; capFlvExist {
					capResFlv[wlResFlv] += v * m
				}
			}
		}
	}
}

func (c *Cache) AddCapacity(ctx context.Context, cap *kueue.Capacity) error {
	c.Lock()
	defer c.Unlock()

	if _, ok := c.capacities[cap.Name]; ok {
		return fmt.Errorf("capacity already exists")
	}
	capImpl := NewCapacity(cap)
	c.addCapacityToCohort(capImpl, cap.Spec.Cohort)
	c.capacities[cap.Name] = capImpl
	// On controller restart, an add capacity event may come after
	// add workload events, and so here we explicitly list and add existing workloads.
	var workloads kueue.QueuedWorkloadList
	if err := c.client.List(ctx, &workloads, client.MatchingFields{workloadCapacityKey: cap.Name}); err != nil {
		return fmt.Errorf("listing workloads that match the queue: %w", err)
	}
	for i, w := range workloads.Items {
		// Checking capacity name again because the field index is not available in tests.
		if string(w.Spec.AssignedCapacity) != cap.Name {
			continue
		}
		c.addOrUpdateWorkload(&workloads.Items[i])
	}

	return nil
}

func (c *Cache) UpdateCapacity(cap *kueue.Capacity) error {
	c.Lock()
	defer c.Unlock()
	capImpl, ok := c.capacities[cap.Name]
	if !ok {
		return fmt.Errorf("capacity doesn't exist")
	}
	capImpl.RequestableResources = resourcesByName(cap.Spec.RequestableResources)
	if capImpl.Cohort != nil {
		if capImpl.Cohort.Name != cap.Spec.Cohort {
			c.deleteCapacityFromCohort(capImpl)
			c.addCapacityToCohort(capImpl, cap.Spec.Cohort)
		}
	} else {
		c.addCapacityToCohort(capImpl, cap.Spec.Cohort)
	}
	return nil
}

func (c *Cache) DeleteCapacity(cap *kueue.Capacity) {
	c.Lock()
	defer c.Unlock()
	capImpl, ok := c.capacities[cap.Name]
	if !ok {
		return
	}
	c.deleteCapacityFromCohort(capImpl)
	delete(c.capacities, cap.Name)
}

func (c *Cache) AddOrUpdateWorkload(w *kueue.QueuedWorkload) bool {
	c.Lock()
	defer c.Unlock()
	return c.addOrUpdateWorkload(w)
}

func (c *Cache) addOrUpdateWorkload(w *kueue.QueuedWorkload) bool {
	if w.Spec.AssignedCapacity == "" {
		return false
	}

	cap, ok := c.capacities[string(w.Spec.AssignedCapacity)]
	if !ok {
		return false
	}

	c.cleanupAssumedState(w)

	if _, exist := cap.Workloads[workload.Key(w)]; exist {
		cap.deleteWorkload(w)
	}

	return cap.addWorkload(w) == nil
}

func (c *Cache) UpdateWorkload(oldWl, newWl *kueue.QueuedWorkload) error {
	c.Lock()
	defer c.Unlock()
	if oldWl.Spec.AssignedCapacity != "" {
		cap, ok := c.capacities[string(oldWl.Spec.AssignedCapacity)]
		if !ok {
			return fmt.Errorf("old capacity doesn't exist")
		}
		cap.deleteWorkload(oldWl)
	}
	c.cleanupAssumedState(oldWl)

	cap, ok := c.capacities[string(newWl.Spec.AssignedCapacity)]
	if !ok {
		return fmt.Errorf("new capacity doesn't exist")
	}
	return cap.addWorkload(newWl)
}

func (c *Cache) DeleteWorkload(w *kueue.QueuedWorkload) error {
	c.Lock()
	defer c.Unlock()
	if w.Spec.AssignedCapacity == "" {
		return fmt.Errorf("workload not assigned a capacity")
	}

	cap, ok := c.capacities[string(w.Spec.AssignedCapacity)]
	if !ok {
		return fmt.Errorf("capacity doesn't exist")
	}

	c.cleanupAssumedState(w)

	cap.deleteWorkload(w)
	return nil
}

func (c *Cache) AssumeWorkload(w *kueue.QueuedWorkload) error {
	c.Lock()
	defer c.Unlock()

	if w.Spec.AssignedCapacity == "" {
		return fmt.Errorf("workload not assigned a capacity")
	}

	k := workload.Key(w)
	assumedCap, assumed := c.assumedWorkloads[k]
	if assumed {
		return fmt.Errorf("the workload is already assumed to capacity %q", assumedCap)
	}

	cap, ok := c.capacities[string(w.Spec.AssignedCapacity)]
	if !ok {
		return fmt.Errorf("capacity doesn't exist")
	}

	if err := cap.addWorkload(w); err != nil {
		return err
	}
	c.assumedWorkloads[k] = string(w.Spec.AssignedCapacity)
	return nil
}

func (c *Cache) ForgetWorkload(w *kueue.QueuedWorkload) error {
	c.Lock()
	defer c.Unlock()

	if _, assumed := c.assumedWorkloads[workload.Key(w)]; !assumed {
		return fmt.Errorf("the workload is not assumed")
	}
	c.cleanupAssumedState(w)

	cap, ok := c.capacities[string(w.Spec.AssignedCapacity)]
	if !ok {
		return fmt.Errorf("capacity doesn't exist")
	}
	cap.deleteWorkload(w)
	return nil
}

func (c *Cache) cleanupAssumedState(w *kueue.QueuedWorkload) {
	k := workload.Key(w)
	assumedCapName, assumed := c.assumedWorkloads[k]
	if assumed {
		// If the workload's assigned capacity is different from the assumed
		// one, then we should also cleanup the assumed one.
		if assumedCapName != string(w.Spec.AssignedCapacity) {
			if assumedCap, exist := c.capacities[assumedCapName]; exist {
				assumedCap.deleteWorkload(w)
			}
		}
		delete(c.assumedWorkloads, k)
	}
}

func (c *Cache) addCapacityToCohort(cap *Capacity, cohortName string) {
	if cohortName == "" {
		return
	}
	cohort, ok := c.cohorts[cohortName]
	if !ok {
		cohort = newCohort(cohortName, 1)
		c.cohorts[cohortName] = cohort
	}
	cohort.members[cap] = struct{}{}
	cap.Cohort = cohort
}

func (c *Cache) deleteCapacityFromCohort(cap *Capacity) {
	if cap.Cohort == nil {
		return
	}
	delete(cap.Cohort.members, cap)
	if len(cap.Cohort.members) == 0 {
		delete(c.cohorts, cap.Cohort.Name)
	}
	cap.Cohort = nil
}

func resourcesByName(in []kueue.Resource) map[corev1.ResourceName][]kueue.ResourceFlavor {
	out := make(map[corev1.ResourceName][]kueue.ResourceFlavor, len(in))
	for _, r := range in {
		flavors := make([]kueue.ResourceFlavor, len(r.Flavors))
		for i := range flavors {
			flavors[i] = *r.Flavors[i].DeepCopy()
		}
		out[r.Name] = flavors
	}
	return out
}

func SetupIndexes(indexer client.FieldIndexer) error {
	return indexer.IndexField(context.Background(), &kueue.QueuedWorkload{}, workloadCapacityKey, func(o client.Object) []string {
		wl := o.(*kueue.QueuedWorkload)
		return []string{string(wl.Spec.AssignedCapacity)}
	})
}
