package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/controlplane"
)

func assigned(t *testing.T) (*controlplane.Service, controlplane.Assignment) {
	t.Helper()
	ctx := context.Background()
	cp := controlplane.New(30 * time.Second)
	if _, err := cp.RegisterNode(ctx, controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "outbox-node", Capacity: 1, Runtimes: []controlplane.Runtime{{Provider: "test"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := cp.Submit(ctx, relay.Request{AgentID: "agent", IdempotencyKey: "outbox", Runtime: relay.RuntimeRequirement{Provider: "test"}, Input: relay.Input{Prompt: "not persisted in outbox"}}); err != nil {
		t.Fatal(err)
	}
	a, err := cp.Claim(ctx, "outbox-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.Start(ctx, a); err != nil {
		t.Fatal(err)
	}
	return cp, a
}

// The helper remains alive holding the queue lock until the parent SIGKILLs it.
func TestOutboxCrashHelper(t *testing.T) {
	root := os.Getenv("RELAY_TEST_OUTBOX_CRASH")
	if root == "" {
		return
	}
	var a controlplane.Assignment
	if err := json.Unmarshal([]byte(os.Getenv("RELAY_TEST_ASSIGNMENT")), &a); err != nil {
		t.Fatal(err)
	}
	o, err := OpenOutbox(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.save(a, "1", "assistant.message.delta", map[string]string{"delta": "hello"}); err != nil {
		t.Fatal(err)
	}
	fmt.Println("ready")
	time.Sleep(time.Minute)
}

func TestOutboxSurvivesKilledProcess(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprint(committed), func(t *testing.T) {
			cp, a := assigned(t)
			ctx := context.Background()
			root := t.TempDir()
			if committed {
				if err := cp.AppendEvent(ctx, a.RunID, a.AttemptID, a.LeaseToken, "assistant.message.delta", map[string]string{"delta": "hello"}, "1"); err != nil {
					t.Fatal(err)
				}
			}
			raw, _ := json.Marshal(a)
			child := exec.Command(os.Args[0], "-test.run=^TestOutboxCrashHelper$")
			child.Env = append(os.Environ(), "RELAY_TEST_OUTBOX_CRASH="+root, "RELAY_TEST_ASSIGNMENT="+string(raw))
			stdout, err := child.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = child.Process.Kill() }()
			ready := make(chan bool, 1)
			go func() { scanner := bufio.NewScanner(stdout); ready <- scanner.Scan() && scanner.Text() == "ready" }()
			select {
			case ok := <-ready:
				if !ok {
					t.Fatal("child failed to persist")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("child timeout")
			}
			if other, err := OpenOutbox(root, 0); err == nil {
				other.Close()
				t.Fatal("second process acquired outbox")
			}
			_ = child.Process.Kill()
			_ = child.Wait()
			o, err := OpenOutbox(root, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer o.Close()
			if err := o.Recover(ctx, cp, nil); err != nil {
				t.Fatal(err)
			}
			run, err := cp.GetRun(ctx, a.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != relay.RunFailed || !strings.Contains(run.Error, "Node restarted") {
				t.Fatalf("run=%+v", run)
			}
			events, _ := cp.Events(ctx, a.RunID)
			count := 0
			for _, event := range events {
				if event.Type == "assistant.message.delta" {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("deltas=%d", count)
			}
			files, _ := filepath.Glob(filepath.Join(root, "*.json"))
			if len(files) != 0 {
				t.Fatal("acknowledged events retained")
			}
		})
	}
}

type unavailableOutbox struct{ ControlPlane }

func (unavailableOutbox) AppendEvent(context.Context, string, string, string, string, any, ...string) error {
	return errors.New("offline")
}

func TestOutboxBoundsAndRecoveryFailures(t *testing.T) {
	cp, a := assigned(t)
	root := t.TempDir()
	o, err := OpenOutbox(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.save(a, "1", "event", nil); err == nil {
		t.Fatal("disk cap ignored")
	}
	o.Close()
	o, err = OpenOutbox(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	path, err := o.save(a, "1", "event", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "not persisted") {
		t.Fatal("business prompt persisted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := o.Recover(ctx, unavailableOutbox{cp}, nil); err == nil {
		t.Fatal("network error ignored")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("unacknowledged event removed")
	}
	// Invalidate the attempt: recovery must not replay into a terminal run.
	if err := cp.Fail(context.Background(), a, "interrupted"); err != nil {
		t.Fatal(err)
	}
	var reason string
	if err := o.Recover(context.Background(), cp, func(s string) { reason = s }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reason, "discarded") {
		t.Fatalf("reason=%s", reason)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale record not cleaned")
	}
}

func TestOutboxPersistsOversizedEventAsBoundedPreview(t *testing.T) {
	_, a := assigned(t)
	o, err := OpenOutbox(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()

	path, err := o.save(a, "1", "runtime.trae.item.completed", map[string]string{
		"output": strings.Repeat("x", maxEventPayloadBytes),
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record pendingEvent
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if len(record.Data) > maxEventPayloadBytes {
		t.Fatalf("persisted payload size = %d", len(record.Data))
	}
	var data truncatedEventData
	if err := json.Unmarshal(record.Data, &data); err != nil {
		t.Fatal(err)
	}
	if !data.Truncated || data.OriginalBytes <= maxEventPayloadBytes {
		t.Fatalf("truncated=%v original_bytes=%d", data.Truncated, data.OriginalBytes)
	}
}

func TestOutboxExpiredLeaseDoesNotReplay(t *testing.T) {
	ctx := context.Background()
	cp := controlplane.New(20 * time.Millisecond)
	if _, err := cp.RegisterNode(ctx, controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "expired-node", Capacity: 1, Runtimes: []controlplane.Runtime{{Provider: "test"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := cp.Submit(ctx, relay.Request{AgentID: "agent", IdempotencyKey: "expired", Runtime: relay.RuntimeRequirement{Provider: "test"}, Input: relay.Input{Prompt: "work"}}); err != nil {
		t.Fatal(err)
	}
	a, err := cp.Claim(ctx, "expired-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.Start(ctx, a); err != nil {
		t.Fatal(err)
	}
	o, err := OpenOutbox(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if _, err := o.save(a, "1", "assistant.message.delta", map[string]string{"delta": "stale"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(a.LeaseExpiresAt) + 10*time.Millisecond)
	var reason string
	if err := o.Recover(ctx, cp, func(s string) { reason = s }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reason, "discarded") {
		t.Fatalf("reason=%s", reason)
	}
	events, err := cp.Events(ctx, a.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == "assistant.message.delta" {
			t.Fatal("expired event replayed")
		}
	}
}
