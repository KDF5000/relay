package httpapi_test

import (
	"context"
	"encoding/json"
	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/controlplane"
	"github.com/KDF5000/relay/transport/httpapi"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProducerEventBatchPreservesOrderAndDeduplicates(t *testing.T) {
	ctx := context.Background()
	service := controlplane.New(time.Second)
	server := httptest.NewServer(httpapi.NewHandler(service))
	defer server.Close()
	client := httpapi.NewClient(server.URL)
	if _, err := client.RegisterNode(ctx, controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "batch-node", Capacity: 1, Runtimes: []controlplane.Runtime{{Provider: "test"}}}); err != nil {
		t.Fatal(err)
	}
	run, err := client.Submit(ctx, relay.Request{AgentID: "agent", IdempotencyKey: "batch", Runtime: relay.RuntimeRequirement{Provider: "test"}, Input: relay.Input{Prompt: "work"}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := client.Claim(ctx, "batch-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx, a); err != nil {
		t.Fatal(err)
	}
	batch := []controlplane.ProducerEvent{
		{EventID: "1", Type: "assistant.message.delta", Data: json.RawMessage(`{"delta":"one"}`)},
		{EventID: "2", Type: "assistant.message.delta", Data: json.RawMessage(`{"delta":"two"}`)},
	}
	for index := 0; index < 2; index++ {
		if err := client.AppendEvents(ctx, run.ID, a.AttemptID, a.LeaseToken, batch); err != nil {
			t.Fatal(err)
		}
	}
	events, err := client.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	for _, event := range events {
		if event.Type != "assistant.message.delta" {
			continue
		}
		var data map[string]string
		if err := json.Unmarshal(event.Data, &data); err != nil {
			t.Fatal(err)
		}
		deltas = append(deltas, data["delta"])
	}
	if len(deltas) != 2 || deltas[0] != "one" || deltas[1] != "two" {
		t.Fatalf("deltas=%v", deltas)
	}
	conflict := []controlplane.ProducerEvent{
		{EventID: "3", Type: "assistant.message.delta", Data: json.RawMessage(`{"delta":"must roll back"}`)},
		{EventID: "2", Type: "assistant.message.delta", Data: json.RawMessage(`{"delta":"changed"}`)},
	}
	if err := client.AppendEvents(ctx, run.ID, a.AttemptID, a.LeaseToken, conflict); err == nil {
		t.Fatal("conflicting batch retry accepted")
	}
	events, err = client.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == "assistant.message.delta" && string(event.Data) == `{"delta":"must roll back"}` {
			t.Fatal("part of a rejected batch was committed")
		}
	}
}

func TestProducerEventDeduplication(t *testing.T) {
	ctx := context.Background()
	service := controlplane.New(time.Second)
	server := httptest.NewServer(httpapi.NewHandler(service))
	defer server.Close()
	client := httpapi.NewClient(server.URL)
	if _, err := client.RegisterNode(ctx, controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "dedup-node", Capacity: 1, Runtimes: []controlplane.Runtime{{Provider: "test"}}}); err != nil {
		t.Fatal(err)
	}
	run, err := client.Submit(ctx, relay.Request{AgentID: "agent", IdempotencyKey: "dedup", Runtime: relay.RuntimeRequirement{Provider: "test"}, Input: relay.Input{Prompt: "work"}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := client.Claim(ctx, "dedup-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx, a); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := client.AppendEvent(ctx, run.ID, a.AttemptID, a.LeaseToken, "assistant.message.delta", map[string]string{"delta": "hello"}, "event-1"); err != nil {
			t.Fatal(err)
		}
	}
	if err := client.AppendEvent(ctx, run.ID, a.AttemptID, a.LeaseToken, "assistant.message.delta", map[string]string{"delta": "changed"}, "event-1"); err == nil {
		t.Fatal("conflicting content accepted")
	}
	if err := client.AppendEvent(ctx, run.ID, a.AttemptID, "stale-token", "assistant.message.delta", map[string]string{"delta": "hello"}, "event-1"); err == nil {
		t.Fatal("stale lease accepted")
	}
	events, err := client.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Type == "assistant.message.delta" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("count=%d", count)
	}
}
