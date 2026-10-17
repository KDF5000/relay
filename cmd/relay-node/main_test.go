package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestProbeRuntimeAdvertisesModels(t *testing.T) {
	runtime := probeRuntime(runtimeConfig{
		Provider: "mock",
		Command:  "/usr/bin/true",
		Model:    "model-default",
		Models:   []string{"model-default", "model-fast"},
	})
	if runtime.DefaultModel != "model-default" {
		t.Fatalf("default model = %q, want model-default", runtime.DefaultModel)
	}
	if len(runtime.Models) != 2 || runtime.Models[1] != "model-fast" {
		t.Fatalf("models = %#v", runtime.Models)
	}
}

func TestProbeRuntimeSupportsClaudeAndPi(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	dir := t.TempDir()
	for _, test := range []struct {
		name, version string
	}{{"claude", "2.1.30"}, {"pi", "0.55.1"}} {
		command := filepath.Join(dir, test.name)
		if err := os.WriteFile(command, []byte("#!/bin/sh\necho '"+test.name+" "+test.version+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		got := probeRuntime(runtimeConfig{Kind: test.name, Provider: test.name, Command: command})
		if got.State != "healthy" || got.Version != test.version {
			t.Fatalf("%s runtime = %#v", test.name, got)
		}
	}
}

func TestRuntimeConfigIgnoresLegacyPermissionFields(t *testing.T) {
	var cfg config
	if err := json.Unmarshal([]byte(`{"runtimes":[{"provider":"trae","sandbox":"read-only","permission_mode":"bypass_permissions"}]}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Runtimes) != 1 {
		t.Fatalf("runtimes = %#v", cfg.Runtimes)
	}
	_ = compatibleConfig(cfg.Runtimes[0])
}

func TestDiscoverPiModelsAdvertisedAtRegistration(t *testing.T) {
	command := filepath.Join(t.TempDir(), "pi")
	script := `#!/bin/sh
if [ "$1" = '--version' ]; then echo 'pi 1.3.0'; exit 0; fi
printf '%s\n' '{"type":"response","id":"relay-models","success":true,"data":{"models":[{"id":"glm-5.3","provider":"zai-coding-cn","name":"GLM-5.3"}]}}'
printf '%s\n' '{"type":"response","id":"relay-state","success":true,"data":{"model":{"id":"glm-5.3","provider":"zai-coding-cn"}}}'
`
	if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	item := runtimeConfig{Kind: "pi", Provider: "pi", Command: command}
	discoverRuntimeModels(&item, "pi")
	got := probeRuntime(item)
	if len(got.ModelCatalog) != 1 || len(got.Models) != 1 || got.Models[0] != "zai-coding-cn/glm-5.3" || got.DefaultModel != got.Models[0] {
		t.Fatalf("Pi registration omitted model metadata: %+v", got)
	}
}
