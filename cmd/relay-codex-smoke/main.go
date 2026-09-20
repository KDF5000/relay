package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http/httptest"
	"os"
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	version, err := runtimecodex.ProbeVersion(ctx, "codex")
	if err != nil {
		log.Fatal(err)
	}
	workRoot, err := os.MkdirTemp("", "relay-codex-smoke-")
	if err != nil {
		log.Fatal(err)
	}
	service := controlplane.New(time.Minute)
	server := httptest.NewServer(httpapi.NewHandler(service))
	defer server.Close()
	transport := httpapi.NewClient(server.URL)
	host := sdk.New(transport)
	worker := &node.Worker{Registration: controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "local-codex-smoke", Runtimes: []controlplane.Runtime{{Provider: "codex", Version: version}}, Capacity: 1}, ControlPlane: transport, Bindings: binding.NewRegistry(), Executors: node.ExecutorMap{"codex": runtimecodex.Executor{Config: runtimecodex.Config{Binary: "codex", WorkRoot: workRoot, Ephemeral: true}}}}
	if _, err := worker.Register(ctx); err != nil {
		log.Fatal(err)
	}
	queued, err := host.Submit(ctx, relay.Request{AgentID: "smoke-test", IdempotencyKey: "codex-smoke-1", Runtime: relay.RuntimeRequirement{Provider: "codex"}, Input: relay.Input{Type: "task", Version: "1", Prompt: "This is a Relay runtime connectivity test. Do not modify files and do not run shell commands. Reply with exactly: RELAY_CODEX_OK"}, Instructions: relay.InstructionBundle{Runtime: []relay.InstructionFragment{{ID: "smoke", Version: "1", Title: "Smoke test", Content: "Follow the prompt exactly and finish immediately."}}}})
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
	events, err := host.Events(ctx, queued.ID)
	if err != nil {
		log.Fatal(err)
	}
	output := struct {
		CodexVersion string    `json:"codex_version"`
		Run          relay.Run `json:"run"`
		EventCount   int       `json:"event_count"`
		WorkRoot     string    `json:"work_root"`
	}{version, completed, len(events), workRoot}
	encoded, _ := json.MarshalIndent(output, "", "  ")
	fmt.Println(string(encoded))
}
