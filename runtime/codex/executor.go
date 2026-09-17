// Package codex adapts the stable `codex exec` non-interactive CLI to Relay.
package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/KDF5000/relay"
	runtimeprocess "github.com/KDF5000/relay/runtime/process"
	"github.com/KDF5000/relay/runtime/toolbridge"
)

type Config struct {
	Binary                string
	Protocol              string
	ToolDir               string
	PassEnv               []string
	Env                   map[string]string
	Model                 string
	Profile               string
	ReasoningEffort       string
	ServiceTier           string
	Sandbox               string
	AllowDangerousSandbox bool
	WorkRoot              string
	WorkDir               string
	Ephemeral             bool
	RequireGitRepository  bool
	PermissionMode        string
	AllowedTools          []string
	DisallowedTools       []string
	ShellToolTimeout      string
	IgnoreUserConfig      bool
	IgnoreRules           bool
	ExtraArgs             []string
}

type Executor struct{ Config Config }

func (e Executor) Execute(ctx context.Context, execution relay.Execution) (relay.Result, error) {
	return ExecuteFork(ctx, e.Config, execution, ForkOptions{Name: "codex", DefaultBinary: "codex", InstructionProvider: "codex"})
}

// ForkOptions describes a CLI that preserves the codex exec wire contract.
// It lets close forks share process supervision without pretending they are
// the same runtime at the event and artifact boundaries.
type ForkOptions struct {
	Name                string
	DefaultBinary       string
	InstructionProvider string
	PassEnv             []string
}

func ExecuteFork(ctx context.Context, config Config, execution relay.Execution, fork ForkOptions) (relay.Result, error) {
	if execution.Runtime.Model != "" {
		config.Model = execution.Runtime.Model
	}
	if fork.Name == "" {
		fork.Name = "codex"
	}
	if fork.DefaultBinary == "" {
		fork.DefaultBinary = fork.Name
	}
	if fork.InstructionProvider == "" {
		fork.InstructionProvider = "codex"
	}
	if config.Protocol == "app-server" {
		return executeAppServer(ctx, config, execution, fork)
	}
	if config.Protocol != "" && config.Protocol != "exec" {
		return relay.Result{}, fmt.Errorf("relay %s: unsupported protocol %q", fork.Name, config.Protocol)
	}
	binary := config.Binary
	if binary == "" {
		binary = fork.DefaultBinary
	}
	sandbox := config.Sandbox
	if sandbox == "" {
		sandbox = "workspace-write"
	}
	if sandbox != "read-only" && sandbox != "workspace-write" && sandbox != "danger-full-access" {
		return relay.Result{}, fmt.Errorf("relay %s: unsupported sandbox %q", fork.Name, sandbox)
	}
	if sandbox == "danger-full-access" && !config.AllowDangerousSandbox {
		return relay.Result{}, fmt.Errorf("relay %s: danger-full-access requires explicit AllowDangerousSandbox", fork.Name)
	}
	workDir := execution.WorkDir
	if workDir == "" {
		workDir = config.WorkDir
	}
	if workDir == "" {
		root := config.WorkRoot
		if root == "" {
			root = filepath.Join(os.TempDir(), "relay-runs")
		}
		workDir = filepath.Join(root, execution.RunID)
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return relay.Result{}, err
	}
	instructionPath, err := relay.MaterializeInstructions(workDir, fork.InstructionProvider, execution.Instructions)
	if err != nil {
		return relay.Result{}, err
	}
	resultDir := filepath.Join(workDir, ".relay")
	if err := os.MkdirAll(resultDir, 0o700); err != nil {
		return relay.Result{}, err
	}
	if err := resetArtifactManifest(workDir); err != nil {
		return relay.Result{}, err
	}
	imagePaths, cleanupImages, err := relay.MaterializeInputImages(workDir, execution.Input)
	if err != nil {
		return relay.Result{}, err
	}
	defer cleanupImages()
	finalPath := filepath.Join(resultDir, fork.Name+"-last-message.txt")

	bridge, err := toolbridge.StartFile(execution.Capabilities, filepath.Join(resultDir, "tool-bridge"))
	if err != nil {
		return relay.Result{}, fmt.Errorf("relay %s: start tool bridge: %w", fork.Name, err)
	}
	defer bridge.Close()
	resuming := execution.RuntimeSessionID != "" && !config.Ephemeral
	var args []string
	if resuming {
		args = []string{"exec", "resume", "--json", "--output-last-message", finalPath}
	} else {
		args = []string{"exec", "--json", "--color", "never", "--sandbox", sandbox, "--cd", workDir, "--output-last-message", finalPath}
		if config.Ephemeral {
			args = append(args, "--ephemeral")
		}
	}
	if !config.RequireGitRepository {
		args = append(args, "--skip-git-repo-check")
	}
	if config.Model != "" {
		args = append(args, "--model", config.Model)
	}
	if config.Profile != "" {
		if resuming {
			args = append([]string{"--profile", config.Profile}, args...)
		} else {
			args = append(args, "--profile", config.Profile)
		}
	}
	if config.ReasoningEffort != "" {
		args = append(args, "--config", "model_reasoning_effort="+tomlString(config.ReasoningEffort))
	}
	if config.ServiceTier != "" {
		args = append(args, "--config", "service_tier="+tomlString(config.ServiceTier))
	}
	if config.PermissionMode != "" {
		args = append(args, "--permission-mode", config.PermissionMode)
	}
	for _, tool := range config.AllowedTools {
		args = append(args, "--allowed-tool", tool)
	}
	for _, tool := range config.DisallowedTools {
		args = append(args, "--disallowed-tool", tool)
	}
	if config.ShellToolTimeout != "" {
		args = append(args, "--shell-tool-timeout", config.ShellToolTimeout)
	}
	if config.IgnoreUserConfig {
		args = append(args, "--ignore-user-config")
	}
	if config.IgnoreRules {
		args = append(args, "--ignore-rules")
	}
	args = append(args, config.ExtraArgs...)
	for _, imagePath := range imagePaths {
		args = append(args, "--image", imagePath)
	}
	if resuming {
		args = append(args, execution.RuntimeSessionID, "-")
	} else {
		args = append(args, "-")
	}

	command := exec.CommandContext(ctx, binary, args...)
	runtimeprocess.Configure(command)
	command.Dir = workDir
	command.Stdin = strings.NewReader(execution.Instructions.Prompt)
	command.Env = runtimeEnv(config, bridge.Dir, bridge.Token, fork.PassEnv)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return relay.Result{}, err
	}
	var stderr bytes.Buffer
	command.Stderr = &limitedBuffer{buffer: &stderr, remaining: 4 << 20}
	if execution.Emit != nil {
		execution.Emit(ctx, "runtime."+fork.Name+".started", map[string]any{"binary": binary, "work_dir": workDir, "sandbox": sandbox})
	}
	if err := command.Start(); err != nil {
		return relay.Result{}, fmt.Errorf("relay %s: start: %w", fork.Name, err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	threadID := ""
	lastAgentMessage := ""
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			if execution.Emit != nil {
				execution.Emit(ctx, "runtime."+fork.Name+".unparsed", map[string]string{"line": truncate(string(line), 4096)})
			}
			continue
		}
		if value, _ := event["thread_id"].(string); value != "" {
			threadID = value
		}
		eventType, _ := event["type"].(string)
		if eventType == "" {
			eventType = "event"
		}
		if execution.Emit != nil {
			execution.Emit(ctx, "runtime."+fork.Name+"."+eventType, event)
			if item, _ := event["item"].(map[string]any); item != nil && item["type"] == "agent_message" {
				if text, _ := item["text"].(string); text != "" {
					lastAgentMessage = text
					execution.Emit(ctx, "assistant.message.completed", map[string]string{"text": text})
				}
			}
		}
	}
	scanErr := scanner.Err()
	waitErr := command.Wait()
	if scanErr != nil {
		return relay.Result{}, fmt.Errorf("relay %s: read JSONL: %w", fork.Name, scanErr)
	}
	if waitErr != nil {
		if resuming && execution.FallbackPrompt != "" && ctx.Err() == nil && resumeUnavailable(stderr.String()) {
			if execution.Emit != nil {
				execution.Emit(ctx, "runtime."+fork.Name+".thread.resume_failed", map[string]string{"thread_id": execution.RuntimeSessionID, "error": waitErr.Error()})
			}
			recovery := execution
			recovery.RuntimeSessionID = ""
			recovery.Instructions.Prompt = execution.FallbackPrompt
			return ExecuteFork(ctx, config, recovery, fork)
		}
		return relay.Result{}, fmt.Errorf("relay %s: process failed: %w: %s", fork.Name, waitErr, truncate(stderr.String(), 8192))
	}
	message, err := os.ReadFile(finalPath)
	if err != nil {
		return relay.Result{}, fmt.Errorf("relay %s: read final message: %w", fork.Name, err)
	}
	finalMessage := strings.TrimSpace(lastAgentMessage)
	if finalMessage == "" {
		finalMessage = strings.TrimSpace(string(message))
	}
	if err := os.WriteFile(finalPath, []byte(finalMessage), 0o600); err != nil {
		return relay.Result{}, fmt.Errorf("relay %s: normalize final message: %w", fork.Name, err)
	}
	output, _ := json.Marshal(map[string]any{"thread_id": threadID, "message": finalMessage, "work_dir": workDir})
	if execution.Emit != nil {
		execution.Emit(ctx, "assistant.final.completed", map[string]string{"text": finalMessage})
		execution.Emit(ctx, "runtime."+fork.Name+".completed", map[string]string{"thread_id": threadID})
	}
	artifacts := []relay.Artifact{{Type: "instruction_file", Ref: instructionPath, Name: filepath.Base(instructionPath)}, {Type: fork.Name + "_final_message", Ref: finalPath, Name: filepath.Base(finalPath)}}
	declared, err := collectDeclaredArtifacts(workDir)
	if err != nil {
		return relay.Result{}, fmt.Errorf("relay %s: collect artifacts: %w", fork.Name, err)
	}
	artifacts = append(artifacts, declared...)
	result := relay.Result{Summary: finalMessage, Output: output, Artifacts: artifacts}
	if !config.Ephemeral {
		result.RuntimeSessionID = threadID
	}
	return result, nil
}

func resumeUnavailable(stderr string) bool {
	value := strings.ToLower(stderr)
	for _, fragment := range []string{"no rollout found", "thread not found", "session not found", "no saved session", "unknown session", "failed to load thread"} {
		if strings.Contains(value, fragment) {
			return true
		}
	}
	return false
}

func runtimeEnv(config Config, bridgeDir, toolToken string, forkEnv []string) []string {
	allowed := []string{
		"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TMP", "TEMP",
		"LANG", "LC_ALL", "TERM", "COLORTERM",
		"CODEX_HOME", "OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_ORGANIZATION", "OPENAI_PROJECT",
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS",
		"SystemRoot", "ComSpec", "PATHEXT",
	}
	allowed = append(allowed, forkEnv...)
	allowed = append(allowed, config.PassEnv...)
	environment := make([]string, 0, len(allowed)+len(config.Env)+3)
	seen := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		if value, ok := os.LookupEnv(name); ok {
			environment = append(environment, name+"="+value)
		}
	}
	for name, value := range config.Env {
		environment = setEnv(environment, name, value)
	}
	if config.ToolDir != "" {
		toolDir := config.ToolDir
		absolute, err := filepath.Abs(toolDir)
		if err == nil {
			toolDir = absolute
		}
		path := os.Getenv("PATH")
		environment = setEnv(environment, "PATH", toolDir+string(os.PathListSeparator)+path)
	}
	environment = setEnv(environment, "RELAY_TOOL_DIR", bridgeDir)
	environment = setEnv(environment, "RELAY_TOOL_TOKEN", toolToken)
	return environment
}

func setEnv(environment []string, name, value string) []string {
	prefix := name + "="
	for index, entry := range environment {
		separator := strings.IndexByte(entry, '=')
		if separator >= 0 && strings.EqualFold(entry[:separator], name) {
			environment[index] = prefix + value
			return environment
		}
	}
	return append(environment, prefix+value)
}

func ProbeVersion(ctx context.Context, binary string) (string, error) {
	if binary == "" {
		binary = "codex"
	}
	output, err := exec.CommandContext(ctx, binary, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("relay codex: probe version: %w: %s", err, output)
	}
	return ParseVersionOutput(string(output)), nil
}

var semanticVersionPattern = regexp.MustCompile(`\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?`)

func ParseVersionOutput(output string) string {
	if value := semanticVersionPattern.FindString(output); value != "" {
		return value
	}
	return strings.TrimSpace(output)
}
func tomlString(value string) string { encoded, _ := json.Marshal(value); return string(encoded) }
func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

type limitedBuffer struct {
	buffer    *bytes.Buffer
	remaining int
}

func (w *limitedBuffer) Write(value []byte) (int, error) {
	if len(value) > w.remaining {
		return 0, errors.New("relay codex: stderr limit exceeded")
	}
	n, err := w.buffer.Write(value)
	w.remaining -= n
	return n, err
}
