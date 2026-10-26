package postgres_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/controlplane"
)

func TestUsageSnapshotPersistsAcrossServiceRestart(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	service := controlplane.NewWithStorage(st, time.Second)
	if _, err := service.RegisterNode(ctx, controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "usage-node", Capacity: 1, Runtimes: []controlplane.Runtime{{Provider: "codex"}}}); err != nil {
		t.Fatal(err)
	}
	run, err := service.Submit(ctx, relay.Request{AgentID: "agent", IdempotencyKey: "usage", Runtime: relay.RuntimeRequirement{Provider: "codex"}, Input: relay.Input{Prompt: "work"}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := service.Claim(ctx, "usage-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx, a); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := service.AppendEvent(ctx, run.ID, a.AttemptID, a.LeaseToken, "usage.updated", json.RawMessage(`{"input_tokens":100,"output_tokens":20,"total_tokens":120,"cache_read_tokens":80,"status":"complete"}`), "usage-1"); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.AppendEvent(ctx, run.ID, a.AttemptID, a.LeaseToken, "usage.updated", json.RawMessage(`{"input_tokens":-1,"status":"complete"}`), "bad"); err == nil {
		t.Fatal("invalid snapshot accepted")
	}
	if err := service.Complete(ctx, a, relay.Result{Summary: "done"}); err != nil {
		t.Fatal(err)
	}
	restarted := controlplane.NewWithStorage(st, time.Second)
	result, err := restarted.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Usage == nil || *result.Usage.TotalTokens != 120 {
		t.Fatalf("lost/duplicated usage: %+v", result.Usage)
	}
}
