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
	"strings"

	"github.com/KDF5000/relay"
	runtimeprocess "github.com/KDF5000/relay/runtime/process"
	"github.com/KDF5000/relay/runtime/toolbridge"
)

type rpcMessage struct {
	ID     int             `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func executeAppServer(ctx context.Context, config Config, execution relay.Execution, fork ForkOptions) (result relay.Result, resultErr error) {
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

	processCtx, stopProcess := context.WithCancel(ctx)
	defer stopProcess()
	command := exec.CommandContext(processCtx, binary, "app-server")
	runtimeprocess.Configure(command)
	command.Dir = workDir
	command.Env = runtimeEnv(config, bridge.Dir, bridge.Token, fork.PassEnv)
	stdin, err := command.StdinPipe()
	if err != nil {
		return relay.Result{}, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return relay.Result{}, err
	}
	var stderr bytes.Buffer
	command.Stderr = &limitedBuffer{buffer: &stderr, remaining: 4 << 20}
	if execution.Emit != nil {
		execution.Emit(ctx, "runtime."+fork.Name+".started", map[string]any{"binary": binary, "protocol": "app-server", "work_dir": workDir, "sandbox": sandbox})
	}
	if err := command.Start(); err != nil {
		return relay.Result{}, fmt.Errorf("relay %s: start app-server: %w", fork.Name, err)
	}
	defer func() {
		stopProcess()
		waitErr := command.Wait()
		if resultErr == nil && waitErr != nil && ctx.Err() != nil {
			resultErr = ctx.Err()
		}
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	writeRequest := func(value any) error {
		return json.NewEncoder(stdin).Encode(value)
	}
	if err := writeRequest(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]string{"name": "relay", "version": "0.1"}, "capabilities": map[string]bool{"experimentalApi": true}}}); err != nil {
		return relay.Result{}, err
	}
	if _, err := waitRPCResponse(scanner, 1, nil); err != nil {
		return relay.Result{}, appServerError(fork.Name, err, stderr.String())
	}
	if err := writeRequest(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return relay.Result{}, err
	}
	threadParams := map[string]any{"cwd": workDir, "sandbox": sandbox, "approvalPolicy": "never", "ephemeral": config.Ephemeral}
	if config.Model != "" {
		threadParams["model"] = config.Model
	}
	if config.ServiceTier != "" {
		threadParams["serviceTier"] = config.ServiceTier
	}
	if err := writeRequest(map[string]any{"id": 2, "method": "thread/start", "params": threadParams}); err != nil {
		return relay.Result{}, err
	}
	threadResponse, err := waitRPCResponse(scanner, 2, func(message rpcMessage) { emitAppServerEvent(ctx, execution, fork.Name, message) })
	if err != nil {
		return relay.Result{}, appServerError(fork.Name, err, stderr.String())
	}
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(threadResponse, &started); err != nil || started.Thread.ID == "" {
		return relay.Result{}, fmt.Errorf("relay %s: invalid thread/start response", fork.Name)
	}
	threadID := started.Thread.ID
	turnInput := []map[string]string{{"type": "text", "text": execution.Instructions.Prompt}}
	for _, imagePath := range imagePaths {
		turnInput = append(turnInput, map[string]string{"type": "localImage", "path": imagePath})
	}
	if err := writeRequest(map[string]any{"id": 3, "method": "turn/start", "params": map[string]any{"threadId": threadID, "input": turnInput}}); err != nil {
		return relay.Result{}, err
	}

	var message strings.Builder
	messageItemID := ""
	turnStarted := false
	turnCompleted := false
	for scanner.Scan() {
		var rpc rpcMessage
		if err := json.Unmarshal(scanner.Bytes(), &rpc); err != nil {
			continue
		}
		if rpc.ID == 3 {
			if rpc.Error != nil {
				return relay.Result{}, errors.New(rpc.Error.Message)
			}
			turnStarted = true
			continue
		}
		emitAppServerEvent(ctx, execution, fork.Name, rpc)
		if rpc.Method == "item/agentMessage/delta" {
			var params struct {
				ItemID string `json:"itemId"`
				Delta  string `json:"delta"`
			}
			_ = json.Unmarshal(rpc.Params, &params)
			if params.ItemID != "" && messageItemID != "" && params.ItemID != messageItemID {
				message.Reset()
			}
			if params.ItemID != "" {
				messageItemID = params.ItemID
			}
			message.WriteString(params.Delta)
			if execution.Emit != nil && params.Delta != "" {
				execution.Emit(ctx, "assistant.message.delta", map[string]string{"delta": params.Delta, "item_id": params.ItemID})
			}
		}
		if rpc.Method == "item/completed" {
			if text := completedAgentMessage(rpc.Params); text != "" {
				message.Reset()
				message.WriteString(text)
				if execution.Emit != nil {
					execution.Emit(ctx, "assistant.message.completed", map[string]string{"text": text})
				}
			}
		}
		if rpc.Method == "turn/completed" {
			var params struct {
				Turn struct {
					Status string `json:"status"`
					Error  *struct {
						Message string `json:"message"`
					} `json:"error"`
				} `json:"turn"`
			}
			_ = json.Unmarshal(rpc.Params, &params)
			if params.Turn.Status != "completed" {
				cause := "turn " + params.Turn.Status
				if params.Turn.Error != nil && params.Turn.Error.Message != "" {
					cause = params.Turn.Error.Message
				}
				return relay.Result{}, errors.New(cause)
			}
			turnCompleted = true
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return relay.Result{}, fmt.Errorf("relay %s: read app-server stream: %w", fork.Name, err)
	}
	if !turnStarted || !turnCompleted || message.Len() == 0 {
		if turnStarted && !turnCompleted {
			return relay.Result{}, appServerError(fork.Name, errors.New("app-server ended before turn/completed"), stderr.String())
		}
		return relay.Result{}, appServerError(fork.Name, errors.New("app-server ended without a final message"), stderr.String())
	}
	finalMessage := strings.TrimSpace(message.String())
	if err := os.WriteFile(finalPath, []byte(finalMessage), 0o600); err != nil {
		return relay.Result{}, err
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
	return relay.Result{Summary: finalMessage, Output: output, Artifacts: artifacts}, nil
}

func waitRPCResponse(scanner *bufio.Scanner, id int, onNotification func(rpcMessage)) (json.RawMessage, error) {
	for scanner.Scan() {
		var message rpcMessage
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			continue
		}
		if message.ID != id {
			if onNotification != nil {
				onNotification(message)
			}
			continue
		}
		if message.Error != nil {
			return nil, errors.New(message.Error.Message)
		}
		return message.Result, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("app-server closed the event stream")
}

func emitAppServerEvent(ctx context.Context, execution relay.Execution, provider string, message rpcMessage) {
	if execution.Emit == nil || message.Method == "" {
		return
	}
	eventType := strings.ReplaceAll(message.Method, "/", ".")
	var params any
	if len(message.Params) > 0 {
		_ = json.Unmarshal(message.Params, &params)
	}
	execution.Emit(ctx, "runtime."+provider+"."+eventType, params)
}

func completedAgentMessage(data json.RawMessage) string {
	var params struct {
		Item struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"item"`
	}
	_ = json.Unmarshal(data, &params)
	if params.Item.Type == "agentMessage" {
		return params.Item.Text
	}
	return ""
}

func appServerError(provider string, err error, stderr string) error {
	if detail := strings.TrimSpace(stderr); detail != "" {
		return fmt.Errorf("relay %s: %w: %s", provider, err, truncate(detail, 8192))
	}
	return fmt.Errorf("relay %s: %w", provider, err)
}
