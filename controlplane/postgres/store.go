// Package postgres provides the production PostgreSQL storage for Relay's
// distributed control plane.
package postgres

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/controlplane"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct{ pool *pgxpool.Pool }

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	if databaseURL == "" {
		return nil, errors.New("relay postgres: database URL is required")
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("relay postgres: open: %w", err)
	}
	store := &Store{pool: pool}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("relay postgres: ping: %w", err)
	}
	if err := store.Migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return store, nil
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }
func (s *Store) Close()             { s.pool.Close() }

func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('relay_schema_migrations')::bigint)`); err != nil {
		return fmt.Errorf("relay postgres: acquire migration lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS relay_schema_migrations (
			name TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("relay postgres: create migration ledger: %w", err)
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		var applied bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM relay_schema_migrations WHERE name = $1)`, entry.Name()).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		migration, err := migrations.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(migration)); err != nil {
			return fmt.Errorf("relay postgres: migration %s: %w", entry.Name(), err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO relay_schema_migrations (name) VALUES ($1)`, entry.Name()); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) RegisterNode(ctx context.Context, registration controlplane.NodeRegistration) (controlplane.Node, error) {
	labels, _ := json.Marshal(registration.Labels)
	runtimes, _ := json.Marshal(registration.Runtimes)
	capabilities, _ := json.Marshal(registration.Capabilities)
	now := time.Now().UTC()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO relay_nodes (id, version, protocol_version, labels, runtimes, capabilities, capacity, last_seen)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (id) DO UPDATE SET
			version = EXCLUDED.version, protocol_version = EXCLUDED.protocol_version,
			labels = EXCLUDED.labels, runtimes = EXCLUDED.runtimes,
			capabilities = EXCLUDED.capabilities, capacity = EXCLUDED.capacity,
			last_seen = EXCLUDED.last_seen`, registration.ID, registration.Version, registration.ProtocolVersion, labels, runtimes, capabilities, registration.Capacity, now)
	if err != nil {
		return controlplane.Node{}, err
	}
	return s.node(ctx, registration.ID)
}

func (s *Store) Heartbeat(ctx context.Context, nodeID string) (controlplane.Node, error) {
	command, err := s.pool.Exec(ctx, `UPDATE relay_nodes SET last_seen = now() WHERE id = $1`, nodeID)
	if err != nil {
		return controlplane.Node{}, err
	}
	if command.RowsAffected() == 0 {
		return controlplane.Node{}, controlplane.ErrNotFound
	}
	return s.node(ctx, nodeID)
}

func (s *Store) Submit(ctx context.Context, request relay.Request) (relay.Run, error) {
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return relay.Run{}, err
	}
	runtimeJSON, _ := json.Marshal(request.Runtime)
	sourceJSON, _ := json.Marshal(request.Source)
	runID, attemptID := newID("run"), newID("attempt")
	now := time.Now().UTC()
	var deadline *time.Time
	if timeout, _ := time.ParseDuration(request.Timeout); timeout > 0 {
		value := now.Add(timeout)
		deadline = &value
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return relay.Run{}, err
	}
	defer tx.Rollback(ctx)
	var inserted string
	err = tx.QueryRow(ctx, `
		INSERT INTO relay_runs (id, tenant_id, project_id, session_id, agent_id, idempotency_key, runtime, source, request, status, created_at, current_attempt_id, deadline_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (tenant_id, project_id, idempotency_key) DO NOTHING
		RETURNING id`, runID, request.TenantID, request.ProjectID, request.SessionID, request.AgentID, request.IdempotencyKey, runtimeJSON, sourceJSON, requestJSON, relay.RunQueued, now, attemptID, deadline).Scan(&inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		var existingID string
		if err := tx.QueryRow(ctx, `SELECT id FROM relay_runs WHERE tenant_id=$1 AND project_id=$2 AND idempotency_key=$3`, request.TenantID, request.ProjectID, request.IdempotencyKey).Scan(&existingID); err != nil {
			return relay.Run{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return relay.Run{}, err
		}
		return s.GetRun(ctx, existingID)
	}
	if err != nil {
		return relay.Run{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO relay_attempts (id, run_id, number, status) VALUES ($1, $2, 1, $3)`, attemptID, runID, relay.AttemptQueued); err != nil {
		return relay.Run{}, err
	}
	if err := insertEvent(ctx, tx, runID, attemptID, 1, "run.created", map[string]any{"status": relay.RunQueued}); err != nil {
		return relay.Run{}, err
	}
	if err := insertEvent(ctx, tx, runID, attemptID, 2, "attempt.queued", map[string]any{"number": 1}); err != nil {
		return relay.Run{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return relay.Run{}, err
	}
	return s.GetRun(ctx, runID)
}

func (s *Store) Claim(ctx context.Context, nodeID string, leaseTTL time.Duration) (controlplane.Assignment, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return controlplane.Assignment{}, err
	}
	defer tx.Rollback(ctx)
	node, err := scanNode(tx.QueryRow(ctx, nodeSelect+` WHERE n.id = $1 FOR UPDATE OF n`, nodeID))
	if errors.Is(err, pgx.ErrNoRows) {
		return controlplane.Assignment{}, controlplane.ErrNotFound
	}
	if err != nil {
		return controlplane.Assignment{}, err
	}
	if node.Active >= node.Capacity {
		return controlplane.Assignment{}, controlplane.ErrNoAssignment
	}
	rows, err := tx.Query(ctx, `
		SELECT r.id
		FROM relay_runs r
		JOIN relay_attempts a ON a.run_id = r.id
		WHERE r.status = $1 AND r.current_attempt_id = a.id
		  AND (r.deadline_at IS NULL OR r.deadline_at > now())
		  AND (a.status = $2 AND a.available_at <= now() OR (a.status = $3 AND a.lease_expires_at <= now()))
		ORDER BY r.created_at, r.id
		LIMIT 100`, relay.RunQueued, relay.AttemptQueued, relay.AttemptLeased)
	if err != nil {
		return controlplane.Assignment{}, err
	}
	var candidates []string
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			rows.Close()
			return controlplane.Assignment{}, err
		}
		candidates = append(candidates, runID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return controlplane.Assignment{}, err
	}
	for _, runID := range candidates {
		var requestJSON []byte
		var attemptID, attemptStatus, previousNode string
		var previousExpiry *time.Time
		err := tx.QueryRow(ctx, `
			SELECT r.request, a.id, a.status, a.node_id, a.lease_expires_at
			FROM relay_runs r JOIN relay_attempts a ON a.run_id = r.id
			WHERE r.id = $1 AND r.status = $2 AND r.current_attempt_id = a.id
			  AND (r.deadline_at IS NULL OR r.deadline_at > now())
			  AND (a.status = $3 AND a.available_at <= now() OR (a.status = $4 AND a.lease_expires_at <= now()))
			FOR UPDATE OF r, a SKIP LOCKED`, runID, relay.RunQueued, relay.AttemptQueued, relay.AttemptLeased).Scan(&requestJSON, &attemptID, &attemptStatus, &previousNode, &previousExpiry)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return controlplane.Assignment{}, err
		}
		var request relay.Request
		if err := json.Unmarshal(requestJSON, &request); err != nil {
			return controlplane.Assignment{}, err
		}
		if !nodeMatches(node, request) {
			continue
		}
		now := time.Now().UTC()
		if attemptStatus == string(relay.AttemptLeased) && previousExpiry != nil && !now.Before(*previousExpiry) {
			if err := appendEvent(ctx, tx, runID, attemptID, "attempt.lease_expired", map[string]string{"node_id": previousNode}); err != nil {
				return controlplane.Assignment{}, err
			}
		}
		lease, expires := newID("lease"), now.Add(leaseTTL)
		if _, err := tx.Exec(ctx, `
			UPDATE relay_attempts SET status = $1, node_id = $2, lease_token = $3, lease_expires_at = $4
			WHERE id = $5`, relay.AttemptLeased, nodeID, lease, expires, attemptID); err != nil {
			return controlplane.Assignment{}, err
		}
		if err := appendEvent(ctx, tx, runID, attemptID, "attempt.leased", map[string]any{"node_id": nodeID, "lease_expires_at": expires}); err != nil {
			return controlplane.Assignment{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return controlplane.Assignment{}, err
		}
		return controlplane.Assignment{RunID: runID, AttemptID: attemptID, LeaseToken: lease, LeaseExpiresAt: expires, Request: request}, nil
	}
	return controlplane.Assignment{}, controlplane.ErrNoAssignment
}

func (s *Store) Renew(ctx context.Context, assignment controlplane.Assignment, leaseTTL time.Duration) (controlplane.LeaseUpdate, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return controlplane.LeaseUpdate{}, err
	}
	defer tx.Rollback(ctx)
	run, err := scanRun(tx.QueryRow(ctx, runSelect+` WHERE r.id = $1 FOR UPDATE OF r, a`, assignment.RunID))
	if errors.Is(err, pgx.ErrNoRows) {
		return controlplane.LeaseUpdate{}, controlplane.ErrNotFound
	}
	if err != nil {
		return controlplane.LeaseUpdate{}, err
	}
	now := time.Now().UTC()
	if run.Attempt.ID != assignment.AttemptID || assignment.LeaseToken == "" || run.Attempt.LeaseToken != assignment.LeaseToken || run.Attempt.LeaseExpiresAt == nil || !now.Before(*run.Attempt.LeaseExpiresAt) {
		return controlplane.LeaseUpdate{}, controlplane.ErrInvalidLease
	}
	if run.Attempt.Status != relay.AttemptLeased && run.Attempt.Status != relay.AttemptRunning {
		return controlplane.LeaseUpdate{}, controlplane.ErrInvalidTransition
	}
	expires := now.Add(leaseTTL)
	if _, err := tx.Exec(ctx, `UPDATE relay_attempts SET lease_expires_at = $1 WHERE id = $2`, expires, run.Attempt.ID); err != nil {
		return controlplane.LeaseUpdate{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return controlplane.LeaseUpdate{}, err
	}
	return controlplane.LeaseUpdate{LeaseExpiresAt: expires, CancelRequested: run.CancelRequestedAt != nil, CancelReason: run.CancelReason}, nil
}

func (s *Store) Reconcile(ctx context.Context, now time.Time, limit int) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	processed := 0
	deadlineRows, err := tx.Query(ctx, `
		SELECT r.id
		FROM relay_runs r
		JOIN relay_attempts a ON a.id = r.current_attempt_id
		WHERE r.deadline_at <= $1 AND r.cancel_requested_at IS NULL
		  AND r.status IN ($2, $3)
		ORDER BY r.deadline_at, r.id
		LIMIT $4
		FOR UPDATE OF r, a SKIP LOCKED`, now, relay.RunQueued, relay.RunRunning, limit)
	if err != nil {
		return 0, err
	}
	var deadlineRunIDs []string
	for deadlineRows.Next() {
		var runID string
		if err := deadlineRows.Scan(&runID); err != nil {
			deadlineRows.Close()
			return 0, err
		}
		deadlineRunIDs = append(deadlineRunIDs, runID)
	}
	deadlineRows.Close()
	if err := deadlineRows.Err(); err != nil {
		return 0, err
	}
	for _, runID := range deadlineRunIDs {
		run, err := scanRun(tx.QueryRow(ctx, runSelect+` WHERE r.id = $1`, runID))
		if err != nil {
			return 0, err
		}
		if err := requestCancelTx(ctx, tx, run, controlplane.CancelRequest{Reason: "run timeout exceeded", RequestedBy: "relay"}, now); err != nil {
			return 0, err
		}
		processed++
	}
	remaining := limit - processed
	if remaining <= 0 {
		if err := tx.Commit(ctx); err != nil {
			return 0, err
		}
		return processed, nil
	}
	type expiredAttempt struct {
		runID, attemptID, nodeID string
		number                   int
		requestJSON              []byte
		runStatus                relay.RunStatus
	}
	rows, err := tx.Query(ctx, `
		SELECT r.id, a.id, a.number, a.node_id, r.request, r.status
		FROM relay_runs r
		JOIN relay_attempts a ON a.id = r.current_attempt_id
		WHERE ((r.status = $1 AND a.status = $3)
		    OR (r.status = $2 AND a.status IN ($3, $4)))
		  AND a.lease_expires_at <= $5
		ORDER BY a.lease_expires_at, r.id
		LIMIT $6
		FOR UPDATE OF r, a SKIP LOCKED`, relay.RunRunning, relay.RunCancelling, relay.AttemptRunning, relay.AttemptLeased, now, remaining)
	if err != nil {
		return 0, err
	}
	var expired []expiredAttempt
	for rows.Next() {
		var item expiredAttempt
		if err := rows.Scan(&item.runID, &item.attemptID, &item.number, &item.nodeID, &item.requestJSON, &item.runStatus); err != nil {
			rows.Close()
			return 0, err
		}
		expired = append(expired, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, item := range expired {
		if item.runStatus == relay.RunCancelling {
			run, err := scanRun(tx.QueryRow(ctx, runSelect+` WHERE r.id = $1`, item.runID))
			if err != nil {
				return 0, err
			}
			if err := finalizeCancellationTx(ctx, tx, run, now); err != nil {
				return 0, err
			}
			processed++
			continue
		}
		var request relay.Request
		if err := json.Unmarshal(item.requestJSON, &request); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `UPDATE relay_attempts SET status = $1, completed_at = $2 WHERE id = $3`, relay.AttemptLost, now, item.attemptID); err != nil {
			return 0, err
		}
		if err := appendEvent(ctx, tx, item.runID, item.attemptID, "attempt.lost", map[string]any{"node_id": item.nodeID, "reason": "lease_expired"}); err != nil {
			return 0, err
		}
		if item.number >= request.Retry.MaxAttempts {
			cause := "attempt lease expired"
			if _, err := tx.Exec(ctx, `UPDATE relay_runs SET status = $1, error = $2, completed_at = $3 WHERE id = $4`, relay.RunFailed, cause, now, item.runID); err != nil {
				return 0, err
			}
			if err := appendEvent(ctx, tx, item.runID, item.attemptID, "run.failed", map[string]string{"error": cause}); err != nil {
				return 0, err
			}
			processed++
			continue
		}
		nextID := newID("attempt")
		available := now.Add(retryBackoff(request.Retry))
		if _, err := tx.Exec(ctx, `INSERT INTO relay_attempts (id, run_id, number, status, available_at) VALUES ($1, $2, $3, $4, $5)`, nextID, item.runID, item.number+1, relay.AttemptQueued, available); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `UPDATE relay_runs SET status = $1, current_attempt_id = $2, error = '', started_at = NULL, completed_at = NULL WHERE id = $3`, relay.RunQueued, nextID, item.runID); err != nil {
			return 0, err
		}
		if err := appendEvent(ctx, tx, item.runID, nextID, "attempt.queued", map[string]any{"number": item.number + 1, "available_at": available, "reason": "retry"}); err != nil {
			return 0, err
		}
		processed++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return processed, nil
}

func (s *Store) CancelRun(ctx context.Context, runID string, request controlplane.CancelRequest) (relay.Run, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return relay.Run{}, err
	}
	defer tx.Rollback(ctx)
	run, err := scanRun(tx.QueryRow(ctx, runSelect+` WHERE r.id = $1 FOR UPDATE OF r, a`, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return relay.Run{}, controlplane.ErrNotFound
	}
	if err != nil {
		return relay.Run{}, err
	}
	if err := requestCancelTx(ctx, tx, run, request, time.Now().UTC()); err != nil {
		return relay.Run{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return relay.Run{}, err
	}
	return s.GetRun(ctx, runID)
}

func (s *Store) AcknowledgeCancellation(ctx context.Context, assignment controlplane.Assignment) error {
	return s.transition(ctx, assignment, func(tx pgx.Tx, run relay.Run) error {
		if run.Status == relay.RunCancelled {
			return nil
		}
		if run.Status != relay.RunCancelling {
			return controlplane.ErrInvalidTransition
		}
		return finalizeCancellationTx(ctx, tx, run, time.Now().UTC())
	})
}

func (s *Store) Start(ctx context.Context, assignment controlplane.Assignment) error {
	return s.transition(ctx, assignment, func(tx pgx.Tx, run relay.Run) error {
		if run.Status == relay.RunCancelling {
			return controlplane.ErrRunCancelled
		}
		if run.Attempt.Status != relay.AttemptLeased {
			return controlplane.ErrInvalidTransition
		}
		if run.Attempt.LeaseExpiresAt != nil && !time.Now().UTC().Before(*run.Attempt.LeaseExpiresAt) {
			return controlplane.ErrInvalidLease
		}
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `UPDATE relay_runs SET status = $1, started_at = $2 WHERE id = $3`, relay.RunRunning, now, run.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE relay_attempts SET status = $1, started_at = $2 WHERE id = $3`, relay.AttemptRunning, now, run.Attempt.ID); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, run.ID, run.Attempt.ID, "attempt.started", map[string]string{"node_id": run.Attempt.NodeID}); err != nil {
			return err
		}
		return appendEvent(ctx, tx, run.ID, run.Attempt.ID, "run.started", nil)
	})
}

func (s *Store) AppendEvent(ctx context.Context, runID, attemptID, lease, eventType string, data any, eventIDs ...string) error {
	encoded, err := marshalEventData(data)
	if err != nil {
		return err
	}
	data = json.RawMessage(encoded)
	assignment := controlplane.Assignment{RunID: runID, AttemptID: attemptID, LeaseToken: lease}
	return s.transition(ctx, assignment, func(tx pgx.Tx, run relay.Run) error {
		if run.Attempt.Status != relay.AttemptRunning {
			return controlplane.ErrInvalidTransition
		}
		id := controlplane.EventIdentity(attemptID, lease, eventIDs)
		if id == "" {
			return appendEvent(ctx, tx, runID, attemptID, eventType, data)
		}
		var oldType string
		var oldData []byte
		err := tx.QueryRow(ctx, `SELECT type, data FROM relay_events WHERE id=$1`, id).Scan(&oldType, &oldData)
		if err == nil {
			return controlplane.SameEvent(oldType, oldData, eventType, data)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO relay_events (id,run_id,attempt_id,sequence,type,data,created_at) SELECT $1,$2,$3,COALESCE(MAX(sequence),0)+1,$4,$5,now() FROM relay_events WHERE run_id=$2`, id, runID, attemptID, eventType, encoded)
		return err
	})
}

func (s *Store) Complete(ctx context.Context, assignment controlplane.Assignment, result relay.Result) error {
	return s.transition(ctx, assignment, func(tx pgx.Tx, run relay.Run) error {
		if run.Status == relay.RunSucceeded && run.Attempt.Status == relay.AttemptSucceeded {
			if run.Result != nil && controlplane.SameValue(*run.Result, result) {
				return nil
			}
			return fmt.Errorf("%w: completion result differs from the committed result", controlplane.ErrInvalidTransition)
		}
		if run.Status == relay.RunCancelling {
			return controlplane.ErrRunCancelled
		}
		if run.Attempt.Status != relay.AttemptRunning {
			return controlplane.ErrInvalidTransition
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `UPDATE relay_runs SET status = $1, result = $2, completed_at = $3 WHERE id = $4`, relay.RunSucceeded, encoded, now, run.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE relay_attempts SET status = $1, completed_at = $2 WHERE id = $3`, relay.AttemptSucceeded, now, run.Attempt.ID); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, run.ID, run.Attempt.ID, "attempt.succeeded", nil); err != nil {
			return err
		}
		return appendEvent(ctx, tx, run.ID, run.Attempt.ID, "run.succeeded", result)
	})
}

func (s *Store) Fail(ctx context.Context, assignment controlplane.Assignment, cause string) error {
	return s.transition(ctx, assignment, func(tx pgx.Tx, run relay.Run) error {
		if run.Status == relay.RunFailed && run.Attempt.Status == relay.AttemptFailed {
			if run.Error == cause {
				return nil
			}
			return fmt.Errorf("%w: failure cause differs from the committed cause", controlplane.ErrInvalidTransition)
		}
		if run.Status == relay.RunCancelling {
			return controlplane.ErrRunCancelled
		}
		if run.Attempt.Status != relay.AttemptRunning && run.Attempt.Status != relay.AttemptLeased {
			return controlplane.ErrInvalidTransition
		}
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `UPDATE relay_runs SET status = $1, error = $2, completed_at = $3 WHERE id = $4`, relay.RunFailed, cause, now, run.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE relay_attempts SET status = $1, completed_at = $2 WHERE id = $3`, relay.AttemptFailed, now, run.Attempt.ID); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, run.ID, run.Attempt.ID, "attempt.failed", map[string]string{"error": cause}); err != nil {
			return err
		}
		return appendEvent(ctx, tx, run.ID, run.Attempt.ID, "run.failed", map[string]string{"error": cause})
	})
}

func (s *Store) GetRun(ctx context.Context, runID string) (relay.Run, error) {
	run, err := scanRun(s.pool.QueryRow(ctx, runSelect+` WHERE r.id = $1`, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return relay.Run{}, controlplane.ErrNotFound
	}
	return run, err
}

func (s *Store) Events(ctx context.Context, runID string) ([]relay.Event, error) {
	return s.EventsAfter(ctx, runID, 0)
}

func (s *Store) EventsAfter(ctx context.Context, runID string, after int) ([]relay.Event, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM relay_runs WHERE id = $1)`, runID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, controlplane.ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT id, run_id, attempt_id, sequence, type, data, created_at FROM relay_events WHERE run_id = $1 AND sequence > $2 ORDER BY sequence`, runID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []relay.Event
	for rows.Next() {
		var event relay.Event
		if err := rows.Scan(&event.ID, &event.RunID, &event.AttemptID, &event.Sequence, &event.Type, &event.Data, &event.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) Nodes(ctx context.Context) ([]controlplane.Node, error) {
	rows, err := s.pool.Query(ctx, nodeSelect+` ORDER BY n.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var nodes []controlplane.Node
	for rows.Next() {
		node, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

func (s *Store) AddArtifact(ctx context.Context, assignment controlplane.Assignment, artifact relay.Artifact) error {
	return s.transition(ctx, assignment, func(tx pgx.Tx, run relay.Run) error {
		if run.Attempt.Status != relay.AttemptRunning {
			return controlplane.ErrInvalidTransition
		}
		_, err := tx.Exec(ctx, `INSERT INTO relay_artifacts (id, run_id, attempt_id, type, ref, name, content_type, size, sha256) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, artifact.ID, run.ID, run.Attempt.ID, artifact.Type, artifact.Ref, artifact.Name, artifact.ContentType, artifact.Size, artifact.SHA256)
		if err != nil {
			return err
		}
		return appendEvent(ctx, tx, run.ID, run.Attempt.ID, "artifact.created", artifact)
	})
}

func (s *Store) Artifacts(ctx context.Context, runID string) ([]relay.Artifact, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM relay_runs WHERE id=$1)`, runID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, controlplane.ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT id,type,ref,name,content_type,size,sha256 FROM relay_artifacts WHERE run_id=$1 ORDER BY created_at,id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []relay.Artifact
	for rows.Next() {
		var value relay.Artifact
		if err := rows.Scan(&value.ID, &value.Type, &value.Ref, &value.Name, &value.ContentType, &value.Size, &value.SHA256); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (s *Store) Artifact(ctx context.Context, artifactID string) (relay.Artifact, error) {
	var value relay.Artifact
	err := s.pool.QueryRow(ctx, `SELECT id,run_id,type,ref,name,content_type,size,sha256 FROM relay_artifacts WHERE id=$1`, artifactID).Scan(&value.ID, &value.RunID, &value.Type, &value.Ref, &value.Name, &value.ContentType, &value.Size, &value.SHA256)
	if errors.Is(err, pgx.ErrNoRows) {
		return relay.Artifact{}, controlplane.ErrNotFound
	}
	return value, err
}

func (s *Store) ReserveCapability(ctx context.Context, a controlplane.Assignment, key, hash string, request relay.CapabilityRequest) (result relay.CapabilityReservation, err error) {
	err = s.transition(ctx, a, func(tx pgx.Tx, run relay.Run) error {
		encoded, _ := json.Marshal(request)
		id := newID("call")
		if _, err := tx.Exec(ctx, `INSERT INTO relay_capability_calls(id,run_id,attempt_id,idempotency_key,request_hash,request,status) VALUES($1,$2,$3,$4,$5,$6,'pending') ON CONFLICT(run_id,idempotency_key) DO NOTHING`, id, run.ID, run.Attempt.ID, key, hash, encoded); err != nil {
			return err
		}
		var storedHash, status, cause string
		var resultJSON []byte
		if err := tx.QueryRow(ctx, `SELECT id,request_hash,status,result,error FROM relay_capability_calls WHERE run_id=$1 AND idempotency_key=$2`, run.ID, key).Scan(&result.CallID, &storedHash, &status, &resultJSON, &cause); err != nil {
			return err
		}
		if storedHash != hash {
			return relay.ErrIdempotencyConflict
		}
		result.Execute = status == "pending" && result.CallID == id
		result.Error = cause
		if status == "succeeded" {
			var value relay.CapabilityResult
			if err := json.Unmarshal(resultJSON, &value); err != nil {
				return err
			}
			result.Result = &value
		}
		return nil
	})
	return result, err
}

func (s *Store) FinishCapability(ctx context.Context, a controlplane.Assignment, reservation relay.CapabilityReservation, result relay.CapabilityResult, cause string) error {
	return s.transition(ctx, a, func(tx pgx.Tx, run relay.Run) error {
		encoded, _ := json.Marshal(result)
		status := "succeeded"
		if cause != "" {
			status = "failed"
		}
		command, err := tx.Exec(ctx, `UPDATE relay_capability_calls SET status=$1,result=$2,error=$3,completed_at=now() WHERE id=$4 AND run_id=$5 AND status='pending'`, status, encoded, cause, reservation.CallID, run.ID)
		if err != nil {
			return err
		}
		if command.RowsAffected() == 0 {
			return controlplane.ErrInvalidTransition
		}
		return appendEvent(ctx, tx, run.ID, run.Attempt.ID, "capability.recorded", map[string]string{"call_id": reservation.CallID, "status": status})
	})
}

func (s *Store) ListRuns(ctx context.Context, tenant, project, session string, limit int) ([]relay.Run, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, runSelect+` WHERE ($1='' OR r.tenant_id=$1) AND ($2='' OR r.project_id=$2) AND ($3='' OR r.session_id=$3) ORDER BY r.created_at DESC LIMIT $4`, tenant, project, session, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []relay.Run
	for rows.Next() {
		value, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}
func (s *Store) Attempts(ctx context.Context, runID string) ([]relay.Attempt, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,number,node_id,lease_token,lease_expires_at,available_at,status,started_at,completed_at FROM relay_attempts WHERE run_id=$1 ORDER BY number`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []relay.Attempt
	for rows.Next() {
		var value relay.Attempt
		if err := rows.Scan(&value.ID, &value.Number, &value.NodeID, &value.LeaseToken, &value.LeaseExpiresAt, &value.AvailableAt, &value.Status, &value.StartedAt, &value.CompletedAt); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	if len(values) == 0 {
		return nil, controlplane.ErrNotFound
	}
	return values, rows.Err()
}
func (s *Store) CreateInteraction(ctx context.Context, a controlplane.Assignment, request relay.InteractionRequest) (value relay.Interaction, err error) {
	err = s.transition(ctx, a, func(tx pgx.Tx, run relay.Run) error {
		if run.Attempt.Status != relay.AttemptRunning {
			return controlplane.ErrInvalidTransition
		}
		value = relay.Interaction{ID: newID("interaction"), RunID: run.ID, AttemptID: run.Attempt.ID, Kind: request.Kind, State: "pending", Prompt: request.Prompt, Data: request.Data, CreatedAt: time.Now().UTC()}
		_, err := tx.Exec(ctx, `INSERT INTO relay_interactions(id,run_id,attempt_id,kind,state,prompt,data,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, value.ID, value.RunID, value.AttemptID, value.Kind, value.State, value.Prompt, value.Data, value.CreatedAt)
		if err != nil {
			return err
		}
		return appendEvent(ctx, tx, run.ID, run.Attempt.ID, "interaction.requested", value)
	})
	return value, err
}
func (s *Store) GetInteraction(ctx context.Context, a controlplane.Assignment, id string) (value relay.Interaction, err error) {
	err = s.transition(ctx, a, func(tx pgx.Tx, run relay.Run) error {
		err := tx.QueryRow(ctx, `SELECT id,run_id,attempt_id,kind,state,prompt,data,response,created_at,resolved_at FROM relay_interactions WHERE id=$1 AND run_id=$2`, id, run.ID).Scan(&value.ID, &value.RunID, &value.AttemptID, &value.Kind, &value.State, &value.Prompt, &value.Data, &value.Response, &value.CreatedAt, &value.ResolvedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return controlplane.ErrNotFound
		}
		return err
	})
	return value, err
}
func (s *Store) Interactions(ctx context.Context, runID string) ([]relay.Interaction, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,run_id,attempt_id,kind,state,prompt,data,response,created_at,resolved_at FROM relay_interactions WHERE run_id=$1 ORDER BY created_at`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []relay.Interaction
	for rows.Next() {
		var v relay.Interaction
		if err := rows.Scan(&v.ID, &v.RunID, &v.AttemptID, &v.Kind, &v.State, &v.Prompt, &v.Data, &v.Response, &v.CreatedAt, &v.ResolvedAt); err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, rows.Err()
}
func (s *Store) ResolveInteraction(ctx context.Context, id string, response json.RawMessage, tenant, project string) (relay.Interaction, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return relay.Interaction{}, err
	}
	defer tx.Rollback(ctx)
	var value relay.Interaction
	err = tx.QueryRow(ctx, `SELECT i.id,i.run_id,i.attempt_id,i.kind,i.state,i.prompt,i.data,i.response,i.created_at,i.resolved_at FROM relay_interactions i JOIN relay_runs r ON r.id=i.run_id WHERE i.id=$1 AND ($2='' OR r.tenant_id=$2) AND ($3='' OR r.project_id=$3) FOR UPDATE OF i`, id, tenant, project).Scan(&value.ID, &value.RunID, &value.AttemptID, &value.Kind, &value.State, &value.Prompt, &value.Data, &value.Response, &value.CreatedAt, &value.ResolvedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return relay.Interaction{}, controlplane.ErrNotFound
	}
	if err != nil {
		return relay.Interaction{}, err
	}
	if value.State != "pending" {
		return relay.Interaction{}, controlplane.ErrInvalidTransition
	}
	now := time.Now().UTC()
	value.State = "resolved"
	value.Response = response
	value.ResolvedAt = &now
	if _, err := tx.Exec(ctx, `UPDATE relay_interactions SET state='resolved',response=$1,resolved_at=$2 WHERE id=$3`, response, now, id); err != nil {
		return relay.Interaction{}, err
	}
	if err := appendEvent(ctx, tx, value.RunID, value.AttemptID, "interaction.resolved", map[string]string{"interaction_id": id}); err != nil {
		return relay.Interaction{}, err
	}
	return value, tx.Commit(ctx)
}

func (s *Store) node(ctx context.Context, nodeID string) (controlplane.Node, error) {
	node, err := scanNode(s.pool.QueryRow(ctx, nodeSelect+` WHERE n.id = $1`, nodeID))
	if errors.Is(err, pgx.ErrNoRows) {
		return controlplane.Node{}, controlplane.ErrNotFound
	}
	return node, err
}

func (s *Store) transition(ctx context.Context, assignment controlplane.Assignment, change func(pgx.Tx, relay.Run) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	run, err := scanRun(tx.QueryRow(ctx, runSelect+` WHERE r.id = $1 FOR UPDATE OF r, a`, assignment.RunID))
	if errors.Is(err, pgx.ErrNoRows) {
		return controlplane.ErrNotFound
	}
	if err != nil {
		return err
	}
	if run.Attempt.ID != assignment.AttemptID || assignment.LeaseToken == "" || run.Attempt.LeaseToken != assignment.LeaseToken {
		return controlplane.ErrInvalidLease
	}
	if terminalRunStatus(run.Status) {
		if err := change(tx, run); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if run.Attempt.LeaseExpiresAt == nil || !time.Now().UTC().Before(*run.Attempt.LeaseExpiresAt) {
		return controlplane.ErrInvalidLease
	}
	if err := change(tx, run); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const runSelect = `
	SELECT r.id, r.tenant_id, r.project_id, r.session_id, r.agent_id, r.idempotency_key, r.runtime, r.source, r.status,
	       r.result, r.error, r.created_at, r.started_at, r.completed_at,
	       r.deadline_at, r.cancel_requested_at, r.cancel_reason, r.cancelled_at,
	       a.id, a.number, a.node_id, a.lease_token, a.lease_expires_at,
	       a.status, a.started_at, a.completed_at, a.available_at
	FROM relay_runs r JOIN relay_attempts a ON a.id = r.current_attempt_id`

type rowScanner interface{ Scan(...any) error }

func scanRun(row rowScanner) (relay.Run, error) {
	var run relay.Run
	var runtimeJSON, sourceJSON, resultJSON []byte
	err := row.Scan(
		&run.ID, &run.TenantID, &run.ProjectID, &run.SessionID, &run.AgentID, &run.IdempotencyKey, &runtimeJSON, &sourceJSON, &run.Status,
		&resultJSON, &run.Error, &run.CreatedAt, &run.StartedAt, &run.CompletedAt,
		&run.DeadlineAt, &run.CancelRequestedAt, &run.CancelReason, &run.CancelledAt,
		&run.Attempt.ID, &run.Attempt.Number, &run.Attempt.NodeID, &run.Attempt.LeaseToken,
		&run.Attempt.LeaseExpiresAt, &run.Attempt.Status, &run.Attempt.StartedAt, &run.Attempt.CompletedAt, &run.Attempt.AvailableAt,
	)
	if err != nil {
		return relay.Run{}, err
	}
	if err := json.Unmarshal(runtimeJSON, &run.Runtime); err != nil {
		return relay.Run{}, err
	}
	if err := json.Unmarshal(sourceJSON, &run.Source); err != nil {
		return relay.Run{}, err
	}
	if len(resultJSON) > 0 {
		var result relay.Result
		if err := json.Unmarshal(resultJSON, &result); err != nil {
			return relay.Run{}, err
		}
		run.Result = &result
	}
	return run, nil
}

const nodeSelect = `
	SELECT n.id, n.version, n.protocol_version, n.labels, n.runtimes, n.capabilities, n.capacity, n.last_seen,
	       (SELECT count(*) FROM relay_attempts a
	        WHERE a.node_id = n.id AND
	              a.status IN ('running', 'leased') AND a.lease_expires_at > now()) AS active
	FROM relay_nodes n`

func scanNode(row rowScanner) (controlplane.Node, error) {
	var node controlplane.Node
	var labels, runtimes, capabilities []byte
	err := row.Scan(&node.ID, &node.Version, &node.ProtocolVersion, &labels, &runtimes, &capabilities, &node.Capacity, &node.LastSeen, &node.Active)
	if err != nil {
		return controlplane.Node{}, err
	}
	if err := json.Unmarshal(labels, &node.Labels); err != nil {
		return controlplane.Node{}, err
	}
	if err := json.Unmarshal(runtimes, &node.Runtimes); err != nil {
		return controlplane.Node{}, err
	}
	if err := json.Unmarshal(capabilities, &node.Capabilities); err != nil {
		return controlplane.Node{}, err
	}
	return node, nil
}

func appendEvent(ctx context.Context, tx pgx.Tx, runID, attemptID, eventType string, value any) error {
	var sequence int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(sequence), 0) + 1 FROM relay_events WHERE run_id = $1`, runID).Scan(&sequence); err != nil {
		return err
	}
	return insertEvent(ctx, tx, runID, attemptID, sequence, eventType, value)
}

func insertEvent(ctx context.Context, tx pgx.Tx, runID, attemptID string, sequence int, eventType string, value any) error {
	var data []byte
	var err error
	if value != nil {
		data, err = marshalEventData(value)
		if err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO relay_events (id, run_id, attempt_id, sequence, type, data, created_at) VALUES ($1, $2, $3, $4, $5, $6, now())`, newID("event"), runID, attemptID, sequence, eventType, data)
	return err
}

func requestCancelTx(ctx context.Context, tx pgx.Tx, run relay.Run, request controlplane.CancelRequest, now time.Time) error {
	if terminalRunStatus(run.Status) || run.CancelRequestedAt != nil {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE relay_runs
		SET status = $1, cancel_requested_at = $2, cancel_reason = $3, cancel_requested_by = $4
		WHERE id = $5`, relay.RunCancelling, now, request.Reason, request.RequestedBy, run.ID); err != nil {
		return err
	}
	run.Status = relay.RunCancelling
	run.CancelRequestedAt = &now
	run.CancelReason = request.Reason
	if err := appendEvent(ctx, tx, run.ID, run.Attempt.ID, "run.cancel_requested", request); err != nil {
		return err
	}
	if run.Attempt.Status == relay.AttemptQueued {
		return finalizeCancellationTx(ctx, tx, run, now)
	}
	return nil
}

func finalizeCancellationTx(ctx context.Context, tx pgx.Tx, run relay.Run, now time.Time) error {
	if run.Status == relay.RunCancelled {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE relay_attempts SET status = $1, completed_at = $2 WHERE id = $3`, relay.AttemptCancelled, now, run.Attempt.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE relay_runs SET status = $1, completed_at = $2, cancelled_at = $2 WHERE id = $3`, relay.RunCancelled, now, run.ID); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, run.ID, run.Attempt.ID, "attempt.cancelled", map[string]string{"reason": run.CancelReason}); err != nil {
		return err
	}
	return appendEvent(ctx, tx, run.ID, run.Attempt.ID, "run.cancelled", map[string]string{"reason": run.CancelReason})
}

func terminalRunStatus(status relay.RunStatus) bool {
	return status == relay.RunSucceeded || status == relay.RunFailed || status == relay.RunCancelled
}

func nodeMatches(node controlplane.Node, request relay.Request) bool {
	foundRuntime := false
	for _, runtime := range node.Runtimes {
		if controlplane.RuntimeMatches(runtime, request.Runtime) {
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

func newID(prefix string) string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(value[:])
}

func retryBackoff(policy relay.RetryPolicy) time.Duration {
	backoff, _ := time.ParseDuration(policy.Backoff)
	return backoff
}

var _ controlplane.Storage = (*Store)(nil)
