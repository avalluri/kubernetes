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

package cpumanager

import (
	"fmt"

	"github.com/golang/glog"
	"k8s.io/api/core/v1"
	v1qos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/state"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
//	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
)

// PolicyStatic is the name of the static policy
const PolicyStatic policyName = "static"

var _ Policy = &staticPolicy{}

// staticPolicy is a CPU manager policy that does not change CPU
// assignments for exclusively pinned guaranteed containers after the main
// container process starts.
//
// This policy allocates CPUs exclusively for a container if all the following
// conditions are met:
//
// - The pod QoS class is Guaranteed.
// - The CPU request is a positive integer.
//
// The static policy maintains the following sets of logical CPUs:
//
// - SHARED: Burstable, BestEffort, and non-integral Guaranteed containers
//   run here. Initially this contains all CPU IDs on the system. As
//   exclusive allocations are created and destroyed, this CPU set shrinks
//   and grows, accordingly. This is stored in the state as the default
//   CPU set.
//
// - RESERVED: A subset of the shared pool which is not exclusively
//   allocatable. The membership of this pool is static for the lifetime of
//   the Kubelet. The size of the reserved pool is
//   ceil(systemreserved.cpu + kubereserved.cpu).
//   Reserved CPUs are taken topologically starting with lowest-indexed
//   physical core, as reported by cAdvisor.
//
// - ASSIGNABLE: Equal to SHARED - RESERVED. Exclusive CPUs are allocated
//   from this pool.
//
// - EXCLUSIVE ALLOCATIONS: CPU sets assigned exclusively to one container.
//   These are stored as explicit assignments in the state.
//
// When an exclusive allocation is made, the static policy also updates the
// default cpuset in the state abstraction. The CPU manager's periodic
// reconcile loop takes care of rewriting the cpuset in cgroupfs for any
// containers that may be running in the shared pool. For this reason,
// applications running within exclusively-allocated containers must tolerate
// potentially sharing their allocated CPUs for up to the CPU manager
// reconcile period.
type staticPolicy struct {
	// cpu socket topology
	topology *topology.CPUTopology
	// set of CPUs that is not available for exclusive assignment
	reserved int
}

// Ensure staticPolicy implements Policy interface
var _ Policy = &staticPolicy{}

// NewStaticPolicy returns a CPU manager policy that does not change CPU
// assignments for exclusively pinned guaranteed containers after the main
// container process starts.
func NewStaticPolicy(topology *topology.CPUTopology, numReservedCPUs int) Policy {
	return &staticPolicy{
		topology: topology,
		reserved: numReservedCPUs,
	}
}

func (p *staticPolicy) Name() string {
	return string(PolicyStatic)
}

func (p *staticPolicy) Start(s state.State) {
	allCPUs := p.topology.CPUDetails.CPUs()
	// takeByTopology allocates CPUs associated with low-numbered cores from
	// allCPUs.
	//
	// For example: Given a system with 8 CPUs available and HT enabled,
	// if p.reserved=2, then reserved={0,4}
	reserved, _ := takeByTopology(p.topology, allCPUs, p.reserved)

	if reserved.Size() != p.reserved {
		panic(fmt.Sprintf("[cpumanager] unable to reserve the required amount of CPUs (size of %s did not equal %d)", reserved, p.reserved))
	}

	glog.Infof("[cpumanager] reserved %d CPUs (\"%s\") not available for exclusive assignment", reserved.Size(), reserved)

	cfg := state.DefaultPoolConfig(takeByTopology, p.reserved, p.topology)

	s.SetAllocator(takeByTopology, p.topology)

	if err := s.Reconfigure(cfg); err != nil {
		glog.Errorf("[cpumanager] static policy failed to start: %s\n", err.Error())
		panic("[cpumanager] - please drain node and remove policy state file")
	}

	if err := p.validateState(s); err != nil {
		glog.Errorf("[cpumanager] static policy invalid state: %s\n", err.Error())
		panic("[cpumanager] - please drain node and remove policy state file")
	}
}

func (p *staticPolicy) validateState(s state.State) error {
	pools := s.GetPoolCPUs()
	containers := s.GetPoolAssignments()

	res := pools[state.ReservedPool]
	def := pools[state.DefaultPool]

	// State has already been initialized from file (is not empty)
	// 1. Check that the reserved and default cpusets are disjoint:
	// - kube/system reserved have changed (increased) - may lead to some containers not being able to start
	// - user tampered with file
	if !res.Intersection(def).IsEmpty() {
		return fmt.Errorf("overlapping reserved (%s) and default (%s) CPI pools", res.String(), def.String())
	}

	// 2. Check if state for static policy is consistent: exclusive assignments and (default U reserved) are disjoint.
	resdef := res.Union(def)
	for id, cpus := range containers {
		if !cpus.IsEmpty() {
			if !resdef.Union(cpus).IsEmpty() {
				return fmt.Errorf("container id: %s cpuset: \"%s\" overlaps with default cpuset \"%s\"",
					id, cpus.String(), resdef.String())
			}
		}
	}

	return nil
}

func (p *staticPolicy) AddContainer(s state.State, pod *v1.Pod, container *v1.Container, containerID string) error {
	glog.Infof("[cpumanager] static policy: AddContainer (pod: %s, container: %s, container id: %s)", pod.Name, container.Name, containerID)

	mCPU, flags := requestedCPU(pod, container)
	_, err := s.AllocateCPU(containerID, mCPU, flags, state.DefaultPool)
	if err != nil {
		glog.Errorf("[cpumanager] unable to allocate CPUs (container id: %s, error: %v)", containerID, err)
		return err
	}

	return nil
}

func (p *staticPolicy) RemoveContainer(s state.State, containerID string) error {
	glog.Infof("[cpumanager] static policy: RemoveContainer (container id: %s)", containerID)
	s.ReleaseCPU(containerID)

	return nil
}

func requestedCPU(pod *v1.Pod, container *v1.Container) (int64, state.CpuFlags) {
	var flags state.CpuFlags
	var cpu int64

	if v1qos.GetPodQOS(pod) == v1.PodQOSGuaranteed {
		flags = state.AllocExclusive
	} else {
		flags = state.AllocShared
	}

	if req, ok := container.Resources.Requests[v1.ResourceCPU]; ok {
		cpu = req.MilliValue()
	} else {
		cpu = 0
	}

	return cpu, flags
}
