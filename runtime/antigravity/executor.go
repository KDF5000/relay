// Package antigravity adapts Antigravity CLI (agy)'s headless NDJSON protocol.
package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/runtime/agentcli"
	runtimecodex "github.com/KDF5000/relay/runtime/codex"
	runtimeprocess "github.com/KDF5000/relay/runtime/process"
)

type Config struct {
	agentcli.Config
	ReasoningEffort string
}

type Executor struct{ Config Config }

func (e Executor) Execute(ctx context.Context, execution relay.Execution) (relay.Result, error) {
	config := e.Config
	if execution.Runtime.Model != "" {
		config.Model = execution.Runtime.Model
	}
	if execution.Runtime.ReasoningEffort != "" {
		config.ReasoningEffort = execution.Runtime.ReasoningEffort
	}
	if len(config.AllowedTools) > 0 || len(config.DisallowedTools) > 0 {
		return relay.Result{}, errors.New("relay antigravity: configure permissions in agy settings or args; allowed_tools and disallowed_tools are unsupported")
	}
	run, err := agentcli.Prepare(execution, config.Config, "antigravity")
	if err != nil {
		return relay.Result{}, err
	}
	defer run.Close()
	// The stdin protocol only supports text blocks. Never silently drop images.
	if len(run.ImagePaths) > 0 {
		return relay.Result{}, errors.New("relay antigravity: image attachments are unsupported by agy's stream-json input")
	}
	args := []string{"--input-format", "stream-json", "--output-format", "stream-json", "--disable-slash-commands"}
	resuming := execution.RuntimeSessionID != "" && !config.Ephemeral
	if resuming {
		args = append(args, "--conversation", execution.RuntimeSessionID)
	}
	if config.Model != "" {
		args = append(args, "--model", config.Model)
	}
	if config.ReasoningEffort != "" {
		args = append(args, "--effort", config.ReasoningEffort)
	}
	args = append(args, config.ExtraArgs...)
	// One stdin message followed by EOF runs exactly one turn and avoids prompt
	// argument limits and agy's flag-like prompt parsing.
	prompt, _ := json.Marshal(map[string]any{"event": "user", "message": map[string]string{"content": execution.Instructions.Prompt}})
	processCtx, stop := context.WithCancel(ctx)
	defer stop()
	binary := ResolveBinary(config.Binary)
	command := exec.CommandContext(processCtx, binary, args...)
	runtimeprocess.Configure(command)
	command.Dir = run.WorkDir
	command.Env = environment(config, run)
	stdin, err := command.StdinPipe()
	if err != nil {
		return relay.Result{}, err
	}
	defer stdin.Close()
	stdout, err := command.StdoutPipe()
	if err != nil {
		return relay.Result{}, err
	}
	stderr := &agentcli.LimitedBuffer{Remaining: 4 << 20}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return relay.Result{}, fmt.Errorf("relay antigravity: start: %w", err)
	}
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
	emit("runtime.antigravity.started", map[string]string{"binary": binary, "work_dir": run.WorkDir})
	decoder := json.NewDecoder(stdout)
	var sessionID, finalMessage, status, protocolError string
	sawResult := false
	sentPrompt := false
	for {
		var event map[string]any
		if err := decoder.Decode(&event); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if ctx.Err() != nil {
				return relay.Result{}, ctx.Err()
			}
			return relay.Result{}, fmt.Errorf("relay antigravity: read JSON stream: %w", err)
		}
		name, _ := event["event"].(string)
		if name == "" {
			name = "event"
		}
		emit("runtime.antigravity."+name, event)
		switch name {
		case "init":
			if sentPrompt {
				return relay.Result{}, errors.New("relay antigravity: duplicate init event")
			}
			sessionID, _ = event["conversation_id"].(string)
			// agy silently starts a fresh conversation when an ID is missing.
			// Confirm the ID before sending any prompt or allowing tool actions.
			if resuming && sessionID != execution.RuntimeSessionID {
				_ = stdin.Close()
				stop()
				_ = command.Wait()
				reaped = true
				if ctx.Err() != nil {
					return relay.Result{}, ctx.Err()
				}
				return e.recoverSession(ctx, execution, "requested conversation was not loaded")
			}
			if _, err := stdin.Write(append(prompt, '\n')); err != nil {
				return relay.Result{}, fmt.Errorf("relay antigravity: write prompt: %w", err)
			}
			if err := stdin.Close(); err != nil {
				return relay.Result{}, fmt.Errorf("relay antigravity: close prompt input: %w", err)
			}
			sentPrompt = true
		case "step_update":
			step, _ := event["step_update"].(map[string]any)
			if step["step_type"] == "agent_response" {
				if delta, _ := step["text_delta"].(string); delta != "" {
					emit("assistant.message.delta", map[string]string{"delta": delta})
				}
			}
		case "result":
			if sawResult {
				return relay.Result{}, errors.New("relay antigravity: multiple result events for a single turn")
			}
			sawResult = true
			result, _ := event["result"].(map[string]any)
			status, _ = result["status"].(string)
			finalMessage, _ = result["response"].(string)
			protocolError, _ = result["error"].(string)
			if id, _ := result["conversation_id"].(string); id != "" {
				sessionID = id
			}
		}
	}
	waitErr := command.Wait()
	reaped = true
	if ctx.Err() != nil {
		return relay.Result{}, ctx.Err()
	}
	diagnostics := protocolError + "\n" + stderr.String()
	if resuming && !sentPrompt && resumeUnavailable(diagnostics) {
		return e.recoverSession(ctx, execution, diagnostics)
	}
	if sawResult && (status != "SUCCESS" || protocolError != "") {
		return relay.Result{}, fmt.Errorf("relay antigravity: runtime status %q: %s", status, agentcli.Truncate(protocolError, 8192))
	}
	if waitErr != nil {
		return relay.Result{}, fmt.Errorf("relay antigravity: process failed: %w: %s", waitErr, agentcli.Truncate(stderr.String(), 8192))
	}
	if !sawResult {
		return relay.Result{}, errors.New("relay antigravity: stream ended before the result event")
	}
	if !sentPrompt {
		return relay.Result{}, errors.New("relay antigravity: result received before prompt initialization")
	}
	finalMessage = strings.TrimSpace(finalMessage)
	if finalMessage == "" {
		return relay.Result{}, errors.New("relay antigravity: stream completed without a final message")
	}
	if sessionID == "" && !config.Ephemeral {
		return relay.Result{}, errors.New("relay antigravity: stream completed without a conversation ID")
	}
	artifacts, err := run.Artifacts(finalMessage)
	if err != nil {
		return relay.Result{}, fmt.Errorf("relay antigravity: collect artifacts: %w", err)
	}
	emit("assistant.final.completed", map[string]string{"text": finalMessage})
	emit("runtime.antigravity.completed", map[string]string{"session_id": sessionID})
	output, _ := json.Marshal(map[string]string{"session_id": sessionID, "message": finalMessage, "work_dir": run.WorkDir})
	result := relay.Result{Summary: finalMessage, Output: output, Artifacts: artifacts}
	if !config.Ephemeral {
		result.RuntimeSessionID = sessionID
	}
	return result, nil
}

func (e Executor) recoverSession(ctx context.Context, execution relay.Execution, reason string) (relay.Result, error) {
	if execution.Emit != nil {
		execution.Emit(ctx, "runtime.antigravity.session.resume_failed", map[string]string{"session_id": execution.RuntimeSessionID, "error": agentcli.Truncate(reason, 4096)})
	}
	if execution.FallbackPrompt == "" {
		return relay.Result{}, errors.New("relay antigravity: conversation unavailable and no fallback prompt supplied")
	}
	execution.RuntimeSessionID = ""
	execution.Instructions.Prompt = execution.FallbackPrompt
	return e.Execute(ctx, execution)
}

func resumeUnavailable(message string) bool {
	message = strings.ToLower(message)
	return agentcli.ResumeUnavailable(message) || strings.Contains(message, "conversation not found") || strings.Contains(message, "unknown conversation") || strings.Contains(message, "failed to load conversation")
}

func ResolveBinary(binary string) string {
	if binary == "" {
		return "agy"
	}
	return binary
}

func ProbeVersion(ctx context.Context, binary string) (string, error) {
	command := exec.CommandContext(ctx, ResolveBinary(binary), "--version")
	runtimeprocess.Configure(command)
	command.Env = environment(Config{}, agentcli.Run{})
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("relay antigravity: probe version: %w", err)
	}
	return runtimecodex.ParseVersionOutput(string(output)), nil
}

func environment(config Config, run agentcli.Run) []string {
	return agentcli.Environment(config.Config, run, "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME")
}
