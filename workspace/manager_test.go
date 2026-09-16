package workspace_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/workspace"
)

func TestGitWorkspaceUsesMirrorAndCleansWorktree(t *testing.T) {
	source := t.TempDir()
	runGit(t, source, "init")
	runGit(t, source, "config", "user.email", "relay@example.test")
	runGit(t, source, "config", "user.name", "Relay Test")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("workspace\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "add", "README.md")
	runGit(t, source, "commit", "-m", "initial")
	manager := &workspace.Manager{Root: t.TempDir()}
	prepared, err := manager.Prepare(context.Background(), "run-1", "attempt-1", relay.WorkspaceSpec{Kind: "git", Source: source})
	if err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(prepared.Dir, "README.md")); err != nil || string(content) != "workspace\n" {
		t.Fatalf("prepared content = %q, %v", content, err)
	}
	if err := prepared.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(prepared.Dir); !os.IsNotExist(err) {
		t.Fatalf("worktree still exists after cleanup: %v", err)
	}
}

func TestWorkspaceRejectsEscapingSubdir(t *testing.T) {
	manager := &workspace.Manager{Root: t.TempDir()}
	_, err := manager.Prepare(context.Background(), "run", "attempt", relay.WorkspaceSpec{Kind: "temp", Subdir: "../escape"})
	if err == nil {
		t.Fatal("escaping subdir was accepted")
	}
}

func TestReusableGitWorkspacePersistsAcrossAttempts(t *testing.T) {
	source := gitSource(t)
	manager := &workspace.Manager{Root: t.TempDir()}
	spec := relay.WorkspaceSpec{
		Kind:      "git",
		Source:    source,
		Lifecycle: "reusable",
		ReuseKey:  "caller-owned-key",
		Branch:    "agent/session-1",
	}
	first, err := manager.Prepare(context.Background(), "run-1", "attempt-1", spec)
	if err != nil {
		t.Fatal(err)
	}
	change := filepath.Join(first.Dir, "change.txt")
	if err := os.WriteFile(change, []byte("preserved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := first.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, err := manager.Prepare(context.Background(), "run-2", "attempt-2", spec)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Cleanup(context.Background())
	if second.Dir != first.Dir {
		t.Fatalf("reusable dir = %q, want %q", second.Dir, first.Dir)
	}
	content, err := os.ReadFile(filepath.Join(second.Dir, "change.txt"))
	if err != nil || string(content) != "preserved\n" {
		t.Fatalf("preserved content = %q, %v", content, err)
	}
	command := exec.Command("git", "rev-parse", "--verify", "HEAD^{commit}")
	command.Dir = second.Dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("reusable workspace lost HEAD after refresh: %v: %s", err, output)
	}
}

func TestReusableGitWorkspaceRejectsMissingHead(t *testing.T) {
	source := gitSource(t)
	manager := &workspace.Manager{Root: t.TempDir()}
	spec := relay.WorkspaceSpec{
		Kind:      "git",
		Source:    source,
		Lifecycle: "reusable",
		ReuseKey:  "missing-head",
		Branch:    "agent/missing-head",
	}
	prepared, err := manager.Prepare(context.Background(), "run-1", "attempt-1", spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	runGit(t, prepared.Dir, "update-ref", "-d", "refs/heads/agent/missing-head")
	if _, err := manager.Prepare(context.Background(), "run-2", "attempt-2", spec); err == nil {
		t.Fatal("reusable workspace without HEAD was accepted")
	}
}

func TestReusableGitWorkspaceSerializesUsers(t *testing.T) {
	source := gitSource(t)
	manager := &workspace.Manager{Root: t.TempDir()}
	spec := relay.WorkspaceSpec{Kind: "git", Source: source, Lifecycle: "reusable", ReuseKey: "shared"}
	first, err := manager.Prepare(context.Background(), "run-1", "attempt-1", spec)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		second, err := manager.Prepare(context.Background(), "run-2", "attempt-2", spec)
		if err == nil {
			err = second.Cleanup(context.Background())
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("second prepare completed before release: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := first.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second prepare remained blocked after release")
	}
}

func TestReusableWorkspaceRequiresGitAndKey(t *testing.T) {
	manager := &workspace.Manager{Root: t.TempDir()}
	for _, spec := range []relay.WorkspaceSpec{
		{Kind: "git", Source: "repo", Lifecycle: "reusable"},
		{Kind: "local", Source: t.TempDir(), Lifecycle: "reusable", ReuseKey: "key"},
		{Kind: "git", Source: "repo", ReuseKey: "key"},
	} {
		if _, err := manager.Prepare(context.Background(), "run", "attempt", spec); err == nil {
			t.Fatalf("Prepare(%+v) unexpectedly succeeded", spec)
		}
	}
}

func TestReleaseReusableGitWorkspace(t *testing.T) {
	source := gitSource(t)
	manager := &workspace.Manager{Root: t.TempDir()}
	spec := relay.WorkspaceSpec{Kind: "git", Source: source, Lifecycle: "reusable", ReuseKey: "release", Branch: "agent/release"}
	prepared, err := manager.Prepare(context.Background(), "run", "attempt", spec)
	if err != nil {
		t.Fatal(err)
	}
	dir := prepared.Dir
	if err := prepared.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Release(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("released worktree still exists: %v", err)
	}
}

func TestReleasePreservesDirtyReusableGitWorkspace(t *testing.T) {
	source := gitSource(t)
	manager := &workspace.Manager{Root: t.TempDir()}
	spec := relay.WorkspaceSpec{Kind: "git", Source: source, Lifecycle: "reusable", ReuseKey: "dirty"}
	prepared, err := manager.Prepare(context.Background(), "run", "attempt", spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prepared.Dir, "pending.txt"), []byte("pending\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := prepared.Dir
	if err := prepared.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Release(context.Background(), spec); err == nil {
		t.Fatal("dirty reusable workspace was released")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dirty worktree was not preserved: %v", err)
	}
}

func gitSource(t *testing.T) string {
	t.Helper()
	source := t.TempDir()
	runGit(t, source, "init")
	runGit(t, source, "config", "user.email", "relay@example.test")
	runGit(t, source, "config", "user.name", "Relay Test")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("workspace\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "add", "README.md")
	runGit(t, source, "commit", "-m", "initial")
	return source
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}
