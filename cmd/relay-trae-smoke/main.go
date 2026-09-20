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
	runtimetrae "github.com/KDF5000/relay/runtime/trae"
	"github.com/KDF5000/relay/sdk"
	"github.com/KDF5000/relay/transport/httpapi"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	version, err := runtimetrae.ProbeVersion(ctx, "traex")
	if err != nil {
		log.Fatal(err)
	}
	workRoot, err := os.MkdirTemp("", "relay-trae-smoke-")
	if err != nil {
		log.Fatal(err)
	}
	service := controlplane.New(time.Minute)
	server := httptest.NewServer(httpapi.NewHandler(service))
	defer server.Close()
	transport := httpapi.NewClient(server.URL)
	host := sdk.New(transport)
	worker := &node.Worker{Registration: controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "local-trae-smoke", Runtimes: []controlplane.Runtime{{Provider: "trae", Version: version, State: "healthy"}}, Capacity: 1}, ControlPlane: transport, Bindings: binding.NewRegistry(), Executors: node.ExecutorMap{"trae": runtimetrae.Executor{Config: runtimetrae.Config{Binary: "traex", Protocol: "app-server", WorkRoot: workRoot, Ephemeral: true}}}}
	if _, err := worker.Register(ctx); err != nil {
		log.Fatal(err)
	}
	queued, err := host.Submit(ctx, relay.Request{AgentID: "smoke-test", IdempotencyKey: "trae-smoke-1", Runtime: relay.RuntimeRequirement{Provider: "trae"}, Input: relay.Input{Type: "task", Version: "1", Prompt: "This is a Relay runtime connectivity test. Do not modify files and do not run shell commands. Reply with exactly: RELAY_TRAE_OK"}, Instructions: relay.InstructionBundle{Runtime: []relay.InstructionFragment{{ID: "smoke", Version: "1", Title: "Smoke test", Content: "Follow the prompt exactly and finish immediately."}}}})
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
	streamed := false
	for _, event := range events {
		streamed = streamed || event.Type == "assistant.message.delta"
	}
	if !streamed {
		log.Fatal("Trae app-server did not emit assistant.message.delta")
	}
	output := struct {
		TraeVersion string    `json:"trae_version"`
		Run         relay.Run `json:"run"`
		EventCount  int       `json:"event_count"`
		WorkRoot    string    `json:"work_root"`
	}{version, completed, len(events), workRoot}
	encoded, _ := json.MarshalIndent(output, "", "  ")
	fmt.Println(string(encoded))
}
