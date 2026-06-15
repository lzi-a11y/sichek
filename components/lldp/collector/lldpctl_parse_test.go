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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoErrorf(t, err, "read fixture %s", name)
	return b
}

func TestParseLldpctlJSON_Clnet36(t *testing.T) {
	got, err := ParseLldpctlJSON(loadFixture(t, "clnet36.json"))
	require.NoError(t, err)
	require.Len(t, got, 2)

	// Iface with a single-string mgmt-ip (eth0)
	eth0, ok := got["eth0"]
	require.True(t, ok, "eth0 missing")
	assert.Equal(t, "CL-RLF233", eth0.Chassis.Name)
	assert.Equal(t, "mac", eth0.Chassis.IDType)
	assert.Equal(t, "90:74:2e:ed:72:c0", eth0.Chassis.ID)
	assert.Equal(t, []string{"26.9.135.254"}, eth0.Chassis.MgmtIP)
	assert.ElementsMatch(t, []string{"Bridge", "Router"}, eth0.Chassis.Capability)
	assert.Equal(t, "FourHundredGigE1/0/98", eth0.Port.ID)
	assert.Equal(t, "ifname", eth0.Port.IDType)
	assert.Equal(t, 9216, eth0.Port.MFS) // mfs as quoted string
	assert.Equal(t, 52, eth0.VlanID)
	assert.True(t, eth0.VlanPVID)
	assert.Equal(t, int64(55*86400+25*60+58), eth0.AgeSeconds)

	// Iface with no mgmt-ip but auto-neg present (ens14f0np0)
	ens, ok := got["ens14f0np0"]
	require.True(t, ok)
	assert.Empty(t, ens.Chassis.MgmtIP)
	assert.Contains(t, ens.Port.AutoNegCurrent, "25GbaseSR")
}

func TestParseLldpctlJSON_Dracog24MultiMgmtIP(t *testing.T) {
	got, err := ParseLldpctlJSON(loadFixture(t, "dracog24.json"))
	require.NoError(t, err)
	require.Len(t, got, 1)

	eth1, ok := got["eth1"]
	require.True(t, ok)
	// mgmt-ip is a list here (v4 + v6); both should be preserved.
	assert.Equal(t,
		[]string{"33.239.145.151", "fdbd:dc41:199:4901::21ef:9197"},
		eth1.Chassis.MgmtIP)

	// capability list mixes enabled/disabled; only the enabled ones should
	// appear in our flattened slice.
	assert.ElementsMatch(t, []string{"Bridge", "Router"}, eth1.Chassis.Capability)

	// No VLAN block at all in this fixture.
	assert.Equal(t, 0, eth1.VlanID)
	assert.False(t, eth1.VlanPVID)
}

func TestParseLldpctlJSON_Bjg45VlanWithValue(t *testing.T) {
	got, err := ParseLldpctlJSON(loadFixture(t, "bjg45.json"))
	require.NoError(t, err)
	require.Len(t, got, 1)

	ens, ok := got["ens11f0"]
	require.True(t, ok)
	assert.Equal(t, 1, ens.VlanID)
	assert.True(t, ens.VlanPVID)
	assert.Equal(t, "VLAN1", ens.VlanName)
	assert.Equal(t, "139", ens.Port.AggregationID)
}

func TestParseLldpctlJSON_Zy1RailFabric(t *testing.T) {
	got, err := ParseLldpctlJSON(loadFixture(t, "zy1.json"))
	require.NoError(t, err)
	require.Len(t, got, 52, "zy1 has 8 rails x4 ports + storage + mgmt + VF-rep loopbacks")

	// Compute rail uplink: real switch port advertised as ifname.
	p0, ok := got["eth_r0_p0"]
	require.True(t, ok)
	assert.Equal(t, "KL-R0LF01", p0.Chassis.Name)
	assert.Equal(t, "ifname", p0.Port.IDType)
	assert.Equal(t, "TwoHundredGigE1/0/1:1", p0.Port.ID)

	// Storage leaf uplink.
	assert.Equal(t, "KL-SLF01", got["s_eth0"].Chassis.Name)

	// OVS VF representor: chassis is this host itself (a self loopback, not a
	// switch uplink). The display layer filters these out by hostname.
	vf, ok := got["eth_vf_rep_r0"]
	require.True(t, ok)
	assert.Equal(t, "hydra-gpu-214-171-47-1", vf.Chassis.Name)
}

func TestParseLldpctlJSON_Lmg86HostNeighbor(t *testing.T) {
	got, err := ParseLldpctlJSON(loadFixture(t, "lmg86.json"))
	require.NoError(t, err)
	require.Len(t, got, 6)

	// eth0 neighbor is another host, not a switch: it identifies its port by
	// MAC (id_type "mac") rather than an ifname. The display layer uses this
	// to exclude host-to-host links from the switch-uplink table.
	eth0, ok := got["eth0"]
	require.True(t, ok)
	assert.Equal(t, "mac", eth0.Port.IDType)

	// eth1 is a real switch uplink (ifname port on a Lambda leaf).
	eth1, ok := got["eth1"]
	require.True(t, ok)
	assert.Equal(t, "ifname", eth1.Port.IDType)
	assert.Equal(t, "ethernet12a", eth1.Port.ID)
}

func TestParseLldpctlJSON_Empty(t *testing.T) {
	got, err := ParseLldpctlJSON(loadFixture(t, "empty.json"))
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestParseLldpctlJSON_EmptyInput(t *testing.T) {
	got, err := ParseLldpctlJSON([]byte(""))
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestParseLldpctlJSON_InvalidJSON(t *testing.T) {
	_, err := ParseLldpctlJSON([]byte("{not json"))
	require.Error(t, err)
}

func TestParseLldpctlJSON_InterfaceAsObject(t *testing.T) {
	// Some lldpctl builds emit "interface" as a single object (no array)
	// when there is exactly one entry. The parser must handle both shapes.
	raw := []byte(`{
		"lldp": {
			"interface": {
				"eth9": {
					"via": "LLDP",
					"age": "1 day, 00:00:05",
					"chassis": { "sw1": { "id": {"type":"mac","value":"aa:bb:cc:dd:ee:ff"} } },
					"port": { "id": {"type":"ifname","value":"Gi1/0/1"} }
				}
			}
		}
	}`)
	got, err := ParseLldpctlJSON(raw)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "sw1", got["eth9"].Chassis.Name)
	assert.Equal(t, "Gi1/0/1", got["eth9"].Port.ID)
	assert.Equal(t, int64(86400+5), got["eth9"].AgeSeconds)
}

func TestParseAge(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"00:00:05", 5},
		{"00:01:00", 60},
		{"1 day, 00:00:00", 86400},
		{"55 days, 00:25:58", 55*86400 + 25*60 + 58},
		{"84 days, 03:03:34", 84*86400 + 3*3600 + 3*60 + 34},
	}
	for _, c := range cases {
		got := parseAge(c.in)
		assert.Equal(t, c.want, got, "input=%q", c.in)
	}
}
