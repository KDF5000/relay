package pi_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runtimepi "github.com/KDF5000/relay/runtime/pi"
)

func fakeModelCLI(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pi")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProbeModels(t *testing.T) {
	binary := fakeModelCLI(t, `[ "$*" = '--mode rpc --no-session' ] || exit 2
[ "$PI_CODING_AGENT_DIR" = '/custom/pi' ] || exit 3
read -r catalog
read -r state
[ "$catalog" = '{"id":"relay-models","type":"get_available_models"}' ] || exit 4
[ "$state" = '{"id":"relay-state","type":"get_state"}' ] || exit 5
cat <<'JSON'
{"type":"notification","data":"unrelated startup event"}
{"type":"response","id":"unrelated","success":true}
{"type":"response","id":"relay-state","success":true,"data":{"model":{"id":"glm-5.3","provider":"zai-coding-cn"}}}
{"type":"response","id":"relay-models","success":true,"data":{"models":[{"id":"glm-5.3","provider":"zai-coding-cn","name":"GLM-5.3"},{"id":"glm-5.3","provider":"other"},{"id":"glm-5.3","provider":"other"},{"id":"invalid"}]}}
JSON
`)
	models, err := runtimepi.ProbeModels(context.Background(), runtimepi.Config{Binary: binary, Env: map[string]string{"PI_CODING_AGENT_DIR": "/custom/pi"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "zai-coding-cn/glm-5.3" || !models[0].Default || models[1].ID != "other/glm-5.3" || models[1].Default {
		t.Fatalf("models = %+v", models)
	}
	if models[0].DisplayName != "GLM-5.3 (zai-coding-cn)" {
		t.Fatalf("model label = %q", models[0].DisplayName)
	}
}

func TestProbeModelsResponses(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		wantError    bool
		count        int
	}{
		{"empty", `{"type":"response","id":"relay-models","success":true,"data":{"models":[]}}`, false, 0},
		{"missing default", `{"type":"response","id":"relay-models","success":true,"data":{"models":[{"id":"model","provider":"provider"}]}}`, false, 1},
		{"failure", `{"type":"response","id":"relay-models","success":false,"error":"private-provider-secret"}`, true, 0},
		{"broken stream", `{"type":`, true, 0},
		{"no response", `{"type":"response","id":"unrelated","success":true}`, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binary := fakeModelCLI(t, "cat <<'JSON'\n"+tc.output+"\nJSON\n")
			models, err := runtimepi.ProbeModels(context.Background(), runtimepi.Config{Binary: binary})
			if (err != nil) != tc.wantError || len(models) != tc.count {
				t.Fatalf("models=%+v err=%v", models, err)
			}
			if err != nil && strings.Contains(err.Error(), "private-provider-secret") {
				t.Fatal("private diagnostic leaked")
			}
		})
	}
}

func TestProbeModelsHonorsDeadline(t *testing.T) {
	binary := fakeModelCLI(t, "sleep 30\n")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := runtimepi.ProbeModels(ctx, runtimepi.Config{Binary: binary})
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("deadline failed: %v", err)
	}
}
