/*
Copyright 2018 The Kubernetes Authors.

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

//
// Notes: CPU and core is used interchangably here, both referring to CPU cores.
//

package state

import (
	"fmt"
	"strings"
	"math"
	"io/ioutil"
	"encoding/json"

	"github.com/golang/glog"

	"k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"

	admission "k8s.io/kubernetes/plugin/pkg/admission/cpupool"
)

// Predefined CPU pool names.
const (
	IgnoredPool  = admission.IgnoredPool  // CPUs we have to ignore
	OfflinePool  = admission.OfflinePool  // CPUs which are offline
	ReservedPool = admission.ReservedPool // CPUs reserved for kube and system
	DefaultPool  = admission.DefaultPool  // CPUs in the default set
)

// CPU allocation flags
type CpuFlags int

const (
	AllocShared    CpuFlags = 0x00 // allocate to shared set in pool
	AllocExclusive CpuFlags = 0x01 // allocate exclusively in pool
	KubePinned     CpuFlags = 0x00 // we take care of CPU pinning
	WorkloadPinned CpuFlags = 0x02 // workload takes care of CPU pinning
	DefaultFlags   CpuFlags = AllocShared | KubePinned
)

// Node CPU pool configuration (PoolSetConfig could be a more apt name).
type PoolConfig map[string]cpuset.CPUSet

// A container assigned to run in a pool.
type PoolContainer struct {
	id   string         // container ID
	pool string         // assigned pool
	cpus cpuset.CPUSet  // exclusive CPUs, if any
	mCPU int64          // requested milliCPUs
}

// A CPU pool is a set of cores, typically set aside for a class of workloads.
type Pool struct {
	shared cpuset.CPUSet    // shared set of CPUs
	exclusive cpuset.CPUSet // exclusively allocated CPUs
}

// A CPU allocator function.
type AllocCpuFunc func(*topology.CPUTopology, cpuset.CPUSet, int) (cpuset.CPUSet, error)

// All pools available for kube on this node.
type PoolSet struct {
	active      PoolConfig               // active pool configuration
	pools       map[string]Pool          // all CPU pools
	containers  map[string]PoolContainer // containers assignments
	target      PoolConfig               // requested pool configuration
	topology   *topology.CPUTopology     // CPU topology info
	allocfn     AllocCpuFunc             // CPU allocator function
}

// Create a default pool configuration (all except reserved CPUs in default pool).
func DefaultPoolConfig(allocfn AllocCpuFunc, reserve int, t *topology.CPUTopology) PoolConfig {
	all := t.CPUDetails.CPUs()
	res, _ := allocfn(t, all, reserve)
	def := all.Difference(res)

	cfg := make(PoolConfig)
	cfg[ReservedPool] = res
	cfg[DefaultPool] = def

	return cfg
}

// Parse the given CPU pool configuration (file or string).
func ParsePoolConfig(config string, allCPUs cpuset.CPUSet) (PoolConfig, error) {
	var data []byte
	var err error
	var pools map[string]string

	if strings.TrimLeft(config, " \t\n\r")[0] == '{' {
		data = []byte(config)
	} else {
		if data, err = ioutil.ReadFile(config); err != nil {
			return nil, err
		}
	}

	if err = json.Unmarshal(data, &pools); err != nil {
		return nil, err
	}

	cfg := make(PoolConfig)
	free := allCPUs.Clone()

	for name, cpus := range pools {
		if cpus != "*" {
			if cfg[name], err = cpuset.Parse(cpus); err != nil {
				return nil, err
			}
			free = free.Difference(cfg[name])
		} else {
			cfg[name] = free
			free = cpuset.NewCPUSet()
		}
	}

	return cfg, nil
}

// Get the CPU pool, request, and limit of a container.
func GetContainerPoolResources(c *v1.Container) (string, int64, int64) {
	var pool string = DefaultPool
	var req, lim int64

	if c.Resources.Requests == nil {
		return DefaultPool, 0, 0
	}

	for name, _ := range c.Resources.Requests {
		if strings.HasPrefix(name.String(), admission.ResourcePrefix) {
			pool = strings.TrimPrefix(name.String(), admission.ResourcePrefix)
			break
		}
	}

	if res, ok := c.Resources.Requests[v1.ResourceCPU]; ok {
		req = res.MilliValue()
	}

	if res, ok := c.Resources.Limits[v1.ResourceCPU]; ok {
		lim = res.MilliValue()
	}

	return pool, req, lim
}

// Create a new CPU pool set with the given configuration.
func NewPoolSet(cfg PoolConfig) (*PoolSet, error) {
	glog.Infof("[cpumanager]: creating new CPU pool set")

	var ps *PoolSet = &PoolSet{
		pools:      make(map[string]Pool),
		containers: make(map[string]PoolContainer),
	}

	if err := ps.Reconfigure(cfg); err != nil {
		return nil, err
	}

	return ps, nil
}

// Verify the current pool state.
func (ps *PoolSet) Verify() error {
	required := []string{ ReservedPool, DefaultPool }

	for _, name := range required {
		if _, ok := ps.pools[name]; !ok {
			return fmt.Errorf("[cpumanager/pool]: missing %s pool", name)
		}
	}

	return nil
}

// Reconfigure the CPU pool set.
func (ps *PoolSet) Reconfigure(cfg PoolConfig) error {
	if cfg == nil {
		return nil
	}

	glog.Infof("[cpumanager]: reconfiguring CPU pools with %v", cfg)

	ps.target = cfg
	_, err := ps.ReconcileConfig()

	return err
}

// Run one round of reconcilation of the CPU pool set configuration.
func (ps *PoolSet) ReconcileConfig() (bool, error) {
	glog.Infof("[cpumanager]: trying to reconciling configuration: %v -> %v", ps.active, ps.target)

	if ps.target == nil {
		return false, nil
	}

	//
	// trivial case: no active container assignments
	//
	// Discard everything, and take the configuration in use.
	//
	if len(ps.containers) == 0 {
		ps.active     = ps.target
		ps.target     = nil
		ps.pools      = make(map[string]Pool)
		ps.containers = make(map[string]PoolContainer)

		for name, cpus := range ps.active {
			ps.pools[name] = Pool{
				shared:    cpus.Clone(),
				exclusive: cpuset.NewCPUSet(),
			}
		}

		for name, cpus := range ps.active {
			ps.pools[name] = Pool{
				shared:    cpus.Clone(),
				exclusive: cpuset.NewCPUSet(),
			}
		}

		if err := ps.Verify(); err != nil {
			return false, err
		}

		return true, nil
	}

	return false, nil
}

// Set the CPU allocator function, and CPU topology information.
func (ps *PoolSet) SetAllocator(allocfn AllocCpuFunc, t *topology.CPUTopology) {
	ps.allocfn  = allocfn
	ps.topology = t
}

// Allocate CPU for a container from a pool.
func (ps *PoolSet) AllocateCPU(id string, milliCPU int64, flags CpuFlags, pool string) (*cpuset.CPUSet, error) {
	var cpus cpuset.CPUSet

	p, ok := ps.pools[pool]
	if !ok {
		return nil, fmt.Errorf("cpumanager/pool] pool %s not found", pool)
	}

	if milliCPU % 1000 != 0 {
		flags &^= AllocExclusive
	}

	if flags & AllocExclusive != 0 {
		ncpu := int(math.Ceil(float64(milliCPU) / 1000))
		cpus, err := ps.allocfn(ps.topology, p.shared, ncpu)
		if err != nil {
			return nil, err
		}

		p.shared = p.shared.Difference(cpus)
		p.exclusive = p.exclusive.Union(cpus)

		ps.containers[id] = PoolContainer{
			id:   id,
			pool: pool,
			cpus: cpus,
			mCPU: milliCPU,
		}

		cpus = cpus.Clone()

		glog.Infof("[cpumanager/pool] container %s got exclusive %s.%s", id, pool, cpus.String())
	} else {
		ps.containers[id] = PoolContainer{
			id:   id,
			pool: pool,
			cpus: cpuset.NewCPUSet(),
			mCPU: milliCPU,
		}

		cpus = p.shared.Clone()

		glog.Infof("[cpumanager/pool] container %s got shared %d mCPU of %s.%s", id, milliCPU, pool, cpus.String())
	}

	return &cpus, nil
}

// Return CPU from a container to a pool.
func (ps *PoolSet) ReleaseCPU(id string) {
	c, ok := ps.containers[id]
	if !ok {
		glog.Warningf("[cpumanager/pool] couldn't find allocations for container %s", id)
		return
	}

	delete(ps.containers, id)

	p, ok := ps.pools[c.pool]
	if !ok {
		glog.Warningf("[cpumanager/pool] couldn't find pool %s for container %s", c.pool, id)
		return
	}

	p.shared    = p.shared.Union(c.cpus)
	p.exclusive = p.exclusive.Difference(c.cpus)
}

// Get the (shared) CPU sets for pools.
func (ps *PoolSet) GetPoolCPUs() map[string]cpuset.CPUSet {
	cpus := make(map[string]cpuset.CPUSet)

	for name, p := range ps.pools {
		cpus[name] = p.shared.Clone()
	}

	return cpus
}

// Get the exclusively allocated CPU sets.
func (ps *PoolSet) GetPoolAssignments() map[string]cpuset.CPUSet {
	cpus := make(map[string]cpuset.CPUSet)

	for id, c := range ps.containers {
		cpus[id] = c.cpus.Clone()
	}

	return cpus
}

// Get the CPU allocations for a container.
func (ps *PoolSet) GetContainerCPUSet(id string) (cpuset.CPUSet, bool) {
	c, ok := ps.containers[id]
	if !ok {
		return cpuset.NewCPUSet(), false
	}

	cpus := c.cpus.Clone()

	if c.pool == DefaultPool {
		cpus = cpus.Union(ps.pools[ReservedPool].shared)
	}

	return cpus, true
}

// Get the shared CPUs of a pool.
func (ps *PoolSet) GetPoolCPUSet(pool string) (cpuset.CPUSet, bool) {
	p, ok := ps.pools[pool]
	if !ok {
		return cpuset.NewCPUSet(), false
	}

	return p.shared.Clone(), true
}

// Get the exclusive CPU assignments as ContainerCPUAssignments.
func (ps *PoolSet) GetCPUAssignments() ContainerCPUAssignments {
	a := make(map[string]cpuset.CPUSet)

	for _, c := range ps.containers {
		if !c.cpus.IsEmpty() {
			a[c.id] = c.cpus.Clone()
		}
	}

	return a
}

//
// JSON mashalling and unmarshalling
//


// PoolContainer JSON marshalling interface
type marshalPoolContainer struct {
	Id   string        `json:"id"`
	Pool string        `json:"pool"`
	Cpus cpuset.CPUSet `json:"cpus"`
	MCPU int64         `json:"mCPU"`
}

func (pc PoolContainer) MarshalJSON() ([]byte, error) {
	return json.Marshal(marshalPoolContainer{
		Id:   pc.id,
		Pool: pc.pool,
		Cpus: pc.cpus,
		MCPU: pc.mCPU,
	})
}

func (pc *PoolContainer) UnmarshalJSON(b []byte) error {
	var m marshalPoolContainer

	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}

	pc.id   = m.Id
	pc.pool = m.Pool
	pc.cpus = m.Cpus
	pc.mCPU = m.MCPU

	return nil
}

// Pool JSON marshalling interface
type marshalPool struct {
	Shared    cpuset.CPUSet `json:"shared"`
	Exclusive cpuset.CPUSet `json:"exclusive"`
}

func (p Pool) MarshalJSON() ([]byte, error) {
	return json.Marshal(marshalPool{
		Shared:    p.shared,
		Exclusive: p.exclusive,
	})
}

func (p *Pool) UnmarshalJSON(b []byte) error {
	var m marshalPool

	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}

	p.shared    = m.Shared
	p.exclusive = m.Exclusive

	return nil
}

// PoolSet JSON marshalling interface
type marshalPoolSet struct {
	Active     PoolConfig               `json:"active"`
	Pools      map[string]Pool          `json:"pools"`
	Containers map[string]PoolContainer `json:"containers"`
	Target     PoolConfig               `json:"target,omitempty"`
}

func (ps PoolSet) MarshalJSON() ([]byte, error) {
	return json.Marshal(marshalPoolSet{
		Active:     ps.active,
		Pools:      ps.pools,
		Containers: ps.containers,
		Target:     ps.target,
	})
}

func (ps *PoolSet) UnmarshalJSON(b []byte) error {
	var m marshalPoolSet

	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}

	ps.active     = m.Active
	ps.pools      = m.Pools
	ps.containers = m.Containers
	ps.target     = m.Target

	return nil
}
