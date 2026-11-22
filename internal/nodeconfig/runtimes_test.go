package nodeconfig

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMergePreservesIdentityAndSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.json")
	original := []byte(`{"server":"https://relay.example","token":"secret","node":{"id":"original-node","capacity":7,"labels":{"name":"machine"}},"workspace_root":"/custom/workspaces","skill_cache_root":"/custom/skills","bindings":[{"name":"custom"}],"extra":{"future":true},"runtimes":[{"id":"original-node/custom-trae","provider":"trae","kind":"trae","command":"/custom/traex","work_root":"/custom/runs","env":{"HTTP_PROXY":"keep-me"},"model":"selected-model","args":["custom"]},{"id":"original-node/legacy-agy","provider":"agy","kind":"agy","command":"/custom/agy"}]}`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	result, err := MergeRuntimes(path, []byte(`[{"id":"wrong-node/trae","provider":"trae","kind":"trae","command":"/new/traex"},{"id":"wrong-node/antigravity","provider":"antigravity","kind":"antigravity","command":"/new/agy"},{"id":"wrong-node/codex","provider":"codex","kind":"codex","command":"/bin/codex","work_root":"/new/runs","ephemeral":false},{"provider":"codex","command":"/duplicate/codex"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Added) != 1 || result.Added[0] != "codex" {
		t.Fatalf("result=%+v", result)
	}
	backup, err := os.ReadFile(result.BackupPath)
	if err != nil || !bytes.Equal(backup, original) {
		t.Fatalf("backup altered: %v", err)
	}
	var before, after map[string]any
	if err := json.Unmarshal(original, &before); err != nil {
		t.Fatal(err)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(updated, &after); err != nil {
		t.Fatal(err)
	}
	runtimes := after["runtimes"].([]any)
	if len(runtimes) != 3 {
		t.Fatal("duplicated existing provider/alias")
	}
	for i, old := range before["runtimes"].([]any) {
		oldJSON, _ := json.Marshal(old)
		newJSON, _ := json.Marshal(runtimes[i])
		if !bytes.Equal(oldJSON, newJSON) {
			t.Fatal("overwrote existing Runtime")
		}
	}
	added := runtimes[2].(map[string]any)
	if added["id"] != "original-node/codex" || added["work_root"] != "/custom/runs" {
		t.Fatalf("new runtime=%v", added)
	}
	before["runtimes"] = runtimes
	expected, _ := json.Marshal(before)
	actual, _ := json.Marshal(after)
	if !bytes.Equal(expected, actual) {
		t.Fatal("changed unrelated configuration")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("permissions changed")
	}
	statBefore := info
	again, err := MergeRuntimes(path, []byte(`[{"provider":"codex","command":"/different/path"},{"provider":"antigravity","command":"/new/agy"}]`))
	if err != nil || len(again.Added) != 0 || again.BackupPath != "" {
		t.Fatalf("no-op=%+v %v", again, err)
	}
	unchanged, _ := os.ReadFile(path)
	statAfter, _ := os.Stat(path)
	if !bytes.Equal(unchanged, updated) || !os.SameFile(statBefore, statAfter) || !statBefore.ModTime().Equal(statAfter.ModTime()) {
		t.Fatal("no-op rewrote configuration")
	}
}

func TestMergeAddsAntigravityToOlderNode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.json")
	if err := os.WriteFile(path, []byte(`{"node":{"id":"node-before-upgrade"},"workspace_root":"/node-data/workspaces","runtimes":[{"provider":"trae","command":"/bin/traex"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := MergeRuntimes(path, []byte(`[{"id":"new-random-id/antigravity","provider":"antigravity","kind":"antigravity","command":"/bin/agy","work_root":"/default/runs"}]`))
	if err != nil || len(result.Added) != 1 || result.Added[0] != "antigravity" {
		t.Fatalf("%+v %v", result, err)
	}
	var c map[string]json.RawMessage
	raw, _ := os.ReadFile(path)
	_ = json.Unmarshal(raw, &c)
	var runtimes []map[string]json.RawMessage
	_ = json.Unmarshal(c["runtimes"], &runtimes)
	if field(runtimes[1], "id") != "node-before-upgrade/antigravity" || field(runtimes[1], "work_root") != "/node-data/runs" {
		t.Fatal("did not reuse existing Node identity and data directory")
	}
}

func TestMergeRejectsMalformedInputWithoutChangingConfig(t *testing.T) {
	for _, data := range []string{`{`, `{"node":{}}`, `{"node":{"id":"node"},"runtimes":{}}`, `{"node":{"id":"node"},"runtimes":[null]}`} {
		t.Run(data, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "node.json")
			_ = os.WriteFile(path, []byte(data), 0600)
			if _, err := MergeRuntimes(path, []byte(`[]`)); err == nil {
				t.Fatal("accepted malformed config")
			}
			raw, _ := os.ReadFile(path)
			if string(raw) != data {
				t.Fatal("changed rejected configuration")
			}
		})
	}
	path := filepath.Join(t.TempDir(), "node.json")
	original := []byte(`{"node":{"id":"node"}}`)
	_ = os.WriteFile(path, original, 0600)
	if _, err := MergeRuntimes(path, []byte(`[{"provider":"antigravity"}]`)); err == nil {
		t.Fatal("accepted missing command")
	}
	raw, _ := os.ReadFile(path)
	if !bytes.Equal(raw, original) {
		t.Fatal("changed config after invalid discovery")
	}
}
