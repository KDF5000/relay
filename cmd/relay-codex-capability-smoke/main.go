package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/binding"
	"github.com/KDF5000/relay/controlplane"
	"github.com/KDF5000/relay/node"
	runtimecodex "github.com/KDF5000/relay/runtime/codex"
	"github.com/KDF5000/relay/sdk"
	"github.com/KDF5000/relay/transport/httpapi"
)

func main() {
	toolDirFlag := flag.String("tool-dir", "./bin", "directory containing relay-tool")
	bindingFlag := flag.String("binding", "./bin/relay-example-capability", "capability CLI executable")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	toolDir := mustAbs(*toolDirFlag)
	bindingCommand := mustAbs(*bindingFlag)
	if err := requireExecutable(filepath.Join(toolDir, "relay-tool")); err != nil {
		log.Fatal(err)
	}
	if err := requireExecutable(bindingCommand); err != nil {
		log.Fatal(err)
	}
	version, err := runtimecodex.ProbeVersion(ctx, "codex")
	if err != nil {
		log.Fatal(err)
	}
	workRoot, err := os.MkdirTemp("", "relay-codex-capability-smoke-")
	if err != nil {
		log.Fatal(err)
	}

	registry := binding.NewRegistry()
	if err := registry.Register(
		binding.Descriptor{Name: "issue.read", Version: "1", Kind: "exec"},
		binding.ExecProvider{Config: binding.Exec{Command: bindingCommand}},
	); err != nil {
		log.Fatal(err)
	}
	service := controlplane.New(time.Minute)
	server := httptest.NewServer(httpapi.NewHandler(service))
	defer server.Close()
	transport := httpapi.NewClient(server.URL)
	host := sdk.New(transport)
	worker := &node.Worker{
		Registration: controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "local-codex-capability-smoke", Runtimes: []controlplane.Runtime{{Provider: "codex", Version: version}}, Capacity: 1},
		ControlPlane: transport,
		Bindings:     registry,
		Executors: node.ExecutorMap{"codex": runtimecodex.Executor{Config: runtimecodex.Config{
			Binary: "codex", ToolDir: toolDir, WorkRoot: workRoot, Ephemeral: true,
		}}},
	}
	if _, err := worker.Register(ctx); err != nil {
		log.Fatal(err)
	}
	queued, err := host.Submit(ctx, relay.Request{
		AgentID:        "capability-smoke-test",
		IdempotencyKey: "codex-capability-smoke-1",
		Runtime:        relay.RuntimeRequirement{Provider: "codex"},
		Input:          relay.Input{Type: "task", Version: "1", Prompt: "Use relay-tool to call the granted issue.read capability for resource MUL-42. Read the title from the returned JSON; do not guess it or inspect unrelated files. Reply with RELAY_CODEX_CAPABILITY_OK: followed by that title."},
		Capabilities:   []relay.CapabilityGrant{{Name: "issue.read", Version: "1", Effect: "read", Resources: []string{"MUL-42"}}},
	})
	if err != nil {
		log.Fatal(err)
	}
	if _, err := worker.RunOnce(ctx); err != nil {
		log.Fatal(err)
	}
	completed, err := host.Run(ctx, queued.ID)
	if err != nil {
		log.Fatal(err)
	}
	if completed.Result == nil || strings.TrimSpace(completed.Result.Summary) != "RELAY_CODEX_CAPABILITY_OK: Fix flaky scheduler" {
		log.Fatalf("unexpected Codex result: %+v", completed.Result)
	}
	events, err := host.Events(ctx, queued.ID)
	if err != nil {
		log.Fatal(err)
	}
	eventTypes := make([]string, 0, len(events))
	for _, event := range events {
		eventTypes = append(eventTypes, event.Type)
	}
	if !contains(eventTypes, "capability.succeeded") {
		log.Fatalf("capability was not invoked; events: %v", eventTypes)
	}
	output := struct {
		CodexVersion string   `json:"codex_version"`
		RunID        string   `json:"run_id"`
		Status       string   `json:"status"`
		Summary      string   `json:"summary"`
		Events       []string `json:"events"`
		WorkRoot     string   `json:"work_root"`
	}{version, completed.ID, string(completed.Status), completed.Result.Summary, eventTypes, workRoot}
	encoded, _ := json.MarshalIndent(output, "", "  ")
	fmt.Println(string(encoded))
}

func mustAbs(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		log.Fatal(err)
	}
	return absolute
}

func requireExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("required executable %s: %w", path, err)
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return fmt.Errorf("required executable %s is not executable", path)
	}
	return nil
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
