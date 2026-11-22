// Package nodeconfig updates local Node configuration without replacing its
// identity, credentials, or caller-owned settings.
package nodeconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type MergeResult struct {
	Added      []string
	BackupPath string
}

func field(object map[string]json.RawMessage, key string) string {
	var value string
	_ = json.Unmarshal(object[key], &value)
	return value
}

func canonicalProvider(value string) string {
	switch value = strings.ToLower(strings.TrimSpace(value)); value {
	case "agy":
		return "antigravity"
	case "traex", "trae-cli":
		return "trae"
	case "claude-code":
		return "claude"
	case "qodercli":
		return "qoder"
	case "qoderclicn":
		return "qwenwork"
	default:
		return value
	}
}

// MergeRuntimes appends only newly discovered providers. Existing objects are
// preserved, including custom fields and explicit runtime IDs. A no-op does not
// rewrite or back up the configuration.
func MergeRuntimes(configPath string, discovered []byte) (MergeResult, error) {
	resolved, err := filepath.EvalSymlinks(configPath)
	if err != nil {
		return MergeResult{}, err
	}
	original, err := os.ReadFile(resolved)
	if err != nil {
		return MergeResult{}, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return MergeResult{}, err
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(original, &config); err != nil || config == nil {
		return MergeResult{}, errors.New("invalid Node configuration JSON")
	}
	var node map[string]json.RawMessage
	if err := json.Unmarshal(config["node"], &node); err != nil {
		return MergeResult{}, errors.New("Node configuration requires node.id")
	}
	nodeID := field(node, "id")
	if strings.TrimSpace(nodeID) == "" {
		return MergeResult{}, errors.New("Node configuration requires node.id")
	}
	var existing []map[string]json.RawMessage
	if raw := config["runtimes"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &existing); err != nil {
			return MergeResult{}, errors.New("invalid configured runtimes")
		}
	}
	var candidates []map[string]json.RawMessage
	if err := json.Unmarshal(discovered, &candidates); err != nil {
		return MergeResult{}, errors.New("invalid discovered runtimes")
	}
	seen := map[string]bool{}
	workRoot := ""
	for _, runtime := range existing {
		if runtime == nil {
			return MergeResult{}, errors.New("invalid configured runtime object")
		}
		for _, key := range []string{"provider", "kind"} {
			if value := canonicalProvider(field(runtime, key)); value != "" {
				seen[value] = true
			}
		}
		if workRoot == "" {
			workRoot = field(runtime, "work_root")
		}
	}
	if workRoot == "" {
		if root := field(config, "workspace_root"); root != "" {
			workRoot = filepath.Join(filepath.Dir(root), "runs")
		}
	}
	result := MergeResult{}
	for _, runtime := range candidates {
		provider := canonicalProvider(field(runtime, "provider"))
		if runtime == nil || provider == "" || field(runtime, "command") == "" {
			return MergeResult{}, errors.New("discovered runtime requires provider and command")
		}
		if seen[provider] {
			continue
		}
		runtime["id"], _ = json.Marshal(nodeID + "/" + provider)
		runtime["provider"], _ = json.Marshal(provider)
		if workRoot != "" {
			runtime["work_root"], _ = json.Marshal(workRoot)
		}
		existing = append(existing, runtime)
		seen[provider] = true
		result.Added = append(result.Added, provider)
	}
	if len(result.Added) == 0 {
		return result, nil
	}
	config["runtimes"], err = json.Marshal(existing)
	if err != nil {
		return MergeResult{}, err
	}
	updated, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return MergeResult{}, err
	}
	updated = append(updated, '\n')
	// Keep concurrent edits from being overwritten during discovery.
	latest, err := os.ReadFile(resolved)
	if err != nil {
		return MergeResult{}, err
	}
	if !bytes.Equal(latest, original) {
		return MergeResult{}, errors.New("Node configuration changed during discovery; retry the upgrade")
	}
	backup := configPath + ".backup." + time.Now().Format("20060102150405.000000000")
	file, err := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return MergeResult{}, err
	}
	_, writeErr := file.Write(original)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(backup)
		return MergeResult{}, err
	}
	temp, err := os.CreateTemp(filepath.Dir(resolved), ".relay-node-*")
	if err != nil {
		return MergeResult{}, err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(updated); err != nil {
		_ = temp.Close()
		return MergeResult{}, err
	}
	if err := temp.Chmod(info.Mode().Perm()); err != nil {
		_ = temp.Close()
		return MergeResult{}, err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return MergeResult{}, err
	}
	if err := temp.Close(); err != nil {
		return MergeResult{}, err
	}
	if err := os.Rename(temp.Name(), resolved); err != nil {
		return MergeResult{}, fmt.Errorf("publish Node configuration: %w", err)
	}
	result.BackupPath = backup
	return result, nil
}
