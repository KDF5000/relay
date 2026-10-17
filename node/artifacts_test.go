package node_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/controlplane"
	"github.com/KDF5000/relay/node"
	runtimepi "github.com/KDF5000/relay/runtime/pi"
)

type countingBlobStore struct {
	controlplane.BlobStore
	uploads int
}

func (s *countingBlobStore) Put(ctx context.Context, key string, reader io.Reader) (int64, error) {
	s.uploads++
	return s.BlobStore.Put(ctx, key, reader)
}

func TestPiTaskUploadsOnlyDeclaredDeliverables(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	for _, deliverable := range []bool{false, true} {
		name := "plain-answer"
		wantUploads := 0
		if deliverable {
			name, wantUploads = "report", 1
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			binary := filepath.Join(dir, "pi")
			script := "#!/bin/sh\n"
			if deliverable {
				script += `printf '# Report' > report.md
printf '%s' '{"artifacts":[{"path":"report.md","type":"report"}]}' > .relay/artifacts.json
`
			}
			script += `printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"Task complete"}],"stopReason":"stop"}}'
printf '%s\n' '{"type":"agent_settled","aborted":false}'
`
			if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			blobs := &countingBlobStore{BlobStore: controlplane.NewMemoryBlobStore()}
			service := controlplane.NewWithOptions(controlplane.NewMemoryStorage(), controlplane.Options{LeaseTTL: 30 * time.Second, BlobStore: blobs})
			worker := &node.Worker{
				Registration: controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "pi-node", Capacity: 1, Runtimes: []controlplane.Runtime{{Provider: "pi"}}},
				ControlPlane: service,
				Executors:    node.ExecutorMap{"pi": runtimepi.Executor{Config: runtimepi.Config{Binary: binary, WorkRoot: dir, Ephemeral: true}}},
			}
			if _, err := worker.Register(ctx); err != nil {
				t.Fatal(err)
			}
			run, err := service.Submit(ctx, relay.Request{AgentID: "agent", IdempotencyKey: name, Runtime: relay.RuntimeRequirement{Provider: "pi"}, Input: relay.Input{Prompt: "work"}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := worker.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			stored, err := service.GetRun(ctx, run.ID)
			if err != nil || stored.Status != relay.RunSucceeded || stored.Result == nil || stored.Result.Summary != "Task complete" {
				t.Fatalf("run=%+v err=%v", stored, err)
			}
			artifacts, err := service.Artifacts(ctx, run.ID)
			if err != nil || len(artifacts) != wantUploads || blobs.uploads != wantUploads {
				t.Fatalf("artifacts=%+v uploads=%d want=%d err=%v", artifacts, blobs.uploads, wantUploads, err)
			}
			if deliverable {
				if artifacts[0].Name != "report.md" || artifacts[0].Type != "report" {
					t.Fatalf("unexpected artifact: %+v", artifacts[0])
				}
				reader, err := blobs.Open(ctx, artifacts[0].ID)
				if err != nil {
					t.Fatal(err)
				}
				defer reader.Close()
				data, err := io.ReadAll(reader)
				if err != nil || string(data) != "# Report" {
					t.Fatalf("report=%q err=%v", data, err)
				}
			}
		})
	}
}
