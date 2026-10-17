package controlplane

import (
	"context"

	"github.com/KDF5000/relay"
)

type AccessKind string

const (
	AccessHost AccessKind = "host"
	AccessNode AccessKind = "node"
)

type AccessScope struct {
	Kind                         AccessKind
	Subject, TenantID, ProjectID string
}
type accessKey struct{}

func WithAccess(ctx context.Context, scope AccessScope) context.Context {
	return context.WithValue(ctx, accessKey{}, scope)
}
func AccessFrom(ctx context.Context) (AccessScope, bool) {
	value, ok := ctx.Value(accessKey{}).(AccessScope)
	return value, ok
}
func authorizeRunScope(ctx context.Context, run relay.Run) error {
	scope, ok := AccessFrom(ctx)
	if !ok || scope.Kind == AccessNode {
		return nil
	}
	if scope.TenantID != "" && scope.TenantID != run.TenantID {
		return ErrNotFound
	}
	if scope.ProjectID != "" && scope.ProjectID != run.ProjectID {
		return ErrNotFound
	}
	return nil
}

// Scoped Node credentials cannot use another Node's attempt, even if a lease
// was accidentally disclosed. Storage still validates the lease and attempt
// atomically, including expiry and reassignment, after this identity check.
func (s *Service) authorizeNodeRun(ctx context.Context, runID string) error {
	scope, ok := AccessFrom(ctx)
	if !ok || scope.Kind != AccessNode || scope.Subject == "" {
		return nil
	}
	run, err := s.storage.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.Attempt.NodeID != scope.Subject {
		return ErrInvalidLease
	}
	return nil
}
