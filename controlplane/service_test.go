package controlplane_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/controlplane"
)

func TestSchedulerMatchesRuntimeLabelsAndCapabilities(t *testing.T) {
	ctx := context.Background()
	service := controlplane.New(time.Minute)
	_, _ = service.RegisterNode(ctx, controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "wrong-runtime", Labels: map[string]string{"pool": "engineering"}, Runtimes: []controlplane.Runtime{{Provider: "claude"}}, Capabilities: []controlplane.Capability{{Name: "issue.read", Version: "1", Kind: "exec"}}, Capacity: 1})
	_, _ = service.RegisterNode(ctx, controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "missing-tool", Labels: map[string]string{"pool": "engineering"}, Runtimes: []controlplane.Runtime{{Provider: "codex"}}, Capacity: 1})
	_, _ = service.RegisterNode(ctx, controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "matching", Labels: map[string]string{"pool": "engineering"}, Runtimes: []controlplane.Runtime{{Provider: "codex"}}, Capabilities: []controlplane.Capability{{Name: "issue.read", Version: "1", Kind: "exec"}}, Capacity: 1})
	run, err := service.Submit(ctx, relay.Request{AgentID: "agent", IdempotencyKey: "one", Runtime: relay.RuntimeRequirement{Provider: "codex", Labels: map[string]string{"pool": "engineering"}}, Input: relay.Input{Prompt: "work"}, Capabilities: []relay.CapabilityGrant{{Name: "issue.read", Version: "1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Claim(ctx, "wrong-runtime"); !errors.Is(err, controlplane.ErrNoAssignment) {
		t.Fatalf("wrong runtime claimed: %v", err)
	}
	if _, err := service.Claim(ctx, "missing-tool"); !errors.Is(err, controlplane.ErrNoAssignment) {
		t.Fatalf("node without capability claimed: %v", err)
	}
	assignment, err := service.Claim(ctx, "matching")
	if err != nil {
		t.Fatal(err)
	}
	if assignment.RunID != run.ID {
		t.Fatalf("claimed wrong run: %s", assignment.RunID)
	}
	if err := service.Start(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	if err := service.Complete(ctx, assignment, relay.Result{Summary: "done"}); err != nil {
		t.Fatal(err)
	}
	completed, _ := service.GetRun(ctx, run.ID)
	if completed.Status != relay.RunSucceeded || completed.Attempt.NodeID != "matching" {
		t.Fatalf("unexpected run: %#v", completed)
	}
	events, _ := service.Events(ctx, run.ID)
	if len(events) != 7 {
		t.Fatalf("expected 7 events, got %d", len(events))
	}
}

func TestRegisterNodeRejectsIncompatibleProtocol(t *testing.T) {
	service := controlplane.New(time.Minute)
	_, err := service.RegisterNode(context.Background(), controlplane.NodeRegistration{
		ID: "old-node", ProtocolVersion: "0", Capacity: 1,
		Runtimes: []controlplane.Runtime{{Provider: "test"}},
	})
	if !errors.Is(err, controlplane.ErrIncompatibleProtocol) {
		t.Fatalf("RegisterNode error = %v, want incompatible protocol", err)
	}
}

func TestSchedulerCanBindRunToRuntimeInstance(t *testing.T) {
	ctx := context.Background()
	service := controlplane.New(time.Minute)
	first, err := service.RegisterNode(ctx, controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "node-a", Runtimes: []controlplane.Runtime{{Provider: "codex"}}, Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.RegisterNode(ctx, controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "node-b", Runtimes: []controlplane.Runtime{{Provider: "codex"}}, Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	if first.Runtimes[0].ID != "node-a/codex" || second.Runtimes[0].ID != "node-b/codex" {
		t.Fatalf("runtime IDs were not normalized: %#v %#v", first.Runtimes, second.Runtimes)
	}
	_, err = service.Submit(ctx, relay.Request{AgentID: "bound-agent", IdempotencyKey: "bound-runtime", Runtime: relay.RuntimeRequirement{ID: "node-b/codex", Provider: "codex"}, Input: relay.Input{Prompt: "work"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Claim(ctx, "node-a"); !errors.Is(err, controlplane.ErrNoAssignment) {
		t.Fatalf("unselected runtime claimed bound run: %v", err)
	}
	assignment, err := service.Claim(ctx, "node-b")
	if err != nil {
		t.Fatal(err)
	}
	if assignment.Request.Runtime.ID != "node-b/codex" {
		t.Fatalf("runtime binding was not preserved: %#v", assignment.Request.Runtime)
	}
}

func TestExpiredUnstartedLeaseReturnsToQueue(t *testing.T) {
	ctx := context.Background()
	service := controlplane.New(time.Nanosecond)
	_, err := service.RegisterNode(ctx, controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "node", Runtimes: []controlplane.Runtime{{Provider: "codex"}}, Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Submit(ctx, relay.Request{AgentID: "agent", IdempotencyKey: "lease-expiry", Runtime: relay.RuntimeRequirement{Provider: "codex"}, Input: relay.Input{Prompt: "work"}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.Claim(ctx, "node")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx, first); !errors.Is(err, controlplane.ErrInvalidLease) {
		t.Fatalf("expected expired lease, got %v", err)
	}
	second, err := service.Claim(ctx, "node")
	if err != nil {
		t.Fatal(err)
	}
	if first.LeaseToken == second.LeaseToken {
		t.Fatal("requeued assignment reused lease token")
	}
}

func TestCompletionAndFailureReportsAreContentIdempotent(t *testing.T) {
	ctx := context.Background()
	newAssignment := func(key string) (*controlplane.Service, controlplane.Assignment) {
		service := controlplane.New(time.Second)
		if _, err := service.RegisterNode(ctx, controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "node", Capacity: 1, Runtimes: []controlplane.Runtime{{Provider: "test"}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := service.Submit(ctx, relay.Request{AgentID: "agent", IdempotencyKey: key, Runtime: relay.RuntimeRequirement{Provider: "test"}, Input: relay.Input{Prompt: "work"}}); err != nil {
			t.Fatal(err)
		}
		assignment, err := service.Claim(ctx, "node")
		if err != nil {
			t.Fatal(err)
		}
		if err := service.Start(ctx, assignment); err != nil {
			t.Fatal(err)
		}
		return service, assignment
	}

	successService, success := newAssignment("complete-idempotent")
	result := relay.Result{Summary: "done", Output: json.RawMessage(`{"b":2,"a":1}`)}
	if err := successService.Complete(ctx, success, result); err != nil {
		t.Fatal(err)
	}
	if err := successService.Complete(ctx, success, relay.Result{Summary: "done", Output: json.RawMessage(`{ "a": 1, "b": 2 }`)}); err != nil {
		t.Fatalf("identical completion rejected: %v", err)
	}
	if err := successService.Complete(ctx, success, relay.Result{Summary: "different"}); !errors.Is(err, controlplane.ErrInvalidTransition) {
		t.Fatalf("different completion error=%v", err)
	}
	events, _ := successService.Events(ctx, success.RunID)
	terminal := 0
	for _, event := range events {
		if event.Type == "run.succeeded" {
			terminal++
		}
	}
	if terminal != 1 {
		t.Fatalf("success events=%d", terminal)
	}

	failureService, failure := newAssignment("fail-idempotent")
	if err := failureService.Fail(ctx, failure, "runtime exited"); err != nil {
		t.Fatal(err)
	}
	if err := failureService.Fail(ctx, failure, "runtime exited"); err != nil {
		t.Fatalf("identical failure rejected: %v", err)
	}
	if err := failureService.Fail(ctx, failure, "different"); !errors.Is(err, controlplane.ErrInvalidTransition) {
		t.Fatalf("different failure error=%v", err)
	}
}

func TestRunningAttemptRecoveryCreatesNewAttemptAndFencesOldLease(t *testing.T) {
	ctx := context.Background()
	service := controlplane.New(10 * time.Millisecond)
	_, err := service.RegisterNode(ctx, controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "node", Runtimes: []controlplane.Runtime{{Provider: "codex"}}, Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	run, err := service.Submit(ctx, relay.Request{
		AgentID: "agent", IdempotencyKey: "running-recovery",
		Runtime: relay.RuntimeRequirement{Provider: "codex"}, Input: relay.Input{Prompt: "work"},
		Retry: relay.RetryPolicy{MaxAttempts: 2, Backoff: "20ms"},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.Claim(ctx, "node")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx, first); err != nil {
		t.Fatal(err)
	}
	time.Sleep(15 * time.Millisecond)
	if recovered, err := service.Reconcile(ctx, 10); err != nil || recovered != 1 {
		t.Fatalf("reconcile = %d, %v; want 1", recovered, err)
	}
	retried, err := service.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Status != relay.RunQueued || retried.Attempt.Number != 2 || retried.Attempt.Status != relay.AttemptQueued {
		t.Fatalf("unexpected recovered run: %+v", retried)
	}
	if err := service.Complete(ctx, first, relay.Result{Summary: "stale"}); !errors.Is(err, controlplane.ErrInvalidLease) {
		t.Fatalf("stale completion = %v, want invalid lease", err)
	}
	if _, err := service.Claim(ctx, "node"); !errors.Is(err, controlplane.ErrNoAssignment) {
		t.Fatalf("attempt ignored retry backoff: %v", err)
	}
	time.Sleep(25 * time.Millisecond)
	second, err := service.Claim(ctx, "node")
	if err != nil {
		t.Fatal(err)
	}
	if second.AttemptID == first.AttemptID || second.LeaseToken == first.LeaseToken {
		t.Fatal("recovery reused the lost attempt or lease")
	}
}

func TestRunUpdatesSignalsNewEvents(t *testing.T) {
	ctx := context.Background()
	service := controlplane.New(time.Second)
	if _, err := service.RegisterNode(ctx, controlplane.NodeRegistration{
		ProtocolVersion: relay.ProtocolVersion,
		ID:              "node",
		Capacity:        1,
		Runtimes:        []controlplane.Runtime{{Provider: "test"}},
	}); err != nil {
		t.Fatal(err)
	}
	run, err := service.Submit(ctx, relay.Request{
		AgentID: "agent", IdempotencyKey: "update-signal",
		Runtime: relay.RuntimeRequirement{Provider: "test"}, Input: relay.Input{Prompt: "work"},
	})
	if err != nil {
		t.Fatal(err)
	}
	assignment, err := service.Claim(ctx, "node")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(ctx, assignment); err != nil {
		t.Fatal(err)
	}

	updated := service.RunUpdates(run.ID)
	if err := service.AppendEvent(ctx, run.ID, assignment.AttemptID, assignment.LeaseToken, "assistant.message.delta", map[string]string{"delta": "hello"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-updated:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Run update signal was not closed after appending an event")
	}
	select {
	case <-service.RunUpdates(run.ID):
		t.Fatal("new Run update signal was already closed")
	default:
	}
}

func TestRunningAttemptWithoutRetryFailsAfterLeaseExpiry(t *testing.T) {
	ctx := context.Background()
	service := controlplane.New(5 * time.Millisecond)
	_, _ = service.RegisterNode(ctx, controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "node", Runtimes: []controlplane.Runtime{{Provider: "codex"}}, Capacity: 1})
	run, _ := service.Submit(ctx, relay.Request{AgentID: "agent", IdempotencyKey: "no-retry", Runtime: relay.RuntimeRequirement{Provider: "codex"}, Input: relay.Input{Prompt: "work"}})
	assignment, _ := service.Claim(ctx, "node")
	if err := service.Start(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := service.Reconcile(ctx, 10); err != nil {
		t.Fatal(err)
	}
	failed, err := service.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != relay.RunFailed || failed.Attempt.Status != relay.AttemptLost {
		t.Fatalf("unexpected exhausted run: %+v", failed)
	}
}

func TestNodeStateBecomesOfflineWithoutHeartbeat(t *testing.T) {
	ctx := context.Background()
	service := controlplane.NewWithOptions(controlplane.NewMemoryStorage(), controlplane.Options{LeaseTTL: time.Second, NodeOfflineAfter: 5 * time.Millisecond})
	_, _ = service.RegisterNode(ctx, controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "node", Runtimes: []controlplane.Runtime{{Provider: "codex"}}, Capacity: 1})
	nodes, _ := service.Nodes(ctx)
	if nodes[0].State != controlplane.NodeOnline {
		t.Fatalf("new node state = %q", nodes[0].State)
	}
	time.Sleep(10 * time.Millisecond)
	nodes, _ = service.Nodes(ctx)
	if nodes[0].State != controlplane.NodeOffline {
		t.Fatalf("stale node state = %q", nodes[0].State)
	}
}

func TestQueuedRunCancellationIsImmediateAndIdempotent(t *testing.T) {
	ctx := context.Background()
	service := controlplane.New(time.Second)
	run, err := service.Submit(ctx, relay.Request{AgentID: "agent", IdempotencyKey: "cancel-queued", Runtime: relay.RuntimeRequirement{Provider: "codex"}, Input: relay.Input{Prompt: "work"}})
	if err != nil {
		t.Fatal(err)
	}
	request := controlplane.CancelRequest{Reason: "no longer needed", RequestedBy: "user-1"}
	cancelled, err := service.CancelRun(ctx, run.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != relay.RunCancelled || cancelled.Attempt.Status != relay.AttemptCancelled || cancelled.CancelledAt == nil {
		t.Fatalf("unexpected cancelled run: %+v", cancelled)
	}
	if _, err := service.CancelRun(ctx, run.ID, request); err != nil {
		t.Fatal(err)
	}
	events, _ := service.Events(ctx, run.ID)
	if len(events) != 5 {
		t.Fatalf("idempotent cancellation produced %d events, want 5", len(events))
	}
}

func TestRunningCancellationIsDeliveredByLeaseRenewal(t *testing.T) {
	ctx := context.Background()
	service := controlplane.New(time.Second)
	_, _ = service.RegisterNode(ctx, controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "node", Runtimes: []controlplane.Runtime{{Provider: "codex"}}, Capacity: 1})
	run, _ := service.Submit(ctx, relay.Request{AgentID: "agent", IdempotencyKey: "cancel-running", Runtime: relay.RuntimeRequirement{Provider: "codex"}, Input: relay.Input{Prompt: "work"}})
	assignment, _ := service.Claim(ctx, "node")
	if err := service.Start(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	pending, err := service.CancelRun(ctx, run.ID, controlplane.CancelRequest{Reason: "stop", RequestedBy: "user-1"})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != relay.RunCancelling {
		t.Fatalf("status = %s, want cancelling", pending.Status)
	}
	update, err := service.Renew(ctx, assignment)
	if err != nil {
		t.Fatal(err)
	}
	if !update.CancelRequested || update.CancelReason != "stop" {
		t.Fatalf("unexpected renewal directive: %+v", update)
	}
	if err := service.Complete(ctx, assignment, relay.Result{Summary: "stale"}); !errors.Is(err, controlplane.ErrRunCancelled) {
		t.Fatalf("completion during cancellation = %v", err)
	}
	if err := service.AcknowledgeCancellation(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	cancelled, _ := service.GetRun(ctx, run.ID)
	if cancelled.Status != relay.RunCancelled || cancelled.Attempt.Status != relay.AttemptCancelled {
		t.Fatalf("unexpected acknowledged cancellation: %+v", cancelled)
	}
}

func TestRunTimeoutUsesCancellationPath(t *testing.T) {
	ctx := context.Background()
	service := controlplane.New(time.Second)
	run, err := service.Submit(ctx, relay.Request{AgentID: "agent", IdempotencyKey: "timeout", Runtime: relay.RuntimeRequirement{Provider: "codex"}, Input: relay.Input{Prompt: "work"}, Timeout: "5ms"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if processed, err := service.Reconcile(ctx, 10); err != nil || processed != 1 {
		t.Fatalf("reconcile timeout = %d, %v", processed, err)
	}
	cancelled, _ := service.GetRun(ctx, run.ID)
	if cancelled.Status != relay.RunCancelled || cancelled.CancelReason != "run timeout exceeded" {
		t.Fatalf("unexpected timeout result: %+v", cancelled)
	}
}
