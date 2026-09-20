package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/KDF5000/relay/controlplane"
)

// Outbox is a single-process, bounded durable event spool. Open a separate
// directory for each server/node identity; Close only after workers have drained.
type Outbox struct {
	root     string
	maxBytes int64
	lock     *os.File
	mu       sync.Mutex
}

type pendingEvent struct {
	Assignment controlplane.Assignment `json:"assignment"`
	ID         string                  `json:"id"`
	Type       string                  `json:"type"`
	Data       json.RawMessage         `json:"data"`
}

func OpenOutbox(root string, maxBytes int64) (*Outbox, error) {
	if maxBytes <= 0 {
		maxBytes = 64 << 20
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	lock, err := lockOutbox(filepath.Join(root, "owner.lock"))
	if err != nil {
		return nil, err
	}
	o := &Outbox{root: root, maxBytes: maxBytes, lock: lock}
	// An unpublished temporary file was never sent to the server.
	entries, err := os.ReadDir(root)
	if err != nil {
		lock.Close()
		return nil, err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".pending-") {
			if err := os.Remove(filepath.Join(root, entry.Name())); err != nil {
				lock.Close()
				return nil, err
			}
		}
	}
	return o, nil
}

func (o *Outbox) Close() error { return o.lock.Close() }

func (o *Outbox) syncDir() error {
	dir, err := os.Open(o.root)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (o *Outbox) save(a controlplane.Assignment, id, kind string, data any) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	payload, err := marshalEventData(data)
	if err != nil {
		return "", err
	}
	// Do not persist the business prompt or full Request alongside lease secrets.
	a.Request = controlplane.Assignment{}.Request
	encoded, err := json.Marshal(pendingEvent{a, id, kind, payload})
	if err != nil {
		return "", err
	}
	if len(encoded) > 2<<20 {
		return "", errors.New("outbox record exceeds 2 MiB")
	}
	path := filepath.Join(o.root, controlplane.EventIdentity(a.AttemptID, a.LeaseToken, []string{id})+".json")
	entries, err := os.ReadDir(o.root)
	if err != nil {
		return "", err
	}
	used := int64(len(encoded))
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			info, err := entry.Info()
			if err != nil {
				return "", err
			}
			used += info.Size()
		}
	}
	if used > o.maxBytes {
		return "", errors.New("relay node: outbox disk limit reached")
	}
	if _, err := os.Stat(path); err == nil {
		return "", errors.New("outbox event identity already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	file, err := os.CreateTemp(o.root, ".pending-")
	if err != nil {
		return "", err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if _, err = file.Write(encoded); err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return "", err
	}
	if err = os.Rename(temp, path); err != nil {
		return "", err
	}
	if err = o.syncDir(); err != nil {
		return "", err
	}
	return path, nil
}

func (o *Outbox) remove(path string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := os.Remove(path); err != nil {
		return err
	}
	return o.syncDir()
}

func (o *Outbox) deliver(ctx context.Context, cp ControlPlane, a controlplane.Assignment, id, kind string, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	frozen := json.RawMessage(payload)
	path, err := o.save(a, id, kind, frozen)
	if err != nil {
		return err
	}
	if err = deliverEvent(ctx, cp, a, id, kind, frozen); err != nil {
		return err
	}
	return o.remove(path)
}

func staleEvent(err error) bool {
	return errors.Is(err, controlplane.ErrInvalidLease) || errors.Is(err, controlplane.ErrInvalidTransition) || errors.Is(err, controlplane.ErrNotFound) || errors.Is(err, controlplane.ErrRunCancelled)
}

// Recover must run before claiming any work. The server atomically checks the
// lease while appending; local cached expiry is not authoritative after renewals.
// Replaying events does not revive a runtime. Recovered attempts are failed, or
// cancellation is acknowledged; unknown network outcomes retain their files.
func (o *Outbox) Recover(ctx context.Context, cp ControlPlane, report func(string)) error {
	entries, err := os.ReadDir(o.root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(o.root, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > 2<<20 {
			return fmt.Errorf("invalid outbox record %s", entry.Name())
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var record pendingEvent
		if err := json.Unmarshal(raw, &record); err != nil {
			return fmt.Errorf("outbox record %s: %w", entry.Name(), err)
		}
		a := record.Assignment
		if a.RunID == "" || a.AttemptID == "" || a.LeaseToken == "" || record.ID == "" || record.Type == "" {
			return fmt.Errorf("incomplete outbox record %s", entry.Name())
		}
		err = deliverEvent(ctx, cp, a, record.ID, record.Type, record.Data)
		reason := "replayed; runtime was interrupted"
		if err == nil {
			err = cp.Fail(ctx, a, "Node restarted: pending events recovered, runtime execution was interrupted")
		} else if errors.Is(err, controlplane.ErrRunCancelled) {
			err = cp.AcknowledgeCancellation(ctx, a)
			reason = "cancellation acknowledged"
		} else if staleEvent(err) {
			reason = "discarded: lease invalid or attempt no longer running"
		}
		if err != nil && !staleEvent(err) {
			return err
		}
		if err := o.remove(path); err != nil {
			return err
		}
		if report != nil {
			report(fmt.Sprintf("outbox attempt %s: %s", a.AttemptID, reason))
		}
	}
	return nil
}
