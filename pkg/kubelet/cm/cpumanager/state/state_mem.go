/*
Copyright 2017 The Kubernetes Authors.

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

package state

import (
	"sync"
	"fmt"

	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
)

type stateMemory struct {
	sync.RWMutex
	PoolSet
}

var _ State = &stateMemory{}

// NewMemoryState creates new State for keeping track of cpu/pod assignment
func NewMemoryState(ps *PoolSet) State {
	var err error

	glog.Infof("[cpumanager] initializing new in-memory state store")

	if ps == nil {
		if ps, err = NewPoolSet(nil); err != nil {
			panic(fmt.Sprintf("[cpumanager] failed to create in-memory state store: %v", err))
		}
	}

	return &stateMemory{
		PoolSet: *ps,
	}
}

func (s *stateMemory) GetCPUPools() *PoolSet {
	return &s.PoolSet
}

func (s *stateMemory) GetCPUSet(containerID string) (cpuset.CPUSet, bool) {
	s.RLock()
	defer s.RUnlock()

	return s.PoolSet.GetContainerCPUSet(containerID)
}

func (s *stateMemory) GetDefaultCPUSet() cpuset.CPUSet {
	cset, _ := s.PoolSet.GetPoolCPUSet(DefaultPool)

	return cset
}

func (s *stateMemory) GetCPUSetOrDefault(containerID string) cpuset.CPUSet {
	s.RLock()
	defer s.RUnlock()

	cset, _ := s.PoolSet.GetContainerCPUSet(containerID)

	return cset
}

func (s *stateMemory) GetCPUAssignments() ContainerCPUAssignments {
	s.RLock()
	defer s.RUnlock()

	return s.PoolSet.GetCPUAssignments()
}

func (s *stateMemory) SetCPUSet(containerID string, cset cpuset.CPUSet) {
	glog.Warningf("[cpumanager] deprecated SetCPUSet called")
}

func (s *stateMemory) SetDefaultCPUSet(cset cpuset.CPUSet) {
	glog.Warningf("[cpumanager] deprecated SetDefaultCPUSet called")
}

func (s *stateMemory) SetCPUAssignments(a ContainerCPUAssignments) {
	glog.Warningf("[cpumanager] deprecated SetCPUAssignments called")
}

func (s *stateMemory) Delete(containerID string) {
	glog.Warningf("[cpumanager] deprecated Delete called")
}

func (s *stateMemory) ClearState() {
	s.Lock()
	defer s.Unlock()

	ps, err := NewPoolSet(nil)
	if err != nil {
		glog.Warningf("[cpumanager] clearing state failed: %s", err.Error())
	} else {
		s.PoolSet = *ps
		glog.V(2).Infof("[cpumanager] cleared state")
	}
}
