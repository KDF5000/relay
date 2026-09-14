package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCollectDeclaredArtifacts(t *testing.T) {
	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, ".relay"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "report.md"), []byte("# Report"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := `{"artifacts":[{"path":"report.md","type":"report","name":"research.md"}]}`
	if err := os.WriteFile(filepath.Join(workDir, artifactManifestPath), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	artifacts, err := collectDeclaredArtifacts(workDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 || artifacts[0].Name != "research.md" || artifacts[0].Type != "report" || !strings.HasPrefix(artifacts[0].ContentType, "text/markdown") {
		t.Fatalf("artifacts=%+v", artifacts)
	}
}

func TestResetArtifactManifest(t *testing.T) {
	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, ".relay"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, artifactManifestPath), []byte(`{"artifacts":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := resetArtifactManifest(workDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workDir, artifactManifestPath)); !os.IsNotExist(err) {
		t.Fatalf("manifest still exists: %v", err)
	}
}

func TestCollectDeclaredArtifactsRejectsEscape(t *testing.T) {
	parent := t.TempDir()
	workDir := filepath.Join(parent, "work")
	if err := os.MkdirAll(filepath.Join(workDir, ".relay"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := `{"artifacts":[{"path":"../secret.txt","type":"report"}]}`
	if err := os.WriteFile(filepath.Join(workDir, artifactManifestPath), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := collectDeclaredArtifacts(workDir); err == nil {
		t.Fatal("expected escaping artifact path to fail")
	}
}

func TestCollectDeclaredArtifactsRejectsInternalFile(t *testing.T) {
	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, ".relay"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, ".relay", "internal.txt"), []byte("internal"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := `{"artifacts":[{"path":".relay/internal.txt","type":"report"}]}`
	if err := os.WriteFile(filepath.Join(workDir, artifactManifestPath), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := collectDeclaredArtifacts(workDir); err == nil {
		t.Fatal("expected internal artifact path to fail")
	}
}
