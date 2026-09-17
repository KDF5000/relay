package relay

import (
	"context"
	"encoding/json"
	"time"
)

type RunStatus string

const (
	RunQueued     RunStatus = "queued"
	RunRunning    RunStatus = "running"
	RunCancelling RunStatus = "cancelling"
	RunCancelled  RunStatus = "cancelled"
	RunSucceeded  RunStatus = "succeeded"
	RunFailed     RunStatus = "failed"
)

type AttemptStatus string

const (
	AttemptQueued    AttemptStatus = "queued"
	AttemptLeased    AttemptStatus = "leased"
	AttemptRunning   AttemptStatus = "running"
	AttemptSucceeded AttemptStatus = "succeeded"
	AttemptFailed    AttemptStatus = "failed"
	AttemptLost      AttemptStatus = "lost"
	AttemptCancelled AttemptStatus = "cancelled"
)

type Request struct {
	TenantID       string             `json:"tenant_id,omitempty"`
	ProjectID      string             `json:"project_id,omitempty"`
	SessionID      string             `json:"session_id,omitempty"`
	AgentID        string             `json:"agent_id"`
	IdempotencyKey string             `json:"idempotency_key"`
	Runtime        RuntimeRequirement `json:"runtime"`
	Source         Source             `json:"source"`
	Input          Input              `json:"input"`
	Instructions   InstructionBundle  `json:"instructions,omitempty"`
	Context        []ContextItem      `json:"context,omitempty"`
	Capabilities   []CapabilityGrant  `json:"capabilities,omitempty"`
	Principal      Principal          `json:"principal"`
	Retry          RetryPolicy        `json:"retry,omitempty"`
	Timeout        string             `json:"timeout,omitempty"`
	Workspace      WorkspaceSpec      `json:"workspace,omitempty"`
}

type WorkspaceSpec struct {
	Kind      string `json:"kind,omitempty"`
	Source    string `json:"source,omitempty"`
	Ref       string `json:"ref,omitempty"`
	Subdir    string `json:"subdir,omitempty"`
	Ephemeral bool   `json:"ephemeral,omitempty"`
	// ReuseKey is an opaque, caller-owned identity for a prepared workspace.
	// Relay scopes it to the workspace source and never assigns product meaning
	// (such as project, task, or conversation) to the value.
	ReuseKey string `json:"reuse_key,omitempty"`
	// Lifecycle controls when a prepared workspace is removed. The empty value
	// and "attempt" preserve the original per-attempt behavior. "reusable"
	// retains the workspace for later attempts using the same ReuseKey.
	Lifecycle string `json:"lifecycle,omitempty"`
	// Branch is an optional Git provider hint used when a reusable worktree is
	// created. It is ignored by non-Git workspace providers.
	Branch string `json:"branch,omitempty"`
}

type RetryPolicy struct {
	MaxAttempts int    `json:"max_attempts,omitempty"`
	Backoff     string `json:"backoff,omitempty"`
}

type RuntimeRequirement struct {
	ID       string            `json:"id,omitempty"`
	Provider string            `json:"provider"`
	Model    string            `json:"model,omitempty"`
	Version  string            `json:"version,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
}

type Source struct {
	Kind       string          `json:"kind"`
	ExternalID string          `json:"external_id"`
	URL        string          `json:"url,omitempty"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
}

type Input struct {
	Type               string          `json:"type"`
	Version            string          `json:"version"`
	Prompt             string          `json:"prompt"`
	ContinuationPrompt string          `json:"continuation_prompt,omitempty"`
	Data               json.RawMessage `json:"data,omitempty"`
}

type InstructionFragment struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Title   string `json:"title"`
	Content string `json:"content"`
}

type InstructionBundle struct {
	Runtime   []InstructionFragment `json:"runtime,omitempty"`
	Host      []InstructionFragment `json:"host,omitempty"`
	Workspace []InstructionFragment `json:"workspace,omitempty"`
	Agent     []InstructionFragment `json:"agent,omitempty"`
	Turn      []InstructionFragment `json:"turn,omitempty"`
}

type CompiledInstructions struct {
	Stable string `json:"stable"`
	Prompt string `json:"prompt"`
}

type ContextItem struct {
	Type       string          `json:"type"`
	Version    string          `json:"version"`
	ExternalID string          `json:"external_id,omitempty"`
	Revision   string          `json:"revision,omitempty"`
	CapturedAt time.Time       `json:"captured_at"`
	Data       json.RawMessage `json:"data,omitempty"`
}

type Principal struct {
	Type          string `json:"type"`
	ID            string `json:"id"`
	AccountableID string `json:"accountable_id,omitempty"`
}

type CapabilityGrant struct {
	Name      string   `json:"name"`
	Version   string   `json:"version"`
	Effect    string   `json:"effect"`
	Resources []string `json:"resources,omitempty"`
}

type CapabilityCall struct {
	Name           string          `json:"name"`
	Version        string          `json:"version,omitempty"`
	IdempotencyKey string          `json:"idempotency_key"`
	Resource       string          `json:"resource,omitempty"`
	Input          json.RawMessage `json:"input,omitempty"`
}

type CapabilityRequest struct {
	CallID    string          `json:"call_id"`
	RunID     string          `json:"run_id"`
	AgentID   string          `json:"agent_id"`
	Principal Principal       `json:"principal"`
	Name      string          `json:"name"`
	Version   string          `json:"version"`
	Effect    string          `json:"effect"`
	Resource  string          `json:"resource,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

type CapabilityResult struct {
	CallID string          `json:"call_id"`
	Output json.RawMessage `json:"output,omitempty"`
}

type Result struct {
	Summary          string          `json:"summary"`
	Output           json.RawMessage `json:"output,omitempty"`
	Artifacts        []Artifact      `json:"artifacts,omitempty"`
	RuntimeSessionID string          `json:"runtime_session_id,omitempty"`
}

type Artifact struct {
	ID          string `json:"id,omitempty"`
	RunID       string `json:"-"`
	Type        string `json:"type"`
	Ref         string `json:"ref"`
	Name        string `json:"name,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
}

type Attempt struct {
	ID             string        `json:"id"`
	Number         int           `json:"number"`
	NodeID         string        `json:"node_id,omitempty"`
	LeaseToken     string        `json:"-"`
	LeaseExpiresAt *time.Time    `json:"lease_expires_at,omitempty"`
	AvailableAt    *time.Time    `json:"available_at,omitempty"`
	Status         AttemptStatus `json:"status"`
	StartedAt      *time.Time    `json:"started_at,omitempty"`
	CompletedAt    *time.Time    `json:"completed_at,omitempty"`
}

type Run struct {
	ID                string             `json:"id"`
	TenantID          string             `json:"tenant_id,omitempty"`
	ProjectID         string             `json:"project_id,omitempty"`
	SessionID         string             `json:"session_id,omitempty"`
	AgentID           string             `json:"agent_id"`
	IdempotencyKey    string             `json:"idempotency_key"`
	Runtime           RuntimeRequirement `json:"runtime"`
	Source            Source             `json:"source"`
	Status            RunStatus          `json:"status"`
	Attempt           Attempt            `json:"attempt"`
	Result            *Result            `json:"result,omitempty"`
	Error             string             `json:"error,omitempty"`
	CreatedAt         time.Time          `json:"created_at"`
	StartedAt         *time.Time         `json:"started_at,omitempty"`
	CompletedAt       *time.Time         `json:"completed_at,omitempty"`
	DeadlineAt        *time.Time         `json:"deadline_at,omitempty"`
	CancelRequestedAt *time.Time         `json:"cancel_requested_at,omitempty"`
	CancelReason      string             `json:"cancel_reason,omitempty"`
	CancelledAt       *time.Time         `json:"cancelled_at,omitempty"`
}

type Event struct {
	ID        string          `json:"id"`
	RunID     string          `json:"run_id"`
	AttemptID string          `json:"attempt_id,omitempty"`
	Sequence  int             `json:"sequence"`
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

type Execution struct {
	RunID        string
	AttemptID    string
	AgentID      string
	Runtime      RuntimeRequirement
	Source       Source
	Input        Input
	Context      []ContextItem
	Instructions CompiledInstructions
	Capabilities CapabilityInvoker
	Emit         func(context.Context, string, any)
	WorkDir      string
	// RuntimeSessionID is the provider-native conversation/thread identity
	// previously committed for this Relay Session on the selected Runtime.
	RuntimeSessionID string
	// FallbackPrompt contains the full recovery context used when a native
	// Runtime session cannot be resumed.
	FallbackPrompt string
	Interactions   InteractionBroker
}

type InteractionKind string

const (
	InteractionInput    InteractionKind = "input"
	InteractionApproval InteractionKind = "approval"
)

type InteractionRequest struct {
	Kind   InteractionKind `json:"kind"`
	Prompt string          `json:"prompt"`
	Data   json.RawMessage `json:"data,omitempty"`
}
type Interaction struct {
	ID         string          `json:"id"`
	RunID      string          `json:"run_id"`
	AttemptID  string          `json:"attempt_id"`
	Kind       InteractionKind `json:"kind"`
	State      string          `json:"state"`
	Prompt     string          `json:"prompt"`
	Data       json.RawMessage `json:"data,omitempty"`
	Response   json.RawMessage `json:"response,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	ResolvedAt *time.Time      `json:"resolved_at,omitempty"`
}
type InteractionBroker interface {
	Request(context.Context, InteractionRequest) (json.RawMessage, error)
}

type Executor interface {
	Execute(context.Context, Execution) (Result, error)
}
type ExecutorFunc func(context.Context, Execution) (Result, error)

func (f ExecutorFunc) Execute(ctx context.Context, execution Execution) (Result, error) {
	return f(ctx, execution)
}

type CapabilityInvoker interface {
	Call(context.Context, CapabilityCall) (CapabilityResult, error)
}
type CapabilityProvider interface {
	Invoke(context.Context, CapabilityRequest) (json.RawMessage, error)
}
type CapabilityProviderFunc func(context.Context, CapabilityRequest) (json.RawMessage, error)

func (f CapabilityProviderFunc) Invoke(ctx context.Context, request CapabilityRequest) (json.RawMessage, error) {
	return f(ctx, request)
}

type InstructionCompiler interface {
	Compile(Input, InstructionBundle) (CompiledInstructions, error)
}
type Store interface {
	Create(context.Context, Run) (run Run, created bool, err error)
	Update(context.Context, Run) error
	AppendEvent(context.Context, Event) (Event, error)
	Get(context.Context, string) (Run, error)
	Events(context.Context, string) ([]Event, error)
}
type Observer interface{ OnEvent(context.Context, Event) }
type ObserverFunc func(context.Context, Event)

func (f ObserverFunc) OnEvent(ctx context.Context, event Event) { f(ctx, event) }
