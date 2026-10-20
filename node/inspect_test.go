package node

import (
	"bytes"
	"context"
	"encoding/base64"
	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/controlplane"
	"github.com/KDF5000/relay/transport/httpapi"
	"github.com/KDF5000/relay/workspace"
	"image"
	"image/png"
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
	var imageData bytes.Buffer
	if err := png.Encode(&imageData, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "diagram.png"), imageData.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	preview, err := client.InspectNodeWorkspace(ctx, worker.Registration.ID, workspaceSpec, "read", "diagram.png")
	decoded, decodeErr := base64.StdEncoding.DecodeString(preview.Content)
	if err != nil || preview.Encoding != "base64" || preview.ContentType != "image/png" || decodeErr != nil || !bytes.Equal(decoded, imageData.Bytes()) {
		t.Fatalf("image preview: encoding=%q type=%q error=%v decode=%v", preview.Encoding, preview.ContentType, err, decodeErr)
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

func TestWorkspaceImageReadLimitsAndBoundaries(t *testing.T) {
	root := t.TempDir()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	oversized := append(data.Bytes(), make([]byte, 2<<20)...)
	if err := os.WriteFile(filepath.Join(root, "large.png"), oversized, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectDirectory(context.Background(), root, "read", "large.png"); err == nil || !strings.Contains(err.Error(), "image exceeds") {
		t.Fatalf("large image: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside.png")
	if err := os.WriteFile(outside, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape.png")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"escape.png", "../outside.png", outside} {
		if _, err := inspectDirectory(context.Background(), root, "read", path); err == nil {
			t.Fatalf("outside image accepted: %s", path)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "fake.png"), []byte("text, not an image"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := inspectDirectory(context.Background(), root, "read", "fake.png")
	if err != nil || result.Encoding != "" || result.Content != "text, not an image" {
		t.Fatalf("fake extension: %+v %v", result, err)
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
