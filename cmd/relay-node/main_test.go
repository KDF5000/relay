package main

import (
	"encoding/json"
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
