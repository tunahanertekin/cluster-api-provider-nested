/*
Copyright 2021 The Kubernetes Authors.

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

package engine

import (
	"fmt"
	"sync"

	"k8s.io/klog/v2"

	"sigs.k8s.io/cluster-api-provider-nested/virtualcluster/experiment/pkg/scheduler/algorithm"
	internalcache "sigs.k8s.io/cluster-api-provider-nested/virtualcluster/experiment/pkg/scheduler/cache"
	"sigs.k8s.io/cluster-api-provider-nested/virtualcluster/experiment/pkg/scheduler/util"
)

type Engine interface {
	ScheduleNamespace(*internalcache.Namespace) (*internalcache.Namespace, error)
	EnsureNamespacePlacements(*internalcache.Namespace) error
	DeScheduleNamespace(key string) error
	SchedulePod(pod *internalcache.Pod) (*internalcache.Pod, error)
	DeSchedulePod(key string) error
}

var _ Engine = &schedulerEngine{}

type schedulerEngine struct {
	mu sync.RWMutex

	cache internalcache.Cache
}

func NewSchedulerEngine(schedulerCache internalcache.Cache) Engine {
	return &schedulerEngine{cache: schedulerCache}
}

func GetSlicesToSchedule(namespace *internalcache.Namespace, oldPlacements map[string]int) algorithm.SliceInfoArray {
	key := namespace.GetKey()
	slicesToSchedule := make(algorithm.SliceInfoArray, 0)
	size := namespace.GetQuotaSlice()

	remainingToSchedule := namespace.GetTotalSlices()
	// handle slices that have mandatory placements
	// TODO: sorting the mandatory placements
	for cluster, num := range namespace.GetPlacementMap() {
		if remainingToSchedule == 0 {
			// it is possible when namespace quota is reduced
			break
		}
		mandatory := util.Min(num, remainingToSchedule)
		if val, ok := oldPlacements[cluster]; ok {
			used := util.Min(val, mandatory)
			oldPlacements[cluster] = val - used
		}
		slicesToSchedule.Repeat(mandatory, key, size, cluster, "")
		remainingToSchedule = remainingToSchedule - mandatory
	}

	// use old placements as hints
	// TODO: sorting the oldPlacements
	for cluster, num := range oldPlacements {
		if remainingToSchedule == 0 {
			break
		}
		hinted := util.Min(num, remainingToSchedule)
		slicesToSchedule.Repeat(hinted, key, size, "", cluster)
		remainingToSchedule = remainingToSchedule - hinted
	}
	slicesToSchedule.Repeat(remainingToSchedule, key, size, "", "")
	return slicesToSchedule
}

func GetNewPlacement(slices algorithm.SliceInfoArray) (map[string]int, error) {
	newPlacement := make(map[string]int)
	for _, each := range slices {
		if each.Err != nil {
			return nil, each.Err
		}
		newPlacement[each.Result] = newPlacement[each.Result] + 1
	}
	return newPlacement, nil
}

func (e *schedulerEngine) ScheduleNamespace(namespace *internalcache.Namespace) (*internalcache.Namespace, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// The namespace may already exist in cache. The reasons could be:
	// 1. it was scheduled successfully but the result was failed to be updated in tenant namespace;
	// 2. it is rescheduled due to the namespace quota change or previous placement results were manually modified;
	// All slices have to be re-examined against the cache since some placed clusters may become invalid. However,
	// we can use old placement as hint for new placement. The idea is that we should maximally avoid
	// changing the placement clusters since the overhead of switching super clusters is nontrivial.
	var oldPlacements map[string]int
	key := namespace.GetKey()
	curState := e.cache.GetNamespace(key)
	if curState != nil {
		if !namespace.Comparable(curState) {
			return nil, fmt.Errorf("updating namespace with quotaslcie change is not supported")
		}
		oldPlacements = curState.GetPlacementMap()
	}

	var newPlacement map[string]int
	var snapshot *internalcache.NamespaceSchedSnapshot
	var err error
	slicesToSchedule := GetSlicesToSchedule(namespace, oldPlacements)
	snapshot, err = e.cache.SnapshotForNamespaceSched(curState)
	if err != nil {
		return nil, err
	}
	slicesToSchedule = algorithm.ScheduleNamespaceSlices(slicesToSchedule, snapshot)
	newPlacement, err = GetNewPlacement(slicesToSchedule)
	if err != nil {
		return nil, err
	}
	ret := namespace.DeepCopy()
	ret.SetNewPlacements(newPlacement)

	// update the cache
	if curState != nil {
		err = e.cache.UpdateNamespace(curState, ret)
	} else {
		err = e.cache.AddNamespace(ret)
	}
	return ret, err
}

func (e *schedulerEngine) DeScheduleNamespace(key string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ns := e.cache.GetNamespace(key); ns != nil {
		e.cache.RemoveNamespace(ns)
	} else {
		klog.V(4).Infof("the namespace %s has been removed, deschedule is not needed", key)
	}
	return nil
}

func (e *schedulerEngine) EnsureNamespacePlacements(namespace *internalcache.Namespace) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if ns := e.cache.GetNamespace(namespace.GetKey()); ns != nil {
		if !namespace.Comparable(ns) {
			return fmt.Errorf("updating namespace with quotaslcie change is not supported")
		}
		return e.cache.UpdateNamespace(ns, namespace)
	}
	return e.cache.AddNamespace(namespace)
}

func (e *schedulerEngine) SchedulePod(pod *internalcache.Pod) (*internalcache.Pod, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	nsKey := pod.GetNamespaceKey()
	ns := e.cache.GetNamespace(nsKey)
	if ns == nil {
		return nil, fmt.Errorf("namespace %s has not been schduled", nsKey)
	}

	snapshot, err := e.cache.SnapshotForPodSched(pod)
	if err != nil {
		return nil, err
	}

	result, err := algorithm.SchedulePod(pod, snapshot)
	if err != nil {
		return nil, err
	}

	ret := pod.DeepCopy()
	ret.SetCluster(result)

	err = e.cache.AddPod(ret)

	return ret, err
}

func (e *schedulerEngine) DeSchedulePod(key string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if pod := e.cache.GetPod(key); pod != nil {
		e.cache.RemovePod(pod)
	} else {
		klog.V(4).Infof("the pod %s has been removed, deschedule is not needed", key)
	}
	return nil
}
