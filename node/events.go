package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/KDF5000/relay/controlplane"
)

// Delivery is bounded in both time and payload size. Runtime output is blocked
// while reconnecting; lease renewal runs independently. No unbounded queue or
// background sender survives Execute, so completion cannot overtake events.
func deliverEvent(ctx context.Context, cp ControlPlane, a controlplane.Assignment, id, kind string, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if len(payload) > 1<<20 {
		return errors.New("event payload exceeds 1 MiB")
	}
	deliveryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	delay := 100 * time.Millisecond
	for {
		err = cp.AppendEvent(deliveryCtx, a.RunID, a.AttemptID, a.LeaseToken, kind, json.RawMessage(payload), id)
		if err == nil {
			return nil
		}
		if errors.Is(err, controlplane.ErrInvalidLease) || errors.Is(err, controlplane.ErrInvalidTransition) || errors.Is(err, controlplane.ErrRunCancelled) || errors.Is(err, controlplane.ErrNotFound) {
			return err
		}
		var httpError interface{ HTTPStatusCode() int }
		if errors.As(err, &httpError) {
			switch httpError.HTTPStatusCode() {
			case 400, 413, 422:
				return err
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-deliveryCtx.Done():
			timer.Stop()
			return fmt.Errorf("event delivery deadline: %w", errors.Join(err, deliveryCtx.Err()))
		case <-timer.C:
		}
		if delay < time.Second {
			delay *= 2
		}
	}
}
