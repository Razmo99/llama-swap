package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMergeBaseConfig_PreservesStaticSettingsAndInjectsModels(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	basePath := filepath.Join(tempDir, "base.yaml")
	require.NoError(t, os.WriteFile(basePath, []byte(`
startPort: 10001
healthCheckTimeout: 500
logLevel: debug
logToStdout: both
models:
  stale:
    cmd: old
`), 0o644))

	merged, err := mergeBaseConfig(basePath, generatedConfig{
		HealthCheckTimeout: 3600,
		LogLevel:           "info",
		Models: map[string]generatedModel{
			"qwen3-4b-128k": {
				Name:          "qwen3-4b-128k",
				Cmd:           "start qwen",
				CmdStop:       "stop qwen",
				Proxy:         "http://qwen3-4b-128k:8080",
				CheckEndpoint: "/v1/models",
			},
		},
	})
	require.NoError(t, err)

	require.Equal(t, 10001, merged["startPort"])
	require.Equal(t, 500, merged["healthCheckTimeout"])
	require.Equal(t, "debug", merged["logLevel"])
	require.Equal(t, "both", merged["logToStdout"])

	models, ok := merged["models"].(map[string]generatedModel)
	require.True(t, ok)
	require.Contains(t, models, "qwen3-4b-128k")
	require.NotContains(t, models, "stale")
}

func TestMergeBaseConfig_SetsDefaultsWhenBaseOmitsThem(t *testing.T) {
	t.Parallel()

	merged, err := mergeBaseConfig("", generatedConfig{
		HealthCheckTimeout: 3600,
		LogLevel:           "info",
		Models: map[string]generatedModel{
			"model-a": {
				Name:    "model-a",
				Cmd:     "start",
				CmdStop: "stop",
				Proxy:   "http://model-a:8080",
			},
		},
	})
	require.NoError(t, err)

	require.Equal(t, 3600, merged["healthCheckTimeout"])
	require.Equal(t, "info", merged["logLevel"])

	models, ok := merged["models"].(map[string]generatedModel)
	require.True(t, ok)
	require.Contains(t, models, "model-a")
}
