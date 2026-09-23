package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/KDF5000/relay"
)

var (
	ErrNotFound             = errors.New("relay control plane: not found")
	ErrNoAssignment         = errors.New("relay control plane: no matching assignment")
	ErrInvalidLease         = errors.New("relay control plane: invalid lease")
	ErrInvalidTransition    = errors.New("relay control plane: invalid transition")
	ErrRunCancelled         = errors.New("relay control plane: run cancelled")
	ErrIncompatibleProtocol = errors.New("relay control plane: incompatible protocol")
)

const MaxNodeCapacity = 32

// Storage is the durable state boundary of the distributed control plane.
// Implementations must make every state transition and its events atomic.
type Storage interface {
	RegisterNode(context.Context, NodeRegistration) (Node, error)
	Heartbeat(context.Context, string) (Node, error)
	UpdateNodeCapacity(context.Context, string, int) (Node, error)
	Submit(context.Context, relay.Request) (relay.Run, error)
	Claim(context.Context, string, time.Duration) (Assignment, error)
	Renew(context.Context, Assignment, time.Duration) (LeaseUpdate, error)
	Reconcile(context.Context, time.Time, int) (int, error)
	CancelRun(context.Context, string, CancelRequest) (relay.Run, error)
	AcknowledgeCancellation(context.Context, Assignment) error
	Start(context.Context, Assignment) error
	AppendEvent(context.Context, string, string, string, string, any, ...string) error
	Complete(context.Context, Assignment, relay.Result) error
	Fail(context.Context, Assignment, string) error
	GetRun(context.Context, string) (relay.Run, error)
	Events(context.Context, string) ([]relay.Event, error)
	EventsAfter(context.Context, string, int) ([]relay.Event, error)
	Nodes(context.Context) ([]Node, error)
	AddArtifact(context.Context, Assignment, relay.Artifact) error
	Artifacts(context.Context, string) ([]relay.Artifact, error)
	Artifact(context.Context, string) (relay.Artifact, error)
	ReserveCapability(context.Context, Assignment, string, string, relay.CapabilityRequest) (relay.CapabilityReservation, error)
	FinishCapability(context.Context, Assignment, relay.CapabilityReservation, relay.CapabilityResult, string) error
	ListRuns(context.Context, string, string, string, int) ([]relay.Run, error)
	Attempts(context.Context, string) ([]relay.Attempt, error)
	CreateInteraction(context.Context, Assignment, relay.InteractionRequest) (relay.Interaction, error)
	GetInteraction(context.Context, Assignment, string) (relay.Interaction, error)
	Interactions(context.Context, string) ([]relay.Interaction, error)
	ResolveInteraction(context.Context, string, json.RawMessage, string, string) (relay.Interaction, error)
}

type BlobStore interface {
	Put(context.Context, string, io.Reader) (int64, error)
	Open(context.Context, string) (io.ReadCloser, error)
	Delete(context.Context, string) error
}

type Service struct {
	storage          Storage
	leaseTTL         time.Duration
	nodeOfflineAfter time.Duration
	blobs            BlobStore
	updatesMu        sync.Mutex
	updates          map[string]chan struct{}
}

type Options struct {
	LeaseTTL         time.Duration
	NodeOfflineAfter time.Duration
	BlobStore        BlobStore
}

// New creates an in-memory control plane for tests and demos.
func New(leaseTTL time.Duration) *Service {
	return NewWithStorage(NewMemoryStorage(), leaseTTL)
}

func NewWithStorage(storage Storage, leaseTTL time.Duration) *Service {
	return NewWithOptions(storage, Options{LeaseTTL: leaseTTL})
}

func NewWithOptions(storage Storage, options Options) *Service {
	if storage == nil {
		panic("relay control plane: storage is required")
	}
	if options.LeaseTTL <= 0 {
		options.LeaseTTL = 30 * time.Second
	}
	if options.NodeOfflineAfter <= 0 {
		options.NodeOfflineAfter = 15 * time.Second
	}
	if options.BlobStore == nil {
		options.BlobStore = NewMemoryBlobStore()
	}
	return &Service{storage: storage, leaseTTL: options.LeaseTTL, nodeOfflineAfter: options.NodeOfflineAfter, blobs: options.BlobStore, updates: make(map[string]chan struct{})}
}

func (s *Service) RegisterNode(ctx context.Context, registration NodeRegistration) (Node, error) {
	if registration.ProtocolVersion != relay.ProtocolVersion {
		return Node{}, fmt.Errorf("%w: server=%s node=%q", ErrIncompatibleProtocol, relay.ProtocolVersion, registration.ProtocolVersion)
	}
	if registration.ID == "" || len(registration.Runtimes) == 0 {
		return Node{}, errors.New("node ID and at least one runtime are required")
	}
	if registration.Capacity <= 0 {
		registration.Capacity = 1
	}
	if registration.Capacity > MaxNodeCapacity {
		return Node{}, fmt.Errorf("capacity must be between 1 and %d", MaxNodeCapacity)
	}
	seenRuntimeIDs := make(map[string]struct{}, len(registration.Runtimes))
	for index := range registration.Runtimes {
		runtime := &registration.Runtimes[index]
		if runtime.ID == "" {
			runtime.ID = RuntimeInstanceID(registration.ID, runtime.Provider)
		}
		if _, duplicate := seenRuntimeIDs[runtime.ID]; duplicate {
			return Node{}, errors.New("runtime instance IDs must be unique within a node")
		}
		seenRuntimeIDs[runtime.ID] = struct{}{}
	}
	return s.storage.RegisterNode(ctx, registration)
}

func (s *Service) Heartbeat(ctx context.Context, nodeID string) (Node, error) {
	return s.storage.Heartbeat(ctx, nodeID)
}

func (s *Service) UpdateNodeCapacity(ctx context.Context, nodeID string, capacity int) (Node, error) {
	if nodeID == "" {
		return Node{}, errors.New("node ID is required")
	}
	if capacity < 1 || capacity > MaxNodeCapacity {
		return Node{}, fmt.Errorf("capacity must be between 1 and %d", MaxNodeCapacity)
	}
	return s.storage.UpdateNodeCapacity(ctx, nodeID, capacity)
}

func (s *Service) Submit(ctx context.Context, request relay.Request) (relay.Run, error) {
	if scope, ok := AccessFrom(ctx); ok && scope.Kind == AccessHost {
		request.TenantID, request.ProjectID = scope.TenantID, scope.ProjectID
	}
	if request.AgentID == "" || request.IdempotencyKey == "" || request.Input.Prompt == "" || request.Runtime.Provider == "" {
		return relay.Run{}, errors.New("agent ID, idempotency key, prompt, and runtime provider are required")
	}
	if _, err := relay.InputImages(request.Input); err != nil {
		return relay.Run{}, err
	}
	if request.Retry.MaxAttempts <= 0 {
		request.Retry.MaxAttempts = 1
	}
	if request.Retry.Backoff != "" {
		if _, err := time.ParseDuration(request.Retry.Backoff); err != nil {
			return relay.Run{}, errors.New("retry backoff must be a valid duration")
		}
	}
	if request.Timeout != "" {
		if duration, err := time.ParseDuration(request.Timeout); err != nil || duration <= 0 {
			return relay.Run{}, errors.New("timeout must be a positive duration")
		}
	}
	run, err := s.storage.Submit(ctx, request)
	if err == nil {
		s.notifyRun(run.ID)
	}
	return run, err
}

func (s *Service) Claim(ctx context.Context, nodeID string) (Assignment, error) {
	assignment, err := s.storage.Claim(ctx, nodeID, s.leaseTTL)
	if err == nil {
		s.notifyRun(assignment.RunID)
	}
	return assignment, err
}

func (s *Service) Start(ctx context.Context, assignment Assignment) error {
	err := s.storage.Start(ctx, assignment)
	if err == nil {
		s.notifyRun(assignment.RunID)
	}
	return err
}

func (s *Service) Renew(ctx context.Context, assignment Assignment) (LeaseUpdate, error) {
	return s.storage.Renew(ctx, assignment, s.leaseTTL)
}

func (s *Service) Reconcile(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	return s.storage.Reconcile(ctx, time.Now().UTC(), limit)
}

func (s *Service) CancelRun(ctx context.Context, runID string, request CancelRequest) (relay.Run, error) {
	if run, err := s.storage.GetRun(ctx, runID); err != nil {
		return relay.Run{}, err
	} else if err := authorizeRunScope(ctx, run); err != nil {
		return relay.Run{}, err
	}
	run, err := s.storage.CancelRun(ctx, runID, request)
	if err == nil {
		s.notifyRun(runID)
	}
	return run, err
}

func (s *Service) AcknowledgeCancellation(ctx context.Context, assignment Assignment) error {
	err := s.storage.AcknowledgeCancellation(ctx, assignment)
	if err == nil {
		s.notifyRun(assignment.RunID)
	}
	return err
}

func (s *Service) AppendEvent(ctx context.Context, runID, attemptID, lease, eventType string, data any, eventIDs ...string) error {
	err := s.storage.AppendEvent(ctx, runID, attemptID, lease, eventType, data, eventIDs...)
	if err == nil {
		s.notifyRun(runID)
	}
	return err
}

func (s *Service) Complete(ctx context.Context, assignment Assignment, result relay.Result) error {
	err := s.storage.Complete(ctx, assignment, result)
	if err == nil {
		s.notifyRun(assignment.RunID)
	}
	return err
}

func (s *Service) Fail(ctx context.Context, assignment Assignment, cause string) error {
	err := s.storage.Fail(ctx, assignment, cause)
	if err == nil {
		s.notifyRun(assignment.RunID)
	}
	return err
}

// RunUpdates returns a process-local signal that closes after the Run changes.
// Event streams retain a polling fallback for changes committed by another
// Relay Server instance sharing the same durable store.
func (s *Service) RunUpdates(runID string) <-chan struct{} {
	s.updatesMu.Lock()
	defer s.updatesMu.Unlock()
	if signal := s.updates[runID]; signal != nil {
		return signal
	}
	signal := make(chan struct{})
	s.updates[runID] = signal
	return signal
}

func (s *Service) notifyRun(runID string) {
	if runID == "" {
		return
	}
	s.updatesMu.Lock()
	defer s.updatesMu.Unlock()
	if signal := s.updates[runID]; signal != nil {
		close(signal)
	}
	s.updates[runID] = make(chan struct{})
}

func (s *Service) GetRun(ctx context.Context, runID string) (relay.Run, error) {
	run, err := s.storage.GetRun(ctx, runID)
	if err != nil {
		return relay.Run{}, err
	}
	if err := authorizeRunScope(ctx, run); err != nil {
		return relay.Run{}, err
	}
	return run, nil
}

func (s *Service) Events(ctx context.Context, runID string) ([]relay.Event, error) {
	if _, err := s.GetRun(ctx, runID); err != nil {
		return nil, err
	}
	return s.storage.Events(ctx, runID)
}

func (s *Service) EventsAfter(ctx context.Context, runID string, after int) ([]relay.Event, error) {
	if _, err := s.GetRun(ctx, runID); err != nil {
		return nil, err
	}
	return s.storage.EventsAfter(ctx, runID, after)
}

func (s *Service) Nodes(ctx context.Context) ([]Node, error) {
	nodes, err := s.storage.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	for index := range nodes {
		nodes[index].State = NodeOnline
		if now.Sub(nodes[index].LastSeen) > s.nodeOfflineAfter {
			nodes[index].State = NodeOffline
		}
	}
	return nodes, nil
}

func (s *Service) UploadArtifact(ctx context.Context, assignment Assignment, artifact relay.Artifact, reader io.Reader) (relay.Artifact, error) {
	artifact.ID = newControlPlaneID("artifact")
	artifact.RunID = assignment.RunID
	artifact.Ref = "/v1/artifacts/" + artifact.ID
	digest := sha256.New()
	size, err := s.blobs.Put(ctx, artifact.ID, io.TeeReader(reader, digest))
	if err != nil {
		return relay.Artifact{}, err
	}
	artifact.Size = size
	artifact.SHA256 = hex.EncodeToString(digest.Sum(nil))
	if err := s.storage.AddArtifact(ctx, assignment, artifact); err != nil {
		_ = s.blobs.Delete(context.Background(), artifact.ID)
		return relay.Artifact{}, err
	}
	s.notifyRun(assignment.RunID)
	return artifact, nil
}

func (s *Service) Artifacts(ctx context.Context, runID string) ([]relay.Artifact, error) {
	if _, err := s.GetRun(ctx, runID); err != nil {
		return nil, err
	}
	return s.storage.Artifacts(ctx, runID)
}

func (s *Service) OpenArtifact(ctx context.Context, artifactID string) (relay.Artifact, io.ReadCloser, error) {
	metadata, err := s.storage.Artifact(ctx, artifactID)
	if err != nil {
		return relay.Artifact{}, nil, err
	}
	if _, err := s.GetRun(ctx, metadata.RunID); err != nil {
		return relay.Artifact{}, nil, err
	}
	reader, err := s.blobs.Open(ctx, artifactID)
	return metadata, reader, err
}

func (s *Service) ReserveCapability(ctx context.Context, a Assignment, key, hash string, request relay.CapabilityRequest) (relay.CapabilityReservation, error) {
	return s.storage.ReserveCapability(ctx, a, key, hash, request)
}

func (s *Service) FinishCapability(ctx context.Context, a Assignment, reservation relay.CapabilityReservation, result relay.CapabilityResult, cause string) error {
	return s.storage.FinishCapability(ctx, a, reservation, result, cause)
}

func (s *Service) ListRuns(ctx context.Context, limit int) ([]relay.Run, error) {
	scope, _ := AccessFrom(ctx)
	return s.storage.ListRuns(ctx, scope.TenantID, scope.ProjectID, "", limit)
}
func (s *Service) SessionRuns(ctx context.Context, sessionID string, limit int) ([]relay.Run, error) {
	scope, _ := AccessFrom(ctx)
	return s.storage.ListRuns(ctx, scope.TenantID, scope.ProjectID, sessionID, limit)
}
func (s *Service) Attempts(ctx context.Context, runID string) ([]relay.Attempt, error) {
	if _, err := s.GetRun(ctx, runID); err != nil {
		return nil, err
	}
	return s.storage.Attempts(ctx, runID)
}
func (s *Service) CreateInteraction(ctx context.Context, a Assignment, r relay.InteractionRequest) (relay.Interaction, error) {
	if r.Kind != relay.InteractionInput && r.Kind != relay.InteractionApproval {
		return relay.Interaction{}, errors.New("interaction kind must be input or approval")
	}
	return s.storage.CreateInteraction(ctx, a, r)
}
func (s *Service) GetInteraction(ctx context.Context, a Assignment, id string) (relay.Interaction, error) {
	return s.storage.GetInteraction(ctx, a, id)
}
func (s *Service) Interactions(ctx context.Context, runID string) ([]relay.Interaction, error) {
	if _, err := s.GetRun(ctx, runID); err != nil {
		return nil, err
	}
	return s.storage.Interactions(ctx, runID)
}
func (s *Service) ResolveInteraction(ctx context.Context, id string, response json.RawMessage) (relay.Interaction, error) {
	scope, _ := AccessFrom(ctx)
	return s.storage.ResolveInteraction(ctx, id, response, scope.TenantID, scope.ProjectID)
}
