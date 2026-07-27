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
package checker

import (
	"context"
	"fmt"
	"testing"

	"github.com/scitix/sichek/components/infiniband/collector"
	"github.com/scitix/sichek/consts"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// railDev describes one IB device as the collector would report it.
type railDev struct {
	ibDev string
	rate  string // raw sysfs port rate, e.g. "400 Gb/sec (4X NDR)"
	port  int    // 0 means single-port; >0 lets a device appear twice
}

func hwInfoFromRailDevs(devs []railDev) map[string]collector.IBHardWareInfo {
	hws := make(map[string]collector.IBHardWareInfo, len(devs))
	for i, d := range devs {
		port := d.port
		if port == 0 {
			port = 1
		}
		// Key must be unique per (device, port); the real collector uses
		// HWInfoKey for this.
		key := fmt.Sprintf("%s_p%d_%d", d.ibDev, port, i)
		hws[key] = collector.IBHardWareInfo{
			IBDev:     d.ibDev,
			Port:      port,
			PortSpeed: d.rate,
		}
	}
	return hws
}

func rail(rate string, names ...string) []railDev {
	devs := make([]railDev, 0, len(names))
	for _, n := range names {
		devs = append(devs, railDev{ibDev: n, rate: rate})
	}
	return devs
}

// TestIBRailCount_FleetTopologies pins the checker against the real topologies
// measured across the fleet on 2026-07-27. Every healthy node must stay silent
// — a false Warning on any of these would make the check unusable — and
// lmg132, which had lost 0000:69:01.0, must be flagged.
func TestIBRailCount_FleetTopologies(t *testing.T) {
	tests := []struct {
		name         string
		devs         []railDev
		wantAbnormal bool
		wantCount    string
	}{
		{
			// longmen-g86-132: BF3 CX7 at 0000:69:01.0 failed its mlx5_core
			// probe, leaving mlx5_1/2/4 where four rails are expected.
			name:         "lmg132 with one lost rail is flagged",
			devs:         rail("400 Gb/sec (4X NDR)", "mlx5_1", "mlx5_2", "mlx5_4"),
			wantAbnormal: true,
			wantCount:    "3",
		},
		{
			name:      "lmg104 four rails",
			devs:      rail("400 Gb/sec (4X NDR)", "mlx5_1", "mlx5_2", "mlx5_3", "mlx5_4"),
			wantCount: "4",
		},
		{
			// lh-g23-141 / lh-g23-299: eight 400G compute rails plus a single
			// 200G storage HCA. Counting every device would give 9 and warn on a
			// healthy node; the max-rate selection must drop ib8.
			name: "bjg66 eight compute rails plus one slower storage HCA",
			devs: append(
				rail("400 Gb/sec (4X NDR)", "mlx5_0", "mlx5_1", "mlx5_2", "mlx5_3",
					"mlx5_4", "mlx5_5", "mlx5_6", "mlx5_7"),
				railDev{ibDev: "mlx5_8", rate: "200 Gb/sec (4X HDR)"},
			),
			wantCount: "8",
		},
		{
			// taihua-g92-001: eight 400G rails plus two 200G HCAs.
			name: "thg1 eight compute rails plus two slower HCAs",
			devs: append(
				rail("400 Gb/sec (4X NDR)", "mlx5_0", "mlx5_1", "mlx5_2", "mlx5_3",
					"mlx5_4", "mlx5_5", "mlx5_6", "mlx5_7"),
				rail("200 Gb/sec (4X HDR)", "mlx5_8", "mlx5_9")...,
			),
			wantCount: "8",
		},
		{
			// hydra-gpu-214-171-47-3: eight roce_r* rails plus two storage PFs,
			// all at 200G, so every one of them is "max rate" — still even.
			name: "zy3 uniform-rate RoCE node stays even",
			devs: rail("200 Gb/sec (4X HDR)",
				"roce_r0", "roce_r1", "roce_r2", "roce_r3",
				"roce_r4", "roce_r5", "roce_r6", "roce_r7",
				"mlx5_0", "mlx5_1"),
			wantCount: "10",
		},
		{
			// draco-g30-*: one compute plus one storage HCA at the same rate.
			name:      "dracog24 two same-rate HCAs",
			devs:      rail("200 Gb/sec (4X HDR)", "mlx5_0", "mlx5_1"),
			wantCount: "2",
		},
		{
			name:      "single-rail node is legitimate",
			devs:      rail("400 Gb/sec (4X NDR)", "mlx5_0"),
			wantCount: "1",
		},
		{
			// A dual-port HCA yields one IBHardWareInfo entry per port; without
			// per-device dedup this would count 2 and mask an odd rail count.
			name: "dual-port HCA counts once",
			devs: []railDev{
				{ibDev: "mlx5_0", rate: "400 Gb/sec (4X NDR)", port: 1},
				{ibDev: "mlx5_0", rate: "400 Gb/sec (4X NDR)", port: 2},
				{ibDev: "mlx5_1", rate: "400 Gb/sec (4X NDR)", port: 1},
				{ibDev: "mlx5_1", rate: "400 Gb/sec (4X NDR)", port: 2},
				{ibDev: "mlx5_2", rate: "400 Gb/sec (4X NDR)", port: 1},
			},
			wantAbnormal: true,
			wantCount:    "3",
		},
		{
			name:      "no devices is not reported here",
			devs:      nil,
			wantCount: "0",
		},
		{
			// An unreadable rate must not be coerced to 0 Gb/sec, which would
			// invent a bogus slowest tier and could flip the verdict.
			name: "device with unparseable rate is excluded",
			devs: append(
				rail("400 Gb/sec (4X NDR)", "mlx5_0", "mlx5_1"),
				railDev{ibDev: "mlx5_2", rate: ""},
			),
			wantCount: "2",
		},
	}

	checker, err := NewIBRailCountChecker(nil)
	require.NoError(t, err)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := &collector.InfinibandInfo{
				IBHardWareInfo: hwInfoFromRailDevs(tt.devs),
			}

			result, err := checker.Check(context.Background(), info)
			require.NoError(t, err)
			assert.Equal(t, tt.wantCount, result.Curr, "counted rail devices")

			if tt.wantAbnormal {
				assert.Equal(t, consts.StatusAbnormal, result.Status)
				// Heuristic checks must not cordon a node.
				assert.Equal(t, consts.LevelWarning, result.Level)
				assert.Equal(t, "IBRailCountOdd", result.ErrorName)
			} else {
				assert.Equal(t, consts.StatusNormal, result.Status,
					"healthy topology must not warn: %s", result.Detail)
			}
		})
	}
}
