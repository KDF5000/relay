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
	worker := &Worker{Registration: controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "inspection-node", Capacity: 1, Runtimes: []controlplane.Runtime{{Provider: "test"}}}, ControlPlane: client, Workspaces: &workspace.Manager{Root: t.TempDir()}}
	if _, err := worker.Register(ctx); err != nil {
		t.Fatal(err)
	}
	go worker.serveInspections(ctx)
	workspaceSpec := relay.WorkspaceSpec{Kind: "local", Source: root}
	listed, err := client.InspectNodeWorkspace(ctx, worker.Registration.ID, workspaceSpec, "list", "")
	if err != nil || len(listed.Entries) != 1 {
		t.Fatalf("list: %+v %v", listed, err)
	}
	read, err := client.InspectNodeWorkspace(ctx, worker.Registration.ID, workspaceSpec, "read", "README.md")
	if err != nil || read.Content != "before\n" {
		t.Fatalf("read: %+v %v", read, err)
	}
	if err := os.WriteFile(file, []byte("after\n"), 0600); err != nil {
		t.Fatal(err)
	}
	diff, err := client.InspectNodeWorkspace(ctx, worker.Registration.ID, workspaceSpec, "diff", "")
	if err != nil || !strings.Contains(diff.Content, "+after") {
		t.Fatalf("diff: %+v %v", diff, err)
	}
	gitStatus, err := client.InspectNodeWorkspace(ctx, worker.Registration.ID, workspaceSpec, "git-status", "")
	if err != nil || gitStatus.Git == nil || !gitStatus.Git.Repository || gitStatus.Git.Branch == "" || gitStatus.Git.Detached || len(gitStatus.Git.Commit) != 7 {
		t.Fatalf("git-status: %+v %v", gitStatus, err)
	}
	if _, err = client.InspectNodeWorkspace(ctx, worker.Registration.ID, workspaceSpec, "read", "../outside"); err == nil {
		t.Fatal("traversal accepted")
	}
	outside := filepath.Join(t.TempDir(), "secret")
	_ = os.WriteFile(outside, []byte("secret"), 0600)
	if err = os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err = client.InspectNodeWorkspace(ctx, worker.Registration.ID, workspaceSpec, "read", "escape"); err == nil {
		t.Fatal("symlink escape accepted")
	}
}

func TestInspectGitStatusForNonRepositoryAndDetachedHead(t *testing.T) {
	ctx := context.Background()
	plain := t.TempDir()
	result, err := inspectDirectory(ctx, plain, "git-status", "")
	if err != nil || result.Git == nil || result.Git.Repository {
		t.Fatalf("plain directory: %+v %v", result, err)
	}

	repository := t.TempDir()
	if err = os.WriteFile(filepath.Join(repository, "README.md"), []byte("test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init"}, {"add", "README.md"}, {"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial"}, {"checkout", "--detach"}} {
		command := exec.Command("git", args...)
		command.Dir = repository
		if output, commandErr := command.CombinedOutput(); commandErr != nil {
			t.Fatalf("git: %s %v", output, commandErr)
		}
	}
	result, err = inspectDirectory(ctx, repository, "git-status", "")
	if err != nil || result.Git == nil || !result.Git.Repository || !result.Git.Detached || result.Git.Branch != "" || len(result.Git.Commit) != 7 {
		t.Fatalf("detached repository: %+v %v", result, err)
	}
}

func TestInspectDiffIncludesSubmoduleChanges(t *testing.T) {
	ctx := context.Background()
	submodule := t.TempDir()
	if err := os.WriteFile(filepath.Join(submodule, "file.txt"), []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, submodule, "init")
	gitCommand(t, submodule, "add", "file.txt")
	gitCommand(t, submodule, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")

	repository := t.TempDir()
	gitCommand(t, repository, "init")
	gitCommand(t, repository, "-c", "protocol.file.allow=always", "submodule", "add", submodule, "vendor/module")
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
	if err := os.WriteFile(filepath.Join(repository, "vendor/module/file.txt"), []byte("after\n"), 0600); err != nil {
		t.Fatal(err)
	}

	result, err := inspectDirectory(ctx, repository, "diff", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "diff --git a/vendor/module/file.txt b/vendor/module/file.txt") || !strings.Contains(result.Content, "+after") {
		t.Fatalf("submodule file diff missing: %s", result.Content)
	}
}

func gitCommand(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s %v", args, output, err)
	}
}
