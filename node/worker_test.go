package node_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/controlplane"
	"github.com/KDF5000/relay/node"
	"github.com/KDF5000/relay/workspace"
)

type preparedWorkspace struct {
	dir string
}

func (p preparedWorkspace) Prepare(context.Context, string, string, relay.WorkspaceSpec) (workspace.Prepared, error) {
	return workspace.Prepared{Dir: p.dir, Cleanup: func(context.Context) error { return nil }}, nil
}

type failingEvents struct {
	node.ControlPlane
	cause error
}

type lostEventResponse struct {
	node.ControlPlane
	calls int
}

type lostCompletionResponse struct {
	node.ControlPlane
	calls atomic.Int32
}

func (c *lostCompletionResponse) Complete(ctx context.Context, assignment controlplane.Assignment, result relay.Result) error {
	c.calls.Add(1)
	if err := c.ControlPlane.Complete(ctx, assignment, result); err != nil {
		return err
	}
	if c.calls.Load() == 1 {
		return errors.New("completion response lost after commit")
	}
	return nil
}

type slowArtifactControlPlane struct {
	node.ControlPlane
	renewals atomic.Int32
	delay    time.Duration
}

func (c *slowArtifactControlPlane) Renew(ctx context.Context, assignment controlplane.Assignment) (controlplane.LeaseUpdate, error) {
	c.renewals.Add(1)
	return c.ControlPlane.Renew(ctx, assignment)
}

func (c *slowArtifactControlPlane) UploadArtifact(ctx context.Context, assignment controlplane.Assignment, artifact relay.Artifact, content io.Reader) (relay.Artifact, error) {
	select {
	case <-ctx.Done():
		return relay.Artifact{}, ctx.Err()
	case <-time.After(c.delay):
	}
	_, err := io.Copy(io.Discard, content)
	return artifact, err
}

func TestWorkerRetriesLostCompletionResponse(t *testing.T) {
	ctx := context.Background()
	service := controlplane.New(time.Second)
	cp := &lostCompletionResponse{ControlPlane: service}
	worker := &node.Worker{
		Registration: controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "completion-node", Runtimes: []controlplane.Runtime{{Provider: "test"}}, Capacity: 1},
		ControlPlane: cp,
		Executors: node.ExecutorMap{"test": relay.ExecutorFunc(func(context.Context, relay.Execution) (relay.Result, error) {
			return relay.Result{Summary: "done"}, nil
		})},
	}
	if _, err := worker.Register(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := service.Submit(ctx, relay.Request{AgentID: "agent", IdempotencyKey: "completion-response", Runtime: relay.RuntimeRequirement{Provider: "test"}, Input: relay.Input{Prompt: "work"}})
	if err != nil {
		t.Fatal(err)
	}
	if completed, err := worker.RunOnce(ctx); err != nil || completed.Status != relay.RunSucceeded {
		t.Fatalf("completed=%+v err=%v", completed, err)
	}
	if cp.calls.Load() != 2 {
		t.Fatalf("completion calls=%d", cp.calls.Load())
	}
	events, _ := service.Events(ctx, run.ID)
	terminal := 0
	for _, event := range events {
		if event.Type == "run.succeeded" {
			terminal++
		}
	}
	if terminal != 1 {
		t.Fatalf("terminal events=%d", terminal)
	}
}

func TestWorkerPinsExecutionToPreparedWorkspace(t *testing.T) {
	ctx := context.Background()
	service := controlplane.New(time.Second)
	workDir := t.TempDir()
	var execution relay.Execution
	worker := &node.Worker{
		Registration: controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "workspace-node", Runtimes: []controlplane.Runtime{{Provider: "test"}}, Capacity: 1},
		ControlPlane: service,
		Workspaces:   preparedWorkspace{dir: workDir},
		Executors: node.ExecutorMap{"test": relay.ExecutorFunc(func(_ context.Context, value relay.Execution) (relay.Result, error) {
			execution = value
			return relay.Result{Summary: "done"}, nil
		})},
	}
	if _, err := worker.Register(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := service.Submit(ctx, relay.Request{
		AgentID:        "agent",
		IdempotencyKey: "prepared-workspace",
		Runtime:        relay.RuntimeRequirement{Provider: "test"},
		Input:          relay.Input{Prompt: "work"},
		Workspace:      relay.WorkspaceSpec{Kind: "git", Source: "git@example.test:acme/project.git"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if execution.WorkDir != workDir {
		t.Fatalf("work dir = %q, want %q", execution.WorkDir, workDir)
	}
	if !strings.Contains(execution.Instructions.Stable, "authoritative project root") ||
		!strings.Contains(execution.Instructions.Stable, workDir) ||
		!strings.Contains(execution.Instructions.Stable, "Do not search the host") {
		t.Fatalf("workspace instruction missing from stable instructions:\n%s", execution.Instructions.Stable)
	}
	events, err := service.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Type == "workspace.prepared" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("workspace.prepared event was not emitted")
	}
}

func TestWorkerUsesContinuationPromptForPersistedRuntimeSession(t *testing.T) {
	ctx := context.Background()
	service := controlplane.New(time.Second)
	var executions []relay.Execution
	worker := &node.Worker{
		Registration: controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "session-node", Runtimes: []controlplane.Runtime{{Provider: "test"}}, Capacity: 1},
		ControlPlane: service,
		Executors: node.ExecutorMap{"test": relay.ExecutorFunc(func(_ context.Context, execution relay.Execution) (relay.Result, error) {
			executions = append(executions, execution)
			return relay.Result{Summary: "done", RuntimeSessionID: "native-session"}, nil
		})},
	}
	if _, err := worker.Register(ctx); err != nil {
		t.Fatal(err)
	}
	request := relay.Request{SessionID: "chat", AgentID: "agent", IdempotencyKey: "turn-1", Runtime: relay.RuntimeRequirement{Provider: "test"}, Input: relay.Input{Prompt: "first turn"}}
	if _, err := service.Submit(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	request.IdempotencyKey = "turn-2"
	request.Input = relay.Input{Prompt: "full recovery transcript", ContinuationPrompt: "current turn only"}
	if _, err := service.Submit(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(executions) != 2 {
		t.Fatalf("executions = %d", len(executions))
	}
	second := executions[1]
	if second.RuntimeSessionID != "native-session" || !strings.Contains(second.Instructions.Prompt, "current turn only") || strings.Contains(second.Instructions.Prompt, "full recovery transcript") {
		t.Fatalf("unexpected resumed execution: %+v", second)
	}
	if !strings.Contains(second.FallbackPrompt, "full recovery transcript") {
		t.Fatalf("fallback prompt = %q", second.FallbackPrompt)
	}
}

func TestWorkerRenewsLeaseThroughArtifactUpload(t *testing.T) {
	ctx := context.Background()
	service := controlplane.New(30 * time.Millisecond)
	cp := &slowArtifactControlPlane{ControlPlane: service, delay: 120 * time.Millisecond}
	artifactPath := t.TempDir() + "/result.txt"
	if err := os.WriteFile(artifactPath, []byte("result"), 0o600); err != nil {
		t.Fatal(err)
	}
	worker := &node.Worker{
		Registration: controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "artifact-node", Runtimes: []controlplane.Runtime{{Provider: "test"}}, Capacity: 1},
		ControlPlane: cp,
		Executors: node.ExecutorMap{"test": relay.ExecutorFunc(func(context.Context, relay.Execution) (relay.Result, error) {
			return relay.Result{Summary: "done", Artifacts: []relay.Artifact{{Name: "result.txt", Ref: artifactPath}}}, nil
		})},
	}
	if _, err := worker.Register(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Submit(ctx, relay.Request{AgentID: "agent", IdempotencyKey: "slow-artifact", Runtime: relay.RuntimeRequirement{Provider: "test"}, Input: relay.Input{Prompt: "work"}}); err != nil {
		t.Fatal(err)
	}
	if completed, err := worker.RunOnce(ctx); err != nil || completed.Status != relay.RunSucceeded {
		t.Fatalf("completed=%+v err=%v", completed, err)
	}
	if cp.renewals.Load() < 2 {
		t.Fatalf("lease renewed only %d times", cp.renewals.Load())
	}
}

func (f *lostEventResponse) AppendEvent(ctx context.Context, run, attempt, lease, kind string, data any, ids ...string) error {
	f.calls++
	if err := f.ControlPlane.AppendEvent(ctx, run, attempt, lease, kind, data, ids...); err != nil {
		return err
	}
	if f.calls == 1 {
		return errors.New("response lost after commit")
	}
	return nil
}

func TestWorkerRetriesLostResponseWithoutDuplicatingOutput(t *testing.T) {
	ctx := context.Background()
	service := controlplane.New(time.Second)
	cp := &lostEventResponse{ControlPlane: service}
	worker := &node.Worker{
		Registration: controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "retry-node", Runtimes: []controlplane.Runtime{{Provider: "test"}}, Capacity: 1},
		ControlPlane: cp,
		Executors: node.ExecutorMap{"test": relay.ExecutorFunc(func(ctx context.Context, execution relay.Execution) (relay.Result, error) {
			execution.Emit(ctx, "assistant.message.delta", map[string]string{"delta": "hello"})
			return relay.Result{Summary: "hello"}, nil
		})},
	}
	if _, err := worker.Register(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := service.Submit(ctx, relay.Request{AgentID: "agent", IdempotencyKey: "lost-response", Runtime: relay.RuntimeRequirement{Provider: "test"}, Input: relay.Input{Prompt: "work"}})
	if err != nil {
		t.Fatal(err)
	}
	completed, err := worker.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != relay.RunSucceeded || cp.calls != 2 {
		t.Fatalf("status=%s calls=%d", completed.Status, cp.calls)
	}
	events, err := service.Events(ctx, run.ID)
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
		t.Fatalf("persisted deltas=%d", count)
	}
}

func (f failingEvents) AppendEvent(context.Context, string, string, string, string, any, ...string) error {
	return f.cause
}

func TestEventDeliveryFailureCannotCompleteRun(t *testing.T) {
	ctx := context.Background()
	service := controlplane.New(time.Second)
	cause := errors.New("event connection interrupted")
	worker := &node.Worker{
		Registration: controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "event-node", Runtimes: []controlplane.Runtime{{Provider: "test"}}, Capacity: 1},
		ControlPlane: failingEvents{ControlPlane: service, cause: cause},
		Executors: node.ExecutorMap{"test": relay.ExecutorFunc(func(ctx context.Context, execution relay.Execution) (relay.Result, error) {
			execution.Emit(ctx, "assistant.message.delta", map[string]string{"delta": "hello"})
			if ctx.Err() == nil {
				t.Error("failed delivery must cancel runtime execution")
			}
			// Even an adapter that returns success after cancellation cannot hide
			// the delivery failure from the host.
			return relay.Result{Summary: "done"}, nil
		})},
	}
	if _, err := worker.Register(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := service.Submit(ctx, relay.Request{AgentID: "agent", IdempotencyKey: "event-failure", Runtime: relay.RuntimeRequirement{Provider: "test"}, Input: relay.Input{Prompt: "work"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.RunOnce(ctx); !errors.Is(err, cause) {
		t.Fatalf("expected delivery error, got %v", err)
	}
	persisted, err := service.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != relay.RunFailed {
		t.Fatalf("status = %s, want failed", persisted.Status)
	}
}

func TestOversizedRuntimeEventDoesNotFailRun(t *testing.T) {
	ctx := context.Background()
	service := controlplane.New(time.Second)
	worker := &node.Worker{
		Registration: controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "large-event-node", Runtimes: []controlplane.Runtime{{Provider: "test"}}, Capacity: 1},
		ControlPlane: service,
		Executors: node.ExecutorMap{"test": relay.ExecutorFunc(func(ctx context.Context, execution relay.Execution) (relay.Result, error) {
			execution.Emit(ctx, "runtime.trae.item.completed", map[string]string{"output": strings.Repeat("x", 1<<20)})
			return relay.Result{Summary: "completed after large tool output"}, nil
		})},
	}
	if _, err := worker.Register(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := service.Submit(ctx, relay.Request{AgentID: "agent", IdempotencyKey: "large-event", Runtime: relay.RuntimeRequirement{Provider: "test"}, Input: relay.Input{Prompt: "work"}})
	if err != nil {
		t.Fatal(err)
	}
	completed, err := worker.RunOnce(ctx)
	if err != nil || completed.Status != relay.RunSucceeded {
		t.Fatalf("completed=%+v err=%v", completed, err)
	}
	events, err := service.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Type != "runtime.trae.item.completed" {
			continue
		}
		encoded, err := json.Marshal(event.Data)
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded) > 1<<20 || !strings.Contains(string(encoded), `"truncated":true`) {
			t.Fatalf("event data was not bounded: %s", encoded)
		}
		found = true
	}
	if !found {
		t.Fatal("large runtime event was not retained as a preview")
	}
}

func TestWorkerRenewsLeaseDuringLongExecution(t *testing.T) {
	ctx := context.Background()
	service := controlplane.New(30 * time.Millisecond)
	worker := &node.Worker{
		Registration: controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "node", Runtimes: []controlplane.Runtime{{Provider: "slow"}}, Capacity: 1},
		ControlPlane: service,
		Executors: node.ExecutorMap{"slow": relay.ExecutorFunc(func(ctx context.Context, _ relay.Execution) (relay.Result, error) {
			select {
			case <-ctx.Done():
				return relay.Result{}, ctx.Err()
			case <-time.After(120 * time.Millisecond):
				return relay.Result{Summary: "done"}, nil
			}
		})},
	}
	if _, err := worker.Register(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := service.Submit(ctx, relay.Request{AgentID: "agent", IdempotencyKey: "long", Runtime: relay.RuntimeRequirement{Provider: "slow"}, Input: relay.Input{Prompt: "work"}})
	if err != nil {
		t.Fatal(err)
	}
	completed, err := worker.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != relay.RunSucceeded || completed.Result == nil || completed.Result.Summary != "done" {
		t.Fatalf("unexpected completed run: %+v", completed)
	}
}

func TestWorkerStopsExecutionAndAcknowledgesCancellation(t *testing.T) {
	ctx := context.Background()
	service := controlplane.New(60 * time.Millisecond)
	started := make(chan struct{})
	worker := &node.Worker{
		Registration: controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "node", Runtimes: []controlplane.Runtime{{Provider: "blocking"}}, Capacity: 1},
		ControlPlane: service,
		Executors: node.ExecutorMap{"blocking": relay.ExecutorFunc(func(ctx context.Context, _ relay.Execution) (relay.Result, error) {
			close(started)
			<-ctx.Done()
			return relay.Result{}, ctx.Err()
		})},
	}
	if _, err := worker.Register(ctx); err != nil {
		t.Fatal(err)
	}
	submitted, err := service.Submit(ctx, relay.Request{AgentID: "agent", IdempotencyKey: "cancel-worker", Runtime: relay.RuntimeRequirement{Provider: "blocking"}, Input: relay.Input{Prompt: "work"}})
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		run relay.Run
		err error
	}
	done := make(chan result, 1)
	go func() {
		run, runErr := worker.RunOnce(ctx)
		done <- result{run: run, err: runErr}
	}()
	<-started
	if _, err := service.CancelRun(ctx, submitted.ID, controlplane.CancelRequest{Reason: "test cancellation", RequestedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.run.Status != relay.RunCancelled {
			t.Fatalf("worker returned status %s", result.run.Status)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after cancellation")
	}
	persisted, _ := service.GetRun(ctx, submitted.ID)
	if persisted.Status != relay.RunCancelled || persisted.Attempt.Status != relay.AttemptCancelled {
		t.Fatalf("unexpected persisted cancellation: %+v", persisted)
	}
}

func TestRunPoolUsesConfiguredCapacityConcurrently(t *testing.T) {
	service := controlplane.New(time.Second)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var mu sync.Mutex
	active, peak := 0, 0
	executor := relay.ExecutorFunc(func(ctx context.Context, _ relay.Execution) (relay.Result, error) {
		mu.Lock()
		active++
		if active > peak {
			peak = active
		}
		mu.Unlock()
		started <- struct{}{}
		select {
		case <-ctx.Done():
			return relay.Result{}, ctx.Err()
		case <-release:
		}
		mu.Lock()
		active--
		mu.Unlock()
		return relay.Result{Summary: "done"}, nil
	})
	worker := &node.Worker{
		Registration: controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "node", Runtimes: []controlplane.Runtime{{Provider: "parallel"}}, Capacity: 2},
		ControlPlane: service,
		Executors:    node.ExecutorMap{"parallel": executor},
	}
	ctx := context.Background()
	if _, err := worker.Register(ctx); err != nil {
		t.Fatal(err)
	}
	for index := range 2 {
		_, err := service.Submit(ctx, relay.Request{AgentID: "agent", IdempotencyKey: fmt.Sprintf("parallel-%d", index), Runtime: relay.RuntimeRequirement{Provider: "parallel"}, Input: relay.Input{Prompt: "work"}})
		if err != nil {
			t.Fatal(err)
		}
	}
	claimCtx, stopClaims := context.WithCancel(ctx)
	executionCtx, stopExecutions := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		worker.RunPool(claimCtx, executionCtx, time.Millisecond, func(err error) { t.Errorf("pool: %v", err) })
		close(done)
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("capacity slots did not execute concurrently")
		}
	}
	close(release)
	stopClaims()
	select {
	case <-done:
	case <-time.After(time.Second):
		stopExecutions()
		t.Fatal("worker pool did not drain")
	}
	stopExecutions()
	mu.Lock()
	defer mu.Unlock()
	if peak != 2 {
		t.Fatalf("peak concurrency = %d, want 2", peak)
	}
}

func TestRunPoolAppliesCapacityChangesWithoutCancellingActiveRuns(t *testing.T) {
	service := controlplane.New(time.Second)
	started := make(chan struct{}, 3)
	release := make(chan struct{}, 3)
	executor := relay.ExecutorFunc(func(ctx context.Context, _ relay.Execution) (relay.Result, error) {
		started <- struct{}{}
		select {
		case <-ctx.Done():
			return relay.Result{}, ctx.Err()
		case <-release:
			return relay.Result{Summary: "done"}, nil
		}
	})
	worker := &node.Worker{Registration: controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "node", Runtimes: []controlplane.Runtime{{Provider: "parallel"}}, Capacity: 2}, ControlPlane: service, Executors: node.ExecutorMap{"parallel": executor}}
	ctx := context.Background()
	if _, err := worker.Register(ctx); err != nil {
		t.Fatal(err)
	}
	for index := range 3 {
		if _, err := service.Submit(ctx, relay.Request{AgentID: "agent", IdempotencyKey: fmt.Sprintf("resize-%d", index), Runtime: relay.RuntimeRequirement{Provider: "parallel"}, Input: relay.Input{Prompt: "work"}}); err != nil {
			t.Fatal(err)
		}
	}
	claimCtx, stopClaims := context.WithCancel(ctx)
	executionCtx, stopExecutions := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		worker.RunPool(claimCtx, executionCtx, time.Millisecond, func(err error) { t.Errorf("pool: %v", err) })
		close(done)
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("initial runs did not start")
		}
	}
	worker.SetCapacity(1)
	release <- struct{}{}
	select {
	case <-started:
		t.Fatal("third run started while one of two active runs remained after reducing capacity")
	case <-time.After(40 * time.Millisecond):
	}
	release <- struct{}{}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("third run did not start after active runs drained to target capacity")
	}
	release <- struct{}{}
	stopClaims()
	select {
	case <-done:
	case <-time.After(time.Second):
		stopExecutions()
		t.Fatal("worker pool did not drain")
	}
	stopExecutions()
}
