package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
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

func TestProbeRuntimeSupportsNativeAgentCLIs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	dir := t.TempDir()
	for _, test := range []struct {
		name, version string
	}{{"claude", "2.1.30"}, {"pi", "0.55.1"}, {"opencode", "2.0.32"}, {"qoder", "1.1.26"}, {"qwenwork", "1.1.71"}} {
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

func TestDiscoverOpenCodeModelsAdvertisedAtRegistration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	command := filepath.Join(t.TempDir(), "opencode")
	script := "#!/bin/sh\nif [ \"$1\" = '--version' ]; then echo 'opencode v2.0.32'; exit 0; fi\nprintf '%s\\n' 'provider/model-a' 'provider/model-b'\n"
	if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	item := runtimeConfig{Provider: "opencode", Command: command, Model: "provider/model-b"}
	discoverRuntimeModels(&item, "opencode")
	got := probeRuntime(item)
	if !isOpenCode(item) || got.State != "healthy" || got.Version != "2.0.32" || len(got.ModelCatalog) != 2 || got.DefaultModel != "provider/model-b" {
		t.Fatalf("OpenCode registration = %+v", got)
	}
}

func TestQoderVariantsAdvertisedAtRegistration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	for _, test := range []struct{ kind, provider, binary string }{
		{"qoder", "qoder", "qodercli"},
		{"", "qwenwork", "qoderclicn"},
		{"qoderclicn", "qwenwork-team", "qoderclicn"},
	} {
		command := filepath.Join(t.TempDir(), test.binary)
		script := "#!/bin/sh\nif [ \"$1\" = '--version' ]; then echo '1.1.71'; exit 0; fi\nprintf '%s\\n' MODEL Auto Qwen3.8-Max 'Team (mode-1)'\n"
		if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		item := runtimeConfig{Kind: test.kind, Provider: test.provider, Command: command}
		variant, ok := qoderVariant(item)
		if !ok || variant.Binary != test.binary {
			t.Fatalf("%s/%s variant = %+v", test.kind, test.provider, variant)
		}
		discoverRuntimeModels(&item, variant.Provider)
		got := probeRuntime(item)
		if got.State != "healthy" || got.Version != "1.1.71" || len(got.ModelCatalog) != 3 || got.DefaultModel != "Auto" || got.Models[2] != "mode-1" {
			t.Fatalf("%s registration = %+v", test.provider, got)
		}
	}
}

func TestCompatibleCatalogRefreshKeepsLoadTimestampAndReplacesModels(t *testing.T) {
	command := filepath.Join(t.TempDir(), "traex")
	script := `#!/bin/sh
if [ "$1" = '--version' ]; then echo 'traex 1.0.0'; exit 0; fi
read -r initialize
printf '%s\n' '{"id":1,"result":{}}'
read -r initialized
read -r models
printf '%s\n' '{"id":2,"result":{"data":[{"id":"gpt","model":"gpt","isDefault":true,"supportedReasoningEfforts":[{"reasoningEffort":"high"}],"defaultReasoningEffort":"high","businessMetadata":{"load":{"load_percent":0}}}],"nextCursor":null}}'
`
	if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	item := runtimeConfig{Kind: "trae", Provider: "trae", Command: command}
	discoverRuntimeModels(&item, "trae")
	if item.DetectedAt == nil || len(item.DetectedModels) != 1 || item.DetectedModels[0].LoadPercent == nil || *item.DetectedModels[0].LoadPercent != 0 {
		t.Fatalf("missing model snapshot: %+v", item)
	}
	original := *item.DetectedAt
	discoverRuntimeModels(&item, "trae")
	if len(item.DetectedModels) != 1 {
		t.Fatal("refresh duplicated the model list")
	}
	registered := probeRuntime(item)
	if registered.ModelCatalogCheckedAt == nil || !registered.ModelCatalogCheckedAt.Equal(*item.DetectedAt) || !registered.SupportsReasoningEffort || len(registered.ModelCatalog[0].SupportedReasoningEfforts) != 1 {
		t.Fatalf("registration lost metadata: %+v", registered)
	}
	item.Command = filepath.Join(t.TempDir(), "missing")
	item.LastModelProbe = time.Now().Add(-2 * time.Minute)
	lastSuccess := *item.DetectedAt
	refreshModelCatalog(&item)
	if !item.DetectedAt.Equal(lastSuccess) || item.DetectedAt.Before(original) || len(item.DetectedModels) != 1 {
		t.Fatal("failed discovery refreshed stale load or lost usable catalog")
	}
}
