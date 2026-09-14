package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type Options struct {
	Executor     Executor
	Capabilities CapabilityProvider
	Store        Store
	Observer     Observer
	Compiler     InstructionCompiler
}

type Engine struct {
	executor     Executor
	capabilities CapabilityProvider
	store        Store
	observer     Observer
	compiler     InstructionCompiler
}

func New(options Options) (*Engine, error) {
	if options.Executor == nil {
		return nil, errors.New("relay: executor is required")
	}
	if options.Store == nil {
		options.Store = NewMemoryStore()
	}
	if options.Compiler == nil {
		options.Compiler = DefaultInstructionCompiler{}
	}
	return &Engine{
		executor: options.Executor, capabilities: options.Capabilities,
		store: options.Store, observer: options.Observer, compiler: options.Compiler,
	}, nil
}

// Execute runs one request synchronously in the caller's process. The returned Run is
// terminal unless the idempotency key already belongs to an in-progress execution.
func (e *Engine) Execute(ctx context.Context, request Request) (Run, error) {
	if err := validateRequest(request); err != nil {
		return Run{}, err
	}
	now := time.Now().UTC()
	run := Run{
		ID: newID("run"), TenantID: request.TenantID, ProjectID: request.ProjectID, SessionID: request.SessionID, AgentID: request.AgentID, IdempotencyKey: request.IdempotencyKey,
		Runtime: request.Runtime, Source: request.Source, Status: RunQueued, CreatedAt: now,
		Attempt: Attempt{ID: newID("attempt"), Number: 1, Status: AttemptQueued},
	}
	stored, created, err := e.store.Create(ctx, run)
	if err != nil {
		return Run{}, err
	}
	if !created {
		return stored, nil
	}
	e.emit(ctx, run, "run.created", map[string]any{"status": RunQueued})

	compiled, err := e.compiler.Compile(request.Input, request.Instructions)
	if err != nil {
		return e.fail(ctx, run, fmt.Errorf("compile instructions: %w", err))
	}
	now = time.Now().UTC()
	run.Status = RunRunning
	run.StartedAt = &now
	run.Attempt.Status = AttemptRunning
	run.Attempt.StartedAt = &now
	if err := e.store.Update(ctx, run); err != nil {
		return Run{}, err
	}
	e.emit(ctx, run, "attempt.started", nil)
	e.emit(ctx, run, "run.started", nil)

	invoker := NewCapabilityInvoker(CapabilityInvokerOptions{
		Run: run, Principal: request.Principal, Grants: request.Capabilities,
		Provider: e.capabilities, Emit: func(callCtx context.Context, eventType string, data any) {
			e.emit(callCtx, run, eventType, data)
		},
	})
	result, executionErr := e.executor.Execute(ctx, Execution{
		RunID: run.ID, AttemptID: run.Attempt.ID, AgentID: run.AgentID,
		Source: request.Source, Input: request.Input, Context: append([]ContextItem(nil), request.Context...),
		Instructions: compiled, Capabilities: invoker,
		Emit: func(eventCtx context.Context, eventType string, data any) { e.emit(eventCtx, run, eventType, data) },
	})
	if executionErr != nil {
		return e.fail(ctx, run, executionErr)
	}

	now = time.Now().UTC()
	run.Status = RunSucceeded
	run.Result = &result
	run.CompletedAt = &now
	run.Attempt.Status = AttemptSucceeded
	run.Attempt.CompletedAt = &now
	if err := e.store.Update(ctx, run); err != nil {
		return Run{}, err
	}
	e.emit(ctx, run, "attempt.succeeded", nil)
	e.emit(ctx, run, "run.succeeded", result)
	return run, nil
}

func (e *Engine) Run(ctx context.Context, runID string) (Run, error) {
	return e.store.Get(ctx, runID)
}

func (e *Engine) Events(ctx context.Context, runID string) ([]Event, error) {
	return e.store.Events(ctx, runID)
}

func (e *Engine) fail(ctx context.Context, run Run, cause error) (Run, error) {
	now := time.Now().UTC()
	run.Status = RunFailed
	run.Error = cause.Error()
	run.CompletedAt = &now
	run.Attempt.Status = AttemptFailed
	run.Attempt.CompletedAt = &now
	if err := e.store.Update(ctx, run); err != nil {
		return Run{}, errors.Join(cause, err)
	}
	e.emit(ctx, run, "attempt.failed", map[string]string{"error": cause.Error()})
	e.emit(ctx, run, "run.failed", map[string]string{"error": cause.Error()})
	return run, cause
}

func (e *Engine) emit(ctx context.Context, run Run, eventType string, value any) {
	var data json.RawMessage
	if value != nil {
		data, _ = json.Marshal(value)
	}
	event, err := e.store.AppendEvent(ctx, Event{
		ID: newID("event"), RunID: run.ID, AttemptID: run.Attempt.ID,
		Type: eventType, Data: data, CreatedAt: time.Now().UTC(),
	})
	if err == nil && e.observer != nil {
		e.observer.OnEvent(ctx, event)
	}
}

func validateRequest(request Request) error {
	if request.AgentID == "" || request.IdempotencyKey == "" || request.Input.Prompt == "" {
		return errors.New("relay: agent ID, idempotency key, and input prompt are required")
	}
	if _, err := InputImages(request.Input); err != nil {
		return err
	}
	return nil
}
