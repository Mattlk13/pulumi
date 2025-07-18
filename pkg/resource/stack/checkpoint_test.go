// Copyright 2016-2018, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package stack

import (
	"os"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/encoding"
	"github.com/stretchr/testify/require"
)

func TestLoadV0Checkpoint(t *testing.T) {
	t.Parallel()

	bytes, err := os.ReadFile("testdata/checkpoint-v0.json")
	require.NoError(t, err)

	chk, err := UnmarshalVersionedCheckpointToLatestCheckpoint(encoding.JSON, bytes)
	require.NoError(t, err)
	require.NotNil(t, chk.Latest)
	require.Len(t, chk.Latest.Resources, 30)
}

func TestLoadV1Checkpoint(t *testing.T) {
	t.Parallel()

	bytes, err := os.ReadFile("testdata/checkpoint-v1.json")
	require.NoError(t, err)

	chk, err := UnmarshalVersionedCheckpointToLatestCheckpoint(encoding.JSON, bytes)
	require.NoError(t, err)
	require.NotNil(t, chk.Latest)
	require.Len(t, chk.Latest.Resources, 30)
}

func TestLoadV3Checkpoint(t *testing.T) {
	t.Parallel()

	bytes, err := os.ReadFile("testdata/checkpoint-v3.json")
	require.NoError(t, err)

	chk, err := UnmarshalVersionedCheckpointToLatestCheckpoint(encoding.JSON, bytes)
	require.NoError(t, err)
	require.NotNil(t, chk.Latest)
	require.Len(t, chk.Latest.Resources, 30)
}

func TestLoadV4Checkpoint(t *testing.T) {
	t.Parallel()

	bytes, err := os.ReadFile("testdata/checkpoint-v4.json")
	require.NoError(t, err)

	chk, err := UnmarshalVersionedCheckpointToLatestCheckpoint(encoding.JSON, bytes)
	require.NoError(t, err)
	require.NotNil(t, chk.Latest)
	require.Len(t, chk.Latest.Resources, 30)
}

func TestLoadV4CheckpointUnsupportedFeature(t *testing.T) {
	t.Parallel()

	bytes, err := os.ReadFile("testdata/checkpoint-v4-unsupported-feature.json")
	require.NoError(t, err)

	chk, err := UnmarshalVersionedCheckpointToLatestCheckpoint(encoding.JSON, bytes)
	require.Nil(t, chk)
	var expectedErr *ErrDeploymentUnsupportedFeatures
	require.ErrorAs(t, err, &expectedErr)
	require.Equal(t, []string{"unsupported-feature"}, expectedErr.Features)
}
