package node_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/controlplane"
	"github.com/KDF5000/relay/node"
)

type delayedBatchControlPlane struct {
	*controlplane.Service
	delay      time.Duration
	batchCalls atomic.Int32
}

func (c *delayedBatchControlPlane) AppendEvents(ctx context.Context, runID, attemptID, lease string, events []controlplane.ProducerEvent) error {
	c.batchCalls.Add(1)
	timer := time.NewTimer(c.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}
	return c.Service.AppendEvents(ctx, runID, attemptID, lease, events)
}

func TestWorkerBatchesEventsAcrossHighLatencyLink(t *testing.T) {
	ctx := context.Background()
	service := controlplane.New(time.Second)
	cp := &delayedBatchControlPlane{Service: service, delay: 80 * time.Millisecond}
	worker := &node.Worker{
		Registration: controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "batch-node", Runtimes: []controlplane.Runtime{{Provider: "test"}}, Capacity: 1},
		ControlPlane: cp,
		Executors: node.ExecutorMap{"test": relay.ExecutorFunc(func(ctx context.Context, execution relay.Execution) (relay.Result, error) {
			for index := 0; index < 192; index++ {
				execution.Emit(ctx, "assistant.message.delta", map[string]string{"delta": fmt.Sprint(index)})
			}
			return relay.Result{Summary: "done"}, nil
		})},
	}
	if _, err := worker.Register(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := service.Submit(ctx, relay.Request{AgentID: "agent", IdempotencyKey: "batch-latency", Runtime: relay.RuntimeRequirement{Provider: "test"}, Input: relay.Input{Prompt: "work"}})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("batched delivery took %s", elapsed)
	}
	if calls := cp.batchCalls.Load(); calls != 3 {
		t.Fatalf("batch calls=%d, want 3", calls)
	}
	events, err := service.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	deltas := 0
	for _, event := range events {
		if event.Type == "assistant.message.delta" {
			deltas++
		}
	}
	if deltas != 192 {
		t.Fatalf("persisted deltas=%d", deltas)
	}
}

type lostBatchResponse struct {
	*controlplane.Service
	calls atomic.Int32
}

func (c *lostBatchResponse) AppendEvents(ctx context.Context, runID, attemptID, lease string, events []controlplane.ProducerEvent) error {
	call := c.calls.Add(1)
	if err := c.Service.AppendEvents(ctx, runID, attemptID, lease, events); err != nil {
		return err
	}
	if call == 1 {
		return errors.New("response lost after batch commit")
	}
	return nil
}

func TestWorkerRetriesLostBatchResponseWithoutDuplicates(t *testing.T) {
	ctx := context.Background()
	service := controlplane.New(time.Second)
	cp := &lostBatchResponse{Service: service}
	worker := &node.Worker{
		Registration: controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "batch-retry-node", Runtimes: []controlplane.Runtime{{Provider: "test"}}, Capacity: 1},
		ControlPlane: cp,
		Executors: node.ExecutorMap{"test": relay.ExecutorFunc(func(ctx context.Context, execution relay.Execution) (relay.Result, error) {
			execution.Emit(ctx, "assistant.message.delta", json.RawMessage(`{"delta":"one"}`))
			execution.Emit(ctx, "assistant.message.delta", json.RawMessage(`{"delta":"two"}`))
			return relay.Result{Summary: "done"}, nil
		})},
	}
	if _, err := worker.Register(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := service.Submit(ctx, relay.Request{AgentID: "agent", IdempotencyKey: "lost-batch-response", Runtime: relay.RuntimeRequirement{Provider: "test"}, Input: relay.Input{Prompt: "work"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if calls := cp.calls.Load(); calls != 2 {
		t.Fatalf("batch calls=%d, want 2", calls)
	}
	events, err := service.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	deltas := 0
	for _, event := range events {
		if event.Type == "assistant.message.delta" {
			deltas++
		}
	}
	if deltas != 2 {
		t.Fatalf("persisted deltas=%d", deltas)
	}
}
