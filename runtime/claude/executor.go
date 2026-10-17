// Package claude adapts Claude Code's headless stream-json protocol to Relay.
package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/runtime/agentcli"
	runtimecodex "github.com/KDF5000/relay/runtime/codex"
	runtimeprocess "github.com/KDF5000/relay/runtime/process"
)

type Config = agentcli.Config
type Executor struct{ Config Config }

func (e Executor) Execute(ctx context.Context, execution relay.Execution) (relay.Result, error) {
	config := e.Config
	if execution.Runtime.Model != "" {
		config.Model = execution.Runtime.Model
	}
	run, err := agentcli.Prepare(execution, config, "claude")
	if err != nil {
		return relay.Result{}, err
	}
	defer run.Close()
	binary := config.Binary
	if binary == "" {
		binary = "claude"
	}
	resuming := execution.RuntimeSessionID != "" && !config.Ephemeral
	args := []string{"--print", "--output-format", "stream-json", "--verbose"}
	if resuming {
		args = append(args, "--resume", execution.RuntimeSessionID)
	} else if config.Ephemeral {
		args = append(args, "--no-session-persistence")
	}
	if config.Model != "" {
		args = append(args, "--model", config.Model)
	}
	if len(config.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(config.AllowedTools, ","))
	}
	if len(config.DisallowedTools) > 0 {
		args = append(args, "--disallowedTools", strings.Join(config.DisallowedTools, ","))
	}
	args = append(args, config.ExtraArgs...)
	prompt := execution.Instructions.Prompt
	if len(run.ImagePaths) > 0 {
		prompt += "\nInput images:\n@" + strings.Join(run.ImagePaths, "\n@") + "\n"
	}
	command := exec.CommandContext(ctx, binary, args...)
	runtimeprocess.Configure(command)
	command.Dir = run.WorkDir
	command.Stdin = strings.NewReader(prompt)
	command.Env = agentcli.Environment(config, run, "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_OAUTH_TOKEN")
	stdout, err := command.StdoutPipe()
	if err != nil {
		return relay.Result{}, err
	}
	stderr := &agentcli.LimitedBuffer{Remaining: 4 << 20}
	command.Stderr = stderr
	if execution.Emit != nil {
		execution.Emit(ctx, "runtime.claude.started", map[string]any{"binary": binary, "work_dir": run.WorkDir})
	}
	if err := command.Start(); err != nil {
		return relay.Result{}, fmt.Errorf("relay claude: start: %w", err)
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	var sessionID, finalMessage, protocolError string
	sawResult := false
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			if execution.Emit != nil {
				execution.Emit(ctx, "runtime.claude.unparsed", map[string]string{"line": agentcli.Truncate(string(line), 4096)})
			}
			continue
		}
		if value, _ := event["session_id"].(string); value != "" {
			sessionID = value
		}
		typeName, _ := event["type"].(string)
		if typeName == "" {
			typeName = "event"
		}
		if execution.Emit != nil {
			execution.Emit(ctx, "runtime.claude."+typeName, event)
		}
		if typeName == "assistant" {
			if text := claudeMessageText(event["message"]); text != "" {
				finalMessage = text
				if execution.Emit != nil {
					execution.Emit(ctx, "assistant.message.completed", map[string]string{"text": text})
				}
			}
		}
		if typeName == "result" {
			sawResult = true
			if value, _ := event["result"].(string); value != "" {
				finalMessage = value
			}
			if failed, _ := event["is_error"].(bool); failed {
				protocolError = finalMessage
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return relay.Result{}, fmt.Errorf("relay claude: read JSONL: %w", err)
	}
	waitErr := command.Wait()
	if waitErr != nil {
		if resuming && execution.FallbackPrompt != "" && ctx.Err() == nil && agentcli.ResumeUnavailable(stderr.String()) {
			if execution.Emit != nil {
				execution.Emit(ctx, "runtime.claude.session.resume_failed", map[string]string{"session_id": execution.RuntimeSessionID, "error": waitErr.Error()})
			}
			recovery := execution
			recovery.RuntimeSessionID = ""
			recovery.Instructions.Prompt = execution.FallbackPrompt
			return e.Execute(ctx, recovery)
		}
		return relay.Result{}, fmt.Errorf("relay claude: process failed: %w: %s", waitErr, agentcli.Truncate(stderr.String(), 8192))
	}
	if protocolError != "" {
		return relay.Result{}, fmt.Errorf("relay claude: runtime error: %s", protocolError)
	}
	if !sawResult {
		return relay.Result{}, fmt.Errorf("relay claude: stream ended before the result event")
	}
	finalMessage = strings.TrimSpace(finalMessage)
	if finalMessage == "" {
		return relay.Result{}, fmt.Errorf("relay claude: stream completed without a final message")
	}
	artifacts, err := run.Artifacts(finalMessage)
	if err != nil {
		return relay.Result{}, fmt.Errorf("relay claude: collect artifacts: %w", err)
	}
	if execution.Emit != nil {
		execution.Emit(ctx, "assistant.final.completed", map[string]string{"text": finalMessage})
		execution.Emit(ctx, "runtime.claude.completed", map[string]string{"session_id": sessionID})
	}
	output, _ := json.Marshal(map[string]string{"session_id": sessionID, "message": finalMessage, "work_dir": run.WorkDir})
	result := relay.Result{Summary: finalMessage, Output: output, Artifacts: artifacts}
	if !config.Ephemeral {
		result.RuntimeSessionID = sessionID
	}
	return result, nil
}

func claudeMessageText(value any) string {
	message, _ := value.(map[string]any)
	content, _ := message["content"].([]any)
	var parts []string
	for _, item := range content {
		block, _ := item.(map[string]any)
		if block["type"] == "text" {
			if text, _ := block["text"].(string); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, ""))
}

func ProbeVersion(ctx context.Context, binary string) (string, error) {
	if binary == "" {
		binary = "claude"
	}
	output, err := exec.CommandContext(ctx, binary, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("relay claude: probe version: %w: %s", err, output)
	}
	return runtimecodex.ParseVersionOutput(string(output)), nil
}
