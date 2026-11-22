package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigMergeIsLocalAndDoesNotExposeCredentials(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "node.json")
	discovered := filepath.Join(dir, "discovered.json")
	if err := os.WriteFile(config, []byte(`{"token":"must-not-be-printed","node":{"id":"kept"},"runtimes":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(discovered, []byte(`[{"provider":"antigravity","kind":"antigravity","command":"/bin/agy"}]`), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"--server", "http://127.0.0.1:1", "config", "merge-runtimes", "--config", config, "--discovered", discovered}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String()+stderr.String(), "must-not-be-printed") {
		t.Fatal("printed credentials")
	}
	if !strings.Contains(stdout.String(), "Added Runtimes: antigravity") {
		t.Fatalf("output=%s", stdout.String())
	}
	raw, err := os.ReadFile(config)
	if err != nil || !bytes.Contains(raw, []byte("kept/antigravity")) {
		t.Fatalf("configuration=%s %v", raw, err)
	}
	stdout.Reset()
	if err := run(context.Background(), []string{"config", "merge-runtimes", "--config", config, "--discovered", discovered}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "no new Runtimes") {
		t.Fatalf("output=%s", stdout.String())
	}
}
