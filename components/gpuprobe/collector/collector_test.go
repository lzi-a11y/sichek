/*
Copyright 2024 The Scitix Authors.

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
package collector

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/scitix/sichek/components/gpuprobe/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var realNvidiaSmi = nvidiaSmi

// idleOneGPU returns a stub that reports 1 idle GPU (util 0, ~99% free, no MIG, no procs).
func idleOneGPU() func(ctx context.Context, args ...string) (string, error) {
	return func(ctx context.Context, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "--query-compute-apps=pid" {
			return "", nil
		}
		return "0, 00000000:BB:00.0, 0, 100, 81920, Disabled\n", nil
	}
}

func writeFakeProbe(t *testing.T, exitCode int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gpu_probe")
	require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\necho RESULT=stub\nexit "+strconv.Itoa(exitCode)+"\n"), 0755))
	return p
}

func writeSleepProbe(t *testing.T, seconds int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gpu_probe")
	require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\nsleep "+strconv.Itoa(seconds)+"\n"), 0755))
	return p
}

func TestCollect_IdleGPUPass(t *testing.T) {
	nvidiaSmi = idleOneGPU()
	defer func() { nvidiaSmi = realNvidiaSmi }()
	spec := config.DefaultSpec()
	spec.ProbeBinaryPath = writeFakeProbe(t, 0)
	info, err := NewGpuProbeCollector(spec).Collect(context.Background())
	require.NoError(t, err)
	gi := info.(*GpuProbeInfo)
	require.Len(t, gi.PerGPU, 1)
	assert.Equal(t, "idle", gi.PerGPU[0].State)
	assert.Equal(t, OutcomePass, gi.PerGPU[0].Outcome)
	assert.Equal(t, "bb:00", gi.PerGPU[0].BDF)
}

func TestCollect_BusyGPUSkipped(t *testing.T) {
	nvidiaSmi = func(ctx context.Context, args ...string) (string, error) {
		return "0, 00000000:BB:00.0, 90, 80000, 81920, Disabled\n", nil // util=90, near-full mem
	}
	defer func() { nvidiaSmi = realNvidiaSmi }()
	spec := config.DefaultSpec()
	spec.ProbeBinaryPath = writeFakeProbe(t, 1) // even a FAILing probe must NOT run on a busy card
	info, _ := NewGpuProbeCollector(spec).Collect(context.Background())
	gi := info.(*GpuProbeInfo)
	assert.Equal(t, "busy", gi.PerGPU[0].State)
	assert.Equal(t, OutcomeSkip, gi.PerGPU[0].Outcome)
}

func TestCollect_IdleGPUFail(t *testing.T) {
	nvidiaSmi = idleOneGPU()
	defer func() { nvidiaSmi = realNvidiaSmi }()
	spec := config.DefaultSpec()
	spec.ProbeBinaryPath = writeFakeProbe(t, 1)
	info, _ := NewGpuProbeCollector(spec).Collect(context.Background())
	gi := info.(*GpuProbeInfo)
	assert.Equal(t, OutcomeFail, gi.PerGPU[0].Outcome)
	assert.Equal(t, 1, gi.PerGPU[0].ExitCode)
}

func TestCollect_MissingBinaryExecErr(t *testing.T) {
	nvidiaSmi = idleOneGPU()
	defer func() { nvidiaSmi = realNvidiaSmi }()
	spec := config.DefaultSpec()
	spec.ProbeBinaryPath = "/nonexistent/gpu_probe"
	info, _ := NewGpuProbeCollector(spec).Collect(context.Background())
	gi := info.(*GpuProbeInfo)
	assert.Equal(t, OutcomeExecErr, gi.PerGPU[0].Outcome)
}

func TestCollect_IdleGPUTimeoutDoesNotWedge(t *testing.T) {
	nvidiaSmi = idleOneGPU()
	defer func() { nvidiaSmi = realNvidiaSmi }()
	spec := config.DefaultSpec()
	spec.ProbeBinaryPath = writeSleepProbe(t, 10)
	spec.ProbeTimeoutSec = 1
	spec.KillGraceSec = 1
	start := time.Now()
	info, _ := NewGpuProbeCollector(spec).Collect(context.Background())
	elapsed := time.Since(start)
	gi := info.(*GpuProbeInfo)
	assert.Equal(t, OutcomeTimeout, gi.PerGPU[0].Outcome)
	assert.Less(t, elapsed, 5*time.Second) // proves the 10s sleep did not wedge us
}

func TestCollect_MigSkipped(t *testing.T) {
	nvidiaSmi = func(ctx context.Context, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "--query-compute-apps=pid" {
			return "", nil
		}
		return "0, 00000000:BB:00.0, 0, 100, 81920, Enabled\n", nil
	}
	defer func() { nvidiaSmi = realNvidiaSmi }()
	spec := config.DefaultSpec()
	spec.ProbeBinaryPath = writeFakeProbe(t, 0)
	info, _ := NewGpuProbeCollector(spec).Collect(context.Background())
	gi := info.(*GpuProbeInfo)
	assert.Equal(t, OutcomeSkip, gi.PerGPU[0].Outcome)
	assert.Contains(t, gi.PerGPU[0].Detail, "MIG")
}

func TestCollect_NoGPU(t *testing.T) {
	nvidiaSmi = func(ctx context.Context, args ...string) (string, error) { return "", nil }
	defer func() { nvidiaSmi = realNvidiaSmi }()
	info, err := NewGpuProbeCollector(config.DefaultSpec()).Collect(context.Background())
	require.NoError(t, err)
	assert.Empty(t, info.(*GpuProbeInfo).PerGPU)
}
