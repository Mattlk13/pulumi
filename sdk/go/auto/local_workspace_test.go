// Copyright 2016-2021, Pulumi Corporation.
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

package auto

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/auto/optimport"

	"github.com/blang/semver"
	"github.com/go-git/go-git/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pulumi/pulumi/sdk/v3/go/auto/debug"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/events"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optdestroy"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optlist"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optpreview"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optrefresh"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optremove"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optup"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	resourceConfig "github.com/pulumi/pulumi/sdk/v3/go/common/resource/config"
	ptesting "github.com/pulumi/pulumi/sdk/v3/go/common/testing"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

var pulumiOrg = getTestOrg()

const (
	pName         = "testproj"
	agent         = "pulumi/pulumi/test"
	pulumiTestOrg = "moolumi"
)

type mockPulumiCommand struct {
	version      semver.Version
	stdout       string
	stderr       string
	exitCode     int
	err          error
	capturedArgs []string
}

func (m *mockPulumiCommand) Version() semver.Version {
	return m.version
}

func (m *mockPulumiCommand) Run(ctx context.Context,
	workdir string,
	stdin io.Reader,
	additionalOutput []io.Writer,
	additionalErrorOutput []io.Writer,
	additionalEnv []string,
	args ...string,
) (string, string, int, error) {
	m.capturedArgs = args
	return m.stdout, m.stderr, m.exitCode, m.err
}

func TestWorkspaceSecretsProvider(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sName := ptesting.RandomStackName()
	stackName := FullyQualifiedStackName(pulumiOrg, pName, sName)

	mkstack := func(passphrase string) Stack {
		opts := []LocalWorkspaceOption{
			SecretsProvider("passphrase"),
			EnvVars(map[string]string{
				"PULUMI_CONFIG_PASSPHRASE": passphrase,
			}),
		}

		// initialize
		s, err := UpsertStackInlineSource(ctx, stackName, pName, func(ctx *pulumi.Context) error {
			c := config.New(ctx, "")
			ctx.Export("exp_static", pulumi.String("foo"))
			ctx.Export("exp_cfg", pulumi.String(c.Get("bar")))
			ctx.Export("exp_secret", c.GetSecret("buzz"))
			return nil
		}, opts...)
		require.NoError(t, err, "failed to initialize stack")
		return s
	}

	s := mkstack("password")

	defer func() {
		err := os.Unsetenv("PULUMI_CONFIG_PASSPHRASE")
		require.NoError(t, err, "failed to unset EnvVar.")

		// -- pulumi stack rm --
		err = s.Workspace().RemoveStack(ctx, s.Name())
		require.NoError(t, err, "failed to remove stack. Resources have leaked.")
	}()

	passwordVal := "Password1234!"
	err := s.SetConfig(ctx, "MySecretDatabasePassword", ConfigValue{Value: passwordVal, Secret: true})
	if err != nil {
		t.Errorf("setConfig failed, err: %v", err)
		t.FailNow()
	}

	// -- pulumi up --
	res, err := s.Up(ctx)
	if err != nil {
		t.Errorf("up failed, err: %v", err)
		t.FailNow()
	}

	assert.Equal(t, "update", res.Summary.Kind)
	assert.Equal(t, "succeeded", res.Summary.Result)

	// -- get config --
	conf, err := s.GetConfig(ctx, "MySecretDatabasePassword")
	if err != nil {
		t.Errorf("GetConfig failed, err: %v", err)
		t.FailNow()
	}
	assert.Equal(t, passwordVal, conf.Value)
	assert.Equal(t, true, conf.Secret)

	// -- change passphrase --
	newPassphrase := "newpassphrase"
	err = s.Workspace().ChangeStackSecretsProvider(ctx, s.Name(), "passphrase", &ChangeSecretsProviderOptions{
		NewPassphrase: &newPassphrase,
	})
	require.NoError(t, err)
	s = mkstack("newpassphrase")

	// -- pulumi destroy --

	dRes, err := s.Destroy(ctx)
	if err != nil {
		t.Errorf("destroy failed, err: %v", err)
		t.FailNow()
	}

	assert.Equal(t, "destroy", dRes.Summary.Kind)
	assert.Equal(t, "succeeded", dRes.Summary.Result)
}

//nolint:paralleltest // mutates environment variables
func TestRemoveWithForce(t *testing.T) {
	ctx := context.Background()
	sName := ptesting.RandomStackName()
	stackName := FullyQualifiedStackName(pulumiOrg, pName, sName)
	cfg := ConfigMap{
		"bar": ConfigValue{
			Value: "abc",
		},
		"buzz": ConfigValue{
			Value:  "secret",
			Secret: true,
		},
	}

	// initialize
	pDir := filepath.Join(".", "test", "testproj")
	s, err := NewStackLocalSource(ctx, stackName, pDir)
	if err != nil {
		t.Errorf("failed to initialize stack, err: %v", err)
		t.FailNow()
	}

	err = s.SetAllConfig(ctx, cfg)
	if err != nil {
		t.Errorf("failed to set config, err: %v", err)
		t.FailNow()
	}

	// Set environment variables scoped to the workspace.
	envvars := map[string]string{
		"foo":    "bar",
		"barfoo": "foobar",
	}
	err = s.Workspace().SetEnvVars(envvars)
	require.NoError(t, err, "failed to set environment values")
	envvars = s.Workspace().GetEnvVars()
	require.NotNil(t, envvars, "failed to get environment values after setting many")

	s.Workspace().SetEnvVar("bar", "buzz")
	envvars = s.Workspace().GetEnvVars()
	require.NotNil(t, envvars, "failed to get environment value after setting")

	s.Workspace().UnsetEnvVar("bar")
	envvars = s.Workspace().GetEnvVars()
	require.NotNil(t, envvars, "failed to get environment values after unsetting.")

	// -- pulumi up --
	res, err := s.Up(ctx)
	if err != nil {
		t.Errorf("up failed, err: %v", err)
		t.FailNow()
	}

	assert.Equal(t, 3, len(res.Outputs), "expected two plain outputs")
	assert.Equal(t, "foo", res.Outputs["exp_static"].Value)
	assert.False(t, res.Outputs["exp_static"].Secret)
	assert.Equal(t, "abc", res.Outputs["exp_cfg"].Value)
	assert.False(t, res.Outputs["exp_cfg"].Secret)
	assert.Equal(t, "secret", res.Outputs["exp_secret"].Value)
	assert.True(t, res.Outputs["exp_secret"].Secret)
	assert.Equal(t, "update", res.Summary.Kind)
	assert.Equal(t, "succeeded", res.Summary.Result)

	const permalinkSearchStr = "https://app.pulumi.com"
	startRegex := regexp.MustCompile(permalinkSearchStr)
	permalink, err := GetPermalink(res.StdOut)
	require.NoError(t, err, "failed to get permalink.")
	assert.True(t, startRegex.MatchString(permalink))

	if err = s.Workspace().RemoveStack(ctx, stackName, optremove.Force()); err != nil {
		t.Errorf("remove stack with force failed")
		t.FailNow()
	}

	// to make sure stack was removed
	err = s.Workspace().SelectStack(ctx, s.Name())
	assert.ErrorContains(t, err, "no stack named")
}

//nolint:paralleltest // mutates environment variables
func TestNewStackLocalSource(t *testing.T) {
	ctx := context.Background()
	sName := ptesting.RandomStackName()
	stackName := FullyQualifiedStackName(pulumiOrg, pName, sName)
	cfg := ConfigMap{
		"bar": ConfigValue{
			Value: "abc",
		},
		"buzz": ConfigValue{
			Value:  "secret",
			Secret: true,
		},
	}

	// initialize
	pDir := filepath.Join(".", "test", "testproj")
	s, err := NewStackLocalSource(ctx, stackName, pDir)
	if err != nil {
		t.Errorf("failed to initialize stack, err: %v", err)
		t.FailNow()
	}

	defer func() {
		// -- pulumi stack rm --
		err = s.Workspace().RemoveStack(ctx, s.Name())
		require.NoError(t, err, "failed to remove stack. Resources have leaked.")
	}()

	err = s.SetAllConfig(ctx, cfg)
	if err != nil {
		t.Errorf("failed to set config, err: %v", err)
		t.FailNow()
	}

	// Set environment variables scoped to the workspace.
	envvars := map[string]string{
		"foo":    "bar",
		"barfoo": "foobar",
	}
	err = s.Workspace().SetEnvVars(envvars)
	require.NoError(t, err, "failed to set environment values")
	envvars = s.Workspace().GetEnvVars()
	require.NotNil(t, envvars, "failed to get environment values after setting many")

	s.Workspace().SetEnvVar("bar", "buzz")
	envvars = s.Workspace().GetEnvVars()
	require.NotNil(t, envvars, "failed to get environment value after setting")

	s.Workspace().UnsetEnvVar("bar")
	envvars = s.Workspace().GetEnvVars()
	require.NotNil(t, envvars, "failed to get environment values after unsetting.")

	// -- pulumi up --
	res, err := s.Up(ctx, optup.UserAgent(agent))
	if err != nil {
		t.Errorf("up failed, err: %v", err)
		t.FailNow()
	}

	assert.Equal(t, 3, len(res.Outputs), "expected two plain outputs")
	assert.Equal(t, "foo", res.Outputs["exp_static"].Value)
	assert.False(t, res.Outputs["exp_static"].Secret)
	assert.Equal(t, "abc", res.Outputs["exp_cfg"].Value)
	assert.False(t, res.Outputs["exp_cfg"].Secret)
	assert.Equal(t, "secret", res.Outputs["exp_secret"].Value)
	assert.True(t, res.Outputs["exp_secret"].Secret)
	assert.Equal(t, "update", res.Summary.Kind)
	assert.Equal(t, "succeeded", res.Summary.Result)

	const permalinkSearchStr = "https://app.pulumi.com"
	startRegex := regexp.MustCompile(permalinkSearchStr)
	permalink, err := GetPermalink(res.StdOut)
	require.NoError(t, err, "failed to get permalink.")
	assert.True(t, startRegex.MatchString(permalink))

	// -- pulumi preview --

	var previewEvents []events.EngineEvent
	prevCh := make(chan events.EngineEvent)
	wg := collectEvents(prevCh, &previewEvents)
	prev, err := s.Preview(ctx, optpreview.EventStreams(prevCh), optpreview.UserAgent(agent))
	if err != nil {
		t.Errorf("preview failed, err: %v", err)
		t.FailNow()
	}
	wg.Wait()
	assert.Equal(t, 1, prev.ChangeSummary[apitype.OpSame])
	steps := countSteps(previewEvents)
	assert.Equal(t, 1, steps)

	// -- pulumi refresh --

	ref, err := s.Refresh(ctx, optrefresh.UserAgent(agent))
	if err != nil {
		t.Errorf("refresh failed, err: %v", err)
		t.FailNow()
	}
	assert.Equal(t, "refresh", ref.Summary.Kind)
	assert.Equal(t, "succeeded", ref.Summary.Result)

	// -- pulumi destroy --

	dRes, err := s.Destroy(ctx, optdestroy.UserAgent(agent))
	if err != nil {
		t.Errorf("destroy failed, err: %v", err)
		t.FailNow()
	}

	assert.Equal(t, "destroy", dRes.Summary.Kind)
	assert.Equal(t, "succeeded", dRes.Summary.Result)
}

//nolint:paralleltest // mutates environment variables
func TestUpsertStackLocalSource(t *testing.T) {
	ctx := context.Background()
	sName := ptesting.RandomStackName()
	stackName := FullyQualifiedStackName(pulumiOrg, pName, sName)
	cfg := ConfigMap{
		"bar": ConfigValue{
			Value: "abc",
		},
		"buzz": ConfigValue{
			Value:  "secret",
			Secret: true,
		},
	}

	// initialize
	pDir := filepath.Join(".", "test", "testproj")
	s, err := UpsertStackLocalSource(ctx, stackName, pDir)
	if err != nil {
		t.Errorf("failed to initialize stack, err: %v", err)
		t.FailNow()
	}

	defer func() {
		// -- pulumi stack rm --
		err = s.Workspace().RemoveStack(ctx, s.Name())
		require.NoError(t, err, "failed to remove stack. Resources have leaked.")
	}()

	err = s.SetAllConfig(ctx, cfg)
	if err != nil {
		t.Errorf("failed to set config, err: %v", err)
		t.FailNow()
	}

	// Set environment variables scoped to the workspace.
	envvars := map[string]string{
		"foo":    "bar",
		"barfoo": "foobar",
	}
	err = s.Workspace().SetEnvVars(envvars)
	require.NoError(t, err, "failed to set environment values")
	envvars = s.Workspace().GetEnvVars()
	require.NotNil(t, envvars, "failed to get environment values after setting many")

	s.Workspace().SetEnvVar("bar", "buzz")
	envvars = s.Workspace().GetEnvVars()
	require.NotNil(t, envvars, "failed to get environment value after setting")

	s.Workspace().UnsetEnvVar("bar")
	envvars = s.Workspace().GetEnvVars()
	require.NotNil(t, envvars, "failed to get environment values after unsetting.")

	// -- pulumi up --
	res, err := s.Up(ctx)
	if err != nil {
		t.Errorf("up failed, err: %v", err)
		t.FailNow()
	}

	assert.Equal(t, 3, len(res.Outputs), "expected two plain outputs")
	assert.Equal(t, "foo", res.Outputs["exp_static"].Value)
	assert.False(t, res.Outputs["exp_static"].Secret)
	assert.Equal(t, "abc", res.Outputs["exp_cfg"].Value)
	assert.False(t, res.Outputs["exp_cfg"].Secret)
	assert.Equal(t, "secret", res.Outputs["exp_secret"].Value)
	assert.True(t, res.Outputs["exp_secret"].Secret)
	assert.Equal(t, "update", res.Summary.Kind)
	assert.Equal(t, "succeeded", res.Summary.Result)

	// -- pulumi preview --

	var previewEvents []events.EngineEvent
	prevCh := make(chan events.EngineEvent)
	wg := collectEvents(prevCh, &previewEvents)
	prev, err := s.Preview(ctx, optpreview.EventStreams(prevCh))
	if err != nil {
		t.Errorf("preview failed, err: %v", err)
		t.FailNow()
	}
	wg.Wait()

	assert.Equal(t, 1, prev.ChangeSummary[apitype.OpSame])
	steps := countSteps(previewEvents)
	assert.Equal(t, 1, steps)

	// -- pulumi refresh --
	ref, err := s.Refresh(ctx)
	if err != nil {
		t.Errorf("refresh failed, err: %v", err)
		t.FailNow()
	}

	assert.Equal(t, "refresh", ref.Summary.Kind)
	assert.Equal(t, "succeeded", ref.Summary.Result)

	// -- pulumi destroy --
	dRes, err := s.Destroy(ctx)
	if err != nil {
		t.Errorf("destroy failed, err: %v", err)
		t.FailNow()
	}

	assert.Equal(t, "destroy", dRes.Summary.Kind)
	assert.Equal(t, "succeeded", dRes.Summary.Result)
}

func TestNewStackRemoteSource(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	pName := "go_remote_proj"
	sName := ptesting.RandomStackName()
	stackName := FullyQualifiedStackName(pulumiOrg, pName, sName)
	cfg := ConfigMap{
		"bar": ConfigValue{
			Value: "abc",
		},
		"buzz": ConfigValue{
			Value:  "secret",
			Secret: true,
		},
	}
	repo := GitRepo{
		URL:         "https://github.com/pulumi/test-repo.git",
		ProjectPath: "goproj",
	}

	// initialize
	s, err := NewStackRemoteSource(ctx, stackName, repo)
	if err != nil {
		t.Errorf("failed to initialize stack, err: %v", err)
		t.FailNow()
	}

	defer func() {
		// -- pulumi stack rm --
		err = s.Workspace().RemoveStack(ctx, s.Name())
		require.NoError(t, err, "failed to remove stack. Resources have leaked.")
	}()

	err = s.SetAllConfig(ctx, cfg)
	if err != nil {
		t.Errorf("failed to set config, err: %v", err)
		t.FailNow()
	}

	// -- pulumi up --
	res, err := s.Up(ctx)
	if err != nil {
		t.Errorf("up failed, err: %v", err)
		t.FailNow()
	}

	assert.Equal(t, 3, len(res.Outputs), "expected two plain outputs")
	assert.Equal(t, "foo", res.Outputs["exp_static"].Value)
	assert.False(t, res.Outputs["exp_static"].Secret)
	assert.Equal(t, "abc", res.Outputs["exp_cfg"].Value)
	assert.False(t, res.Outputs["exp_cfg"].Secret)
	assert.Equal(t, "secret", res.Outputs["exp_secret"].Value)
	assert.True(t, res.Outputs["exp_secret"].Secret)
	assert.Equal(t, "update", res.Summary.Kind)
	assert.Equal(t, "succeeded", res.Summary.Result)

	// -- pulumi preview --

	var previewEvents []events.EngineEvent
	prevCh := make(chan events.EngineEvent)
	wg := collectEvents(prevCh, &previewEvents)
	prev, err := s.Preview(ctx, optpreview.EventStreams(prevCh))
	if err != nil {
		t.Errorf("preview failed, err: %v", err)
		t.FailNow()
	}
	wg.Wait()
	assert.Equal(t, 1, prev.ChangeSummary[apitype.OpSame])
	steps := countSteps(previewEvents)
	assert.Equal(t, 1, steps)

	// -- pulumi refresh --

	ref, err := s.Refresh(ctx)
	if err != nil {
		t.Errorf("refresh failed, err: %v", err)
		t.FailNow()
	}
	assert.Equal(t, "refresh", ref.Summary.Kind)
	assert.Equal(t, "succeeded", ref.Summary.Result)

	// -- pulumi destroy --

	dRes, err := s.Destroy(ctx)
	if err != nil {
		t.Errorf("destroy failed, err: %v", err)
		t.FailNow()
	}

	assert.Equal(t, "destroy", dRes.Summary.Kind)
	assert.Equal(t, "succeeded", dRes.Summary.Result)
}

func TestUpsertStackRemoteSource(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	pName := "go_remote_proj"
	sName := ptesting.RandomStackName()
	stackName := FullyQualifiedStackName(pulumiOrg, pName, sName)
	cfg := ConfigMap{
		"bar": ConfigValue{
			Value: "abc",
		},
		"buzz": ConfigValue{
			Value:  "secret",
			Secret: true,
		},
	}
	repo := GitRepo{
		URL:         "https://github.com/pulumi/test-repo.git",
		ProjectPath: "goproj",
	}

	// initialize
	s, err := UpsertStackRemoteSource(ctx, stackName, repo)
	if err != nil {
		t.Errorf("failed to initialize stack, err: %v", err)
		t.FailNow()
	}

	defer func() {
		// -- pulumi stack rm --
		err = s.Workspace().RemoveStack(ctx, s.Name())
		require.NoError(t, err, "failed to remove stack. Resources have leaked.")
	}()

	err = s.SetAllConfig(ctx, cfg)
	if err != nil {
		t.Errorf("failed to set config, err: %v", err)
		t.FailNow()
	}

	// -- pulumi up --
	res, err := s.Up(ctx)
	if err != nil {
		t.Errorf("up failed, err: %v", err)
		t.FailNow()
	}

	assert.Equal(t, 3, len(res.Outputs), "expected two plain outputs")
	assert.Equal(t, "foo", res.Outputs["exp_static"].Value)
	assert.False(t, res.Outputs["exp_static"].Secret)
	assert.Equal(t, "abc", res.Outputs["exp_cfg"].Value)
	assert.False(t, res.Outputs["exp_cfg"].Secret)
	assert.Equal(t, "secret", res.Outputs["exp_secret"].Value)
	assert.True(t, res.Outputs["exp_secret"].Secret)
	assert.Equal(t, "update", res.Summary.Kind)
	assert.Equal(t, "succeeded", res.Summary.Result)

	// -- pulumi preview --

	var previewEvents []events.EngineEvent
	prevCh := make(chan events.EngineEvent)
	wg := collectEvents(prevCh, &previewEvents)
	prev, err := s.Preview(ctx, optpreview.EventStreams(prevCh))
	if err != nil {
		t.Errorf("preview failed, err: %v", err)
		t.FailNow()
	}
	wg.Wait()
	assert.Equal(t, 1, prev.ChangeSummary[apitype.OpSame])
	steps := countSteps(previewEvents)
	assert.Equal(t, 1, steps)

	// -- pulumi refresh --

	ref, err := s.Refresh(ctx)
	if err != nil {
		t.Errorf("refresh failed, err: %v", err)
		t.FailNow()
	}
	assert.Equal(t, "refresh", ref.Summary.Kind)
	assert.Equal(t, "succeeded", ref.Summary.Result)

	// -- pulumi destroy --

	dRes, err := s.Destroy(ctx)
	if err != nil {
		t.Errorf("destroy failed, err: %v", err)
		t.FailNow()
	}

	assert.Equal(t, "destroy", dRes.Summary.Kind)
	assert.Equal(t, "succeeded", dRes.Summary.Result)
}

func TestNewStackRemoteSourceWithSetup(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	pName := "go_remote_proj"
	sName := ptesting.RandomStackName()
	stackName := FullyQualifiedStackName(pulumiOrg, pName, sName)
	cfg := ConfigMap{
		"bar": ConfigValue{
			Value: "abc",
		},
		"buzz": ConfigValue{
			Value:  "secret",
			Secret: true,
		},
	}
	binName := "examplesBinary"
	if runtime.GOOS == "windows" {
		binName = binName + ".exe"
	}
	repo := GitRepo{
		URL:         "https://github.com/pulumi/test-repo.git",
		ProjectPath: "goproj",
		Setup: func(ctx context.Context, workspace Workspace) error {
			cmd := exec.Command("go", "build", "-o", binName, "main.go")
			cmd.Dir = workspace.WorkDir()
			return cmd.Run()
		},
	}
	project := workspace.Project{
		Name: tokens.PackageName(pName),
		Runtime: workspace.NewProjectRuntimeInfo("go", map[string]interface{}{
			"binary": binName,
		}),
	}

	// initialize
	s, err := NewStackRemoteSource(ctx, stackName, repo, Project(project))
	if err != nil {
		t.Errorf("failed to initialize stack, err: %v", err)
		t.FailNow()
	}

	defer func() {
		// -- pulumi stack rm --
		err = s.Workspace().RemoveStack(ctx, s.Name())
		require.NoError(t, err, "failed to remove stack. Resources have leaked.")
	}()

	err = s.SetAllConfig(ctx, cfg)
	if err != nil {
		t.Errorf("failed to set config, err: %v", err)
		t.FailNow()
	}

	// -- pulumi up --
	res, err := s.Up(ctx)
	if err != nil {
		t.Errorf("up failed, err: %v", err)
		t.FailNow()
	}

	assert.Equal(t, 3, len(res.Outputs), "expected two plain outputs")
	assert.Equal(t, "foo", res.Outputs["exp_static"].Value)
	assert.False(t, res.Outputs["exp_static"].Secret)
	assert.Equal(t, "abc", res.Outputs["exp_cfg"].Value)
	assert.False(t, res.Outputs["exp_cfg"].Secret)
	assert.Equal(t, "secret", res.Outputs["exp_secret"].Value)
	assert.True(t, res.Outputs["exp_secret"].Secret)
	assert.Equal(t, "update", res.Summary.Kind)
	assert.Equal(t, "succeeded", res.Summary.Result)

	// -- pulumi preview --

	var previewEvents []events.EngineEvent
	prevCh := make(chan events.EngineEvent)
	wg := collectEvents(prevCh, &previewEvents)
	prev, err := s.Preview(ctx, optpreview.EventStreams(prevCh))
	if err != nil {
		t.Errorf("preview failed, err: %v", err)
		t.FailNow()
	}
	wg.Wait()
	assert.Equal(t, 1, prev.ChangeSummary[apitype.OpSame])
	steps := countSteps(previewEvents)
	assert.Equal(t, 1, steps)

	// -- pulumi refresh --

	ref, err := s.Refresh(ctx)
	if err != nil {
		t.Errorf("refresh failed, err: %v", err)
		t.FailNow()
	}
	assert.Equal(t, "refresh", ref.Summary.Kind)
	assert.Equal(t, "succeeded", ref.Summary.Result)

	// -- pulumi destroy --

	dRes, err := s.Destroy(ctx)
	if err != nil {
		t.Errorf("destroy failed, err: %v", err)
		t.FailNow()
	}

	assert.Equal(t, "destroy", dRes.Summary.Kind)
	assert.Equal(t, "succeeded", dRes.Summary.Result)
}

func TestUpsertStackRemoteSourceWithSetup(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	pName := "go_remote_proj"
	sName := ptesting.RandomStackName()
	stackName := FullyQualifiedStackName(pulumiOrg, pName, sName)
	cfg := ConfigMap{
		"bar": ConfigValue{
			Value: "abc",
		},
		"buzz": ConfigValue{
			Value:  "secret",
			Secret: true,
		},
	}
	binName := "examplesBinary"
	if runtime.GOOS == "windows" {
		binName = binName + ".exe"
	}
	repo := GitRepo{
		URL:         "https://github.com/pulumi/test-repo.git",
		ProjectPath: "goproj",
		Setup: func(ctx context.Context, workspace Workspace) error {
			cmd := exec.Command("go", "build", "-o", binName, "main.go")
			cmd.Dir = workspace.WorkDir()
			return cmd.Run()
		},
	}
	project := workspace.Project{
		Name: tokens.PackageName(pName),
		Runtime: workspace.NewProjectRuntimeInfo("go", map[string]interface{}{
			"binary": binName,
		}),
	}

	// initialize or select
	s, err := UpsertStackRemoteSource(ctx, stackName, repo, Project(project))
	if err != nil {
		t.Errorf("failed to initialize stack, err: %v", err)
		t.FailNow()
	}

	defer func() {
		// -- pulumi stack rm --
		err = s.Workspace().RemoveStack(ctx, s.Name())
		require.NoError(t, err, "failed to remove stack. Resources have leaked.")
	}()

	err = s.SetAllConfig(ctx, cfg)
	if err != nil {
		t.Errorf("failed to set config, err: %v", err)
		t.FailNow()
	}

	// -- pulumi up --
	res, err := s.Up(ctx)
	if err != nil {
		t.Errorf("up failed, err: %v", err)
		t.FailNow()
	}

	assert.Equal(t, 3, len(res.Outputs), "expected two plain outputs")
	assert.Equal(t, "foo", res.Outputs["exp_static"].Value)
	assert.False(t, res.Outputs["exp_static"].Secret)
	assert.Equal(t, "abc", res.Outputs["exp_cfg"].Value)
	assert.False(t, res.Outputs["exp_cfg"].Secret)
	assert.Equal(t, "secret", res.Outputs["exp_secret"].Value)
	assert.True(t, res.Outputs["exp_secret"].Secret)
	assert.Equal(t, "update", res.Summary.Kind)
	assert.Equal(t, "succeeded", res.Summary.Result)

	// -- pulumi preview --

	var previewEvents []events.EngineEvent
	prevCh := make(chan events.EngineEvent)
	wg := collectEvents(prevCh, &previewEvents)
	prev, err := s.Preview(ctx, optpreview.EventStreams(prevCh))
	if err != nil {
		t.Errorf("preview failed, err: %v", err)
		t.FailNow()
	}
	wg.Wait()
	assert.Equal(t, 1, prev.ChangeSummary[apitype.OpSame])
	steps := countSteps(previewEvents)
	assert.Equal(t, 1, steps)

	// -- pulumi refresh --

	ref, err := s.Refresh(ctx)
	if err != nil {
		t.Errorf("refresh failed, err: %v", err)
		t.FailNow()
	}
	assert.Equal(t, "refresh", ref.Summary.Kind)
	assert.Equal(t, "succeeded", ref.Summary.Result)

	// -- pulumi destroy --

	dRes, err := s.Destroy(ctx)
	if err != nil {
		t.Errorf("destroy failed, err: %v", err)
		t.FailNow()
	}

	assert.Equal(t, "destroy", dRes.Summary.Kind)
	assert.Equal(t, "succeeded", dRes.Summary.Result)
}

func TestNewStackInlineSource(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sName := ptesting.RandomStackName()
	stackName := FullyQualifiedStackName(pulumiOrg, pName, sName)
	cfg := ConfigMap{
		"bar": ConfigValue{
			Value: "abc",
		},
		"buzz": ConfigValue{
			Value:  "secret",
			Secret: true,
		},
	}

	// initialize
	s, err := NewStackInlineSource(ctx, stackName, pName, func(ctx *pulumi.Context) error {
		c := config.New(ctx, "")
		ctx.Export("exp_static", pulumi.String("foo"))
		ctx.Export("exp_cfg", pulumi.String(c.Get("bar")))
		ctx.Export("exp_secret", c.GetSecret("buzz"))
		return nil
	})
	if err != nil {
		t.Errorf("failed to initialize stack, err: %v", err)
		t.FailNow()
	}

	defer func() {
		// -- pulumi stack rm --
		err = s.Workspace().RemoveStack(ctx, s.Name())
		require.NoError(t, err, "failed to remove stack. Resources have leaked.")
	}()

	require.NoError(t, s.SetAllConfig(ctx, cfg))

	// -- pulumi up --
	res, err := s.Up(ctx, optup.UserAgent(agent), optup.Refresh())
	require.NoError(t, err, "up failed")

	assert.Equal(t, 3, len(res.Outputs), "expected two plain outputs")
	assert.Equal(t, "foo", res.Outputs["exp_static"].Value)
	assert.False(t, res.Outputs["exp_static"].Secret)
	assert.Equal(t, "abc", res.Outputs["exp_cfg"].Value)
	assert.False(t, res.Outputs["exp_cfg"].Secret)
	assert.Equal(t, "secret", res.Outputs["exp_secret"].Value)
	assert.True(t, res.Outputs["exp_secret"].Secret)
	assert.Equal(t, "update", res.Summary.Kind)
	assert.Equal(t, "succeeded", res.Summary.Result)
	assert.Greater(t, res.Summary.Version, 0)

	// -- pulumi preview --

	var previewEvents []events.EngineEvent
	prevCh := make(chan events.EngineEvent)
	wg := collectEvents(prevCh, &previewEvents)
	prev, err := s.Preview(ctx, optpreview.EventStreams(prevCh), optpreview.UserAgent(agent), optpreview.Refresh())
	require.NoError(t, err, "preview failed")
	wg.Wait()
	assert.Equal(t, 1, prev.ChangeSummary[apitype.OpSame])
	steps := countSteps(previewEvents)
	assert.Equal(t, 2, steps)

	// -- pulumi refresh --preview-only --

	pref, err := s.PreviewRefresh(ctx, optrefresh.UserAgent(agent))
	require.NoError(t, err)
	assert.Equal(t, 1, pref.ChangeSummary[apitype.OpSame])

	// -- pulumi refresh --

	ref, err := s.Refresh(ctx, optrefresh.UserAgent(agent))
	require.NoError(t, err, "refresh failed")
	assert.Equal(t, "refresh", ref.Summary.Kind)
	assert.Equal(t, "succeeded", ref.Summary.Result)

	// -- pulumi destroy --preview-only --

	pdRes, err := s.PreviewDestroy(ctx, optdestroy.UserAgent(agent), optdestroy.Refresh())
	require.NoError(t, err, "preview-only destroy failed")
	assert.Equal(t, map[apitype.OpType]int{apitype.OpDelete: 1}, pdRes.ChangeSummary)

	// -- pulumi destroy --

	dRes, err := s.Destroy(ctx, optdestroy.UserAgent(agent), optdestroy.Refresh())
	require.NoError(t, err, "destroy failed")
	assert.Equal(t, "destroy", dRes.Summary.Kind)
	assert.Equal(t, "succeeded", dRes.Summary.Result)
}

func TestStackLifecycleInlineProgramRemoveWithoutDestroy(t *testing.T) {
	t.Parallel()

	// Arrange.
	ctx := context.Background()
	sName := ptesting.RandomStackName()
	stackName := FullyQualifiedStackName(pulumiOrg, pName, sName)

	s, err := NewStackInlineSource(ctx, stackName, pName, func(ctx *pulumi.Context) error {
		_, err := NewMyResource(ctx, "res")
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		t.Errorf("failed to initialize stack, err: %v", err)
		t.FailNow()
	}

	_, err = s.Up(ctx, optup.UserAgent(agent), optup.Refresh())
	require.NoError(t, err, "up failed")

	// Act.
	err = s.Workspace().RemoveStack(ctx, s.Name())

	// Assert.
	assert.ErrorContains(t, err, "still has resources; removal rejected")
}

func TestStackLifecycleInlineProgramDestroyWithRemove(t *testing.T) {
	t.Parallel()

	// Arrange.
	ctx := context.Background()
	sName := ptesting.RandomStackName()
	stackName := FullyQualifiedStackName(pulumiOrg, pName, sName)

	s, err := NewStackInlineSource(ctx, stackName, pName, func(ctx *pulumi.Context) error {
		_, err := NewMyResource(ctx, "res")
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		t.Errorf("failed to initialize stack, err: %v", err)
		t.FailNow()
	}

	_, err = s.Up(ctx, optup.UserAgent(agent), optup.Refresh())
	require.NoError(t, err, "up failed")

	// Act.
	_, err = s.Destroy(ctx, optdestroy.Remove())
	require.NoError(t, err, "destroy failed")
	err = s.Workspace().SelectStack(ctx, s.Name())

	// Assert.
	assert.ErrorContains(t, err, "no stack named")
}

// If not run with "-race", this test has little value over the prior test.
func TestUpsertStackInlineSourceParallel(t *testing.T) {
	t.Parallel()

	for i := 0; i < 4; i++ {
		// Verify that shared context doesn't affect result
		ctx := context.Background()
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Parallel()
			sName := ptesting.RandomStackName()
			stackName := FullyQualifiedStackName(pulumiOrg, pName, sName)
			cfg := ConfigMap{
				"bar": ConfigValue{
					Value: "abc",
				},
				"buzz": ConfigValue{
					Value:  "secret",
					Secret: true,
				},
			}
			// initialize or select
			s, err := UpsertStackInlineSource(ctx, stackName, pName, func(ctx *pulumi.Context) error {
				c := config.New(ctx, "")
				ctx.Export("exp_static", pulumi.String("foo"))
				ctx.Export("exp_cfg", pulumi.String(c.Get("bar")))
				ctx.Export("exp_secret", c.GetSecret("buzz"))
				return nil
			})
			if err != nil {
				t.Errorf("failed to initialize stack, err: %v", err)
				t.FailNow()
			}

			t.Cleanup(func() {
				// -- pulumi stack rm --
				err = s.Workspace().RemoveStack(ctx, s.Name())
				require.NoError(t, err, "failed to remove stack. Resources have leaked.")
			})

			err = s.SetAllConfig(ctx, cfg)
			if err != nil {
				t.Errorf("failed to set config, err: %v", err)
				t.FailNow()
			}

			// -- pulumi up --
			res, err := s.Up(ctx)
			if err != nil {
				t.Errorf("up failed, err: %v", err)
				t.FailNow()
			}

			assert.Equal(t, 3, len(res.Outputs), "expected two plain outputs")
			assert.Equal(t, "foo", res.Outputs["exp_static"].Value)
			assert.False(t, res.Outputs["exp_static"].Secret)
			assert.Equal(t, "abc", res.Outputs["exp_cfg"].Value)
			assert.False(t, res.Outputs["exp_cfg"].Secret)
			assert.Equal(t, "secret", res.Outputs["exp_secret"].Value)
			assert.True(t, res.Outputs["exp_secret"].Secret)
			assert.Equal(t, "update", res.Summary.Kind)
			assert.Equal(t, "succeeded", res.Summary.Result)

			// -- pulumi preview --

			var previewEvents []events.EngineEvent
			prevCh := make(chan events.EngineEvent)
			wg := collectEvents(prevCh, &previewEvents)
			prev, err := s.Preview(ctx, optpreview.EventStreams(prevCh))
			if err != nil {
				t.Errorf("preview failed, err: %v", err)
				t.FailNow()
			}
			wg.Wait()
			assert.Equal(t, 1, prev.ChangeSummary[apitype.OpSame])
			steps := countSteps(previewEvents)
			assert.Equal(t, 1, steps)

			// -- pulumi refresh --

			ref, err := s.Refresh(ctx)
			if err != nil {
				t.Errorf("refresh failed, err: %v", err)
				t.FailNow()
			}
			assert.Equal(t, "refresh", ref.Summary.Kind)
			assert.Equal(t, "succeeded", ref.Summary.Result)

			// -- pulumi destroy --

			dRes, err := s.Destroy(ctx)
			if err != nil {
				t.Errorf("destroy failed, err: %v", err)
				t.FailNow()
			}

			assert.Equal(t, "destroy", dRes.Summary.Kind)
			assert.Equal(t, "succeeded", dRes.Summary.Result)
		})
	}
}

func TestNestedStackFails(t *testing.T) {
	t.Parallel()

	testCtx := context.Background()
	sName := ptesting.RandomStackName()
	parentstackName := FullyQualifiedStackName(pulumiOrg, "parent", sName)
	nestedstackName := FullyQualifiedStackName(pulumiOrg, "nested", sName)

	nestedStack, err := NewStackInlineSource(testCtx, nestedstackName, "nested", func(ctx *pulumi.Context) error {
		ctx.Export("exp_static", pulumi.String("foo"))
		return nil
	})
	if err != nil {
		t.Errorf("failed to initialize stack, err: %v", err)
		t.FailNow()
	}

	// initialize
	s, err := NewStackInlineSource(testCtx, parentstackName, "parent", func(ctx *pulumi.Context) error {
		_, err := nestedStack.Up(testCtx)
		return err
	})
	if err != nil {
		t.Errorf("failed to initialize stack, err: %v", err)
		t.FailNow()
	}

	defer func() {
		// -- pulumi stack rm --
		err = s.Workspace().RemoveStack(testCtx, s.Name())
		require.NoError(t, err, "failed to remove stack. Resources have leaked.")

		err = nestedStack.Workspace().RemoveStack(testCtx, nestedStack.Name())
		require.NoError(t, err, "failed to remove stack. Resources have leaked.")
	}()

	result, err := s.Up(testCtx)

	t.Log(result)

	assert.ErrorContains(t, err, "nested stack operations are not supported")

	// -- pulumi destroy --

	dRes, err := s.Destroy(testCtx)
	if err != nil {
		t.Errorf("destroy failed, err: %v", err)
		t.FailNow()
	}
	assert.Equal(t, "destroy", dRes.Summary.Kind)
	assert.Equal(t, "succeeded", dRes.Summary.Result)

	dRes, err = nestedStack.Destroy(testCtx)
	if err != nil {
		t.Errorf("destroy failed, err: %v", err)
		t.FailNow()
	}
	assert.Equal(t, "destroy", dRes.Summary.Kind)
	assert.Equal(t, "succeeded", dRes.Summary.Result)
}

func TestErrorProgressStreams(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	pName := "inline_error_progress_streams"
	sName := ptesting.RandomStackName()
	stackName := FullyQualifiedStackName(pulumiOrg, pName, sName)

	logLevel := uint(4)
	debugOptions := debug.LoggingOptions{
		LogToStdErr: true,
		LogLevel:    &logLevel,
	}

	// initialize
	s, err := NewStackInlineSource(ctx, stackName, pName, func(ctx *pulumi.Context) error {
		return nil
	})
	if err != nil {
		t.Errorf("failed to initialize stack, err: %v", err)
		t.FailNow()
	}

	defer func() {
		// -- pulumi stack rm --
		err := s.Workspace().RemoveStack(ctx, s.Name(), optremove.Force())
		require.NoError(t, err, "failed to remove stack. Resources have leaked.")
	}()

	// -- pulumi up --
	var upErr bytes.Buffer
	upRes, err := s.Up(ctx, optup.ErrorProgressStreams(&upErr), optup.DebugLogging(debugOptions))
	if err != nil {
		t.Errorf("up failed, err: %v", err)
		t.FailNow()
	}
	assert.Equal(t, upErr.String(), upRes.StdErr, "expected stderr writers to contain same contents")
	assert.NotEmpty(t, upRes.StdErr)

	// -- pulumi refresh --
	var refErr bytes.Buffer
	refRes, err := s.Refresh(ctx, optrefresh.ErrorProgressStreams(&refErr), optrefresh.DebugLogging(debugOptions))
	if err != nil {
		t.Errorf("refresh failed, err: %v", err)
		t.FailNow()
	}
	assert.Equal(t, refErr.String(), refRes.StdErr, "expected stderr writers to contain same contents")
	assert.NotEmpty(t, refRes.StdErr)

	// -- pulumi destroy --
	var desErr bytes.Buffer
	desRes, err := s.Destroy(ctx, optdestroy.ErrorProgressStreams(&desErr), optdestroy.DebugLogging(debugOptions))
	if err != nil {
		t.Errorf("destroy failed, err: %v", err)
		t.FailNow()
	}
	assert.Equal(t, desErr.String(), desRes.StdErr, "expected stderr writers to contain same contents")
	assert.NotEmpty(t, desRes.StdErr)
}

func TestProgressStreams(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	pName := "inline_progress_streams"
	sName := ptesting.RandomStackName()
	stackName := FullyQualifiedStackName(pulumiOrg, pName, sName)
	cfg := ConfigMap{
		"bar": ConfigValue{
			Value: "abc",
		},
		"buzz": ConfigValue{
			Value:  "secret",
			Secret: true,
		},
	}

	// initialize
	s, err := NewStackInlineSource(ctx, stackName, pName, func(ctx *pulumi.Context) error {
		c := config.New(ctx, "")
		ctx.Export("exp_static", pulumi.String("foo"))
		ctx.Export("exp_cfg", pulumi.String(c.Get("bar")))
		ctx.Export("exp_secret", c.GetSecret("buzz"))
		return nil
	})
	if err != nil {
		t.Errorf("failed to initialize stack, err: %v", err)
		t.FailNow()
	}

	defer func() {
		// -- pulumi stack rm --
		err = s.Workspace().RemoveStack(ctx, s.Name())
		require.NoError(t, err, "failed to remove stack. Resources have leaked.")
	}()

	err = s.SetAllConfig(ctx, cfg)
	if err != nil {
		t.Errorf("failed to set config, err: %v", err)
		t.FailNow()
	}

	// -- pulumi up --
	var upOut bytes.Buffer
	res, err := s.Up(ctx, optup.ProgressStreams(&upOut))
	if err != nil {
		t.Errorf("up failed, err: %v", err)
		t.FailNow()
	}

	assert.Equal(t, upOut.String(), res.StdOut, "expected stdout writers to contain same contents")

	// -- pulumi refresh --
	var refOut bytes.Buffer
	ref, err := s.Refresh(ctx, optrefresh.ProgressStreams(&refOut))
	if err != nil {
		t.Errorf("refresh failed, err: %v", err)
		t.FailNow()
	}
	assert.Equal(t, refOut.String(), ref.StdOut, "expected stdout writers to contain same contents")

	// -- pulumi destroy --
	var desOut bytes.Buffer
	dRes, err := s.Destroy(ctx, optdestroy.ProgressStreams(&desOut))
	if err != nil {
		t.Errorf("destroy failed, err: %v", err)
		t.FailNow()
	}

	assert.Equal(t, desOut.String(), dRes.StdOut, "expected stdout writers to contain same contents")
}

func TestImportExportStack(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sName := ptesting.RandomStackName()
	stackName := FullyQualifiedStackName(pulumiOrg, pName, sName)
	cfg := ConfigMap{
		"bar": ConfigValue{
			Value: "abc",
		},
		"buzz": ConfigValue{
			Value:  "secret",
			Secret: true,
		},
	}

	// initialize
	s, err := NewStackInlineSource(ctx, stackName, pName, func(ctx *pulumi.Context) error {
		c := config.New(ctx, "")
		ctx.Export("exp_static", pulumi.String("foo"))
		ctx.Export("exp_cfg", pulumi.String(c.Get("bar")))
		ctx.Export("exp_secret", c.GetSecret("buzz"))
		return nil
	})
	if err != nil {
		t.Errorf("failed to initialize stack, err: %v", err)
		t.FailNow()
	}

	defer func() {
		// -- pulumi stack rm --
		err = s.Workspace().RemoveStack(ctx, s.Name())
		require.NoError(t, err, "failed to remove stack. Resources have leaked.")
	}()

	err = s.SetAllConfig(ctx, cfg)
	if err != nil {
		t.Errorf("failed to set config, err: %v", err)
		t.FailNow()
	}

	// -- pulumi up --
	_, err = s.Up(ctx)
	if err != nil {
		t.Errorf("up failed, err: %v", err)
		t.FailNow()
	}

	// -- pulumi stack export --
	state, err := s.Export(ctx)
	if err != nil {
		t.Errorf("export failed, err: %v", err)
		t.FailNow()
	}

	// -- pulumi stack import --
	err = s.Import(ctx, state)
	if err != nil {
		t.Errorf("import failed, err: %v", err)
		t.FailNow()
	}

	// -- pulumi destroy --

	dRes, err := s.Destroy(ctx)
	if err != nil {
		t.Errorf("destroy failed, err: %v", err)
		t.FailNow()
	}

	assert.Equal(t, "destroy", dRes.Summary.Kind)
	assert.Equal(t, "succeeded", dRes.Summary.Result)
}

func TestConfigFlagLike(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sName := ptesting.RandomStackName()
	stackName := FullyQualifiedStackName(pulumiOrg, pName, sName)
	// initialize
	pDir := filepath.Join(".", "test", "testproj")
	s, err := NewStackLocalSource(ctx, stackName, pDir)
	if err != nil {
		t.Errorf("failed to initialize stack, err: %v", err)
		t.FailNow()
	}

	err = s.SetConfig(ctx, "key", ConfigValue{"-value", false})
	if err != nil {
		t.Error(err)
	}
	err = s.SetConfig(ctx, "secret-key", ConfigValue{"-value", true})
	if err != nil {
		t.Error(err)
	}
	cm, err := s.GetAllConfig(ctx)
	if err != nil {
		t.Error(err)
	}
	assert.Equalf(t, "-value", cm["testproj:key"].Value, "wrong key")
	assert.Equalf(t, "-value", cm["testproj:secret-key"].Value, "wrong secret-key")
	assert.Equalf(t, false, cm["testproj:key"].Secret, "key should not be secret")
	assert.Equalf(t, true, cm["testproj:secret-key"].Secret, "secret-key should be secret")

	err = s.Workspace().RemoveStack(ctx, stackName)
	require.NoError(t, err, "failed to remove stack. Resources have leaked.")
}

func TestGetAllConfigCorrectArgs(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	pDir := filepath.Join(".", "test", "testproj")
	m := mockPulumiCommand{
		stdout:   `{"key1": {"Value": "value1", "Secret": false}}`,
		stderr:   "",
		exitCode: 0,
		err:      nil,
	}

	workspace, err := NewLocalWorkspace(ctx, WorkDir(pDir), Pulumi(&m))
	require.NoError(t, err)

	sName := ptesting.RandomStackName()
	stackName := FullyQualifiedStackName(pulumiOrg, pName, sName)

	err = workspace.CreateStack(ctx, stackName)
	require.NoError(t, err)

	_, err = workspace.GetAllConfig(ctx, stackName)

	require.NoError(t, err)
	assert.Equal(t, []string{"config", "--show-secrets", "--json", "--stack", stackName}, m.capturedArgs)
}

func TestConfigWithOptions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sName := ptesting.RandomStackName()
	stackName := FullyQualifiedStackName(pulumiOrg, pName, sName)
	// initialize
	pDir := filepath.Join(".", "test", "testproj")
	s, err := NewStackLocalSource(ctx, stackName, pDir)
	if err != nil {
		t.Errorf("failed to initialize stack, err: %v", err)
		t.FailNow()
	}

	configYAML := ptesting.RandomStackName() + ".yaml"
	configJSON := ptesting.RandomStackName() + ".json"

	defer func() {
		err = s.Workspace().RemoveStack(ctx, stackName)
		require.NoError(t, err, "failed to remove stack. Resources have leaked.")
		err = os.RemoveAll(filepath.Join(s.Workspace().WorkDir(), configJSON))
		require.NoError(t, err, "failed to remove test.json. File has leaked.")
		err = os.RemoveAll(filepath.Join(s.Workspace().WorkDir(), configYAML))
		require.NoError(t, err, "failed to remove test.yaml. File has leaked.")
	}()

	// test backward compatibility
	err = s.SetConfigWithOptions(ctx, "key1", ConfigValue{"value1", false}, nil)
	if err != nil {
		t.Error(err)
	}
	// test new flag without subPath
	err = s.SetConfigWithOptions(ctx, "key2", ConfigValue{"value2", false}, &ConfigOptions{Path: false})
	if err != nil {
		t.Error(err)
	}
	// test new flag with subPath
	err = s.SetConfigWithOptions(ctx, "key3.subKey1", ConfigValue{"value3", false}, &ConfigOptions{Path: true})
	if err != nil {
		t.Error(err)
	}
	// test old method and key as secret
	err = s.SetConfigWithOptions(ctx, "key4", ConfigValue{"value4", true}, nil)
	if err != nil {
		t.Error(err)
	}
	// test subPath and key as secret
	err = s.SetConfigWithOptions(ctx, "key5.subKey1", ConfigValue{"value5", true}, &ConfigOptions{Path: true})
	if err != nil {
		t.Error(err)
	}
	// test string with dots
	err = s.SetConfigWithOptions(ctx, "key6.subKey1", ConfigValue{"value6", true}, nil)
	if err != nil {
		t.Error(err)
	}
	// test string with dots
	err = s.SetConfigWithOptions(ctx, "key7.subKey1", ConfigValue{"value7", true}, &ConfigOptions{Path: false})
	if err != nil {
		t.Error(err)
	}
	// test subPath
	err = s.SetConfigWithOptions(ctx, "key7.subKey2", ConfigValue{"value8", false}, &ConfigOptions{Path: true})
	if err != nil {
		t.Error(err)
	}
	// test subPath
	err = s.SetConfigWithOptions(ctx, "key7.subKey3", ConfigValue{"value9", false}, &ConfigOptions{Path: true})
	if err != nil {
		t.Error(err)
	}

	// test config file with JSON without subPath
	err = s.SetConfigWithOptions(ctx, "key8", ConfigValue{"value10", false},
		&ConfigOptions{Path: false, ConfigFile: filepath.Join(".", configJSON)})
	if err != nil {
		t.Error(err)
	}

	// test config file with JSON with subPath
	err = s.SetConfigWithOptions(ctx, "key9.subKey1", ConfigValue{"value11", false},
		&ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configJSON)})
	if err != nil {
		t.Error(err)
	}

	// test config file with JSON and key as secret
	err = s.SetConfigWithOptions(ctx, "key10", ConfigValue{"value12", true},
		&ConfigOptions{ConfigFile: filepath.Join(".", configJSON)})
	if err != nil {
		t.Error(err)
	}

	// test config file with JSON and subPath and key as secret
	err = s.SetConfigWithOptions(ctx, "key11.subKey1", ConfigValue{"value13", true},
		&ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configJSON)})
	if err != nil {
		t.Error(err)
	}

	// test config file with JSON and subPath
	err = s.SetConfigWithOptions(ctx, "key11.subKey2", ConfigValue{"value14", false},
		&ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configJSON)})
	if err != nil {
		t.Error(err)
	}

	// test config file with YAML without subPath
	err = s.SetConfigWithOptions(ctx, "key12", ConfigValue{"value15", false},
		&ConfigOptions{Path: false, ConfigFile: filepath.Join(".", configYAML)})
	if err != nil {
		t.Error(err)
	}

	// test config file with YAML with subPath
	err = s.SetConfigWithOptions(ctx, "key13.subKey1", ConfigValue{"value16", false},
		&ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configYAML)})
	if err != nil {
		t.Error(err)
	}

	// test config file with YAML and key as secret
	err = s.SetConfigWithOptions(ctx, "key14", ConfigValue{"value17", true},
		&ConfigOptions{ConfigFile: filepath.Join(".", configYAML)})
	if err != nil {
		t.Error(err)
	}

	// test config file with YAML and subPath and key as secret
	err = s.SetConfigWithOptions(ctx, "key15.subKey1", ConfigValue{"value18", true},
		&ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configYAML)})
	if err != nil {
		t.Error(err)
	}

	// test config file with YAML and subPath
	err = s.SetConfigWithOptions(ctx, "key15.subKey2", ConfigValue{"value19", false},
		&ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configYAML)})
	if err != nil {
		t.Error(err)
	}

	// test backward compatibility
	cv1, err := s.GetConfigWithOptions(ctx, "key1", nil)
	if err != nil {
		t.Error(err)
	}

	// test new flag without subPath
	cv2, err := s.GetConfigWithOptions(ctx, "key2", &ConfigOptions{Path: false})
	if err != nil {
		t.Error(err)
	}

	// test new flag with subPath
	cv3, err := s.GetConfigWithOptions(ctx, "key3.subKey1", &ConfigOptions{Path: true})
	if err != nil {
		t.Error(err)
	}

	// test old method and key as secret
	cv4, err := s.GetConfigWithOptions(ctx, "key4", nil)
	if err != nil {
		t.Error(err)
	}

	// test subPath and key as secret
	cv5, err := s.GetConfigWithOptions(ctx, "key5.subKey1", &ConfigOptions{Path: true})
	if err != nil {
		t.Error(err)
	}

	// test string with dots
	cv6, err := s.GetConfigWithOptions(ctx, "key6.subKey1", nil)
	if err != nil {
		t.Error(err)
	}

	// test string with dots
	cv7, err := s.GetConfigWithOptions(ctx, "key7.subKey1", &ConfigOptions{Path: false})
	if err != nil {
		t.Error(err)
	}
	// test string with dots
	cv8, err := s.GetConfigWithOptions(ctx, "key7.subKey2", &ConfigOptions{Path: true})
	if err != nil {
		t.Error(err)
	}
	// test string with dots
	cv9, err := s.GetConfigWithOptions(ctx, "key7.subKey3", &ConfigOptions{Path: true})
	if err != nil {
		t.Error(err)
	}
	// test config file with JSON without subPath
	cv10, err := s.GetConfigWithOptions(ctx, "key8",
		&ConfigOptions{Path: false, ConfigFile: filepath.Join(".", configJSON)})
	if err != nil {
		t.Error(err)
	}
	// test config file with JSON with subPath
	cv11, err := s.GetConfigWithOptions(ctx, "key9.subKey1",
		&ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configJSON)})
	if err != nil {
		t.Error(err)
	}
	// test config file with JSON and key as secret
	cv12, err := s.GetConfigWithOptions(ctx, "key10",
		&ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configJSON)})
	if err != nil {
		t.Error(err)
	}
	// test config file with JSON and subPath and key as secret
	cv13, err := s.GetConfigWithOptions(ctx, "key11.subKey1",
		&ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configJSON)})
	if err != nil {
		t.Error(err)
	}
	// test config file with JSON and subPath
	cv14, err := s.GetConfigWithOptions(ctx, "key11.subKey2",
		&ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configJSON)})
	if err != nil {
		t.Error(err)
	}
	// test config file with YAML without subPath
	cv15, err := s.GetConfigWithOptions(ctx, "key12",
		&ConfigOptions{Path: false, ConfigFile: filepath.Join(".", configYAML)})
	if err != nil {
		t.Error(err)
	}
	// test config file with YAML with subPath
	cv16, err := s.GetConfigWithOptions(ctx, "key13.subKey1",
		&ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configYAML)})
	if err != nil {
		t.Error(err)
	}
	// test config file with YAML and key as secret
	cv17, err := s.GetConfigWithOptions(ctx, "key14",
		&ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configYAML)})
	if err != nil {
		t.Error(err)
	}
	// test config file with YAML and subPath and key as secret
	cv18, err := s.GetConfigWithOptions(ctx, "key15.subKey1",
		&ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configYAML)})
	if err != nil {
		t.Error(err)
	}
	// test config file with YAML and subPath
	cv19, err := s.GetConfigWithOptions(ctx, "key15.subKey2",
		&ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configYAML)})
	if err != nil {
		t.Error(err)
	}

	assert.Equalf(t, "value1", cv1.Value, "wrong key")
	assert.Equalf(t, false, cv1.Secret, "key should not be secret")
	assert.Equalf(t, "value2", cv2.Value, "wrong key")
	assert.Equalf(t, false, cv2.Secret, "key should not be secret")
	assert.Equalf(t, "value3", cv3.Value, "wrong key")
	assert.Equalf(t, false, cv3.Secret, "sub-key should not be secret")
	assert.Equalf(t, "value4", cv4.Value, "wrong key")
	assert.Equalf(t, true, cv4.Secret, "key should be secret")
	assert.Equalf(t, "value5", cv5.Value, "wrong key")
	assert.Equalf(t, true, cv5.Secret, "key should be secret")
	assert.Equalf(t, "value6", cv6.Value, "wrong key")
	assert.Equalf(t, true, cv6.Secret, "key should be secret")
	assert.Equalf(t, "value7", cv7.Value, "wrong key")
	assert.Equalf(t, true, cv7.Secret, "key should be secret")
	assert.Equalf(t, "value8", cv8.Value, "wrong key")
	assert.Equalf(t, false, cv8.Secret, "key should be secret")
	assert.Equalf(t, "value9", cv9.Value, "wrong key")
	assert.Equalf(t, false, cv9.Secret, "key should be secret")
	assert.Equalf(t, "value10", cv10.Value, "wrong key")
	assert.Equalf(t, false, cv10.Secret, "key should not be secret")
	assert.Equalf(t, "value11", cv11.Value, "wrong key")
	assert.Equalf(t, false, cv11.Secret, "key should not be secret")
	assert.Equalf(t, "value12", cv12.Value, "wrong key")
	assert.Equalf(t, true, cv12.Secret, "key should be secret")
	assert.Equalf(t, "value13", cv13.Value, "wrong key")
	assert.Equalf(t, true, cv13.Secret, "key should be secret")
	assert.Equalf(t, "value14", cv14.Value, "wrong key")
	assert.Equalf(t, false, cv14.Secret, "key should not be secret")
	assert.Equalf(t, "value15", cv15.Value, "wrong key")
	assert.Equalf(t, false, cv15.Secret, "key should not be secret")
	assert.Equalf(t, "value16", cv16.Value, "wrong key")
	assert.Equalf(t, false, cv16.Secret, "key should not be secret")
	assert.Equalf(t, "value17", cv17.Value, "wrong key")
	assert.Equalf(t, true, cv17.Secret, "key should be secret")
	assert.Equalf(t, "value18", cv18.Value, "wrong key")
	assert.Equalf(t, true, cv18.Secret, "key should be secret")
	assert.Equalf(t, "value19", cv19.Value, "wrong key")
	assert.Equalf(t, false, cv19.Secret, "key should not be secret")

	err = s.RemoveConfigWithOptions(ctx, "key1", nil)
	if err != nil {
		t.Error(err)
	}

	err = s.RemoveConfigWithOptions(ctx, "key2", nil)
	if err != nil {
		t.Error(err)
	}

	err = s.RemoveConfigWithOptions(ctx, "key3", &ConfigOptions{Path: false})
	if err != nil {
		t.Error(err)
	}

	err = s.RemoveConfigWithOptions(ctx, "key4", &ConfigOptions{Path: false})
	if err != nil {
		t.Error(err)
	}

	err = s.RemoveConfigWithOptions(ctx, "key5", nil)
	if err != nil {
		t.Error(err)
	}

	err = s.RemoveConfigWithOptions(ctx, "key6.subKey1", &ConfigOptions{Path: false})
	if err != nil {
		t.Error(err)
	}

	err = s.RemoveConfigWithOptions(ctx, "key7.subKey1", nil)
	if err != nil {
		t.Error(err)
	}

	err = s.RemoveConfigWithOptions(ctx, "key8",
		&ConfigOptions{Path: false, ConfigFile: filepath.Join(".", configJSON)})
	if err != nil {
		t.Error(err)
	}

	err = s.RemoveConfigWithOptions(ctx, "key9.subKey1",
		&ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configJSON)})
	if err != nil {
		t.Error(err)
	}

	err = s.RemoveConfigWithOptions(ctx, "key10",
		&ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configJSON)})
	if err != nil {
		t.Error(err)
	}

	err = s.RemoveConfigWithOptions(ctx, "key12",
		&ConfigOptions{Path: false, ConfigFile: filepath.Join(".", configYAML)})
	if err != nil {
		t.Error(err)
	}

	err = s.RemoveConfigWithOptions(ctx, "key13.subKey1",
		&ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configYAML)})
	if err != nil {
		t.Error(err)
	}

	err = s.RemoveConfigWithOptions(ctx, "key14", &ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configYAML)})
	if err != nil {
		t.Error(err)
	}

	cfg, err := s.GetAllConfig(ctx)
	if err != nil {
		t.Error(err)
	}
	assert.Equalf(t, "{\"subKey2\":\"value8\",\"subKey3\":\"value9\"}",
		cfg["testproj:key7"].Value, "subKey2 and subKey3 have been removed")

	cfgJSON, err := s.GetAllConfigWithOptions(ctx,
		&GetAllConfigOptions{ConfigFile: filepath.Join(".", configJSON)})
	if err != nil {
		t.Error(err)
	}
	assert.Equalf(t, "",
		cfgJSON["testproj:key11"].Value, "key11 should be secret and have no value when ShowSecrets is not set")
	assert.Equalf(t, true,
		cfgJSON["testproj:key11"].Secret, "key11 should be secret")
	assert.Equalf(t, "{}",
		cfgJSON["testproj:key9"].Value, "subKey1 should have been removed")

	cfgJSONSecret, err := s.GetAllConfigWithOptions(ctx,
		&GetAllConfigOptions{ConfigFile: filepath.Join(".", configJSON), ShowSecrets: true})
	if err != nil {
		t.Error(err)
	}
	assert.Equalf(t, "{\"subKey1\":\"value13\",\"subKey2\":\"value14\"}",
		cfgJSONSecret["testproj:key11"].Value, "key11 should have value when ShowSecrets is true")
	assert.Equalf(t, true,
		cfgJSONSecret["testproj:key11"].Secret, "key11 should be secret when ShowSecrets is true")

	cfgYAML, err := s.GetAllConfigWithOptions(ctx,
		&GetAllConfigOptions{ConfigFile: filepath.Join(".", configYAML)})
	if err != nil {
		t.Error(err)
	}
	assert.Equalf(t, "",
		cfgYAML["testproj:key15"].Value, "key15 should be secret and have no value when ShowSecrets is not set")
	assert.Equalf(t, true,
		cfgYAML["testproj:key15"].Secret, "key15 should be secret")
	assert.Equalf(t, "{}",
		cfgYAML["testproj:key13"].Value, "subKey1 should have been removed")

	cfgYAMLSecret, err := s.GetAllConfigWithOptions(ctx,
		&GetAllConfigOptions{ConfigFile: filepath.Join(".", configYAML), ShowSecrets: true})
	if err != nil {
		t.Error(err)
	}
	assert.Equalf(t, "{\"subKey1\":\"value18\",\"subKey2\":\"value19\"}",
		cfgYAMLSecret["testproj:key15"].Value, "key15 should have value when ShowSecrets is true")
	assert.Equalf(t, true,
		cfgYAMLSecret["testproj:key15"].Secret, "key15 should be secret when ShowSecrets is true")
}

func TestConfigAllWithOptions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sName := ptesting.RandomStackName()
	stackName := FullyQualifiedStackName(pulumiOrg, pName, sName)
	// initialize
	pDir := filepath.Join(".", "test", "testproj")
	s, err := NewStackLocalSource(ctx, stackName, pDir)
	if err != nil {
		t.Errorf("failed to initialize stack, err: %v", err)
		t.FailNow()
	}

	configYAML := ptesting.RandomStackName() + ".yaml"
	configJSON := ptesting.RandomStackName() + ".json"

	defer func() {
		err = s.Workspace().RemoveStack(ctx, stackName)
		require.NoError(t, err, "failed to remove stack. Resources have leaked.")
		err = os.RemoveAll(filepath.Join(s.Workspace().WorkDir(), configJSON))
		require.NoError(t, err, "failed to remove test.json. File has leaked.")
		err = os.RemoveAll(filepath.Join(s.Workspace().WorkDir(), configYAML))
		require.NoError(t, err, "failed to remove test.yaml. File has leaked.")
	}()

	err = s.SetAllConfigWithOptions(ctx, ConfigMap{
		"key1": ConfigValue{
			Value:  "value1",
			Secret: false,
		},
		"key2": ConfigValue{
			Value:  "value2",
			Secret: true,
		},
		"key3.subKey1": ConfigValue{
			Value:  "value3",
			Secret: false,
		},
		"key3.subKey2": ConfigValue{
			Value:  "value4",
			Secret: false,
		},
		"key3.subKey3": ConfigValue{
			Value:  "value5",
			Secret: false,
		},
		"key4.subKey1": ConfigValue{
			Value:  "value6",
			Secret: true,
		},
	}, &ConfigOptions{Path: true})
	if err != nil {
		t.Error(err)
	}

	// test the SetAllConfigWithOptions configured the first item
	cv1, err := s.GetConfigWithOptions(ctx, "key1", nil)
	if err != nil {
		t.Error(err)
	}

	// test the SetAllConfigWithOptions configured the second item
	cv2, err := s.GetConfigWithOptions(ctx, "key2", nil)
	if err != nil {
		t.Error(err)
	}

	// test the SetAllConfigWithOptions configured the third item
	cv3, err := s.GetConfigWithOptions(ctx, "key3.subKey1", &ConfigOptions{Path: true})
	if err != nil {
		t.Error(err)
	}

	// test the SetAllConfigWithOptions configured the third item
	cv4, err := s.GetConfigWithOptions(ctx, "key3.subKey2", &ConfigOptions{Path: true})
	if err != nil {
		t.Error(err)
	}

	// test the SetAllConfigWithOptions configured the fourth item
	cv5, err := s.GetConfigWithOptions(ctx, "key4.subKey1", &ConfigOptions{Path: true})
	if err != nil {
		t.Error(err)
	}

	err = s.SetAllConfigWithOptions(ctx, ConfigMap{
		"key5": ConfigValue{
			Value:  "value7",
			Secret: false,
		},
		"key6": ConfigValue{
			Value:  "value8",
			Secret: true,
		},
		"key7.subKey1": ConfigValue{
			Value:  "value9",
			Secret: false,
		},
		"key7.subKey2": ConfigValue{
			Value:  "value10",
			Secret: false,
		},
		"key7.subKey3": ConfigValue{
			Value:  "value11",
			Secret: false,
		},
		"key8.subKey1": ConfigValue{
			Value:  "value12",
			Secret: true,
		},
	}, &ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configJSON)})
	if err != nil {
		t.Error(err)
	}

	cv6, err := s.GetConfigWithOptions(ctx, "key5",
		&ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configJSON)},
	)
	if err != nil {
		t.Error(err)
	}

	cv7, err := s.GetConfigWithOptions(ctx, "key6",
		&ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configJSON)},
	)
	if err != nil {
		t.Error(err)
	}

	cv8, err := s.GetConfigWithOptions(ctx, "key7.subKey1",
		&ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configJSON)},
	)
	if err != nil {
		t.Error(err)
	}

	cv9, err := s.GetConfigWithOptions(ctx, "key7.subKey2",
		&ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configJSON)},
	)
	if err != nil {
		t.Error(err)
	}

	cv10, err := s.GetConfigWithOptions(ctx, "key8.subKey1",
		&ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configJSON)},
	)
	if err != nil {
		t.Error(err)
	}

	err = s.SetAllConfigWithOptions(ctx, ConfigMap{
		"key9": ConfigValue{
			Value:  "value13",
			Secret: false,
		},
		"key10": ConfigValue{
			Value:  "value14",
			Secret: true,
		},
		"key11.subKey1": ConfigValue{
			Value:  "value15",
			Secret: false,
		},
		"key11.subKey2": ConfigValue{
			Value:  "value16",
			Secret: false,
		},
		"key11.subKey3": ConfigValue{
			Value:  "value17",
			Secret: false,
		},
		"key12.subKey1": ConfigValue{
			Value:  "value18",
			Secret: true,
		},
	}, &ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configYAML)})
	if err != nil {
		t.Error(err)
	}

	cv11, err := s.GetConfigWithOptions(ctx, "key9",
		&ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configYAML)},
	)
	if err != nil {
		t.Error(err)
	}

	cv12, err := s.GetConfigWithOptions(ctx, "key10",
		&ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configYAML)},
	)
	if err != nil {
		t.Error(err)
	}

	cv13, err := s.GetConfigWithOptions(ctx, "key11.subKey1",
		&ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configYAML)},
	)
	if err != nil {
		t.Error(err)
	}

	cv14, err := s.GetConfigWithOptions(ctx, "key11.subKey2",
		&ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configYAML)},
	)
	if err != nil {
		t.Error(err)
	}

	cv15, err := s.GetConfigWithOptions(ctx, "key12.subKey1",
		&ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configYAML)},
	)
	if err != nil {
		t.Error(err)
	}

	assert.Equalf(t, "value1", cv1.Value, "wrong key")
	assert.Equalf(t, false, cv1.Secret, "key should not be secret")
	assert.Equalf(t, "value2", cv2.Value, "wrong key")
	assert.Equalf(t, true, cv2.Secret, "key should be secret")
	assert.Equalf(t, "value3", cv3.Value, "wrong key")
	assert.Equalf(t, false, cv3.Secret, "key should not be secret")
	assert.Equalf(t, "value4", cv4.Value, "wrong key")
	assert.Equalf(t, false, cv4.Secret, "key should not be secret")
	assert.Equalf(t, "value6", cv5.Value, "wrong key")
	assert.Equalf(t, true, cv5.Secret, "key should be secret")
	assert.Equalf(t, "value7", cv6.Value, "wrong key")
	assert.Equalf(t, false, cv6.Secret, "key should not be secret")
	assert.Equalf(t, "value8", cv7.Value, "wrong key")
	assert.Equalf(t, true, cv7.Secret, "key should be secret")
	assert.Equalf(t, "value9", cv8.Value, "wrong key")
	assert.Equalf(t, false, cv8.Secret, "key should not be secret")
	assert.Equalf(t, "value10", cv9.Value, "wrong key")
	assert.Equalf(t, false, cv9.Secret, "key should not be secret")
	assert.Equalf(t, "value12", cv10.Value, "wrong key")
	assert.Equalf(t, true, cv10.Secret, "key should be secret")
	assert.Equalf(t, "value13", cv11.Value, "wrong key")
	assert.Equalf(t, false, cv11.Secret, "key should not be secret")
	assert.Equalf(t, "value14", cv12.Value, "wrong key")
	assert.Equalf(t, true, cv12.Secret, "key should be secret")
	assert.Equalf(t, "value15", cv13.Value, "wrong key")
	assert.Equalf(t, false, cv13.Secret, "key should not be secret")
	assert.Equalf(t, "value16", cv14.Value, "wrong key")
	assert.Equalf(t, false, cv14.Secret, "key should not be secret")
	assert.Equalf(t, "value18", cv15.Value, "wrong key")
	assert.Equalf(t, true, cv15.Secret, "key should be secret")

	err = s.RemoveAllConfigWithOptions(ctx,
		[]string{"key1", "key2", "key3.subKey1", "key3.subKey2", "key4"}, &ConfigOptions{Path: true})
	if err != nil {
		t.Error(err)
	}
	err = s.RemoveAllConfigWithOptions(ctx,
		[]string{"key5", "key6", "key7.subKey1", "key7.subKey2", "key8"},
		&ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configJSON)},
	)
	if err != nil {
		t.Error(err)
	}
	err = s.RemoveAllConfigWithOptions(ctx,
		[]string{"key9", "key10", "key11.subKey1", "key11.subKey2", "key12"},
		&ConfigOptions{Path: true, ConfigFile: filepath.Join(".", configYAML)},
	)
	if err != nil {
		t.Error(err)
	}

	cfg, err := s.GetAllConfig(ctx)
	if err != nil {
		t.Error(err)
	}
	assert.Equalf(t,
		"{\"subKey3\":\"value5\"}", cfg["testproj:key3"].Value, "key subKey3 has been removed")

	cfgJSON, err := s.GetAllConfigWithOptions(ctx,
		&GetAllConfigOptions{ConfigFile: filepath.Join(".", configJSON), ShowSecrets: true})
	if err != nil {
		t.Error(err)
	}
	assert.Equalf(t,
		"{\"subKey3\":\"value11\"}", cfgJSON["testproj:key7"].Value, "keys other than subKey3 have been removed")

	cfgYAML, err := s.GetAllConfigWithOptions(ctx,
		&GetAllConfigOptions{ConfigFile: filepath.Join(".", configYAML), ShowSecrets: true})
	if err != nil {
		t.Error(err)
	}
	assert.Equalf(t,
		"{\"subKey3\":\"value17\"}", cfgYAML["testproj:key11"].Value, "keys other than subKey3 have been removed")
}

// This test requires the existence of a Pulumi.dev.yaml file because we are reading the nested
// config from the file. This means we can't remove the stack at the end of the test.
// We should also not include secrets in this config, because the secret encryption is only valid within
// the context of a stack and org, and running this test in different orgs will fail if there are secrets.
func TestNestedConfig(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stackName := FullyQualifiedStackName(pulumiOrg, "nested_config", "dev")

	// initialize
	pDir := filepath.Join(".", "test", "nested_config")
	s, err := UpsertStackLocalSource(ctx, stackName, pDir)
	if err != nil {
		t.Errorf("failed to initialize stack, err: %v", err)
		t.FailNow()
	}

	// Also retrieve the stack settings directly from the yaml file and
	// make sure the config agrees with the config loaded by Pulumi.
	stackSettings, err := s.Workspace().StackSettings(ctx, stackName)
	require.NoError(t, err)
	confKeys := map[string]bool{}
	for k := range stackSettings.Config {
		confKeys[k.String()] = true
	}

	allConfig, err := s.GetAllConfig(ctx)
	if err != nil {
		t.Errorf("failed to get config, err: %v", err)
		t.FailNow()
	}
	allConfKeys := map[string]bool{}
	for k := range allConfig {
		allConfKeys[k] = true
	}
	assert.Equal(t, confKeys, allConfKeys)
	assert.NotEmpty(t, confKeys)

	outerVal, ok := allConfig["nested_config:outer"]
	assert.True(t, ok)
	assert.False(t, outerVal.Secret)
	assert.JSONEq(t, "{\"inner\":\"my_value\", \"other\": \"something_else\"}", outerVal.Value)

	listVal, ok := allConfig["nested_config:myList"]
	assert.True(t, ok)
	assert.False(t, listVal.Secret)
	assert.JSONEq(t, "[\"one\",\"two\",\"three\"]", listVal.Value)

	outer, err := s.GetConfig(ctx, "outer")
	if err != nil {
		t.Errorf("failed to get config, err: %v", err)
		t.FailNow()
	}
	assert.False(t, outer.Secret)
	assert.JSONEq(t, "{\"inner\":\"my_value\", \"other\": \"something_else\"}", outer.Value)

	list, err := s.GetConfig(ctx, "myList")
	if err != nil {
		t.Errorf("failed to get config, err: %v", err)
		t.FailNow()
	}
	assert.False(t, list.Secret)
	assert.JSONEq(t, "[\"one\",\"two\",\"three\"]", list.Value)
}

func TestEnvFunctions(t *testing.T) {
	if getTestOrg() != pulumiTestOrg {
		t.Skip("Skipping test because the required environments are in the moolumi org.")
	}
	t.Parallel()

	ctx := context.Background()
	stackName := FullyQualifiedStackName(pulumiOrg, pName, ptesting.RandomStackName())

	pDir := filepath.Join(".", "test", pName)
	s, err := UpsertStackLocalSource(ctx, stackName, pDir)
	require.NoError(t, err, "failed to initialize stack, err: %v", err)

	defer func() {
		err = s.Workspace().RemoveStack(ctx, stackName)
		require.NoError(t, err, "failed to remove stack. Resources have leaked.")
	}()

	// Errors when trying to add a non-existent env
	assert.Error(t, s.AddEnvironments(ctx, "non-existent-env"))

	// No error when adding an existing env
	require.NoError(t, s.AddEnvironments(ctx, "automation-api-test-env", "automation-api-test-env-2"),
		"adding environments failed, err: %v", err)

	envs, err := s.ListEnvironments(ctx)
	require.NoError(t, err, "listing environments failed, err: %v", err)
	assert.Equal(t, []string{"automation-api-test-env", "automation-api-test-env-2"}, envs)

	// Check that we can access config from the envs
	cfg, err := s.GetAllConfig(ctx)
	require.NoError(t, err, "getting config failed, err: %v", err)
	assert.Equal(t, "test_value", cfg["testproj:new_key"].Value)
	assert.Equal(t, "business", cfg["testproj:also"].Value)

	err = s.RemoveEnvironment(ctx, "automation-api-test-env")
	envs, err = s.ListEnvironments(ctx)
	require.NoError(t, err, "listing environments failed, err: %v", err)
	assert.Equal(t, []string{"automation-api-test-env-2"}, envs)

	require.NoError(t, err, "removing environment failed, err: %v", err)
	_, err = s.GetConfig(ctx, "new_key")
	assert.Error(t, err)
	v, err := s.GetConfig(ctx, "also")
	assert.Equal(t, "business", v.Value)

	err = s.RemoveEnvironment(ctx, "automation-api-test-env-2")
	envs, err = s.ListEnvironments(ctx)
	require.NoError(t, err, "listing environments failed, err: %v", err)
	require.Len(t, envs, 0)
	require.NoError(t, err, "removing environment failed, err: %v", err)
	_, err = s.GetConfig(ctx, "also")
	assert.Error(t, err)

	require.NoError(t, s.AddEnvironments(ctx, "secrets-test-env-DO-NOT-DELETE"),
		"adding environments failed, err: %v", err)
	envs, err = s.ListEnvironments(ctx)
	require.NoError(t, err, "listing environments failed, err: %v", err)
	assert.Contains(t, envs, "secrets-test-env-DO-NOT-DELETE")
	cfg, err = s.GetAllConfig(ctx)
	require.NoError(t, err, "getting config failed, err: %v", err)
	assert.Equal(t, "this_is_my_secret", cfg["testproj:test_secret"].Value)
	v, err = s.GetConfig(ctx, "test_secret")
	require.NoError(t, err, "getting config failed, err: %v", err)
	assert.Equal(t, "this_is_my_secret", v.Value)
}

func TestTagFunctions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	stackName := FullyQualifiedStackName(pulumiOrg, pName, ptesting.RandomStackName())

	pDir := filepath.Join(".", "test", "testproj")
	s, err := UpsertStackLocalSource(ctx, stackName, pDir)
	if err != nil {
		t.Errorf("failed to initialize stack, err: %v", err)
		t.FailNow()
	}
	ws := s.Workspace()

	// -- lists tag values --
	tags, err := ws.ListTags(ctx, stackName)
	if err != nil {
		t.Errorf("failed to list tags, err: %v", err)
		t.FailNow()
	}
	assert.Equal(t, pName, tags["pulumi:project"])

	// -- sets tag values --
	err = ws.SetTag(ctx, stackName, "foo", "bar")
	if err != nil {
		t.Errorf("set tag failed, err: %v", err)
		t.FailNow()
	}

	// -- gets a single tag value --
	tag, err := ws.GetTag(ctx, stackName, "foo")
	if err != nil {
		t.Errorf("get tag failed, err: %v", err)
		t.FailNow()
	}
	assert.Equal(t, "bar", tag)

	// -- removes tag value --
	err = ws.RemoveTag(ctx, stackName, "foo")
	if err != nil {
		t.Errorf("remove tag failed, err: %v", err)
		t.FailNow()
	}
	tags, _ = ws.ListTags(ctx, stackName)
	assert.NotContains(t, tags, "foo", "failed to remove tag")

	err = s.Workspace().RemoveStack(ctx, stackName)
	require.NoError(t, err, "failed to remove stack. Resources have leaked.")
}

//nolint:paralleltest // mutates environment variables
func TestStructuredOutput(t *testing.T) {
	ctx := context.Background()
	sName := ptesting.RandomStackName()
	stackName := FullyQualifiedStackName(pulumiOrg, pName, sName)
	cfg := ConfigMap{
		"bar": ConfigValue{
			Value: "abc",
		},
		"buzz": ConfigValue{
			Value:  "secret",
			Secret: true,
		},
	}

	// initialize
	pDir := filepath.Join(".", "test", "testproj")
	s, err := UpsertStackLocalSource(ctx, stackName, pDir)
	if err != nil {
		t.Errorf("failed to initialize stack, err: %v", err)
		t.FailNow()
	}

	defer func() {
		// -- pulumi stack rm --
		err = s.Workspace().RemoveStack(ctx, s.Name())
		require.NoError(t, err, "failed to remove stack. Resources have leaked.")
	}()

	err = s.SetAllConfig(ctx, cfg)
	if err != nil {
		t.Errorf("failed to set config, err: %v", err)
		t.FailNow()
	}

	// Set environment variables scoped to the workspace.
	envvars := map[string]string{
		"foo":    "bar",
		"barfoo": "foobar",
	}
	err = s.Workspace().SetEnvVars(envvars)
	require.NoError(t, err, "failed to set environment values")
	envvars = s.Workspace().GetEnvVars()
	require.NotNil(t, envvars, "failed to get environment values after setting many")

	s.Workspace().SetEnvVar("bar", "buzz")
	envvars = s.Workspace().GetEnvVars()
	require.NotNil(t, envvars, "failed to get environment value after setting")

	s.Workspace().UnsetEnvVar("bar")
	envvars = s.Workspace().GetEnvVars()
	require.NotNil(t, envvars, "failed to get environment values after unsetting.")

	// -- pulumi up --
	var upEvents []events.EngineEvent
	upCh := make(chan events.EngineEvent)
	wg := collectEvents(upCh, &upEvents)
	res, err := s.Up(ctx, optup.EventStreams(upCh))
	if err != nil {
		t.Errorf("up failed, err: %v", err)
		t.FailNow()
	}
	wg.Wait()

	assert.Equal(t, 3, len(res.Outputs), "expected two plain outputs")
	assert.Equal(t, "foo", res.Outputs["exp_static"].Value)
	assert.False(t, res.Outputs["exp_static"].Secret)
	assert.Equal(t, "abc", res.Outputs["exp_cfg"].Value)
	assert.False(t, res.Outputs["exp_cfg"].Secret)
	assert.Equal(t, "secret", res.Outputs["exp_secret"].Value)
	assert.True(t, res.Outputs["exp_secret"].Secret)
	assert.Equal(t, "update", res.Summary.Kind)
	assert.Equal(t, "succeeded", res.Summary.Result)
	assert.True(t, containsSummary(upEvents))

	// -- pulumi preview --
	var previewEvents []events.EngineEvent
	prevCh := make(chan events.EngineEvent)
	wg = collectEvents(prevCh, &previewEvents)
	prev, err := s.Preview(ctx, optpreview.EventStreams(prevCh))
	if err != nil {
		t.Errorf("preview failed, err: %v", err)
		t.FailNow()
	}
	wg.Wait()

	assert.Equal(t, 1, prev.ChangeSummary[apitype.OpSame])
	steps := countSteps(previewEvents)
	assert.Equal(t, 1, steps)
	assert.True(t, containsSummary(previewEvents))

	// -- pulumi refresh --
	var refreshEvents []events.EngineEvent
	refCh := make(chan events.EngineEvent)
	wg = collectEvents(refCh, &refreshEvents)
	ref, err := s.Refresh(ctx, optrefresh.EventStreams(refCh))
	wg.Wait()
	if err != nil {
		t.Errorf("refresh failed, err: %v", err)
		t.FailNow()
	}

	assert.Equal(t, "refresh", ref.Summary.Kind)
	assert.Equal(t, "succeeded", ref.Summary.Result)
	assert.True(t, containsSummary(refreshEvents))

	// -- pulumi destroy --
	var destroyEvents []events.EngineEvent
	desCh := make(chan events.EngineEvent)
	wg = collectEvents(desCh, &destroyEvents)
	dRes, err := s.Destroy(ctx, optdestroy.EventStreams(desCh))
	if err != nil {
		t.Errorf("destroy failed, err: %v", err)
		t.FailNow()
	}
	wg.Wait()

	assert.Equal(t, "destroy", dRes.Summary.Kind)
	assert.Equal(t, "succeeded", dRes.Summary.Result)
	assert.True(t, containsSummary(destroyEvents))
}

func TestStackImportResources(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sName := ptesting.RandomStackName()
	stackName := FullyQualifiedStackName(pulumiOrg, "import", sName)
	pDir := filepath.Join(".", "test", "import")
	stack, err := UpsertStackLocalSource(ctx, stackName, pDir)
	if err != nil {
		t.Errorf("failed to initialize stack, err: %v", err)
		t.FailNow()
	}

	randomPluginVersion := "4.16.3"
	err = stack.Workspace().InstallPlugin(ctx, "random", randomPluginVersion)
	require.NoError(t, err, "failed to install plugin")
	resourcesToImport := []*optimport.ImportResource{
		{
			Type: "random:index/randomPassword:RandomPassword",
			ID:   "supersecret",
			Name: "randomPassword",
		},
	}

	importResult, err := stack.ImportResources(ctx,
		optimport.Resources(resourcesToImport),
		optimport.Protect(false))

	require.NoError(t, err, "failed to import resources")
	assert.Equal(t, "succeeded", importResult.Summary.Result)
	expectedGeneratedCode, err := os.ReadFile(filepath.Join(pDir, "expected_generated_code.yaml"))
	require.NoError(t, err, "failed to read expected generated code")
	normalize := func(s string) string {
		return strings.ReplaceAll(s, "\r\n", "\n")
	}

	assert.Equal(t, normalize(string(expectedGeneratedCode)), normalize(importResult.GeneratedCode))
	_, err = stack.Destroy(ctx)
	require.NoError(t, err, "failed to destroy stack")
}

func TestSupportsStackOutputs(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sName := ptesting.RandomStackName()
	stackName := FullyQualifiedStackName(pulumiOrg, pName, sName)
	cfg := ConfigMap{
		"bar": ConfigValue{
			Value: "abc",
		},
		"buzz": ConfigValue{
			Value:  "secret",
			Secret: true,
		},
	}

	// initialize
	s, err := NewStackInlineSource(ctx, stackName, pName, func(ctx *pulumi.Context) error {
		c := config.New(ctx, "")

		nestedObj := pulumi.Map{
			"not_a_secret": pulumi.String("foo"),
			"is_a_secret":  pulumi.ToSecret("iamsecret"),
		}
		ctx.Export("exp_static", pulumi.String("foo"))
		ctx.Export("exp_cfg", pulumi.String(c.Get("bar")))
		ctx.Export("exp_secret", c.GetSecret("buzz"))
		ctx.Export("nested_obj", nestedObj)
		return nil
	})
	if err != nil {
		t.Errorf("failed to initialize stack, err: %v", err)
		t.FailNow()
	}

	defer func() {
		// -- pulumi stack rm --
		err = s.Workspace().RemoveStack(ctx, s.Name())
		require.NoError(t, err, "failed to remove stack. Resources have leaked.")
	}()

	err = s.SetAllConfig(ctx, cfg)
	if err != nil {
		t.Errorf("failed to set config, err: %v", err)
		t.FailNow()
	}

	assertOutputs := func(t *testing.T, outputs OutputMap) {
		assert.Equal(t, 4, len(outputs), "expected four outputs")
		assert.Equal(t, "foo", outputs["exp_static"].Value)
		assert.False(t, outputs["exp_static"].Secret)
		assert.Equal(t, "abc", outputs["exp_cfg"].Value)
		assert.False(t, outputs["exp_cfg"].Secret)
		assert.Equal(t, "secret", outputs["exp_secret"].Value)
		assert.True(t, outputs["exp_secret"].Secret)
		assert.Equal(t, map[string]interface{}{
			"is_a_secret":  "iamsecret",
			"not_a_secret": "foo",
		}, outputs["nested_obj"].Value)
		assert.True(t, outputs["nested_obj"].Secret)
	}

	initialOutputs, err := s.Outputs(ctx)
	if err != nil {
		t.Errorf("failed to get initial outputs, err: %v", err)
		t.FailNow()
	}

	assert.Equal(t, 0, len(initialOutputs))

	// -- pulumi up --
	res, err := s.Up(ctx)
	if err != nil {
		t.Errorf("up failed, err: %v", err)
		t.FailNow()
	}

	assert.Equal(t, "update", res.Summary.Kind)
	assert.Equal(t, "succeeded", res.Summary.Result)
	assert.Greater(t, res.Summary.Version, 0)
	assertOutputs(t, res.Outputs)

	outputsAfterUp, err := s.Outputs(ctx)
	if err != nil {
		t.Errorf("failed to get outputs after up, err: %v", err)
		t.FailNow()
	}

	assertOutputs(t, outputsAfterUp)

	// -- pulumi destroy --
	dRes, err := s.Destroy(ctx)
	if err != nil {
		t.Errorf("destroy failed, err: %v", err)
		t.FailNow()
	}

	assert.Equal(t, "destroy", dRes.Summary.Kind)
	assert.Equal(t, "succeeded", dRes.Summary.Result)

	outputsAfterDestroy, err := s.Outputs(ctx)
	if err != nil {
		t.Errorf("failed to get outputs after destroy, err: %v", err)
		t.FailNow()
	}

	assert.Equal(t, 0, len(outputsAfterDestroy))
}

func TestShallowClone(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	tests := []struct {
		name string
		repo GitRepo
	}{
		{
			name: "no ref provided",
			repo: GitRepo{},
		},
		{
			name: "branch provided",
			repo: GitRepo{Branch: "master"},
		},
		{
			name: "commit provided",
			repo: GitRepo{CommitHash: "028e8c5b3c6b19c3ce3b78ed508618e9cd94df1c"},
		},
		{
			name: "branch and commit provided",
			repo: GitRepo{Branch: "master", CommitHash: "028e8c5b3c6b19c3ce3b78ed508618e9cd94df1c"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo := GitRepo{
				URL:         "https://github.com/pulumi/test-repo.git",
				ProjectPath: "goproj",
				Shallow:     true,
				Branch:      tt.repo.Branch,
				CommitHash:  tt.repo.CommitHash,
			}
			ws, err := NewLocalWorkspace(ctx, Repo(repo))
			require.NoError(t, err)

			r, err := git.PlainOpenWithOptions(ws.WorkDir(), &git.PlainOpenOptions{DetectDotGit: true})
			require.NoError(t, err)

			hashes, err := r.Storer.Shallow()
			require.NoError(t, err)

			assert.Equal(t, 1, len(hashes))
		})
	}
}

func TestPulumiVersion(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	ws, err := NewLocalWorkspace(ctx)
	if err != nil {
		t.Errorf("failed to create workspace, err: %v", err)
		t.FailNow()
	}
	version := ws.PulumiVersion()
	assert.NotEqual(t, "v0.0.0", version)
	assert.Regexp(t, `(\d+\.)(\d+\.)(\d+)(-.*)?`, version)
}

func TestPulumiCommand(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	pulumiCommand, err := NewPulumiCommand(nil)
	require.NoError(t, err, "failed to create pulumi command: %s", err)
	ws, err := NewLocalWorkspace(ctx, Pulumi(pulumiCommand))
	require.NoError(t, err, "failed to create workspace: %s", err)
	version := ws.PulumiVersion()
	assert.NotEqual(t, "v0.0.0", version)
	assert.Regexp(t, `(\d+\.)(\d+\.)(\d+)(-.*)?`, version)
}

func TestClIWithoutRemoteSupport(t *testing.T) {
	t.Parallel()

	// We inspect the output of `pulumi preview --help` to determine if the
	// CLI supports remote operations. Set the output to `some output` to
	// simulate a CLI version without remote support.
	m := mockPulumiCommand{stdout: "some output"}

	_, err := NewLocalWorkspace(context.Background(), Pulumi(&m), remote(true))

	require.ErrorContains(t, err, "does not support remote operations")
}

func TestByPassesRemoteCheck(t *testing.T) {
	t.Parallel()

	// We inspect the output of `pulumi preview --help` to determine if the
	// CLI supports remote operations. Set the output to `some output` to
	// simulate a CLI version without remote support.
	m := mockPulumiCommand{stdout: "some output"}
	envVars := map[string]string{"PULUMI_AUTOMATION_API_SKIP_VERSION_CHECK": "true"}

	_, err := NewLocalWorkspace(context.Background(), Pulumi(&m), EnvVars(envVars), remote(true))

	require.NoError(t, err)
}

func TestProjectSettingsRespected(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sName := ptesting.RandomStackName()
	pName := "correct_project"
	stackName := FullyQualifiedStackName(pulumiOrg, pName, sName)
	badProjectName := "project_was_overwritten"
	stack, err := NewStackInlineSource(ctx, stackName, badProjectName, func(ctx *pulumi.Context) error {
		return nil
	}, WorkDir(filepath.Join(".", "test", pName)))

	defer func() {
		// -- pulumi stack rm --
		err = stack.Workspace().RemoveStack(ctx, stack.Name())
		require.NoError(t, err, "failed to remove stack. Resources have leaked.")
	}()

	require.NoError(t, err)
	projectSettings, err := stack.workspace.ProjectSettings(ctx)
	require.NoError(t, err)
	assert.Equal(t, projectSettings.Name, tokens.PackageName("correct_project"))
	assert.Equal(t, *projectSettings.Description, "This is a description")
}

func TestSaveStackSettings(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sName := ptesting.RandomStackName()
	stackName := FullyQualifiedStackName(pulumiOrg, pName, sName)

	opts := []LocalWorkspaceOption{
		SecretsProvider("passphrase"),
		EnvVars(map[string]string{
			"PULUMI_CONFIG_PASSPHRASE": "password",
		}),
	}

	// initialize
	s, err := NewStackInlineSource(ctx, stackName, pName, func(ctx *pulumi.Context) error {
		c := config.New(ctx, "")
		ctx.Export("exp_static", pulumi.String("foo"))
		ctx.Export("exp_cfg", pulumi.String(c.Get("bar")))
		ctx.Export("exp_secret", c.GetSecret("buzz"))
		return nil
	}, opts...)
	require.NoError(t, err, "failed to initialize stack, err: %v", err)

	defer func() {
		// -- pulumi stack rm --
		err = s.Workspace().RemoveStack(ctx, s.Name())
		require.NoError(t, err, "failed to remove stack. Resources have leaked.")
	}()

	// first load settings for created stack
	stackConfig, err := s.Workspace().StackSettings(ctx, stackName)
	require.NoError(t, err)
	// Set the config value and save it
	stackConfig.Config[resourceConfig.MustMakeKey(pName, "bar")] = resourceConfig.NewValue("baz")
	require.NoError(t, s.Workspace().SaveStackSettings(ctx, stackName, stackConfig))

	// -- pulumi up --

	res, err := s.Up(ctx)
	if err != nil {
		t.Errorf("up failed, err: %v", err)
		t.FailNow()
	}
	assert.Equal(t, "update", res.Summary.Kind)
	assert.Equal(t, "succeeded", res.Summary.Result)
	assert.Equal(t, "baz", res.Outputs["exp_cfg"].Value)

	reloaded, err := s.workspace.StackSettings(ctx, stackName)
	require.NoError(t, err)
	// Check each field because if we check the whole struct it picks up the 'raw' field which does differ.
	assert.Equal(t, stackConfig.SecretsProvider, reloaded.SecretsProvider)
	assert.Equal(t, stackConfig.EncryptedKey, reloaded.EncryptedKey)
	assert.Equal(t, stackConfig.EncryptionSalt, reloaded.EncryptionSalt)
	assert.Equal(t, stackConfig.Config, reloaded.Config)

	// -- pulumi destroy --

	dRes, err := s.Destroy(ctx)
	if err != nil {
		t.Errorf("destroy failed, err: %v", err)
		t.FailNow()
	}
	assert.Equal(t, "destroy", dRes.Summary.Kind)
	assert.Equal(t, "succeeded", dRes.Summary.Result)
}

func TestConfigSecretWarnings(t *testing.T) {
	t.Parallel()

	// TODO[pulumi/pulumi#7127]: Re-enabled the warning.
	t.Skip("Temporarily skipping test until we've re-enabled the warning - pulumi/pulumi#7127")
	ctx := context.Background()
	sName := ptesting.RandomStackName()
	stackName := FullyQualifiedStackName(pulumiOrg, pName, sName)
	cfg := ConfigMap{
		"plainstr1":    ConfigValue{Value: "1"},
		"plainstr2":    ConfigValue{Value: "2"},
		"plainstr3":    ConfigValue{Value: "3"},
		"plainstr4":    ConfigValue{Value: "4"},
		"plainstr5":    ConfigValue{Value: "5"},
		"plainstr6":    ConfigValue{Value: "6"},
		"plainstr7":    ConfigValue{Value: "7"},
		"plainstr8":    ConfigValue{Value: "8"},
		"plainstr9":    ConfigValue{Value: "9"},
		"plainstr10":   ConfigValue{Value: "10"},
		"plainstr11":   ConfigValue{Value: "11"},
		"plainstr12":   ConfigValue{Value: "12"},
		"plainbool1":   ConfigValue{Value: "true"},
		"plainbool2":   ConfigValue{Value: "true"},
		"plainbool3":   ConfigValue{Value: "true"},
		"plainbool4":   ConfigValue{Value: "true"},
		"plainbool5":   ConfigValue{Value: "true"},
		"plainbool6":   ConfigValue{Value: "true"},
		"plainbool7":   ConfigValue{Value: "true"},
		"plainbool8":   ConfigValue{Value: "true"},
		"plainbool9":   ConfigValue{Value: "true"},
		"plainbool10":  ConfigValue{Value: "true"},
		"plainbool11":  ConfigValue{Value: "true"},
		"plainbool12":  ConfigValue{Value: "true"},
		"plainint1":    ConfigValue{Value: "1"},
		"plainint2":    ConfigValue{Value: "2"},
		"plainint3":    ConfigValue{Value: "3"},
		"plainint4":    ConfigValue{Value: "4"},
		"plainint5":    ConfigValue{Value: "5"},
		"plainint6":    ConfigValue{Value: "6"},
		"plainint7":    ConfigValue{Value: "7"},
		"plainint8":    ConfigValue{Value: "8"},
		"plainint9":    ConfigValue{Value: "9"},
		"plainint10":   ConfigValue{Value: "10"},
		"plainint11":   ConfigValue{Value: "11"},
		"plainint12":   ConfigValue{Value: "12"},
		"plainfloat1":  ConfigValue{Value: "1.1"},
		"plainfloat2":  ConfigValue{Value: "2.2"},
		"plainfloat3":  ConfigValue{Value: "3.3"},
		"plainfloat4":  ConfigValue{Value: "4.4"},
		"plainfloat5":  ConfigValue{Value: "5.5"},
		"plainfloat6":  ConfigValue{Value: "6.6"},
		"plainfloat7":  ConfigValue{Value: "7.7"},
		"plainfloat8":  ConfigValue{Value: "8.8"},
		"plainfloat9":  ConfigValue{Value: "9.9"},
		"plainfloat10": ConfigValue{Value: "10.1"},
		"plainfloat11": ConfigValue{Value: "11.11"},
		"plainfloat12": ConfigValue{Value: "12.12"},
		"plainobj1":    ConfigValue{Value: "{}"},
		"plainobj2":    ConfigValue{Value: "{}"},
		"plainobj3":    ConfigValue{Value: "{}"},
		"plainobj4":    ConfigValue{Value: "{}"},
		"plainobj5":    ConfigValue{Value: "{}"},
		"plainobj6":    ConfigValue{Value: "{}"},
		"plainobj7":    ConfigValue{Value: "{}"},
		"plainobj8":    ConfigValue{Value: "{}"},
		"plainobj9":    ConfigValue{Value: "{}"},
		"plainobj10":   ConfigValue{Value: "{}"},
		"plainobj11":   ConfigValue{Value: "{}"},
		"plainobj12":   ConfigValue{Value: "{}"},
		"str1":         ConfigValue{Value: "1", Secret: true},
		"str2":         ConfigValue{Value: "2", Secret: true},
		"str3":         ConfigValue{Value: "3", Secret: true},
		"str4":         ConfigValue{Value: "4", Secret: true},
		"str5":         ConfigValue{Value: "5", Secret: true},
		"str6":         ConfigValue{Value: "6", Secret: true},
		"str7":         ConfigValue{Value: "7", Secret: true},
		"str8":         ConfigValue{Value: "8", Secret: true},
		"str9":         ConfigValue{Value: "9", Secret: true},
		"str10":        ConfigValue{Value: "10", Secret: true},
		"str11":        ConfigValue{Value: "11", Secret: true},
		"str12":        ConfigValue{Value: "12", Secret: true},
		"bool1":        ConfigValue{Value: "true", Secret: true},
		"bool2":        ConfigValue{Value: "true", Secret: true},
		"bool3":        ConfigValue{Value: "true", Secret: true},
		"bool4":        ConfigValue{Value: "true", Secret: true},
		"bool5":        ConfigValue{Value: "true", Secret: true},
		"bool6":        ConfigValue{Value: "true", Secret: true},
		"bool7":        ConfigValue{Value: "true", Secret: true},
		"bool8":        ConfigValue{Value: "true", Secret: true},
		"bool9":        ConfigValue{Value: "true", Secret: true},
		"bool10":       ConfigValue{Value: "true", Secret: true},
		"bool11":       ConfigValue{Value: "true", Secret: true},
		"bool12":       ConfigValue{Value: "true", Secret: true},
		"int1":         ConfigValue{Value: "1", Secret: true},
		"int2":         ConfigValue{Value: "2", Secret: true},
		"int3":         ConfigValue{Value: "3", Secret: true},
		"int4":         ConfigValue{Value: "4", Secret: true},
		"int5":         ConfigValue{Value: "5", Secret: true},
		"int6":         ConfigValue{Value: "6", Secret: true},
		"int7":         ConfigValue{Value: "7", Secret: true},
		"int8":         ConfigValue{Value: "8", Secret: true},
		"int9":         ConfigValue{Value: "9", Secret: true},
		"int10":        ConfigValue{Value: "10", Secret: true},
		"int11":        ConfigValue{Value: "11", Secret: true},
		"int12":        ConfigValue{Value: "12", Secret: true},
		"float1":       ConfigValue{Value: "1.1", Secret: true},
		"float2":       ConfigValue{Value: "2.2", Secret: true},
		"float3":       ConfigValue{Value: "3.3", Secret: true},
		"float4":       ConfigValue{Value: "4.4", Secret: true},
		"float5":       ConfigValue{Value: "5.5", Secret: true},
		"float6":       ConfigValue{Value: "6.6", Secret: true},
		"float7":       ConfigValue{Value: "7.7", Secret: true},
		"float8":       ConfigValue{Value: "8.8", Secret: true},
		"float9":       ConfigValue{Value: "9.9", Secret: true},
		"float10":      ConfigValue{Value: "10.1", Secret: true},
		"float11":      ConfigValue{Value: "11.11", Secret: true},
		"float12":      ConfigValue{Value: "12.12", Secret: true},
		"obj1":         ConfigValue{Value: "{}", Secret: true},
		"obj2":         ConfigValue{Value: "{}", Secret: true},
		"obj3":         ConfigValue{Value: "{}", Secret: true},
		"obj4":         ConfigValue{Value: "{}", Secret: true},
		"obj5":         ConfigValue{Value: "{}", Secret: true},
		"obj6":         ConfigValue{Value: "{}", Secret: true},
		"obj7":         ConfigValue{Value: "{}", Secret: true},
		"obj8":         ConfigValue{Value: "{}", Secret: true},
		"obj9":         ConfigValue{Value: "{}", Secret: true},
		"obj10":        ConfigValue{Value: "{}", Secret: true},
		"obj11":        ConfigValue{Value: "{}", Secret: true},
		"obj12":        ConfigValue{Value: "{}", Secret: true},
	}

	// initialize
	//nolint:errcheck
	s, err := NewStackInlineSource(ctx, stackName, pName, func(ctx *pulumi.Context) error {
		c := config.New(ctx, "")

		config.Get(ctx, "plainstr1")
		config.Require(ctx, "plainstr2")
		config.Try(ctx, "plainstr3")
		config.GetSecret(ctx, "plainstr4")
		config.RequireSecret(ctx, "plainstr5")
		config.TrySecret(ctx, "plainstr6")
		c.Get("plainstr7")
		c.Require("plainstr8")
		c.Try("plainstr9")
		c.GetSecret("plainstr10")
		c.RequireSecret("plainstr11")
		c.TrySecret("plainstr12")

		config.GetBool(ctx, "plainbool1")
		config.RequireBool(ctx, "plainbool2")
		config.TryBool(ctx, "plainbool3")
		config.GetSecretBool(ctx, "plainbool4")
		config.RequireSecretBool(ctx, "plainbool5")
		config.TrySecretBool(ctx, "plainbool6")
		c.GetBool("plainbool7")
		c.RequireBool("plainbool8")
		c.TryBool("plainbool9")
		c.GetSecretBool("plainbool10")
		c.RequireSecretBool("plainbool11")
		c.TrySecretBool("plainbool12")

		config.GetInt(ctx, "plainint1")
		config.RequireInt(ctx, "plainint2")
		config.TryInt(ctx, "plainint3")
		config.GetSecretInt(ctx, "plainint4")
		config.RequireSecretInt(ctx, "plainint5")
		config.TrySecretInt(ctx, "plainint6")
		c.GetInt("plainint7")
		c.RequireInt("plainint8")
		c.TryInt("plainint9")
		c.GetSecretInt("plainint10")
		c.RequireSecretInt("plainint11")
		c.TrySecretInt("plainint12")

		config.GetFloat64(ctx, "plainfloat1")
		config.RequireFloat64(ctx, "plainfloat2")
		config.TryFloat64(ctx, "plainfloat3")
		config.GetSecretFloat64(ctx, "plainfloat4")
		config.RequireSecretFloat64(ctx, "plainfloat5")
		config.TrySecretFloat64(ctx, "plainfloat6")
		c.GetFloat64("plainfloat7")
		c.RequireFloat64("plainfloat8")
		c.TryFloat64("plainfloat9")
		c.GetSecretFloat64("plainfloat10")
		c.RequireSecretFloat64("plainfloat11")
		c.TrySecretFloat64("plainfloat12")

		var obj interface{}
		config.GetObject(ctx, "plainobjj1", &obj)
		config.RequireObject(ctx, "plainobj2", &obj)
		config.TryObject(ctx, "plainobj3", &obj)
		config.GetSecretObject(ctx, "plainobj4", &obj)
		config.RequireSecretObject(ctx, "plainobj5", &obj)
		config.TrySecretObject(ctx, "plainobj6", &obj)
		c.GetObject("plainobjj7", &obj)
		c.RequireObject("plainobj8", &obj)
		c.TryObject("plainobj9", &obj)
		c.GetSecretObject("plainobj10", &obj)
		c.RequireSecretObject("plainobj11", &obj)
		c.TrySecretObject("plainobj12", &obj)

		config.Get(ctx, "str1")
		config.Require(ctx, "str2")
		config.Try(ctx, "str3")
		config.GetSecret(ctx, "str4")
		config.RequireSecret(ctx, "str5")
		config.TrySecret(ctx, "str6")
		c.Get("str7")
		c.Require("str8")
		c.Try("str9")
		c.GetSecret("str10")
		c.RequireSecret("str11")
		c.TrySecret("str12")

		config.GetBool(ctx, "bool1")
		config.RequireBool(ctx, "bool2")
		config.TryBool(ctx, "bool3")
		config.GetSecretBool(ctx, "bool4")
		config.RequireSecretBool(ctx, "bool5")
		config.TrySecretBool(ctx, "bool6")
		c.GetBool("bool7")
		c.RequireBool("bool8")
		c.TryBool("bool9")
		c.GetSecretBool("bool10")
		c.RequireSecretBool("bool11")
		c.TrySecretBool("bool12")

		config.GetInt(ctx, "int1")
		config.RequireInt(ctx, "int2")
		config.TryInt(ctx, "int3")
		config.GetSecretInt(ctx, "int4")
		config.RequireSecretInt(ctx, "int5")
		config.TrySecretInt(ctx, "int6")
		c.GetInt("int7")
		c.RequireInt("int8")
		c.TryInt("int9")
		c.GetSecretInt("int10")
		c.RequireSecretInt("int11")
		c.TrySecretInt("int12")

		config.GetFloat64(ctx, "float1")
		config.RequireFloat64(ctx, "float2")
		config.TryFloat64(ctx, "float3")
		config.GetSecretFloat64(ctx, "float4")
		config.RequireSecretFloat64(ctx, "float5")
		config.TrySecretFloat64(ctx, "float6")
		c.GetFloat64("float7")
		c.RequireFloat64("float8")
		c.TryFloat64("float9")
		c.GetSecretFloat64("float10")
		c.RequireSecretFloat64("float11")
		c.TrySecretFloat64("float12")

		config.GetObject(ctx, "obj1", &obj)
		config.RequireObject(ctx, "obj2", &obj)
		config.TryObject(ctx, "obj3", &obj)
		config.GetSecretObject(ctx, "obj4", &obj)
		config.RequireSecretObject(ctx, "obj5", &obj)
		config.TrySecretObject(ctx, "obj6", &obj)
		c.GetObject("obj7", &obj)
		c.RequireObject("obj8", &obj)
		c.TryObject("obj9", &obj)
		c.GetSecretObject("obj10", &obj)
		c.RequireSecretObject("obj11", &obj)
		c.TrySecretObject("obj12", &obj)

		return nil
	})
	if err != nil {
		t.Errorf("failed to initialize stack, err: %v", err)
		t.FailNow()
	}

	defer func() {
		// -- pulumi stack rm --
		err = s.Workspace().RemoveStack(ctx, s.Name())
		require.NoError(t, err, "failed to remove stack. Resources have leaked.")
	}()

	err = s.SetAllConfig(ctx, cfg)
	if err != nil {
		t.Errorf("failed to set config, err: %v", err)
		t.FailNow()
	}

	validate := func(engineEvents []events.EngineEvent) {
		expectedWarnings := []string{
			"Configuration 'testproj:str1' value is a secret; use `GetSecret` instead of `Get`",
			"Configuration 'testproj:str2' value is a secret; use `RequireSecret` instead of `Require`",
			"Configuration 'testproj:str3' value is a secret; use `TrySecret` instead of `Try`",
			"Configuration 'testproj:str7' value is a secret; use `GetSecret` instead of `Get`",
			"Configuration 'testproj:str8' value is a secret; use `RequireSecret` instead of `Require`",
			"Configuration 'testproj:str9' value is a secret; use `TrySecret` instead of `Try`",

			"Configuration 'testproj:bool1' value is a secret; use `GetSecretBool` instead of `GetBool`",
			"Configuration 'testproj:bool2' value is a secret; use `RequireSecretBool` instead of `RequireBool`",
			"Configuration 'testproj:bool3' value is a secret; use `TrySecretBool` instead of `TryBool`",
			"Configuration 'testproj:bool7' value is a secret; use `GetSecretBool` instead of `GetBool`",
			"Configuration 'testproj:bool8' value is a secret; use `RequireSecretBool` instead of `RequireBool`",
			"Configuration 'testproj:bool9' value is a secret; use `TrySecretBool` instead of `TryBool`",

			"Configuration 'testproj:int1' value is a secret; use `GetSecretInt` instead of `GetInt`",
			"Configuration 'testproj:int2' value is a secret; use `RequireSecretInt` instead of `RequireInt`",
			"Configuration 'testproj:int3' value is a secret; use `TrySecretInt` instead of `TryInt`",
			"Configuration 'testproj:int7' value is a secret; use `GetSecretInt` instead of `GetInt`",
			"Configuration 'testproj:int8' value is a secret; use `RequireSecretInt` instead of `RequireInt`",
			"Configuration 'testproj:int9' value is a secret; use `TrySecretInt` instead of `TryInt`",

			"Configuration 'testproj:float1' value is a secret; use `GetSecretFloat64` instead of `GetFloat64`",
			"Configuration 'testproj:float2' value is a secret; use `RequireSecretFloat64` instead of `RequireFloat64`",
			"Configuration 'testproj:float3' value is a secret; use `TrySecretFloat64` instead of `TryFloat64`",
			"Configuration 'testproj:float7' value is a secret; use `GetSecretFloat64` instead of `GetFloat64`",
			"Configuration 'testproj:float8' value is a secret; use `RequireSecretFloat64` instead of `RequireFloat64`",
			"Configuration 'testproj:float9' value is a secret; use `TrySecretFloat64` instead of `TryFloat64`",

			"Configuration 'testproj:obj1' value is a secret; use `GetSecretObject` instead of `GetObject`",
			"Configuration 'testproj:obj2' value is a secret; use `RequireSecretObject` instead of `RequireObject`",
			"Configuration 'testproj:obj3' value is a secret; use `TrySecretObject` instead of `TryObject`",
			"Configuration 'testproj:obj7' value is a secret; use `GetSecretObject` instead of `GetObject`",
			"Configuration 'testproj:obj8' value is a secret; use `RequireSecretObject` instead of `RequireObject`",
			"Configuration 'testproj:obj9' value is a secret; use `TrySecretObject` instead of `TryObject`",
		}
		for _, warning := range expectedWarnings {
			var found bool
			for _, event := range engineEvents {
				if event.DiagnosticEvent != nil && event.DiagnosticEvent.Severity == "warning" &&
					strings.Contains(event.DiagnosticEvent.Message, warning) {
					found = true
					break
				}
			}
			assert.True(t, found, "expected warning %q", warning)
		}

		// These keys should not be in any warning messages.
		unexpectedWarnings := []string{
			"plainstr1",
			"plainstr2",
			"plainstr3",
			"plainstr4",
			"plainstr5",
			"plainstr6",
			"plainstr7",
			"plainstr8",
			"plainstr9",
			"plainstr10",
			"plainstr11",
			"plainstr12",
			"plainbool1",
			"plainbool2",
			"plainbool3",
			"plainbool4",
			"plainbool5",
			"plainbool6",
			"plainbool7",
			"plainbool8",
			"plainbool9",
			"plainbool10",
			"plainbool11",
			"plainbool12",
			"plainint1",
			"plainint2",
			"plainint3",
			"plainint4",
			"plainint5",
			"plainint6",
			"plainint7",
			"plainint8",
			"plainint9",
			"plainint10",
			"plainint11",
			"plainint12",
			"plainfloat1",
			"plainfloat2",
			"plainfloat3",
			"plainfloat4",
			"plainfloat5",
			"plainfloat6",
			"plainfloat7",
			"plainfloat8",
			"plainfloat9",
			"plainfloat10",
			"plainfloat11",
			"plainfloat12",
			"plainobj1",
			"plainobj2",
			"plainobj3",
			"plainobj4",
			"plainobj5",
			"plainobj6",
			"plainobj7",
			"plainobj8",
			"plainobj9",
			"plainobj10",
			"plainobj11",
			"plainobj12",
		}
		for _, warning := range unexpectedWarnings {
			for _, event := range engineEvents {
				if event.DiagnosticEvent != nil {
					assert.NotContains(t, event.DiagnosticEvent.Message, warning)
				}
			}
		}
	}

	// -- pulumi up --
	var upEvents []events.EngineEvent
	upCh := make(chan events.EngineEvent)
	wg := collectEvents(upCh, &upEvents)
	_, err = s.Up(ctx, optup.EventStreams(upCh))
	if err != nil {
		t.Errorf("up failed, err: %v", err)
		t.FailNow()
	}
	wg.Wait()
	validate(upEvents)

	// -- pulumi preview --
	var previewEvents []events.EngineEvent
	prevCh := make(chan events.EngineEvent)
	wg = collectEvents(prevCh, &previewEvents)
	_, err = s.Preview(ctx, optpreview.EventStreams(prevCh))
	if err != nil {
		t.Errorf("preview failed, err: %v", err)
		t.FailNow()
	}
	wg.Wait()
	validate(previewEvents)
}

func TestWhoAmIDetailed(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	stackName := FullyQualifiedStackName(pulumiOrg, pName, ptesting.RandomStackName())

	// initialize
	pDir := filepath.Join(".", "test", "testproj")
	s, err := UpsertStackLocalSource(ctx, stackName, pDir)
	if err != nil {
		t.Errorf("failed to initialize stack, err: %v", err)
		t.FailNow()
	}

	whoAmIDetailedInfo, err := s.Workspace().WhoAmIDetails(ctx)
	if err != nil {
		t.Errorf("failed to get WhoAmIDetailedInfo, err: %v", err)
		t.FailNow()
	}
	require.NotNil(t, whoAmIDetailedInfo.User, "failed to get WhoAmIDetailedInfo user")
	require.NotNil(t, whoAmIDetailedInfo.URL, "failed to get WhoAmIDetailedInfo url")

	// cleanup
	_, err = s.Destroy(ctx)
	if err != nil {
		t.Errorf("destroy failed during cleanup, err: %v", err)
		t.FailNow()
	}
	err = s.Workspace().RemoveStack(ctx, s.Name())
	if err != nil {
		t.Errorf("failed to remove stack during cleanup. Resources have leaked, err: %v", err)
		t.FailNow()
	}
}

func TestListStacks(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	pDir := filepath.Join(".", "test", "testproj")
	m := mockPulumiCommand{
		stdout: `[{"name": "testorg1/testproj1/teststack1",
				   "current": false,
				   "url": "https://app.pulumi.com/testorg1/testproj1/teststack1"},
				  {"name": "testorg1/testproj1/teststack2",
				   "current": false,
				   "url": "https://app.pulumi.com/testorg1/testproj1/teststack2"}]`,
		stderr:   "",
		exitCode: 0,
		err:      nil,
	}

	workspace, err := NewLocalWorkspace(ctx, WorkDir(pDir), Pulumi(&m))
	require.NoError(t, err)

	stacks, err := workspace.ListStacks(ctx)

	require.NoError(t, err)
	require.Len(t, stacks, 2)
	assert.Equal(t, "testorg1/testproj1/teststack1", stacks[0].Name)
	assert.Equal(t, false, stacks[0].Current)
	assert.Equal(t, "https://app.pulumi.com/testorg1/testproj1/teststack1", stacks[0].URL)
	assert.Equal(t, "testorg1/testproj1/teststack2", stacks[1].Name)
	assert.Equal(t, false, stacks[1].Current)
	assert.Equal(t, "https://app.pulumi.com/testorg1/testproj1/teststack2", stacks[1].URL)
}

func TestListStacksCorrectArgs(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	pDir := filepath.Join(".", "test", "testproj")
	m := mockPulumiCommand{
		stdout: `[{"name": "testorg1/testproj1/teststack1",
				"current": false,
				"url": "https://app.pulumi.com/testorg1/testproj1/teststack1"},
				{"name": "testorg1/testproj1/teststack2",
				"current": false,
				"url": "https://app.pulumi.com/testorg1/testproj1/teststack2"}]`,
		stderr:   "",
		exitCode: 0,
		err:      nil,
	}

	workspace, err := NewLocalWorkspace(ctx, WorkDir(pDir), Pulumi(&m))
	require.NoError(t, err)

	_, err = workspace.ListStacks(ctx)

	require.NoError(t, err)
	assert.Equal(t, []string{"stack", "ls", "--json"}, m.capturedArgs)
}

func TestListAllStacks(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	pDir := filepath.Join(".", "test", "testproj")
	m := mockPulumiCommand{
		stdout: `[{"name": "testorg1/testproj1/teststack1",
				   "current": false,
				   "url": "https://app.pulumi.com/testorg1/testproj1/teststack1"},
				  {"name": "testorg1/testproj2/teststack2",
				   "current": false,
				   "url": "https://app.pulumi.com/testorg1/testproj2/teststack2"}]`,
		stderr:   "",
		exitCode: 0,
		err:      nil,
	}

	workspace, err := NewLocalWorkspace(ctx, WorkDir(pDir), Pulumi(&m))
	require.NoError(t, err)

	stacks, err := workspace.ListStacks(ctx, optlist.All())

	require.NoError(t, err)
	require.Len(t, stacks, 2)
	assert.Equal(t, "testorg1/testproj1/teststack1", stacks[0].Name)
	assert.Equal(t, false, stacks[0].Current)
	assert.Equal(t, "https://app.pulumi.com/testorg1/testproj1/teststack1", stacks[0].URL)
	assert.Equal(t, "testorg1/testproj2/teststack2", stacks[1].Name)
	assert.Equal(t, false, stacks[1].Current)
	assert.Equal(t, "https://app.pulumi.com/testorg1/testproj2/teststack2", stacks[1].URL)
}

func TestListStacksAllCorrectArgs(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	pDir := filepath.Join(".", "test", "testproj")
	m := mockPulumiCommand{
		stdout: `[{"name": "testorg1/testproj1/teststack1",
				"current": false,
				"url": "https://app.pulumi.com/testorg1/testproj1/teststack1"},
				{"name": "testorg1/testproj1/teststack2",
				"current": false,
				"url": "https://app.pulumi.com/testorg1/testproj1/teststack2"}]`,
		stderr:   "",
		exitCode: 0,
		err:      nil,
	}

	workspace, err := NewLocalWorkspace(ctx, WorkDir(pDir), Pulumi(&m))
	require.NoError(t, err)

	_, err = workspace.ListStacks(ctx, optlist.All())

	require.NoError(t, err)
	assert.Equal(t, []string{"stack", "ls", "--json", "--all"}, m.capturedArgs)
}

func TestInstallWithOptions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pDir := filepath.Join(".", "test", "install")

	defer os.RemoveAll(filepath.Join(pDir, "venv"))
	workspace, err := NewLocalWorkspace(ctx, WorkDir(pDir))
	require.NoError(t, err)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	// Run with options
	err = workspace.Install(ctx, &InstallOptions{
		Stdout: stdout,
		Stderr: stderr,
	})

	require.NoError(t, err)
	require.Contains(t, stdout.String(), "Creating virtual environment...")
	require.Contains(t, stdout.String(), "Successfully installed urllib3")
	require.Contains(t, stdout.String(), "Finished installing dependencies")
	require.Empty(t, stderr.String())
	require.DirExists(t, filepath.Join(pDir, "venv"))

	// Run without options
	err = workspace.Install(ctx, nil)

	require.NoError(t, err)
}

func TestInstallOptions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pDir := filepath.Join(".", "test", "install")
	m := mockPulumiCommand{
		// Set a version high enough to support UseLanguageVersionTools
		version: semver.Version{Major: 3, Minor: 130},
	}
	workspace, err := NewLocalWorkspace(ctx, WorkDir(pDir), Pulumi(&m))
	require.NoError(t, err)

	err = workspace.Install(ctx, &InstallOptions{})
	require.NoError(t, err)
	require.Equal(t, []string{"install"}, m.capturedArgs)

	err = workspace.Install(ctx, &InstallOptions{
		UseLanguageVersionTools: true,
	})
	require.NoError(t, err)
	require.Equal(t, []string{"install", "--use-language-version-tools"}, m.capturedArgs)

	err = workspace.Install(ctx, &InstallOptions{
		NoPlugins: true,
	})
	require.NoError(t, err)
	require.Equal(t, []string{"install", "--no-plugins"}, m.capturedArgs)

	err = workspace.Install(ctx, &InstallOptions{
		NoDependencies: true,
	})
	require.NoError(t, err)
	require.Equal(t, []string{"install", "--no-dependencies"}, m.capturedArgs)

	err = workspace.Install(ctx, &InstallOptions{
		Reinstall: true,
	})
	require.NoError(t, err)
	require.Equal(t, []string{"install", "--reinstall"}, m.capturedArgs)

	err = workspace.Install(ctx, &InstallOptions{
		UseLanguageVersionTools: true,
		NoDependencies:          true,
		NoPlugins:               true,
		Reinstall:               true,
	})
	require.NoError(t, err)
	require.Equal(t, []string{
		"install",
		"--use-language-version-tools",
		"--no-plugins",
		"--no-dependencies",
		"--reinstall",
	}, m.capturedArgs)
}

func TestInstallWithUseLanguageVersionTools(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pDir := filepath.Join(".", "test", "install-use-language-version-tools")

	// Option is not available on < 3.130
	m := mockPulumiCommand{
		version: semver.Version{Major: 3, Minor: 129},
	}

	workspace, err := NewLocalWorkspace(ctx, WorkDir(pDir), Pulumi(&m))
	require.NoError(t, err)
	err = workspace.Install(ctx, &InstallOptions{
		UseLanguageVersionTools: true,
	})
	require.ErrorContains(t, err, "UseLanguageVersionTools requires Pulumi CLI version >= 3.130.0")

	// Option is available on >= 3.130
	m = mockPulumiCommand{
		version: semver.Version{Major: 3, Minor: 130},
	}

	workspace, err = NewLocalWorkspace(ctx, WorkDir(pDir), Pulumi(&m))
	require.NoError(t, err)
	err = workspace.Install(ctx, &InstallOptions{
		UseLanguageVersionTools: true,
	})
	require.NoError(t, err)
	require.Equal(t, []string{"install", "--use-language-version-tools"}, m.capturedArgs)
}

func BenchmarkBulkSetConfigMixed(b *testing.B) {
	ctx := context.Background()
	stackName := FullyQualifiedStackName(pulumiOrg, "set_config_mixed", "dev")

	// initialize
	s, err := NewStackInlineSource(ctx, stackName, "set_config_mixed", func(ctx *pulumi.Context) error { return nil })
	if err != nil {
		b.Errorf("failed to initialize stack, err: %v", err)
		b.FailNow()
	}

	cfg := ConfigMap{
		"one":        ConfigValue{Value: "one", Secret: true},
		"two":        ConfigValue{Value: "two"},
		"three":      ConfigValue{Value: "three", Secret: true},
		"four":       ConfigValue{Value: "four"},
		"five":       ConfigValue{Value: "five", Secret: true},
		"six":        ConfigValue{Value: "six"},
		"seven":      ConfigValue{Value: "seven", Secret: true},
		"eight":      ConfigValue{Value: "eight"},
		"nine":       ConfigValue{Value: "nine", Secret: true},
		"ten":        ConfigValue{Value: "ten"},
		"eleven":     ConfigValue{Value: "one", Secret: true},
		"twelve":     ConfigValue{Value: "two"},
		"thirteen":   ConfigValue{Value: "three", Secret: true},
		"fourteen":   ConfigValue{Value: "four"},
		"fifteen":    ConfigValue{Value: "five", Secret: true},
		"sixteen":    ConfigValue{Value: "six"},
		"seventeen":  ConfigValue{Value: "seven", Secret: true},
		"eighteen":   ConfigValue{Value: "eight"},
		"nineteen":   ConfigValue{Value: "nine", Secret: true},
		"twenty":     ConfigValue{Value: "ten"},
		"one1":       ConfigValue{Value: "one", Secret: true},
		"two1":       ConfigValue{Value: "two"},
		"three1":     ConfigValue{Value: "three", Secret: true},
		"four1":      ConfigValue{Value: "four"},
		"five1":      ConfigValue{Value: "five", Secret: true},
		"six1":       ConfigValue{Value: "six"},
		"seven1":     ConfigValue{Value: "seven", Secret: true},
		"eight1":     ConfigValue{Value: "eight"},
		"nine1":      ConfigValue{Value: "nine", Secret: true},
		"ten1":       ConfigValue{Value: "ten"},
		"eleven1":    ConfigValue{Value: "one", Secret: true},
		"twelve1":    ConfigValue{Value: "two"},
		"thirteen1":  ConfigValue{Value: "three", Secret: true},
		"fourteen1":  ConfigValue{Value: "four"},
		"fifteen1":   ConfigValue{Value: "five", Secret: true},
		"sixteen1":   ConfigValue{Value: "six"},
		"seventeen1": ConfigValue{Value: "seven", Secret: true},
		"eighteen1":  ConfigValue{Value: "eight"},
		"nineteen1":  ConfigValue{Value: "nine", Secret: true},
		"twenty1":    ConfigValue{Value: "ten"},
	}

	err = s.SetAllConfig(ctx, cfg)
	if err != nil {
		b.Errorf("failed to set config, err: %v", err)
		b.FailNow()
	}

	defer func() {
		// -- pulumi stack rm --
		err = s.Workspace().RemoveStack(ctx, s.Name())
		require.NoError(b, err, "failed to remove stack. Resources have leaked.")
	}()
}

func BenchmarkBulkSetConfigPlain(b *testing.B) {
	ctx := context.Background()
	stackName := FullyQualifiedStackName(pulumiOrg, "set_config_plain", "dev")

	// initialize
	s, err := NewStackInlineSource(ctx, stackName, "set_config_plain", func(ctx *pulumi.Context) error { return nil })
	if err != nil {
		b.Errorf("failed to initialize stack, err: %v", err)
		b.FailNow()
	}

	cfg := ConfigMap{
		"one":        ConfigValue{Value: "one"},
		"two":        ConfigValue{Value: "two"},
		"three":      ConfigValue{Value: "three"},
		"four":       ConfigValue{Value: "four"},
		"five":       ConfigValue{Value: "five"},
		"six":        ConfigValue{Value: "six"},
		"seven":      ConfigValue{Value: "seven"},
		"eight":      ConfigValue{Value: "eight"},
		"nine":       ConfigValue{Value: "nine"},
		"ten":        ConfigValue{Value: "ten"},
		"eleven":     ConfigValue{Value: "one"},
		"twelve":     ConfigValue{Value: "two"},
		"thirteen":   ConfigValue{Value: "three"},
		"fourteen":   ConfigValue{Value: "four"},
		"fifteen":    ConfigValue{Value: "five"},
		"sixteen":    ConfigValue{Value: "six"},
		"seventeen":  ConfigValue{Value: "seven"},
		"eighteen":   ConfigValue{Value: "eight"},
		"nineteen":   ConfigValue{Value: "nine"},
		"twenty":     ConfigValue{Value: "ten"},
		"one1":       ConfigValue{Value: "one"},
		"two1":       ConfigValue{Value: "two"},
		"three1":     ConfigValue{Value: "three"},
		"four1":      ConfigValue{Value: "four"},
		"five1":      ConfigValue{Value: "five"},
		"six1":       ConfigValue{Value: "six"},
		"seven1":     ConfigValue{Value: "seven"},
		"eight1":     ConfigValue{Value: "eight"},
		"nine1":      ConfigValue{Value: "nine"},
		"ten1":       ConfigValue{Value: "ten"},
		"eleven1":    ConfigValue{Value: "one"},
		"twelve1":    ConfigValue{Value: "two"},
		"thirteen1":  ConfigValue{Value: "three"},
		"fourteen1":  ConfigValue{Value: "four"},
		"fifteen1":   ConfigValue{Value: "five"},
		"sixteen1":   ConfigValue{Value: "six"},
		"seventeen1": ConfigValue{Value: "seven"},
		"eighteen1":  ConfigValue{Value: "eight"},
		"nineteen1":  ConfigValue{Value: "nine"},
		"twenty1":    ConfigValue{Value: "ten"},
	}

	err = s.SetAllConfig(ctx, cfg)
	if err != nil {
		b.Errorf("failed to set config, err: %v", err)
		b.FailNow()
	}

	defer func() {
		// -- pulumi stack rm --
		err = s.Workspace().RemoveStack(ctx, s.Name())
		require.NoError(b, err, "failed to remove stack. Resources have leaked.")
	}()
}

func BenchmarkBulkSetConfigSecret(b *testing.B) {
	ctx := context.Background()
	stackName := FullyQualifiedStackName(pulumiOrg, "set_config_plain", "dev")

	// initialize
	s, err := NewStackInlineSource(ctx, stackName, "set_config_plain", func(ctx *pulumi.Context) error { return nil })
	if err != nil {
		b.Errorf("failed to initialize stack, err: %v", err)
		b.FailNow()
	}

	cfg := ConfigMap{
		"one":        ConfigValue{Value: "one", Secret: true},
		"two":        ConfigValue{Value: "two", Secret: true},
		"three":      ConfigValue{Value: "three", Secret: true},
		"four":       ConfigValue{Value: "four", Secret: true},
		"five":       ConfigValue{Value: "five", Secret: true},
		"six":        ConfigValue{Value: "six", Secret: true},
		"seven":      ConfigValue{Value: "seven", Secret: true},
		"eight":      ConfigValue{Value: "eight", Secret: true},
		"nine":       ConfigValue{Value: "nine", Secret: true},
		"ten":        ConfigValue{Value: "ten", Secret: true},
		"eleven":     ConfigValue{Value: "one", Secret: true},
		"twelve":     ConfigValue{Value: "two", Secret: true},
		"thirteen":   ConfigValue{Value: "three", Secret: true},
		"fourteen":   ConfigValue{Value: "four", Secret: true},
		"fifteen":    ConfigValue{Value: "five", Secret: true},
		"sixteen":    ConfigValue{Value: "six", Secret: true},
		"seventeen":  ConfigValue{Value: "seven", Secret: true},
		"eighteen":   ConfigValue{Value: "eight", Secret: true},
		"nineteen":   ConfigValue{Value: "nine", Secret: true},
		"1twenty":    ConfigValue{Value: "ten", Secret: true},
		"one1":       ConfigValue{Value: "one", Secret: true},
		"two1":       ConfigValue{Value: "two", Secret: true},
		"three1":     ConfigValue{Value: "three", Secret: true},
		"four1":      ConfigValue{Value: "four", Secret: true},
		"five1":      ConfigValue{Value: "five", Secret: true},
		"six1":       ConfigValue{Value: "six", Secret: true},
		"seven1":     ConfigValue{Value: "seven", Secret: true},
		"eight1":     ConfigValue{Value: "eight", Secret: true},
		"nine1":      ConfigValue{Value: "nine", Secret: true},
		"ten1":       ConfigValue{Value: "ten", Secret: true},
		"eleven1":    ConfigValue{Value: "one", Secret: true},
		"twelve1":    ConfigValue{Value: "two", Secret: true},
		"thirteen1":  ConfigValue{Value: "three", Secret: true},
		"fourteen1":  ConfigValue{Value: "four", Secret: true},
		"fifteen1":   ConfigValue{Value: "five", Secret: true},
		"sixteen1":   ConfigValue{Value: "six", Secret: true},
		"seventeen1": ConfigValue{Value: "seven", Secret: true},
		"eighteen1":  ConfigValue{Value: "eight", Secret: true},
		"nineteen1":  ConfigValue{Value: "nine", Secret: true},
		"twenty1":    ConfigValue{Value: "ten", Secret: true},
	}

	err = s.SetAllConfig(ctx, cfg)
	if err != nil {
		b.Errorf("failed to set config, err: %v", err)
		b.FailNow()
	}

	defer func() {
		// -- pulumi stack rm --
		err = s.Workspace().RemoveStack(ctx, s.Name())
		require.NoError(b, err, "failed to remove stack. Resources have leaked.")
	}()
}

func getTestOrg() string {
	testOrg := pulumiTestOrg
	if _, set := os.LookupEnv("PULUMI_TEST_ORG"); set {
		testOrg = os.Getenv("PULUMI_TEST_ORG")
	}
	return testOrg
}

func countSteps(log []events.EngineEvent) int {
	steps := 0
	for _, e := range log {
		if e.ResourcePreEvent != nil {
			steps++
		}
	}
	return steps
}

func containsSummary(log []events.EngineEvent) bool {
	hasSummary := false
	for _, e := range log {
		if e.SummaryEvent != nil {
			hasSummary = true
		}
	}
	return hasSummary
}

func collectEvents(eventChannel <-chan events.EngineEvent, events *[]events.EngineEvent) *sync.WaitGroup {
	var wg sync.WaitGroup
	wg.Add(1)
	go (func() {
		for event := range eventChannel {
			*events = append(*events, event)
		}
		wg.Done()
	})()
	return &wg
}

type MyResource struct {
	pulumi.ResourceState
}

func NewMyResource(ctx *pulumi.Context, name string, opts ...pulumi.ResourceOption) (*MyResource, error) {
	myResource := &MyResource{}
	err := ctx.RegisterComponentResource("my:module:MyResource", name, myResource, opts...)
	if err != nil {
		return nil, err
	}

	return myResource, nil
}

func TestStackLifecycleInlineProgramRunProgram(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sName := ptesting.RandomStackName()
	stackName := FullyQualifiedStackName(pulumiOrg, pName, sName)

	s, err := NewStackInlineSource(ctx, stackName, pName, func(ctx *pulumi.Context) error {
		_, err := NewMyResource(ctx, "res")
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		t.Errorf("failed to initialize stack, err: %v", err)
		t.FailNow()
	}

	_, err = s.Up(ctx, optup.UserAgent(agent), optup.Refresh())
	require.NoError(t, err, "up failed")

	_, err = s.Refresh(ctx, optrefresh.RunProgram(true))
	require.NoError(t, err, "refresh failed")

	_, err = s.Destroy(ctx, optdestroy.RunProgram(true))
	require.NoError(t, err, "destroy failed")
}
