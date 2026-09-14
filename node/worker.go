package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/binding"
	"github.com/KDF5000/relay/controlplane"
	"github.com/KDF5000/relay/workspace"
)

var ErrLeaseLost = errors.New("relay node: assignment lease lost")

type ControlPlane interface {
	RegisterNode(context.Context, controlplane.NodeRegistration) (controlplane.Node, error)
	Heartbeat(context.Context, string) (controlplane.Node, error)
	Claim(context.Context, string) (controlplane.Assignment, error)
	Start(context.Context, controlplane.Assignment) error
	Renew(context.Context, controlplane.Assignment) (controlplane.LeaseUpdate, error)
	AppendEvent(context.Context, string, string, string, string, any, ...string) error
	Complete(context.Context, controlplane.Assignment, relay.Result) error
	Fail(context.Context, controlplane.Assignment, string) error
	AcknowledgeCancellation(context.Context, controlplane.Assignment) error
	UploadArtifact(context.Context, controlplane.Assignment, relay.Artifact, io.Reader) (relay.Artifact, error)
	ReserveCapability(context.Context, controlplane.Assignment, string, string, relay.CapabilityRequest) (relay.CapabilityReservation, error)
	FinishCapability(context.Context, controlplane.Assignment, relay.CapabilityReservation, relay.CapabilityResult, string) error
	CreateInteraction(context.Context, controlplane.Assignment, relay.InteractionRequest) (relay.Interaction, error)
	GetInteraction(context.Context, controlplane.Assignment, string) (relay.Interaction, error)
}

type ExecutorResolver interface {
	Resolve(provider string) (relay.Executor, bool)
}

type ExecutorMap map[string]relay.Executor

func (m ExecutorMap) Resolve(provider string) (relay.Executor, bool) {
	executor, ok := m[provider]
	return executor, ok
}

type Worker struct {
	Registration controlplane.NodeRegistration
	ControlPlane ControlPlane
	Bindings     *binding.Registry
	Executors    ExecutorResolver
	Compiler     relay.InstructionCompiler
	Workspaces   workspace.Provider
	Outbox       *Outbox
}

// RunPool runs one claim loop per configured capacity slot. Cancelling
// claimCtx stops new assignments while executionCtx remains alive so callers
// can drain in-flight runtimes before forcing shutdown.
func (w *Worker) RunPool(claimCtx, executionCtx context.Context, poll time.Duration, onError func(error)) {
	capacity := w.Registration.Capacity
	if capacity <= 0 {
		capacity = 1
	}
	if poll <= 0 {
		poll = time.Second
	}
	if onError == nil {
		onError = func(error) {}
	}
	var workers sync.WaitGroup
	workers.Add(capacity)
	for range capacity {
		go func() {
			defer workers.Done()
			for {
				select {
				case <-claimCtx.Done():
					return
				default:
				}
				_, err := w.RunOnce(executionCtx)
				if err != nil && !errors.Is(err, controlplane.ErrNoAssignment) && executionCtx.Err() == nil {
					onError(err)
				}
				timer := time.NewTimer(poll)
				select {
				case <-claimCtx.Done():
					timer.Stop()
					return
				case <-executionCtx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}()
	}
	workers.Wait()
}

func (w *Worker) Register(ctx context.Context) (controlplane.Node, error) {
	if w.ControlPlane == nil {
		return controlplane.Node{}, errors.New("relay node: control plane is required")
	}
	if w.Bindings != nil {
		w.Registration.Capabilities = nil
		for _, descriptor := range w.Bindings.Inventory() {
			w.Registration.Capabilities = append(w.Registration.Capabilities, controlplane.Capability{Name: descriptor.Name, Version: descriptor.Version, Kind: descriptor.Kind})
		}
	}
	return w.ControlPlane.RegisterNode(ctx, w.Registration)
}

func (w *Worker) RunOnce(ctx context.Context) (relay.Run, error) {
	if w.ControlPlane == nil || w.Executors == nil {
		return relay.Run{}, errors.New("relay node: control plane and executors are required")
	}
	assignment, err := w.ControlPlane.Claim(ctx, w.Registration.ID)
	if err != nil {
		return relay.Run{}, err
	}
	executor, ok := w.Executors.Resolve(assignment.Request.Runtime.Provider)
	if !ok {
		cause := fmt.Sprintf("runtime provider %q is unavailable", assignment.Request.Runtime.Provider)
		_ = w.ControlPlane.Fail(ctx, assignment, cause)
		return relay.Run{}, errors.New(cause)
	}
	workDir := ""
	cleanupWorkspace := func(context.Context) error { return nil }
	if assignment.Request.Workspace.Kind != "" {
		if w.Workspaces == nil {
			cause := "workspace requested but no workspace provider is configured"
			_ = w.ControlPlane.Fail(ctx, assignment, cause)
			return relay.Run{}, errors.New(cause)
		}
		prepared, prepareErr := w.Workspaces.Prepare(ctx, assignment.RunID, assignment.AttemptID, assignment.Request.Workspace)
		if prepareErr != nil {
			_ = w.ControlPlane.Fail(ctx, assignment, prepareErr.Error())
			return relay.Run{}, prepareErr
		}
		workDir, cleanupWorkspace = prepared.Dir, prepared.Cleanup
		assignment.Request.Instructions.Workspace = append(
			assignment.Request.Instructions.Workspace,
			relay.InstructionFragment{
				ID:      "relay-prepared-workspace",
				Version: "1",
				Title:   "Prepared workspace",
				Content: preparedWorkspaceInstruction(assignment.Request.Workspace, workDir),
			},
		)
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = cleanupWorkspace(cleanupCtx)
		}()
	}
	compiler := w.Compiler
	if compiler == nil {
		compiler = relay.DefaultInstructionCompiler{}
	}
	instructions := assignment.Request.Instructions
	if len(assignment.Request.Capabilities) > 0 {
		instructions.Runtime = append(append([]relay.InstructionFragment(nil), instructions.Runtime...), relay.CapabilityToolInstruction(assignment.Request.Capabilities))
	}
	compiled, err := compiler.Compile(assignment.Request.Input, instructions)
	if err != nil {
		_ = w.ControlPlane.Fail(ctx, assignment, err.Error())
		return relay.Run{}, err
	}
	if err := w.ControlPlane.Start(ctx, assignment); err != nil {
		if errors.Is(err, controlplane.ErrRunCancelled) {
			if ackErr := w.ControlPlane.AcknowledgeCancellation(ctx, assignment); ackErr != nil {
				return relay.Run{}, ackErr
			}
			return cancelledRun(assignment), nil
		}
		return relay.Run{}, err
	}
	executionCtx, cancelExecution := context.WithCancel(ctx)
	defer cancelExecution()
	keeperCtx, stopKeeper := context.WithCancel(ctx)
	keeperDone := make(chan error, 1)
	go func() {
		keeperDone <- w.keepLease(keeperCtx, assignment, cancelExecution)
	}()
	run := relay.Run{ID: assignment.RunID, AgentID: assignment.Request.AgentID, Runtime: assignment.Request.Runtime, Source: assignment.Request.Source, Attempt: relay.Attempt{ID: assignment.AttemptID, NodeID: w.Registration.ID}}
	provider := relay.CapabilityProvider(nil)
	if w.Bindings != nil {
		provider = w.Bindings
	}
	// One in-flight event provides bounded buffering and backpressure. Keep its
	// identity and immutable payload until acknowledged, including lost responses.
	var eventMu sync.Mutex
	var eventErr error
	eventSequence := 0
	emit := func(eventCtx context.Context, eventType string, data any) {
		eventMu.Lock()
		defer eventMu.Unlock()
		if eventErr != nil {
			return
		}
		eventSequence++
		deliver := deliverEvent
		if w.Outbox != nil {
			deliver = w.Outbox.deliver
		}
		if err := deliver(eventCtx, w.ControlPlane, assignment, fmt.Sprint(eventSequence), eventType, data); err != nil {
			eventErr = fmt.Errorf("relay node: deliver event %s: %w", eventType, err)
			cancelExecution()
		}
	}
	if workDir != "" {
		emit(ctx, "workspace.prepared", map[string]any{
			"kind":     assignment.Request.Workspace.Kind,
			"work_dir": workDir,
		})
	}
	invoker := relay.NewCapabilityInvoker(relay.CapabilityInvokerOptions{
		Run: run, Principal: assignment.Request.Principal, Grants: assignment.Request.Capabilities, Provider: provider,
		Emit: emit,
		Reserve: func(callCtx context.Context, key, hash string, request relay.CapabilityRequest) (relay.CapabilityReservation, error) {
			return w.ControlPlane.ReserveCapability(callCtx, assignment, key, hash, request)
		},
		Finish: func(callCtx context.Context, reservation relay.CapabilityReservation, result relay.CapabilityResult, cause string) error {
			return w.ControlPlane.FinishCapability(callCtx, assignment, reservation, result, cause)
		},
	})
	result, executionErr := executor.Execute(executionCtx, relay.Execution{
		RunID: assignment.RunID, AttemptID: assignment.AttemptID, AgentID: assignment.Request.AgentID,
		Runtime: assignment.Request.Runtime,
		Source:  assignment.Request.Source, Input: assignment.Request.Input, Context: assignment.Request.Context,
		Instructions: compiled, Capabilities: invoker,
		WorkDir:      workDir,
		Interactions: interactionBroker{controlPlane: w.ControlPlane, assignment: assignment},
		Emit:         emit,
	})
	eventMu.Lock()
	executionErr = errors.Join(executionErr, eventErr)
	eventMu.Unlock()
	stopLease := func() error {
		stopKeeper()
		return <-keeperDone
	}
	failRun := func(cause error) (relay.Run, error) {
		reportErr := retryFinalReport(ctx, func(reportCtx context.Context) error {
			return w.ControlPlane.Fail(reportCtx, assignment, cause.Error())
		})
		leaseErr := stopLease()
		if errors.Is(reportErr, controlplane.ErrRunCancelled) || errors.Is(leaseErr, controlplane.ErrRunCancelled) {
			ackErr := w.ControlPlane.AcknowledgeCancellation(ctx, assignment)
			if ackErr != nil && !errors.Is(ackErr, controlplane.ErrInvalidTransition) {
				return relay.Run{}, errors.Join(cause, reportErr, leaseErr, ackErr)
			}
			return cancelledRun(assignment), nil
		}
		// Once the failure was committed, a renewal racing with that terminal
		// transition is harmless and must not hide the original execution error.
		if reportErr == nil {
			return relay.Run{}, cause
		}
		return relay.Run{}, errors.Join(cause, reportErr, leaseErr)
	}
	if executionErr != nil {
		return failRun(executionErr)
	}
	for index, artifact := range result.Artifacts {
		if artifact.Ref == "" {
			continue
		}
		file, openErr := os.Open(artifact.Ref)
		if openErr != nil {
			return failRun(openErr)
		}
		uploaded, uploadErr := w.ControlPlane.UploadArtifact(executionCtx, assignment, artifact, file)
		_ = file.Close()
		if uploadErr != nil {
			return failRun(uploadErr)
		}
		result.Artifacts[index] = uploaded
	}
	completeErr := retryFinalReport(ctx, func(reportCtx context.Context) error {
		return w.ControlPlane.Complete(reportCtx, assignment, result)
	})
	leaseErr := stopLease()
	if errors.Is(completeErr, controlplane.ErrRunCancelled) || errors.Is(leaseErr, controlplane.ErrRunCancelled) {
		if err := w.ControlPlane.AcknowledgeCancellation(ctx, assignment); err != nil && !errors.Is(err, controlplane.ErrInvalidTransition) {
			return relay.Run{}, errors.Join(completeErr, leaseErr, err)
		}
		return cancelledRun(assignment), nil
	}
	if completeErr != nil {
		return relay.Run{}, errors.Join(completeErr, leaseErr)
	}
	return relay.Run{ID: assignment.RunID, AgentID: assignment.Request.AgentID, Runtime: assignment.Request.Runtime, Source: assignment.Request.Source, Status: relay.RunSucceeded, Result: &result}, nil
}

func preparedWorkspaceInstruction(spec relay.WorkspaceSpec, workDir string) string {
	kind := strings.TrimSpace(spec.Kind)
	if kind == "" {
		kind = "temporary"
	}
	return fmt.Sprintf(
		"Relay prepared the selected %s workspace before starting this run. Your current working directory is %q. Treat this directory as the authoritative project root for the task. Run project commands and read or modify project files from this directory. Do not search the host for another copy of the repository and do not clone the primary repository yourself unless the user explicitly asks you to work outside the selected project.",
		kind,
		workDir,
	)
}

type interactionBroker struct {
	controlPlane ControlPlane
	assignment   controlplane.Assignment
}

func (b interactionBroker) Request(ctx context.Context, request relay.InteractionRequest) (json.RawMessage, error) {
	value, err := b.controlPlane.CreateInteraction(ctx, b.assignment, request)
	if err != nil {
		return nil, err
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			value, err = b.controlPlane.GetInteraction(ctx, b.assignment, value.ID)
			if err != nil {
				return nil, err
			}
			if value.State == "resolved" {
				return value.Response, nil
			}
		}
	}
}

func (w *Worker) keepLease(ctx context.Context, assignment controlplane.Assignment, cancelExecution context.CancelFunc) error {
	deadline := assignment.LeaseExpiresAt
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			cancelExecution()
			return ErrLeaseLost
		}
		wait := remaining / 3
		if wait > 5*time.Second {
			wait = 5 * time.Second
		}
		if wait < 10*time.Millisecond {
			wait = 10 * time.Millisecond
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		renewCtx, cancel := context.WithDeadline(ctx, deadline)
		update, err := w.ControlPlane.Renew(renewCtx, assignment)
		cancel()
		if err == nil {
			deadline = update.LeaseExpiresAt
			if update.CancelRequested {
				cancelExecution()
				return fmt.Errorf("%w: %s", controlplane.ErrRunCancelled, update.CancelReason)
			}
			continue
		}
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, controlplane.ErrInvalidLease) || errors.Is(err, controlplane.ErrInvalidTransition) || errors.Is(err, controlplane.ErrNotFound) {
			cancelExecution()
			return fmt.Errorf("%w: %v", ErrLeaseLost, err)
		}
		if time.Now().Before(deadline) {
			continue
		}
		cancelExecution()
		return fmt.Errorf("%w: %v", ErrLeaseLost, err)
	}
}

func cancelledRun(assignment controlplane.Assignment) relay.Run {
	return relay.Run{
		ID: assignment.RunID, AgentID: assignment.Request.AgentID, Runtime: assignment.Request.Runtime,
		Source: assignment.Request.Source, Status: relay.RunCancelled,
		Attempt: relay.Attempt{ID: assignment.AttemptID, Status: relay.AttemptCancelled},
	}
}
