/*
Copyright 2017 Gravitational, Inc.

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

package monitoring

import (
	"bufio"
	"context"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/gravitational/satellite/agent/health"
	pb "github.com/gravitational/satellite/agent/proto/agentpb"

	"github.com/gravitational/trace"
)

// NewKernelModuleChecker creates a new kernel module checker
func NewKernelModuleChecker(names ...string) KernelModuleChecker {
	return KernelModuleChecker{
		Modules:    names,
		getModules: ReadModules,
	}
}

// Name returns name of the checker
func (r KernelModuleChecker) Name() string {
	return kernelModuleCheckerID
}

// Check determines if the modules specified with r.Modules have been loaded
func (r KernelModuleChecker) Check(ctx context.Context, reporter health.Reporter) {
	err := r.check(ctx, reporter)
	if err != nil {
		reporter.Add(NewProbeFromErr(r.Name(), "", trace.Wrap(err)))
		return
	}
	reporter.Add(&pb.Probe{Checker: r.Name(), Status: pb.Probe_Running})
}

func (r KernelModuleChecker) check(ctx context.Context, reporter health.Reporter) error {
	modules, err := r.getModules()
	if err != nil {
		return trace.Wrap(err)
	}

	var errors []error
	for _, module := range r.Modules {
		if !modules.WasLoaded(module) {
			errors = append(errors, trace.NotFound("kernel module %q not loaded", module))
		}
	}

	if len(errors) == 1 {
		return trace.Wrap(errors[0])
	}
	return trace.NewAggregate(errors...)
}

// KernelModuleChecker checks if the specified set of kernel modules are loaded
type KernelModuleChecker struct {
	// Modules lists required kernel modules
	Modules    []string
	getModules moduleGetterFunc
}

// ReadModules reads list of kernel modules from /proc/modules
func ReadModules() (Modules, error) {
	f, err := os.Open("/proc/modules")
	if err != nil {
		return nil, trace.ConvertSystemError(err)
	}
	defer f.Close()

	modules, err := ReadModulesFrom(f)
	if err != nil {
		return nil, trace.ConvertSystemError(err)
	}

	return modules, nil
}

// ReadModulesFrom reads list of kernel modules from the specified reader.
func ReadModulesFrom(r io.Reader) (modules Modules, err error) {
	s := bufio.NewScanner(r)

	modules = Modules{}
	for s.Scan() {
		line := s.Text()
		module, err := parseModule(line)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		modules[module.Name] = *module
	}

	if s.Err() != nil {
		return nil, trace.ConvertSystemError(err)
	}

	return modules, nil
}

// WasLoaded determines whether module name is loaded.
func (r Modules) WasLoaded(name string) bool {
	_, loaded := r[name]
	return loaded
}

// Modules lists kernel modules
type Modules map[string]Module

// IsLoaded determines if this module is loaded
func (r Module) IsLoaded() bool {
	return r.ModuleState == ModuleStateLive
}

// Module describes a kernel module
type Module struct {
	// ModuleState specifies the state of the module: live, loading/unloading
	ModuleState
	// Name identifies the module
	Name string
	// Instances specifies the number of instances this module has loaded
	Instances int
}

// parseModule parses module information from a single line of /proc/modules
// https://www.centos.org/docs/5/html/Deployment_Guide-en-US/s1-proc-topfiles.html#s2-proc-modules
func parseModule(moduleS string) (*Module, error) {
	columns := strings.SplitN(moduleS, " ", len(moduleColumns))
	if len(columns) != len(moduleColumns) {
		return nil, trace.BadParameter("invalid input: expected six whitespace-separated columns, but got %q",
			moduleS)
	}

	instanceS := columns[2]
	instances, err := strconv.ParseInt(instanceS, 10, 32)
	if err != nil {
		return nil, trace.BadParameter("invalid instances field: expected integer, but got %q", instanceS)
	}

	return &Module{
		ModuleState: ModuleState(columns[4]),
		Name:        columns[0],
		Instances:   int(instances),
	}, nil
}

// ModuleState describes the state of a kernel module
type ModuleState string

const (
	// ModuleStateLive defines a live (loaded) module
	ModuleStateLive ModuleState = "Live"
	// ModuleStateLoading defines a loading module
	ModuleStateLoading = "Loading"
	// ModuleStateUnloading defines an unloading module
	ModuleStateUnloading = "Unloading"
)

type moduleGetterFunc func() (Modules, error)

const kernelModuleCheckerID = "kernel-module"

var moduleColumns = []string{"name", "memory_size", "instances", "dependencies", "state", "memory_offset"}