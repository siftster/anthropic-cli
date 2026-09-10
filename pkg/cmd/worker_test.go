package cmd

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
)

// clearWorkerEnv unsets every variable the `beta:worker run` flags read, so
// ambient credentials can't leak into a test; call it before any t.Setenv.
func clearWorkerEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"ANTHROPIC_SESSION_ID", "ANTHROPIC_ENVIRONMENT_KEY", "ANTHROPIC_WORK_ID",
		"ANTHROPIC_ENVIRONMENT_ID", "ANTHROPIC_BASE_URL",
		"ANTHROPIC_WORK_SECRET", "ANTHROPIC_WORK_SECRET_FILE",
	} {
		clearEnv(t, key)
	}
}

// runWorkerRun parses argv against a fresh `beta:worker run` definition and
// resolves the credentials, without touching the network. envKey/workSecret
// are only meaningful when err is nil.
func runWorkerRun(t *testing.T, argv ...string) (envKey, workSecret string, err error) {
	t.Helper()
	def := workerRunCommandDef()
	def.Action = func(_ context.Context, cmd *cli.Command) error {
		envKey, workSecret, err = workerRunCredentials(cmd)
		return err
	}
	runErr := run(t, def, append([]string{"run"}, argv...)...)
	if err == nil {
		err = runErr
	}
	return envKey, workSecret, err
}

func workSecretFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "work-secret")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return path
}

var workerRunRequiredArgs = []string{"--session-id", "ses_1", "--work-id", "work_1", "--environment-id", "env_1"}

func TestWorkerRunWorkSecretFile(t *testing.T) {
	clearWorkerEnv(t)
	path := workSecretFile(t, "  wksec_TOKEN\n")
	envKey, workSecret, err := runWorkerRun(t, append(workerRunRequiredArgs, "--work-secret-file", path)...)
	require.NoError(t, err)
	assert.Equal(t, "wksec_TOKEN", workSecret, "file contents are whitespace-trimmed")
	assert.Empty(t, envKey)
}

func TestWorkerRunWorkSecretFileWorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the permission check is disabled on Windows")
	}
	clearWorkerEnv(t)
	path := workSecretFile(t, "wksec_TOKEN\n")
	require.NoError(t, os.Chmod(path, 0o644))
	_, _, err := runWorkerRun(t, append(workerRunRequiredArgs, "--work-secret-file", path)...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), path)
	assert.Contains(t, err.Error(), "chmod o-rw")
	assert.NotContains(t, err.Error(), "wksec_TOKEN", "the error never carries the secret")
}

func TestWorkerRunWorkSecretFileGroupReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the permission check is disabled on Windows")
	}
	clearWorkerEnv(t)
	path := workSecretFile(t, "wksec_TOKEN\n")
	require.NoError(t, os.Chmod(path, 0o640))
	envKey, workSecret, err := runWorkerRun(t, append(workerRunRequiredArgs, "--work-secret-file", path)...)
	require.NoError(t, err)
	assert.Equal(t, "wksec_TOKEN", workSecret, "group read stays allowed — Kubernetes writes the token 0440 for a group-member worker")
	assert.Empty(t, envKey)
}

func TestWorkerRunWorkSecretFileUnreadable(t *testing.T) {
	clearWorkerEnv(t)
	path := filepath.Join(t.TempDir(), "missing")
	_, _, err := runWorkerRun(t, append(workerRunRequiredArgs, "--work-secret-file", path)...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), path, "error names the file the operator pointed at")
}

func TestWorkerRunWorkSecretFileEmpty(t *testing.T) {
	clearWorkerEnv(t)
	path := workSecretFile(t, " \n\t")
	_, _, err := runWorkerRun(t, append(workerRunRequiredArgs, "--work-secret-file", path)...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), path)
	assert.Contains(t, err.Error(), "empty")
}

func TestWorkerRunNeitherCredential(t *testing.T) {
	clearWorkerEnv(t)
	_, _, err := runWorkerRun(t, workerRunRequiredArgs...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--environment-key")
	assert.Contains(t, err.Error(), "--work-secret-file")
	assert.Contains(t, err.Error(), "ANTHROPIC_WORK_SECRET")
}

func TestWorkerRunEnvironmentKeyOnly(t *testing.T) {
	clearWorkerEnv(t)
	envKey, workSecret, err := runWorkerRun(t, append(workerRunRequiredArgs, "--environment-key", "envkey_1")...)
	require.NoError(t, err)
	assert.Equal(t, "envkey_1", envKey)
	assert.Empty(t, workSecret)
}

func TestWorkerRunWorkSecretEnvVarAlone(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("ANTHROPIC_WORK_SECRET", "wksec_ENV")
	envKey, workSecret, err := runWorkerRun(t, workerRunRequiredArgs...)
	require.NoError(t, err)
	assert.Empty(t, envKey)
	assert.Empty(t, workSecret, "the SDK reads ANTHROPIC_WORK_SECRET itself")
}

// TestWorkerRunOnPremEntrypoint pins the invocation cma-onprem's
// kubernetes/sandbox/worker.sh uses: ids and base URL from env vars, the
// secret from ANTHROPIC_WORK_SECRET_FILE, and no environment key anywhere.
func TestWorkerRunOnPremEntrypoint(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("ANTHROPIC_SESSION_ID", "ses_1")
	t.Setenv("ANTHROPIC_WORK_ID", "work_1")
	t.Setenv("ANTHROPIC_ENVIRONMENT_ID", "env_1")
	t.Setenv("ANTHROPIC_BASE_URL", "https://api.example.com")
	t.Setenv("ANTHROPIC_WORK_SECRET_FILE", workSecretFile(t, "wksec_TOKEN\n"))

	var maxIdle time.Duration
	var envKey, workSecret string
	def := workerRunCommandDef()
	def.Action = func(_ context.Context, cmd *cli.Command) (err error) {
		maxIdle = cmd.Duration("max-idle")
		envKey, workSecret, err = workerRunCredentials(cmd)
		return err
	}
	require.NoError(t, run(t, def, "run", "--workdir", t.TempDir(), "--max-idle", "30m"))
	assert.Equal(t, 30*time.Minute, maxIdle)
	assert.Empty(t, envKey)
	assert.Equal(t, "wksec_TOKEN", workSecret)
}
