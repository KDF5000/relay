// Package pi adapts Pi's JSON event stream to Relay.
package pi

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
	run, err := agentcli.Prepare(execution, config, "pi")
	if err != nil {
		return relay.Result{}, err
	}
	defer run.Close()
	binary := config.Binary
	if binary == "" {
		binary = "pi"
	}
	resuming := execution.RuntimeSessionID != "" && !config.Ephemeral
	args := []string{"--mode", "json"}
	if resuming {
		args = append(args, "--session", execution.RuntimeSessionID)
	} else if config.Ephemeral {
		args = append(args, "--no-session")
	}
	if config.Model != "" {
		args = append(args, "--model", config.Model)
	}
	if len(config.AllowedTools) > 0 {
		args = append(args, "--tools", strings.Join(config.AllowedTools, ","))
	}
	if len(config.DisallowedTools) > 0 {
		args = append(args, "--exclude-tools", strings.Join(config.DisallowedTools, ","))
	}
	args = append(args, config.ExtraArgs...)
	args = append(args, execution.Instructions.Prompt)
	for _, imagePath := range run.ImagePaths {
		args = append(args, "@"+imagePath)
	}
	command := exec.CommandContext(ctx, binary, args...)
	runtimeprocess.Configure(command)
	command.Dir = run.WorkDir
	command.Env = piEnvironment(config, run)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return relay.Result{}, err
	}
	stderr := &agentcli.LimitedBuffer{Remaining: 4 << 20}
	command.Stderr = stderr
	if execution.Emit != nil {
		execution.Emit(ctx, "runtime.pi.started", map[string]any{"binary": binary, "work_dir": run.WorkDir})
	}
	if err := command.Start(); err != nil {
		return relay.Result{}, fmt.Errorf("relay pi: start: %w", err)
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	var sessionID, finalMessage, protocolError string
	sawSettled := false
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			if execution.Emit != nil {
				execution.Emit(ctx, "runtime.pi.unparsed", map[string]string{"line": agentcli.Truncate(string(line), 4096)})
			}
			continue
		}
		typeName, _ := event["type"].(string)
		if typeName == "" {
			typeName = "event"
		}
		if typeName == "session" {
			sessionID, _ = event["id"].(string)
		}
		if execution.Emit != nil {
			execution.Emit(ctx, "runtime.pi."+typeName, event)
		}
		if typeName == "message_update" {
			update, _ := event["assistantMessageEvent"].(map[string]any)
			if update["type"] == "text_delta" {
				if delta, _ := update["delta"].(string); delta != "" && execution.Emit != nil {
					execution.Emit(ctx, "assistant.message.delta", map[string]string{"delta": delta})
				}
			}
		}
		if typeName == "message_end" {
			if text, assistant, failed := piMessageText(event["message"]); assistant {
				if text != "" {
					finalMessage = text
					if execution.Emit != nil {
						execution.Emit(ctx, "assistant.message.completed", map[string]string{"text": text})
					}
				}
				if failed != "" {
					protocolError = failed
				}
			}
		}
		if typeName == "agent_settled" {
			sawSettled = true
			if aborted, _ := event["aborted"].(bool); aborted {
				protocolError = "agent run was aborted"
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return relay.Result{}, fmt.Errorf("relay pi: read JSONL: %w", err)
	}
	waitErr := command.Wait()
	if waitErr != nil {
		if resuming && execution.FallbackPrompt != "" && ctx.Err() == nil && agentcli.ResumeUnavailable(stderr.String()) {
			if execution.Emit != nil {
				execution.Emit(ctx, "runtime.pi.session.resume_failed", map[string]string{"session_id": execution.RuntimeSessionID, "error": waitErr.Error()})
			}
			recovery := execution
			recovery.RuntimeSessionID = ""
			recovery.Instructions.Prompt = execution.FallbackPrompt
			return e.Execute(ctx, recovery)
		}
		return relay.Result{}, fmt.Errorf("relay pi: process failed: %w: %s", waitErr, agentcli.Truncate(stderr.String(), 8192))
	}
	if protocolError != "" {
		return relay.Result{}, fmt.Errorf("relay pi: runtime error: %s", protocolError)
	}
	if !sawSettled {
		return relay.Result{}, fmt.Errorf("relay pi: stream ended before the agent settled")
	}
	finalMessage = strings.TrimSpace(finalMessage)
	if finalMessage == "" {
		return relay.Result{}, fmt.Errorf("relay pi: stream completed without a final message")
	}
	artifacts, err := run.Artifacts("pi", finalMessage)
	if err != nil {
		return relay.Result{}, fmt.Errorf("relay pi: write final message: %w", err)
	}
	if execution.Emit != nil {
		execution.Emit(ctx, "assistant.final.completed", map[string]string{"text": finalMessage})
		execution.Emit(ctx, "runtime.pi.completed", map[string]string{"session_id": sessionID})
	}
	output, _ := json.Marshal(map[string]string{"session_id": sessionID, "message": finalMessage, "work_dir": run.WorkDir})
	result := relay.Result{Summary: finalMessage, Output: output, Artifacts: artifacts}
	if !config.Ephemeral {
		result.RuntimeSessionID = sessionID
	}
	return result, nil
}

func piMessageText(value any) (text string, assistant bool, failure string) {
	message, _ := value.(map[string]any)
	role, _ := message["role"].(string)
	if role != "assistant" {
		return "", false, ""
	}
	content, _ := message["content"].([]any)
	var parts []string
	for _, item := range content {
		block, _ := item.(map[string]any)
		if block["type"] == "text" {
			if value, _ := block["text"].(string); value != "" {
				parts = append(parts, value)
			}
		}
	}
	stopReason, _ := message["stopReason"].(string)
	if stopReason == "error" || stopReason == "aborted" {
		failure, _ = message["errorMessage"].(string)
		if failure == "" {
			failure = "assistant stopped with " + stopReason
		}
	}
	return strings.TrimSpace(strings.Join(parts, "")), true, failure
}

func ProbeVersion(ctx context.Context, binary string) (string, error) {
	if binary == "" {
		binary = "pi"
	}
	output, err := exec.CommandContext(ctx, binary, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("relay pi: probe version: %w: %s", err, output)
	}
	return runtimecodex.ParseVersionOutput(string(output)), nil
}
