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
