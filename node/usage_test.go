package node_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/controlplane"
	"github.com/KDF5000/relay/node"
)

func TestWorkerNormalizesAndPersistsUsage(t *testing.T) {
	for _, failed := range []bool{false, true} {
		ctx := context.Background()
		service := controlplane.New(time.Second)
		worker := &node.Worker{Registration: controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "usage-node", Capacity: 1, Runtimes: []controlplane.Runtime{{Provider: "codex"}}}, ControlPlane: service, Workspaces: preparedWorkspace{dir: t.TempDir()}, Executors: node.ExecutorMap{"codex": relay.ExecutorFunc(func(ctx context.Context, e relay.Execution) (relay.Result, error) {
			e.Emit(ctx, "runtime.codex.turn.completed", json.RawMessage(`{"usage":{"input_tokens":100,"cached_input_tokens":80,"output_tokens":20}}`))
			if failed {
				return relay.Result{}, errors.New("failure after usage")
			}
			return relay.Result{Summary: "done"}, nil
		})}}
		if _, err := worker.Register(ctx); err != nil {
			t.Fatal(err)
		}
		run, err := service.Submit(ctx, relay.Request{AgentID: "agent", IdempotencyKey: "usage", Runtime: relay.RuntimeRequirement{Provider: "codex"}, Input: relay.Input{Prompt: "work"}})
		if err != nil {
			t.Fatal(err)
		}
		result, err := worker.RunOnce(ctx)
		if !failed && err != nil {
			t.Fatal(err)
		}
		persisted, err := service.GetRun(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if persisted.Usage == nil || *persisted.Usage.TotalTokens != 120 || *persisted.Usage.CacheReadTokens != 80 {
			t.Fatalf("usage not persisted %+v", persisted.Usage)
		}
		if failed {
			if persisted.Usage.Status != "partial" {
				t.Fatal("failed execution missing partial flag")
			}
		} else {
			if result.Result.Usage == nil || persisted.Usage.Status != "complete" {
				t.Fatal("completed usage not returned")
			}
		}
	}
}
