// Package qoder adapts the headless stream-json protocol shared by Qoder CLI
// (qodercli) and the QwenWork (千问办公) agent engine (qoderclicn) to Relay.
package qoder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/runtime/agentcli"
	runtimecodex "github.com/KDF5000/relay/runtime/codex"
	runtimeprocess "github.com/KDF5000/relay/runtime/process"
)

type Config = agentcli.Config

// Variant describes one distribution of the Qoder CLI engine. Both variants
// speak the same protocol and differ only in binary, account, and config root.
type Variant struct {
	// Provider is the Relay provider and event namespace.
	Provider string
	// Binary is the command looked up on PATH when no command is configured.
	Binary string
	// Bundled lists application-bundled binaries tried when Binary is not on PATH.
	Bundled []string
	// Env lists provider variables forwarded through the restricted environment.
	Env []string
}

var (
	// Qoder is the international Qoder CLI, authenticated with `qodercli login`.
	Qoder = Variant{
		Provider: "qoder",
		Binary:   "qodercli",
		Bundled:  []string{"/Applications/QoderWork.app/Contents/Resources/bin/qodercli"},
		Env:      []string{"QODER_PERSONAL_ACCESS_TOKEN", "QODER_CONFIG_DIR", "XDG_CONFIG_HOME"},
	}
	// QwenWork is the 千问办公 engine (Qoder CLI CN), authenticated with
	// `qoderclicn login` against the Qoder CN / 千问办公 account.
	QwenWork = Variant{
		Provider: "qwenwork",
		Binary:   "qoderclicn",
		Bundled:  []string{"/Applications/QwenWorkCN.app/Contents/Resources/bin/qoderclicn"},
		Env:      []string{"QODER_PERSONAL_ACCESS_TOKEN", "QODERCN_CONFIG_DIR", "QODERCN_VPC_ENDPOINT", "XDG_CONFIG_HOME"},
	}
)

type Executor struct {
	Config  Config
	Variant Variant
}

func (e Executor) variant() Variant {
	if e.Variant.Provider == "" {
		return Qoder
	}
	return e.Variant
}

func (e Executor) Execute(ctx context.Context, execution relay.Execution) (relay.Result, error) {
	variant := e.variant()
	name := variant.Provider
	config := e.Config
	if execution.Runtime.Model != "" {
		config.Model = execution.Runtime.Model
	}
	// Qoder's --allowed-tools does not pre-approve tools in headless mode, so a
	// direct mapping would silently diverge from other adapters. Permission
	// policy belongs to Qoder settings or explicit --permission-mode args.
	if len(config.AllowedTools) > 0 || len(config.DisallowedTools) > 0 {
		return relay.Result{}, fmt.Errorf("relay %s: configure tool permissions in Qoder settings or args (for example --permission-mode); allowed_tools and disallowed_tools are unsupported", name)
	}
	run, err := agentcli.Prepare(execution, config, name)
	if err != nil {
		return relay.Result{}, err
	}
	defer run.Close()
	binary := ResolveBinary(variant, config.Binary)
	resuming := execution.RuntimeSessionID != "" && !config.Ephemeral
	args := []string{"--print", "--output-format", "stream-json"}
	if resuming {
		args = append(args, "--resume", execution.RuntimeSessionID)
	} else if config.Ephemeral {
		args = append(args, "--no-session-persistence")
	}
	if config.Model != "" {
		args = append(args, "--model", config.Model)
	}
	for _, path := range run.ImagePaths {
		args = append(args, "--attachment", path)
	}
	args = append(args, config.ExtraArgs...)
	processCtx, stop := context.WithCancel(ctx)
	defer stop()
	command := exec.CommandContext(processCtx, binary, args...)
	runtimeprocess.Configure(command)
	command.Dir = run.WorkDir
	// Stdin avoids argument length limits and flag interpretation of prompts.
	command.Stdin = strings.NewReader(execution.Instructions.Prompt)
	command.Env = agentcli.Environment(config, run, variant.Env...)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return relay.Result{}, err
	}
	stderr := &agentcli.LimitedBuffer{Remaining: 4 << 20}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return relay.Result{}, fmt.Errorf("relay %s: start: %w", name, err)
	}
	reaped := false
	defer func() {
		stop()
		if !reaped {
			_ = command.Wait()
		}
	}()
	emit := func(event string, payload any) {
		if execution.Emit != nil {
			execution.Emit(ctx, event, payload)
		}
	}
	emit("runtime."+name+".started", map[string]string{"binary": binary, "work_dir": run.WorkDir})
	// Tool results can exceed any fixed line limit, so decode values directly.
	decoder := json.NewDecoder(stdout)
	var sessionID, finalMessage, protocolError string
	sawResult := false
	for {
		var event map[string]any
		if err := decoder.Decode(&event); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return relay.Result{}, fmt.Errorf("relay %s: read JSON stream: %w", name, err)
		}
		if value, _ := event["session_id"].(string); value != "" {
			sessionID = value
		}
		typeName, _ := event["type"].(string)
		if typeName == "" {
			typeName = "event"
		}
		// User hooks report their raw stdout/stderr on every lifecycle event;
		// they are local diagnostics, not conversation output.
		if subtype, _ := event["subtype"].(string); typeName == "system" && strings.HasPrefix(subtype, "hook_") {
			continue
		}
		emit("runtime."+name+"."+typeName, event)
		switch typeName {
		case "assistant":
			if text := messageText(event["message"]); text != "" {
				finalMessage = text
				emit("assistant.message.completed", map[string]string{"text": text})
			}
		case "result":
			sawResult = true
			if value, _ := event["result"].(string); value != "" {
				finalMessage = value
			}
			if failed, _ := event["is_error"].(bool); failed {
				protocolError = finalMessage
				if protocolError == "" {
					protocolError, _ = event["subtype"].(string)
				}
			}
		}
	}
	waitErr := command.Wait()
	reaped = true
	if ctx.Err() != nil {
		return relay.Result{}, ctx.Err()
	}
	diagnostics := protocolError + "\n" + stderr.String()
	if resuming && execution.FallbackPrompt != "" && resumeUnavailable(diagnostics) {
		emit("runtime."+name+".session.resume_failed", map[string]string{"session_id": execution.RuntimeSessionID, "error": agentcli.Truncate(diagnostics, 4096)})
		recovery := execution
		recovery.RuntimeSessionID = ""
		recovery.Instructions.Prompt = execution.FallbackPrompt
		return e.Execute(ctx, recovery)
	}
	if protocolError != "" {
		return relay.Result{}, fmt.Errorf("relay %s: runtime error: %s%s", name, agentcli.Truncate(protocolError, 8192), loginHint(binary, diagnostics))
	}
	if waitErr != nil {
		return relay.Result{}, fmt.Errorf("relay %s: process failed: %w: %s%s", name, waitErr, agentcli.Truncate(stderr.String(), 8192), loginHint(binary, diagnostics))
	}
	if !sawResult {
		return relay.Result{}, fmt.Errorf("relay %s: stream ended before the result event", name)
	}
	finalMessage = strings.TrimSpace(finalMessage)
	if finalMessage == "" {
		return relay.Result{}, fmt.Errorf("relay %s: stream completed without a final message", name)
	}
	if sessionID == "" && !config.Ephemeral {
		return relay.Result{}, fmt.Errorf("relay %s: stream completed without a session ID", name)
	}
	artifacts, err := run.Artifacts(finalMessage)
	if err != nil {
		return relay.Result{}, fmt.Errorf("relay %s: collect artifacts: %w", name, err)
	}
	emit("assistant.final.completed", map[string]string{"text": finalMessage})
	emit("runtime."+name+".completed", map[string]string{"session_id": sessionID})
	output, _ := json.Marshal(map[string]string{"session_id": sessionID, "message": finalMessage, "work_dir": run.WorkDir})
	result := relay.Result{Summary: finalMessage, Output: output, Artifacts: artifacts}
	if !config.Ephemeral {
		result.RuntimeSessionID = sessionID
	}
	return result, nil
}

// messageText joins visible text blocks; thinking and tool blocks are excluded.
func messageText(value any) string {
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

func resumeUnavailable(message string) bool {
	return agentcli.ResumeUnavailable(message) || strings.Contains(strings.ToLower(message), "error resuming session")
}

func loginHint(binary, diagnostics string) string {
	if !strings.Contains(strings.ToLower(diagnostics), "not logged in") {
		return ""
	}
	return fmt.Sprintf(" (run `%s login` on this Node as the Relay Node user, or set QODER_PERSONAL_ACCESS_TOKEN through pass_env)", filepath.Base(binary))
}

// ResolveBinary returns the configured command, the variant binary on PATH, or
// the first application-bundled binary that exists.
func ResolveBinary(variant Variant, configured string) string {
	if configured != "" {
		return configured
	}
	if variant.Binary == "" {
		variant = Qoder
	}
	if _, err := exec.LookPath(variant.Binary); err == nil {
		return variant.Binary
	}
	for _, candidate := range bundledCandidates(variant) {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return variant.Binary
}

func bundledCandidates(variant Variant) []string {
	candidates := append([]string(nil), variant.Bundled...)
	if home, err := os.UserHomeDir(); err == nil {
		for _, path := range variant.Bundled {
			if strings.HasPrefix(path, "/Applications/") {
				candidates = append(candidates, filepath.Join(home, path))
			}
		}
	}
	return candidates
}

func ProbeVersion(ctx context.Context, binary string) (string, error) {
	if binary == "" {
		binary = ResolveBinary(Qoder, "")
	}
	command := exec.CommandContext(ctx, binary, "--version")
	runtimeprocess.Configure(command)
	command.Env = probeEnvironment()
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("relay qoder: probe version: %w: %s", err, agentcli.Truncate(string(output), 1024))
	}
	return runtimecodex.ParseVersionOutput(string(output)), nil
}
