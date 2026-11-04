package node

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/KDF5000/relay/controlplane"
)

func retryFinalReport(ctx context.Context, report func(context.Context) error) error {
	reportCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	delay := 100 * time.Millisecond
	var last error
	for {
		last = report(reportCtx)
		if last == nil {
			return nil
		}
		if errors.Is(last, controlplane.ErrInvalidLease) || errors.Is(last, controlplane.ErrInvalidTransition) || errors.Is(last, controlplane.ErrRunCancelled) || errors.Is(last, controlplane.ErrNotFound) {
			return last
		}
		timer := time.NewTimer(delay)
		select {
		case <-reportCtx.Done():
			timer.Stop()
			return fmt.Errorf("final report deadline: %w", errors.Join(last, reportCtx.Err()))
		case <-timer.C:
		}
		if delay < time.Second {
			delay *= 2
		}
	}
}
