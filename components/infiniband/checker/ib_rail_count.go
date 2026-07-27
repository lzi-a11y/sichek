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
	"sort"
	"strconv"
	"strings"

	"github.com/scitix/sichek/components/common"
	"github.com/scitix/sichek/components/infiniband/collector"
	"github.com/scitix/sichek/components/infiniband/config"
	"github.com/scitix/sichek/consts"

	"github.com/sirupsen/logrus"
)

// IBRailCountChecker flags an implausible number of compute-rail HCAs.
//
// GPU nodes are built rail-symmetric: the compute fabric always comes in an
// even number of HCAs (2/4/8), or exactly one on single-rail nodes. An odd
// count above one therefore means a rail vanished from the RDMA stack. This is
// the only signal available for a card that fell off the PCIe bus entirely —
// GetLostIBPCIeDevices can only inspect functions sysfs still enumerates, and
// the spec baseline cannot help because FilterSpec trims the expected device
// map down to the hardware actually present (see config.TrimMapByList), so
// "expected but absent" is unrepresentable there.
//
// Deliberate limits, because this is a heuristic and not a baseline:
//   - Parity is a one-bit checksum: losing an *even* number of HCAs (2 of 8)
//     leaves the count even and passes. Even counts mean "not obviously
//     wrong", never "complete".
//   - It cannot name the missing device, only the surviving ones. check_ib_lost
//     localises the case where the function is still on the bus.
//   - If an entire compute fabric dies, the max rate drops to the storage tier
//     and the count becomes that tier's — plausible, so it passes.
//
// Hence Level is Warning with its own ErrorName: downstream must be able to
// tell "this card is dead" (IBLost, Critical) from "the count looks off".
type IBRailCountChecker struct {
	name string
	spec *config.InfinibandSpec
}

func NewIBRailCountChecker(specCfg *config.InfinibandSpec) (common.Checker, error) {
	return &IBRailCountChecker{
		name: config.CheckIBRailCount,
		spec: specCfg,
	}, nil
}

func (c *IBRailCountChecker) Name() string {
	return c.name
}

func (c *IBRailCountChecker) GetSpec() common.CheckerSpec {
	return nil
}

// parsePortRateGbps extracts the leading Gb/sec figure from a sysfs IB port
// rate string, e.g. "400 Gb/sec (4X NDR)" -> 400. It reports false when the
// value is missing or unparseable, so such a device is left out of the count
// rather than silently treated as 0 Gb/sec.
func parsePortRateGbps(portSpeed string) (float64, bool) {
	fields := strings.Fields(strings.TrimSpace(portSpeed))
	if len(fields) == 0 {
		return 0, false
	}
	gbps, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	return gbps, true
}

// computeRailDevices returns the IB devices sitting on the fastest fabric the
// node exposes, which is the compute rail. Selecting by the node's own maximum
// rate keeps this free of cluster-specific naming or spec lookups: a slower
// storage/management HCA (e.g. bjg66's single 200G port alongside eight 400G
// ports) drops out on its own.
//
// hws must already be deduplicated per device — a dual-port HCA contributes one
// entry per port and would otherwise be counted twice.
func computeRailDevices(hws map[string]collector.IBHardWareInfo) (devices []string, rateGbps float64) {
	byRate := make(map[float64][]string)
	maxRate := -1.0
	for _, hw := range hws {
		gbps, ok := parsePortRateGbps(hw.PortSpeed)
		if !ok {
			logrus.WithFields(logrus.Fields{
				"checker": config.CheckIBRailCount,
				"ibdev":   hw.IBDev,
				"rate":    hw.PortSpeed,
			}).Debugf("unparseable port rate, excluding device from rail count")
			continue
		}
		byRate[gbps] = append(byRate[gbps], hw.IBDev)
		if gbps > maxRate {
			maxRate = gbps
		}
	}
	if maxRate < 0 {
		return nil, 0
	}
	devices = byRate[maxRate]
	sort.Strings(devices)
	return devices, maxRate
}

func (c *IBRailCountChecker) Check(ctx context.Context, data any) (*common.CheckerResult, error) {
	infinibandInfo, ok := data.(*collector.InfinibandInfo)
	if !ok {
		return nil, fmt.Errorf("invalid InfinibandInfo type")
	}

	result := config.InfinibandCheckItems[c.name]
	result.Status = consts.StatusNormal

	infinibandInfo.RLock()
	hws := uniqueByDev(infinibandInfo.IBHardWareInfo)
	infinibandInfo.RUnlock()

	railDevs, rateGbps := computeRailDevices(hws)
	count := len(railDevs)
	result.Curr = strconv.Itoa(count)

	logrus.WithFields(logrus.Fields{
		"checker":   c.Name(),
		"railRate":  rateGbps,
		"railCount": count,
		"railDevs":  railDevs,
	}).Infof("Start IB rail count check")

	// No devices at all is NOIBFOUND territory, reported by check_ib_fw and
	// check_ib_kmod; staying silent here avoids a duplicate, weaker warning.
	if count == 0 {
		result.Detail = "No IB devices with a readable port rate; rail count not evaluated"
		return &result, nil
	}

	// A single rail is a legitimate topology (e.g. draco nodes run one compute
	// HCA next to one storage HCA), so only an odd count above one is suspect.
	if count == 1 || count%2 == 0 {
		result.Detail = fmt.Sprintf("Compute-rail HCA count is plausible: %d device(s) at %.0f Gb/sec (%s)",
			count, rateGbps, strings.Join(railDevs, ","))
		return &result, nil
	}

	result.Status = consts.StatusAbnormal
	result.Device = strings.Join(railDevs, ",")
	result.Detail = fmt.Sprintf("Odd compute-rail HCA count: %d device(s) at %.0f Gb/sec (%s). "+
		"Rail-symmetric nodes expose an even number of compute HCAs (or exactly one), "+
		"so one appears to have vanished from the RDMA stack",
		count, rateGbps, strings.Join(railDevs, ","))
	logrus.WithFields(logrus.Fields{
		"checker": c.Name(),
		"detail":  result.Detail,
	}).Errorf("Odd compute-rail HCA count detected")

	return &result, nil
}
