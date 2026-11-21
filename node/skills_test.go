package node_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/controlplane"
	"github.com/KDF5000/relay/node"
)

func TestWorkerInstallsSkillsAndRequiresCapableNode(t *testing.T) {
	ctx := context.Background()
	service := controlplane.New(time.Minute)
	// An old node must not claim a request it would silently execute without skills.
	if _, err := service.RegisterNode(ctx, controlplane.NodeRegistration{ID: "old", ProtocolVersion: relay.ProtocolVersion, Capacity: 1, Runtimes: []controlplane.Runtime{{Provider: "test"}}}); err != nil {
		t.Fatal(err)
	}
	executed := false
	worker := &node.Worker{ControlPlane: service, SkillCacheRoot: t.TempDir(), Registration: controlplane.NodeRegistration{ID: "new", ProtocolVersion: relay.ProtocolVersion, Capacity: 1, Runtimes: []controlplane.Runtime{{Provider: "test"}}}, Executors: node.ExecutorMap{"test": relay.ExecutorFunc(func(ctx context.Context, execution relay.Execution) (relay.Result, error) {
		executed = true
		text := execution.Instructions.Stable
		start := strings.Index(text, "\"")
		if start < 0 {
			t.Fatalf("missing path: %s", text)
		}
		end := strings.Index(text[start+1:], "\"")
		entry := text[start+1 : start+1+end]
		if !strings.Contains(execution.Instructions.Prompt, entry) {
			t.Fatal("current turn is missing skill location")
		}
		raw, err := os.ReadFile(filepath.Join(filepath.Dir(entry), "references", "rules.md"))
		if err != nil || string(raw) != "full rules" {
			t.Fatalf("skill files missing before execution: %s %v", raw, err)
		}
		return relay.Result{Summary: "ok"}, nil
	})}}
	if _, err := worker.Register(ctx); err != nil {
		t.Fatal(err)
	}
	skill := relay.SkillPackage{ID: "skill", Name: "Test", Files: []relay.SkillFile{{Path: "SKILL.md", Content: "Read references/rules.md"}, {Path: "references/rules.md", Content: "full rules"}}}
	skill.Version, _ = relay.SkillVersion(skill)
	request := relay.Request{AgentID: "agent", IdempotencyKey: "turn", Runtime: relay.RuntimeRequirement{Provider: "test"}, Input: relay.Input{Prompt: "check"}, Skills: []relay.SkillPackage{skill}}
	if _, err := service.Submit(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Claim(ctx, "old"); err == nil {
		t.Fatal("old node claimed skill task")
	}
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if !executed {
		t.Fatal("executor was not invoked")
	}
	// A filesystem installation failure must happen before the agent starts.
	cacheFile := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(cacheFile, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	worker.SkillCacheRoot = cacheFile
	executed = false
	request.IdempotencyKey = "turn-2"
	submitted, err := service.Submit(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	_, executionErr := worker.RunOnce(ctx)
	stored, err := service.GetRun(ctx, submitted.ID)
	if executionErr == nil || err != nil || stored.Status != relay.RunFailed || executed {
		t.Fatalf("installation failure run=%+v err=%v executed=%v", stored, executionErr, executed)
	}
}
