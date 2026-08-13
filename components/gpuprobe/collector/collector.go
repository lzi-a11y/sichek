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
	"sync/atomic"
	"time"

	"github.com/scitix/sichek/components/common"
	"github.com/scitix/sichek/components/gpuprobe/config"
	"github.com/sirupsen/logrus"
)

type Collector struct {
	name     string
	spec     *config.GpuProbeSpec
	probing  int32        // reentrancy latch (atomic CAS)
	lastInfo atomic.Value // *GpuProbeInfo, returned when a prior round is still running
}

func NewGpuProbeCollector(spec *config.GpuProbeSpec) *Collector {
	return &Collector{name: "GpuProbeCollector", spec: spec}
}

func (c *Collector) Name() string { return c.name }

func (c *Collector) Collect(ctx context.Context) (common.Info, error) {
	// Reentrancy latch: if a prior round is still running (e.g. a hung probe in its
	// grace window), return the last result rather than probing the same card twice.
	if !atomic.CompareAndSwapInt32(&c.probing, 0, 1) {
		logrus.WithField("component", "gpuprobe").Warn("previous probe round still running; skip this tick")
		if v := c.lastInfo.Load(); v != nil {
			return v.(*GpuProbeInfo), nil
		}
		return &GpuProbeInfo{Time: time.Now()}, nil
	}
	defer atomic.StoreInt32(&c.probing, 0)

	info := &GpuProbeInfo{Time: time.Now()}
	stats, err := queryGPUs(ctx)
	if err != nil || len(stats) == 0 {
		// No GPU / nvidia-smi unavailable → empty result, component stays Normal.
		logrus.WithField("component", "gpuprobe").Infof("no GPU / nvidia-smi unavailable (err=%v); nothing to probe", err)
		c.lastInfo.Store(info)
		return info, nil
	}

	for _, st := range stats {
		r := GpuProbeResult{Index: st.Index, BDF: st.BDF}
		if c.spec.SkipMig && st.MigEnabled {
			r.State, r.Outcome, r.Detail = "busy", OutcomeSkip, "MIG enabled, not supported"
			info.PerGPU = append(info.PerGPU, r)
			continue
		}
		busy := st.FreePct < c.spec.MinFreeMemPct || st.UtilPct > c.spec.MaxGpuUtilPct
		if c.spec.SkipIfComputeApps && countComputeApps(ctx, st.Index) > 0 {
			busy = true
		}
		if busy {
			r.State, r.Outcome = "busy", OutcomeSkip
			r.Detail = "busy: skipped to avoid interfering with workload"
			info.PerGPU = append(info.PerGPU, r)
			continue
		}
		r.State = "idle"
		r.Outcome, r.ExitCode, r.Detail, r.DurationMs = runProbe(
			ctx, c.spec.ProbeBinaryPath, st.Index, c.spec.MinFreeMemPct,
			c.spec.ProbeTimeoutSec, c.spec.KillGraceSec)
		info.PerGPU = append(info.PerGPU, r)
	}
	c.lastInfo.Store(info)
	return info, nil
}
