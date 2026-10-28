// Package opencode adapts OpenCode's non-interactive JSON event stream to Relay.
package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
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
	// OpenCode owns permission policy. These CLI-specific fields cannot be
	// translated without overriding that policy, so reject them explicitly.
	if len(config.AllowedTools) > 0 || len(config.DisallowedTools) > 0 {
		return relay.Result{}, errors.New("relay opencode: configure tool permissions in OpenCode; allowed_tools and disallowed_tools are unsupported")
	}
	run, err := agentcli.Prepare(execution, config, "opencode")
	if err != nil {
		return relay.Result{}, err
	}
	defer run.Close()
	binary := resolveBinary(config.Binary)
	version, err := ProbeVersion(ctx, binary)
	if err != nil {
		return relay.Result{}, err
	}
	args := []string{"run", "--format", "json"}
	if isV2(version) {
		// A shared background server outlives the CLI and would escape Relay's
		// cancellation and run-scoped Tool Bridge environment.
		args = append(args, "--standalone")
	}
	resuming := execution.RuntimeSessionID != "" && !config.Ephemeral
	if resuming {
		// V2 silently creates an empty session when --session is unknown. Check
		// first so a lost session recovers with Relay's full conversation prompt.
		if isV2(version) {
			missing, err := sessionMissing(ctx, binary, config, run, execution.RuntimeSessionID)
			if err != nil {
				return relay.Result{}, err
			}
			if missing {
				if execution.FallbackPrompt == "" {
					return relay.Result{}, errors.New("relay opencode: session not found and no fallback prompt available")
				}
				return e.recoverSession(ctx, execution, "session not found")
			}
		}
		args = append(args, "--session", execution.RuntimeSessionID)
	}
	if config.Model != "" {
		args = append(args, "--model", config.Model)
	}
	for _, path := range run.ImagePaths {
		args = append(args, "--file", path)
	}
	args = append(args, config.ExtraArgs...)
	processCtx, stop := context.WithCancel(ctx)
	defer stop()
	command := exec.CommandContext(processCtx, binary, args...)
	runtimeprocess.Configure(command)
	command.Dir = run.WorkDir
	command.Env = environment(config, run)
	// Stdin avoids argument length limits and flag interpretation of prompts.
	command.Stdin = strings.NewReader(execution.Instructions.Prompt)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return relay.Result{}, err
	}
	stderr := &agentcli.LimitedBuffer{Remaining: 4 << 20}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return relay.Result{}, fmt.Errorf("relay opencode: start: %w", err)
	}
	// Always reap the child, including malformed or truncated stream failures.
	reaped := false
	defer func() {
		stop()
		if !reaped {
			_ = command.Wait()
		}
	}()
	emit := func(name string, payload any) {
		if execution.Emit != nil {
			execution.Emit(ctx, name, payload)
		}
	}
	emit("runtime.opencode.started", map[string]string{"binary": binary, "work_dir": run.WorkDir})
	decoder := json.NewDecoder(stdout)
	sessionID := ""
	messageID := ""
	var text strings.Builder
	var protocolError, finishReason string
	sawFinish := false
	for {
		var event map[string]any
		if err := decoder.Decode(&event); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return relay.Result{}, fmt.Errorf("relay opencode: read JSON stream: %w", err)
		}
		if id, _ := event["sessionID"].(string); id != "" {
			sessionID = id
		}
		typeName, _ := event["type"].(string)
		part, _ := event["part"].(map[string]any)
		switch typeName {
		case "step_start":
			text.Reset()
			sawFinish = false
			finishReason = ""
			messageID, _ = part["messageID"].(string)
		case "text":
			if id, _ := part["messageID"].(string); id != "" {
				if messageID != "" && messageID != id {
					protocolError = "text belongs to a different assistant message"
				}
				messageID = id
			}
			if value, _ := part["text"].(string); value != "" {
				text.WriteString(value)
				emit("assistant.message.completed", map[string]string{"text": value})
				// This text block is already represented by the standard event.
				continue
			}
		case "step_finish":
			sawFinish = true
			finishReason, _ = part["reason"].(string)
		case "error":
			protocolError = eventError(event["error"])
		}
		if typeName == "" {
			typeName = "event"
		}
		emit("runtime.opencode."+typeName, event)
	}
	waitErr := command.Wait()
	reaped = true
	if ctx.Err() != nil {
		return relay.Result{}, ctx.Err()
	}
	if resuming && execution.FallbackPrompt != "" && sessionUnavailable(protocolError+"\n"+stderr.String()) {
		return e.recoverSession(ctx, execution, protocolError+"\n"+stderr.String())
	}
	if protocolError != "" {
		return relay.Result{}, fmt.Errorf("relay opencode: runtime error: %s", agentcli.Truncate(protocolError, 8192))
	}
	if waitErr != nil {
		return relay.Result{}, fmt.Errorf("relay opencode: process failed: %w: %s", waitErr, agentcli.Truncate(stderr.String(), 8192))
	}
	if (!sawFinish || finishReason == "") && isV2(version) && sessionID != "" && messageID != "" && strings.TrimSpace(text.String()) != "" {
		// V2 can emit only step_start and text even when its native message
		// has completed. Verify that exact message, never an earlier reply.
		finishReason, err = verifyNativeCompletion(ctx, binary, config, run, sessionID, messageID)
		if err != nil {
			return relay.Result{}, fmt.Errorf("relay opencode: stream ended without a successful final step: %w", err)
		}
		sawFinish = true
		emit("runtime.opencode.completion.verified", map[string]string{"session_id": sessionID, "message_id": messageID, "reason": finishReason, "source": "session.export"})
	}
	if !sawFinish || !successfulFinish(finishReason) {
		return relay.Result{}, fmt.Errorf("relay opencode: stream ended without a successful final step (reason %q)", finishReason)
	}
	finalMessage := strings.TrimSpace(text.String())
	if finalMessage == "" {
		return relay.Result{}, errors.New("relay opencode: stream completed without a final message")
	}
	if sessionID == "" && !config.Ephemeral {
		return relay.Result{}, errors.New("relay opencode: stream completed without a session ID")
	}
	artifacts, err := run.Artifacts(finalMessage)
	if err != nil {
		return relay.Result{}, fmt.Errorf("relay opencode: collect artifacts: %w", err)
	}
	emit("assistant.final.completed", map[string]string{"text": finalMessage})
	emit("runtime.opencode.completed", map[string]string{"session_id": sessionID})
	output, _ := json.Marshal(map[string]string{"session_id": sessionID, "message": finalMessage, "work_dir": run.WorkDir})
	result := relay.Result{Summary: finalMessage, Output: output, Artifacts: artifacts}
	if !config.Ephemeral {
		result.RuntimeSessionID = sessionID
	}
	return result, nil
}

func (e Executor) recoverSession(ctx context.Context, execution relay.Execution, reason string) (relay.Result, error) {
	if execution.Emit != nil {
		execution.Emit(ctx, "runtime.opencode.session.resume_failed", map[string]string{"session_id": execution.RuntimeSessionID, "error": agentcli.Truncate(reason, 4096)})
	}
	execution.RuntimeSessionID = ""
	execution.Instructions.Prompt = execution.FallbackPrompt
	return e.Execute(ctx, execution)
}

func sessionMissing(ctx context.Context, binary string, config Config, run agentcli.Run, id string) (bool, error) {
	command := exec.CommandContext(ctx, binary, "session", "export", "--standalone", id)
	runtimeprocess.Configure(command)
	command.Dir = run.WorkDir
	command.Env = environment(config, run)
	command.Stdout = io.Discard // Session transcripts must never enter diagnostics.
	stderr := &agentcli.LimitedBuffer{Remaining: 64 << 10}
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if sessionUnavailable(stderr.String()) {
			return true, nil
		}
		return false, fmt.Errorf("relay opencode: check session: %w", err)
	}
	return false, nil
}

func sessionUnavailable(message string) bool {
	return agentcli.ResumeUnavailable(message) || strings.Contains(strings.ToLower(message), "sessionnotfound")
}

func eventError(value any) string {
	if message, ok := value.(string); ok && message != "" {
		return message
	}
	errorInfo, _ := value.(map[string]any)
	if message, _ := errorInfo["message"].(string); message != "" {
		return message
	}
	if data, _ := errorInfo["data"].(map[string]any); data != nil {
		if message, _ := data["message"].(string); message != "" {
			return message
		}
	}
	if name, _ := errorInfo["name"].(string); name != "" {
		return name
	}
	return "OpenCode reported an error"
}

func resolveBinary(binary string) string {
	if binary == "" {
		return "opencode"
	}
	return binary
}

func isV2(version string) bool {
	major, err := strconv.Atoi(strings.SplitN(strings.TrimPrefix(version, "v"), ".", 2)[0])
	return err == nil && major >= 2
}

func ProbeVersion(ctx context.Context, binary string) (string, error) {
	command := exec.CommandContext(ctx, resolveBinary(binary), "--version")
	runtimeprocess.Configure(command)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("relay opencode: probe version: %w", err)
	}
	return runtimecodex.ParseVersionOutput(string(output)), nil
}

func environment(config Config, run agentcli.Run) []string {
	return agentcli.Environment(config, run,
		"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME",
		"OPENCODE_CONFIG", "OPENCODE_CONFIG_DIR", "OPENCODE_CONFIG_CONTENT",
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
		"OPENAI_API_KEY", "OPENAI_BASE_URL", "GOOGLE_API_KEY", "GEMINI_API_KEY",
		"OPENROUTER_API_KEY", "OPENCODE_API_KEY", "GITHUB_TOKEN", "GH_TOKEN",
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_REGION", "AWS_PROFILE")
}
