package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/KDF5000/relay"
)

// MemoryStorage is intended for tests and demos. PostgreSQL is the production
// control-plane storage implementation.
type MemoryStorage struct {
	mu              sync.Mutex
	nodes           map[string]*Node
	runs            map[string]*relay.Run
	requests        map[string]relay.Request
	events          map[string][]relay.Event
	byIdempotency   map[string]string
	artifacts       map[string]relay.Artifact
	artifactRuns    map[string][]string
	capabilityCalls map[string]memoryCapabilityCall
	attemptHistory  map[string][]relay.Attempt
	interactions    map[string]*relay.Interaction
	runtimeSessions map[string]memoryRuntimeSession
}

type memoryRuntimeSession struct {
	nativeID string
	nodeID   string
}

type memoryCapabilityCall struct {
	hash        string
	reservation relay.CapabilityReservation
}

func NewMemoryStorage() *MemoryStorage {
	return &MemoryStorage{nodes: make(map[string]*Node), runs: make(map[string]*relay.Run), requests: make(map[string]relay.Request), events: make(map[string][]relay.Event), byIdempotency: make(map[string]string), artifacts: make(map[string]relay.Artifact), artifactRuns: make(map[string][]string), capabilityCalls: make(map[string]memoryCapabilityCall), attemptHistory: make(map[string][]relay.Attempt), interactions: make(map[string]*relay.Interaction), runtimeSessions: make(map[string]memoryRuntimeSession)}
}

func runtimeSessionKey(request relay.Request) string {
	return request.TenantID + "\x00" + request.ProjectID + "\x00" + request.SessionID + "\x00" + request.AgentID + "\x00" + request.Runtime.ID + "\x00" + request.Runtime.Provider
}

func (s *MemoryStorage) RegisterNode(_ context.Context, registration NodeRegistration) (Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if existing := s.nodes[registration.ID]; existing != nil {
		existing.NodeRegistration = registration
		existing.LastSeen = now
		return *existing, nil
	}
	node := &Node{NodeRegistration: registration, LastSeen: now}
	s.nodes[registration.ID] = node
	return *node, nil
}

func (s *MemoryStorage) Heartbeat(_ context.Context, nodeID string) (Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	node := s.nodes[nodeID]
	if node == nil {
		return Node{}, ErrNotFound
	}
	node.LastSeen = time.Now().UTC()
	return *node, nil
}

func (s *MemoryStorage) Submit(_ context.Context, request relay.Request) (relay.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idempotencyScope := request.TenantID + "\x00" + request.ProjectID + "\x00" + request.IdempotencyKey
	if runID := s.byIdempotency[idempotencyScope]; runID != "" {
		return *s.runs[runID], nil
	}
	now := time.Now().UTC()
	run := &relay.Run{ID: newControlPlaneID("run"), TenantID: request.TenantID, ProjectID: request.ProjectID, SessionID: request.SessionID, AgentID: request.AgentID, IdempotencyKey: request.IdempotencyKey, Runtime: request.Runtime, Source: request.Source, Status: relay.RunQueued, CreatedAt: now, Attempt: relay.Attempt{ID: newControlPlaneID("attempt"), Number: 1, Status: relay.AttemptQueued, AvailableAt: &now}}
	if timeout, _ := time.ParseDuration(request.Timeout); timeout > 0 {
		deadline := now.Add(timeout)
		run.DeadlineAt = &deadline
	}
	s.runs[run.ID] = run
	s.attemptHistory[run.ID] = []relay.Attempt{run.Attempt}
	s.requests[run.ID] = request
	s.byIdempotency[idempotencyScope] = run.ID
	s.appendEvent(run, "run.created", map[string]any{"status": run.Status})
	s.appendEvent(run, "attempt.queued", map[string]any{"number": 1})
	return *run, nil
}

func (s *MemoryStorage) Claim(_ context.Context, nodeID string, leaseTTL time.Duration) (Assignment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	node := s.nodes[nodeID]
	if node == nil {
		return Assignment{}, ErrNotFound
	}
	if node.Active >= node.Capacity {
		return Assignment{}, ErrNoAssignment
	}
	now := time.Now().UTC()
	for _, run := range s.runs {
		if run.DeadlineAt != nil && !now.Before(*run.DeadlineAt) && run.CancelRequestedAt == nil {
			s.cancelRunLocked(run, CancelRequest{Reason: "run timeout exceeded", RequestedBy: "relay"}, now)
		}
		if run.Status == relay.RunCancelling && (run.Attempt.Status == relay.AttemptLeased || run.Attempt.Status == relay.AttemptRunning) && run.Attempt.LeaseExpiresAt != nil && !now.Before(*run.Attempt.LeaseExpiresAt) {
			s.finalizeCancellation(run, now)
			continue
		}
		s.requeueExpired(run, now)
	}
	for _, run := range s.runs {
		if run.Status != relay.RunQueued || run.Attempt.Status != relay.AttemptQueued || (run.Attempt.AvailableAt != nil && now.Before(*run.Attempt.AvailableAt)) {
			continue
		}
		request := s.requests[run.ID]
		if !nodeMatches(*node, request) {
			continue
		}
		runtimeSession := s.runtimeSessions[runtimeSessionKey(request)]
		if runtimeSession.nodeID != nodeID {
			runtimeSession = memoryRuntimeSession{}
		}
		run.Attempt.Status = relay.AttemptLeased
		run.Attempt.NodeID = nodeID
		run.Attempt.LeaseToken = newControlPlaneID("lease")
		node.Active++
		expires := now.Add(leaseTTL)
		run.Attempt.LeaseExpiresAt = &expires
		s.appendEvent(run, "attempt.leased", map[string]any{"node_id": nodeID, "lease_expires_at": expires})
		return Assignment{RunID: run.ID, AttemptID: run.Attempt.ID, LeaseToken: run.Attempt.LeaseToken, LeaseExpiresAt: expires, Request: request, RuntimeSessionID: runtimeSession.nativeID}, nil
	}
	return Assignment{}, ErrNoAssignment
}

func (s *MemoryStorage) Renew(_ context.Context, assignment Assignment, leaseTTL time.Duration) (LeaseUpdate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, err := s.authorize(assignment.RunID, assignment.AttemptID, assignment.LeaseToken)
	if err != nil {
		return LeaseUpdate{}, err
	}
	if run.Attempt.Status != relay.AttemptLeased && run.Attempt.Status != relay.AttemptRunning {
		return LeaseUpdate{}, ErrInvalidTransition
	}
	now := time.Now().UTC()
	if run.Attempt.LeaseExpiresAt == nil || !now.Before(*run.Attempt.LeaseExpiresAt) {
		return LeaseUpdate{}, ErrInvalidLease
	}
	expires := now.Add(leaseTTL)
	run.Attempt.LeaseExpiresAt = &expires
	return LeaseUpdate{LeaseExpiresAt: expires, CancelRequested: run.CancelRequestedAt != nil, CancelReason: run.CancelReason}, nil
}

func (s *MemoryStorage) Reconcile(_ context.Context, now time.Time, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	recovered := 0
	for _, run := range s.runs {
		if recovered >= limit {
			break
		}
		if run.DeadlineAt != nil && !now.Before(*run.DeadlineAt) && !terminalRun(run.Status) && run.CancelRequestedAt == nil {
			s.cancelRunLocked(run, CancelRequest{Reason: "run timeout exceeded", RequestedBy: "relay"}, now)
			recovered++
		}
	}
	for _, run := range s.runs {
		activeAttempt := run.Attempt.Status == relay.AttemptRunning || (run.Status == relay.RunCancelling && run.Attempt.Status == relay.AttemptLeased)
		if recovered >= limit || (run.Status != relay.RunRunning && run.Status != relay.RunCancelling) || !activeAttempt || run.Attempt.LeaseExpiresAt == nil || now.Before(*run.Attempt.LeaseExpiresAt) {
			continue
		}
		if run.Status == relay.RunCancelling {
			s.finalizeCancellation(run, now)
			recovered++
			continue
		}
		s.releaseNode(run.Attempt.NodeID)
		run.Attempt.Status = relay.AttemptLost
		run.Attempt.CompletedAt = timePointer(now)
		s.appendEvent(run, "attempt.lost", map[string]any{"node_id": run.Attempt.NodeID, "reason": "lease_expired"})
		request := s.requests[run.ID]
		if run.Attempt.Number >= request.Retry.MaxAttempts {
			run.Status, run.Error, run.CompletedAt = relay.RunFailed, "attempt lease expired", timePointer(now)
			s.appendEvent(run, "run.failed", map[string]string{"error": run.Error})
		} else {
			next := now.Add(retryBackoff(request.Retry))
			history := s.attemptHistory[run.ID]
			history[len(history)-1] = run.Attempt
			run.Attempt = relay.Attempt{ID: newControlPlaneID("attempt"), Number: run.Attempt.Number + 1, Status: relay.AttemptQueued, AvailableAt: &next}
			s.attemptHistory[run.ID] = append(history, run.Attempt)
			run.Status, run.Error, run.StartedAt, run.CompletedAt = relay.RunQueued, "", nil, nil
			s.appendEvent(run, "attempt.queued", map[string]any{"number": run.Attempt.Number, "available_at": next, "reason": "retry"})
		}
		recovered++
	}
	return recovered, nil
}

func (s *MemoryStorage) CancelRun(_ context.Context, runID string, request CancelRequest) (relay.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.runs[runID]
	if run == nil {
		return relay.Run{}, ErrNotFound
	}
	s.cancelRunLocked(run, request, time.Now().UTC())
	return *run, nil
}

func (s *MemoryStorage) AcknowledgeCancellation(_ context.Context, assignment Assignment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, err := s.authorize(assignment.RunID, assignment.AttemptID, assignment.LeaseToken)
	if err != nil {
		return err
	}
	if run.Status == relay.RunCancelled {
		return nil
	}
	if run.Status != relay.RunCancelling {
		return ErrInvalidTransition
	}
	s.finalizeCancellation(run, time.Now().UTC())
	return nil
}

func (s *MemoryStorage) Start(_ context.Context, assignment Assignment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, err := s.authorize(assignment.RunID, assignment.AttemptID, assignment.LeaseToken)
	if err != nil {
		return err
	}
	if run.Attempt.Status != relay.AttemptLeased {
		return ErrInvalidTransition
	}
	if run.Status == relay.RunCancelling {
		s.finalizeCancellation(run, time.Now().UTC())
		return ErrRunCancelled
	}
	if run.Attempt.LeaseExpiresAt != nil && !time.Now().UTC().Before(*run.Attempt.LeaseExpiresAt) {
		s.requeueExpired(run, time.Now().UTC())
		return ErrInvalidLease
	}
	now := time.Now().UTC()
	run.Status, run.StartedAt = relay.RunRunning, &now
	run.Attempt.Status, run.Attempt.StartedAt = relay.AttemptRunning, &now
	s.appendEvent(run, "attempt.started", map[string]string{"node_id": run.Attempt.NodeID})
	s.appendEvent(run, "run.started", nil)
	return nil
}

func (s *MemoryStorage) AppendEvent(_ context.Context, runID, attemptID, lease, eventType string, data any, eventIDs ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, err := s.authorize(runID, attemptID, lease)
	if err != nil {
		return err
	}
	if run.Attempt.Status != relay.AttemptRunning {
		return ErrInvalidTransition
	}
	id := EventIdentity(attemptID, lease, eventIDs)
	if id != "" {
		for _, event := range s.events[runID] {
			if event.ID == id {
				return SameEvent(event.Type, event.Data, eventType, data)
			}
		}
	}
	if _, err := json.Marshal(data); err != nil {
		return err
	}
	s.appendEvent(run, eventType, data)
	if id != "" {
		s.events[runID][len(s.events[runID])-1].ID = id
	}
	return nil
}

func (s *MemoryStorage) Complete(_ context.Context, assignment Assignment, result relay.Result) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.runs[assignment.RunID]
	if run == nil {
		return ErrNotFound
	}
	if run.Attempt.ID != assignment.AttemptID || assignment.LeaseToken == "" || run.Attempt.LeaseToken != assignment.LeaseToken {
		return ErrInvalidLease
	}
	if run.Status == relay.RunSucceeded && run.Attempt.Status == relay.AttemptSucceeded {
		if run.Result != nil && SameValue(*run.Result, result) {
			return nil
		}
		return fmt.Errorf("%w: completion result differs from the committed result", ErrInvalidTransition)
	}
	if run.Attempt.LeaseExpiresAt == nil || !time.Now().UTC().Before(*run.Attempt.LeaseExpiresAt) {
		return ErrInvalidLease
	}
	if run.Attempt.Status != relay.AttemptRunning {
		return ErrInvalidTransition
	}
	if run.Status == relay.RunCancelling {
		return ErrRunCancelled
	}
	now := time.Now().UTC()
	run.Status, run.Result, run.CompletedAt = relay.RunSucceeded, &result, &now
	run.Attempt.Status, run.Attempt.CompletedAt = relay.AttemptSucceeded, &now
	if request := s.requests[run.ID]; request.SessionID != "" && result.RuntimeSessionID != "" {
		s.runtimeSessions[runtimeSessionKey(request)] = memoryRuntimeSession{nativeID: result.RuntimeSessionID, nodeID: run.Attempt.NodeID}
	}
	s.releaseNode(run.Attempt.NodeID)
	s.appendEvent(run, "attempt.succeeded", nil)
	s.appendEvent(run, "run.succeeded", result)
	return nil
}

func (s *MemoryStorage) Fail(_ context.Context, assignment Assignment, cause string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.runs[assignment.RunID]
	if run == nil {
		return ErrNotFound
	}
	if run.Attempt.ID != assignment.AttemptID || assignment.LeaseToken == "" || run.Attempt.LeaseToken != assignment.LeaseToken {
		return ErrInvalidLease
	}
	if run.Status == relay.RunFailed && run.Attempt.Status == relay.AttemptFailed {
		if run.Error == cause {
			return nil
		}
		return fmt.Errorf("%w: failure cause differs from the committed cause", ErrInvalidTransition)
	}
	if run.Attempt.LeaseExpiresAt == nil || !time.Now().UTC().Before(*run.Attempt.LeaseExpiresAt) {
		return ErrInvalidLease
	}
	if run.Attempt.Status != relay.AttemptRunning && run.Attempt.Status != relay.AttemptLeased {
		return ErrInvalidTransition
	}
	if run.Status == relay.RunCancelling {
		s.finalizeCancellation(run, time.Now().UTC())
		return ErrRunCancelled
	}
	now := time.Now().UTC()
	run.Status, run.Error, run.CompletedAt = relay.RunFailed, cause, &now
	run.Attempt.Status, run.Attempt.CompletedAt = relay.AttemptFailed, &now
	s.releaseNode(run.Attempt.NodeID)
	s.appendEvent(run, "attempt.failed", map[string]string{"error": cause})
	s.appendEvent(run, "run.failed", map[string]string{"error": cause})
	return nil
}

func (s *MemoryStorage) GetRun(_ context.Context, runID string) (relay.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if run := s.runs[runID]; run != nil {
		return *run, nil
	}
	return relay.Run{}, ErrNotFound
}

func (s *MemoryStorage) Events(ctx context.Context, runID string) ([]relay.Event, error) {
	return s.EventsAfter(ctx, runID, 0)
}

func (s *MemoryStorage) EventsAfter(_ context.Context, runID string, after int) ([]relay.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runs[runID] == nil {
		return nil, ErrNotFound
	}
	values := s.events[runID]
	index := 0
	for index < len(values) && values[index].Sequence <= after {
		index++
	}
	return append([]relay.Event(nil), values[index:]...), nil
}

func (s *MemoryStorage) Nodes(_ context.Context) ([]Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Node, 0, len(s.nodes))
	for _, node := range s.nodes {
		result = append(result, *node)
	}
	return result, nil
}

func (s *MemoryStorage) AddArtifact(_ context.Context, assignment Assignment, artifact relay.Artifact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, err := s.authorize(assignment.RunID, assignment.AttemptID, assignment.LeaseToken)
	if err != nil {
		return err
	}
	if run.Attempt.Status != relay.AttemptRunning {
		return ErrInvalidTransition
	}
	s.artifacts[artifact.ID] = artifact
	s.artifactRuns[run.ID] = append(s.artifactRuns[run.ID], artifact.ID)
	s.appendEvent(run, "artifact.created", artifact)
	return nil
}

func (s *MemoryStorage) Artifacts(_ context.Context, runID string) ([]relay.Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runs[runID] == nil {
		return nil, ErrNotFound
	}
	result := make([]relay.Artifact, 0, len(s.artifactRuns[runID]))
	for _, id := range s.artifactRuns[runID] {
		result = append(result, s.artifacts[id])
	}
	return result, nil
}

func (s *MemoryStorage) Artifact(_ context.Context, artifactID string) (relay.Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.artifacts[artifactID]
	if !ok {
		return relay.Artifact{}, ErrNotFound
	}
	return value, nil
}

func (s *MemoryStorage) ReserveCapability(_ context.Context, a Assignment, key, hash string, _ relay.CapabilityRequest) (relay.CapabilityReservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, err := s.authorize(a.RunID, a.AttemptID, a.LeaseToken)
	if err != nil {
		return relay.CapabilityReservation{}, err
	}
	if run.Attempt.Status != relay.AttemptRunning {
		return relay.CapabilityReservation{}, ErrInvalidTransition
	}
	mapKey := a.RunID + "\x00" + key
	if old, ok := s.capabilityCalls[mapKey]; ok {
		if old.hash != hash {
			return relay.CapabilityReservation{}, relay.ErrIdempotencyConflict
		}
		return old.reservation, nil
	}
	reservation := relay.CapabilityReservation{CallID: newControlPlaneID("call"), Execute: true}
	s.capabilityCalls[mapKey] = memoryCapabilityCall{hash: hash, reservation: reservation}
	return reservation, nil
}

func (s *MemoryStorage) FinishCapability(_ context.Context, a Assignment, reservation relay.CapabilityReservation, result relay.CapabilityResult, cause string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, err := s.authorize(a.RunID, a.AttemptID, a.LeaseToken)
	if err != nil {
		return err
	}
	for key, value := range s.capabilityCalls {
		if value.reservation.CallID != reservation.CallID {
			continue
		}
		value.reservation.Execute = false
		value.reservation.Result = &result
		value.reservation.Error = cause
		s.capabilityCalls[key] = value
		s.appendEvent(run, "capability.recorded", map[string]string{"call_id": reservation.CallID})
		return nil
	}
	return ErrNotFound
}

func (s *MemoryStorage) ListRuns(_ context.Context, tenant, project, session string, limit int) ([]relay.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var values []relay.Run
	for _, run := range s.runs {
		if (tenant == "" || run.TenantID == tenant) && (project == "" || run.ProjectID == project) && (session == "" || run.SessionID == session) {
			values = append(values, *run)
		}
	}
	sort.Slice(values, func(i, j int) bool { return values[i].CreatedAt.After(values[j].CreatedAt) })
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if len(values) > limit {
		values = values[:limit]
	}
	return values, nil
}
func (s *MemoryStorage) Attempts(_ context.Context, runID string) ([]relay.Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.runs[runID]
	if run == nil {
		return nil, ErrNotFound
	}
	values := append([]relay.Attempt(nil), s.attemptHistory[runID]...)
	if len(values) > 0 {
		values[len(values)-1] = run.Attempt
	}
	return values, nil
}
func (s *MemoryStorage) CreateInteraction(_ context.Context, a Assignment, request relay.InteractionRequest) (relay.Interaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, err := s.authorize(a.RunID, a.AttemptID, a.LeaseToken)
	if err != nil {
		return relay.Interaction{}, err
	}
	if run.Attempt.Status != relay.AttemptRunning {
		return relay.Interaction{}, ErrInvalidTransition
	}
	value := relay.Interaction{ID: newControlPlaneID("interaction"), RunID: run.ID, AttemptID: run.Attempt.ID, Kind: request.Kind, State: "pending", Prompt: request.Prompt, Data: request.Data, CreatedAt: time.Now().UTC()}
	s.interactions[value.ID] = &value
	s.appendEvent(run, "interaction.requested", value)
	return value, nil
}
func (s *MemoryStorage) GetInteraction(_ context.Context, a Assignment, id string) (relay.Interaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.authorize(a.RunID, a.AttemptID, a.LeaseToken); err != nil {
		return relay.Interaction{}, err
	}
	value := s.interactions[id]
	if value == nil || value.RunID != a.RunID {
		return relay.Interaction{}, ErrNotFound
	}
	return *value, nil
}
func (s *MemoryStorage) Interactions(_ context.Context, runID string) ([]relay.Interaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runs[runID] == nil {
		return nil, ErrNotFound
	}
	var values []relay.Interaction
	for _, v := range s.interactions {
		if v.RunID == runID {
			values = append(values, *v)
		}
	}
	sort.Slice(values, func(i, j int) bool { return values[i].CreatedAt.Before(values[j].CreatedAt) })
	return values, nil
}
func (s *MemoryStorage) ResolveInteraction(_ context.Context, id string, response json.RawMessage, tenant, project string) (relay.Interaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := s.interactions[id]
	if value == nil {
		return relay.Interaction{}, ErrNotFound
	}
	run := s.runs[value.RunID]
	if (tenant != "" && run.TenantID != tenant) || (project != "" && run.ProjectID != project) {
		return relay.Interaction{}, ErrNotFound
	}
	if value.State != "pending" {
		return relay.Interaction{}, ErrInvalidTransition
	}
	now := time.Now().UTC()
	value.State = "resolved"
	value.Response = response
	value.ResolvedAt = &now
	s.appendEvent(run, "interaction.resolved", map[string]string{"interaction_id": id})
	return *value, nil
}

func (s *MemoryStorage) authorize(runID, attemptID, lease string) (*relay.Run, error) {
	run := s.runs[runID]
	if run == nil {
		return nil, ErrNotFound
	}
	if run.Attempt.ID != attemptID || lease == "" || run.Attempt.LeaseToken != lease {
		return nil, ErrInvalidLease
	}
	if run.Attempt.LeaseExpiresAt == nil || !time.Now().UTC().Before(*run.Attempt.LeaseExpiresAt) {
		s.requeueExpired(run, time.Now().UTC())
		return nil, ErrInvalidLease
	}
	return run, nil
}

func (s *MemoryStorage) releaseNode(nodeID string) {
	if node := s.nodes[nodeID]; node != nil && node.Active > 0 {
		node.Active--
	}
}

func (s *MemoryStorage) requeueExpired(run *relay.Run, now time.Time) {
	if run.Attempt.Status != relay.AttemptLeased || run.Attempt.LeaseExpiresAt == nil || now.Before(*run.Attempt.LeaseExpiresAt) {
		return
	}
	s.releaseNode(run.Attempt.NodeID)
	s.appendEvent(run, "attempt.lease_expired", map[string]string{"node_id": run.Attempt.NodeID})
	run.Attempt.Status, run.Attempt.NodeID, run.Attempt.LeaseToken, run.Attempt.LeaseExpiresAt = relay.AttemptQueued, "", "", nil
}

func (s *MemoryStorage) appendEvent(run *relay.Run, eventType string, value any) {
	data, _ := json.Marshal(value)
	if value == nil {
		data = nil
	}
	s.events[run.ID] = append(s.events[run.ID], relay.Event{ID: newControlPlaneID("event"), RunID: run.ID, AttemptID: run.Attempt.ID, Sequence: len(s.events[run.ID]) + 1, Type: eventType, Data: data, CreatedAt: time.Now().UTC()})
}

func (s *MemoryStorage) cancelRunLocked(run *relay.Run, request CancelRequest, now time.Time) {
	if terminalRun(run.Status) || run.CancelRequestedAt != nil {
		return
	}
	run.CancelRequestedAt = timePointer(now)
	run.CancelReason = request.Reason
	s.appendEvent(run, "run.cancel_requested", request)
	if run.Attempt.Status == relay.AttemptQueued {
		s.finalizeCancellation(run, now)
		return
	}
	run.Status = relay.RunCancelling
}

func (s *MemoryStorage) finalizeCancellation(run *relay.Run, now time.Time) {
	if run.Status == relay.RunCancelled {
		return
	}
	s.releaseNode(run.Attempt.NodeID)
	run.Attempt.Status = relay.AttemptCancelled
	run.Attempt.CompletedAt = timePointer(now)
	run.Status = relay.RunCancelled
	run.CancelledAt = timePointer(now)
	run.CompletedAt = timePointer(now)
	s.appendEvent(run, "attempt.cancelled", map[string]string{"reason": run.CancelReason})
	s.appendEvent(run, "run.cancelled", map[string]string{"reason": run.CancelReason})
}

func terminalRun(status relay.RunStatus) bool {
	return status == relay.RunSucceeded || status == relay.RunFailed || status == relay.RunCancelled
}

func nodeMatches(node Node, request relay.Request) bool {
	foundRuntime := false
	for _, runtime := range node.Runtimes {
		if RuntimeMatches(runtime, request.Runtime) {
			foundRuntime = true
			break
		}
	}
	if !foundRuntime {
		return false
	}
	for key, value := range request.Runtime.Labels {
		if node.Labels[key] != value {
			return false
		}
	}
	for _, grant := range request.Capabilities {
		found := false
		for _, available := range node.Capabilities {
			if available.Name == grant.Name && available.Version == grant.Version {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func newControlPlaneID(prefix string) string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%s_%s", prefix, hex.EncodeToString(value[:]))
}

func retryBackoff(policy relay.RetryPolicy) time.Duration {
	backoff, _ := time.ParseDuration(policy.Backoff)
	return backoff
}

func timePointer(value time.Time) *time.Time { return &value }
