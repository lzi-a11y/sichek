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
package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLoadSpec_EmptyFileReturnsDefault(t *testing.T) {
	spec, err := LoadSpec("")
	assert.NoError(t, err)
	assert.Equal(t, 8, spec.NumRails)
	assert.Equal(t, 5, spec.PortsPerBridge)
	assert.Equal(t, 18, spec.MinFlows)
	assert.Equal(t, []int{10, 20, 21, 22, 23, 30, 31, 32, 33}, spec.ExpectedGroupIDs)
	assert.Equal(t, "true", spec.OtherConfig["hw-offload"])
	assert.Contains(t, spec.RequiredPackages, "libnvhws1")
}
