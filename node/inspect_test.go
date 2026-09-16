package node

import (
	"context"
	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/controlplane"
	"github.com/KDF5000/relay/transport/httpapi"
	"github.com/KDF5000/relay/workspace"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceInspectionHTTPChain(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "README.md")
	if err := os.WriteFile(file, []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init"}, {"add", "README.md"}, {"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial"}} {
		command := exec.Command("git", args...)
		command.Dir = root
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", output, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	service := controlplane.New(time.Second)
	server := httptest.NewServer(httpapi.NewHandler(service))
	defer server.Close()
	client := httpapi.NewClient(server.URL)
	worker := &Worker{Registration: controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "inspection-node", Capacity: 1, Runtimes: []controlplane.Runtime{{Provider: "test"}}}, ControlPlane: client, Workspaces: &workspace.Manager{Root: t.TempDir()}, Executors: ExecutorMap{"test": relay.ExecutorFunc(func(context.Context, relay.Execution) (relay.Result, error) {
		return relay.Result{Summary: "changed"}, os.WriteFile(file, []byte("after\n"), 0600)
	})}}
	if _, err := worker.Register(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := client.Submit(ctx, relay.Request{AgentID: "test", IdempotencyKey: "inspect", Runtime: relay.RuntimeRequirement{Provider: "test"}, Input: relay.Input{Prompt: "change"}, Workspace: relay.WorkspaceSpec{Kind: "local", Source: root}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// Simulate a restart: recover the run-to-workspace mapping from disk.
	worker.inspectionDirs.Clear()
	go worker.serveInspections(ctx)
	listed, err := client.Inspect(ctx, run.ID, "list", "")
	if err != nil || len(listed.Entries) != 1 {
		t.Fatalf("list: %+v %v", listed, err)
	}
	read, err := client.Inspect(ctx, run.ID, "read", "README.md")
	if err != nil || read.Content != "after\n" {
		t.Fatalf("read: %+v %v", read, err)
	}
	diff, err := client.Inspect(ctx, run.ID, "diff", "")
	if err != nil || !strings.Contains(diff.Content, "+after") {
		t.Fatalf("diff: %+v %v", diff, err)
	}
	if _, err = client.Inspect(ctx, run.ID, "read", "../outside"); err == nil {
		t.Fatal("traversal accepted")
	}
	outside := filepath.Join(t.TempDir(), "secret")
	_ = os.WriteFile(outside, []byte("secret"), 0600)
	if err = os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Inspect(ctx, run.ID, "read", "escape"); err == nil {
		t.Fatal("symlink escape accepted")
	}
}
