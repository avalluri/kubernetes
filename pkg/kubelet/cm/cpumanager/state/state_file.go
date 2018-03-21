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
	"encoding/json"
	"fmt"
	"github.com/golang/glog"
	"io/ioutil"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	"os"
	"sync"
)

type stateFileData struct {
	PolicyName    string             `json:"policyName"`
	Pools        *PoolSet            `json:"pools"`
}

var _ State = &stateFile{}

type stateFile struct {
	sync.RWMutex
	stateFilePath string
	policyName    string
	cache         State
}

// NewFileState creates new State for keeping track of cpu/pod assignment with file backend
func NewFileState(filePath string, policyName string) State {
	stateFile := &stateFile{
		stateFilePath: filePath,
		cache:         nil,
		policyName:    policyName,
	}

	if pools, err := stateFile.tryRestoreState(); err != nil {
		// could not restore state, init new state file
		glog.Infof("[cpumanager] state file: initializing empty state file - reason: \"%s\"", err)
		stateFile.cache = NewMemoryState(nil)
		stateFile.storeState()
	} else {
		stateFile.cache = NewMemoryState(pools)
	}

	glog.Infof("[cpumanager] state file path: %s", filePath)

	return stateFile
}

// tryRestoreState tries to read state file, upon any error,
// err message is logged and state is left clean. un-initialized
func (sf *stateFile) tryRestoreState() (*PoolSet, error) {
	sf.Lock()
	defer sf.Unlock()
	var err error
	var pools *PoolSet

	var content []byte

	if content, err = ioutil.ReadFile(sf.stateFilePath); os.IsNotExist(err) {
		// Create file
		if _, err = os.Create(sf.stateFilePath); err != nil {
			glog.Errorf("[cpumanager] state file: unable to create state file \"%s\":%s", sf.stateFilePath, err.Error())
			panic("[cpumanager] state file not created")
		}

		glog.Infof("[cpumanager] state file: created empty state file \"%s\"", sf.stateFilePath)
	} else {
		// File exists - try to read
		var readState stateFileData

		if err = json.Unmarshal(content, &readState); err != nil {
			glog.Warningf("[cpumanager] state file: could not unmarshal, corrupted state file - \"%s\"", sf.stateFilePath)
			return nil, err
		}

		if sf.policyName != readState.PolicyName {
			return nil, fmt.Errorf("policy configured \"%s\" != policy from state file \"%s\"", sf.policyName, readState.PolicyName)
		}

		pools = readState.Pools

		glog.V(2).Infof("[cpumanager] state file: restored state from state file \"%s\"", sf.stateFilePath)
	}

	return pools, nil
}

// saves state to a file, caller is responsible for locking
func (sf *stateFile) storeState() {
	var content []byte
	var err error

	data := stateFileData{
		PolicyName:    sf.policyName,
		Pools:         sf.cache.GetCPUPools(),
	}

	if content, err = json.Marshal(data); err != nil {
		panic("[cpumanager] state file: could not serialize state to json")
	}

	if err = ioutil.WriteFile(sf.stateFilePath, content, 0644); err != nil {
		panic("[cpumanager] state file not written")
	}
}

func (sf *stateFile) GetCPUPools() *PoolSet {
	sf.RLock()
	defer sf.RUnlock()
	return sf.cache.GetCPUPools()
}

func (sf *stateFile) GetCPUSet(containerID string) (cpuset.CPUSet, bool) {
	sf.RLock()
	defer sf.RUnlock()

	res, ok := sf.cache.GetCPUSet(containerID)
	return res, ok
}

func (sf *stateFile) GetDefaultCPUSet() cpuset.CPUSet {
	sf.RLock()
	defer sf.RUnlock()

	return sf.cache.GetDefaultCPUSet()
}

func (sf *stateFile) GetCPUSetOrDefault(containerID string) cpuset.CPUSet {
	sf.RLock()
	defer sf.RUnlock()

	return sf.cache.GetCPUSetOrDefault(containerID)
}

func (sf *stateFile) GetCPUAssignments() ContainerCPUAssignments {
	sf.RLock()
	defer sf.RUnlock()
	return sf.cache.GetCPUAssignments()
}

func (sf *stateFile) GetPoolCPUs() map[string]cpuset.CPUSet {
	sf.RLock()
	defer sf.RUnlock()
	return sf.cache.GetPoolCPUs()
}

func (sf *stateFile) GetPoolAssignments() map[string]cpuset.CPUSet {
	sf.RLock()
	defer sf.RUnlock()
	return sf.cache.GetPoolAssignments()
}

func (sf *stateFile) SetCPUSet(containerID string, cset cpuset.CPUSet) {
	sf.Lock()
	defer sf.Unlock()
	sf.cache.SetCPUSet(containerID, cset)
	sf.storeState()
}

func (sf *stateFile) SetDefaultCPUSet(cset cpuset.CPUSet) {
	sf.Lock()
	defer sf.Unlock()
	sf.cache.SetDefaultCPUSet(cset)
	sf.storeState()
}

func (sf *stateFile) SetCPUAssignments(a ContainerCPUAssignments) {
	sf.Lock()
	defer sf.Unlock()
	sf.cache.SetCPUAssignments(a)
	sf.storeState()
}

func (sf *stateFile) Delete(containerID string) {
	sf.Lock()
	defer sf.Unlock()
	sf.cache.Delete(containerID)
	sf.storeState()
}

func (sf *stateFile) ClearState() {
	sf.Lock()
	defer sf.Unlock()
	sf.cache.ClearState()
	sf.storeState()
}

func (sf *stateFile) SetAllocator(allocfn AllocCpuFunc, t *topology.CPUTopology) {
	sf.cache.SetAllocator(allocfn, t)
}

func (sf *stateFile) Reconfigure(cfg PoolConfig) error {
	sf.Lock()
	defer sf.Unlock()

	err := sf.cache.Reconfigure(cfg)
	if err == nil {
		sf.storeState()
	}

	return err
}

func (sf *stateFile) AllocateCPU(id string, milliCPU int64, flags CpuFlags, pool string) (*cpuset.CPUSet, error) {
	sf.Lock()
	defer sf.Unlock()

	cset, err := sf.cache.AllocateCPU(id, milliCPU, flags, pool)
	sf.storeState()

	return cset, err
}

func (sf *stateFile) ReleaseCPU(id string) {
	sf.Lock()
	defer sf.Unlock()
	sf.cache.ReleaseCPU(id)
	sf.storeState()
}
