package antigravity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/runtime/agentcli"
	runtimeprocess "github.com/KDF5000/relay/runtime/process"
)

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("ACP error %d: %s", e.Code, e.Message) }

type rpcMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}
type rpcRead struct {
	message rpcMessage
	err     error
	hookAck chan struct{}
}
type acpClient struct {
	ctx              context.Context
	stop             context.CancelFunc
	command          *exec.Cmd
	stdin            io.WriteCloser
	encoder          *json.Encoder
	incoming         chan rpcRead
	stderr           *agentcli.LimitedBuffer
	closeOnce        sync.Once
	nextID           int
	updates          *acpEvents
	execution        relay.Execution
	active           bool
	replaySessionID  string
	replayComplete   bool
	lifecycle        *acpLifecycle
	sessionID        string
	hookSeen         bool
	fullyIdle        bool
	stopError        string
	waitedBackground bool
	snapshot         *strings.Builder
}

func startACP(ctx context.Context, config Config, run agentcli.Run, execution relay.Execution, args []string) (*acpClient, error) {
	processCtx, stop := context.WithCancel(ctx)
	c := &acpClient{ctx: processCtx, stop: stop, incoming: make(chan rpcRead, 64), execution: execution, updates: newACPEvents(ctx, execution)}
	lifecycle, err := startACPLifecycle(c, config.ToolDir)
	if err != nil {
		stop()
		return nil, err
	}
	c.lifecycle = lifecycle
	args = append(args, "--add-dir", lifecycle.dir)
	command := exec.CommandContext(processCtx, ResolveBinary(config.Binary), args...)
	runtimeprocess.Configure(command)
	command.Dir = run.WorkDir
	command.Env = environment(config, run)
	command.Env = append(command.Env, "RELAY_AGY_HOOK_URL="+lifecycle.url, "RELAY_AGY_HOOK_TOKEN="+lifecycle.token)
	stdin, err := command.StdinPipe()
	if err != nil {
		stop()
		lifecycle.close()
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		stop()
		lifecycle.close()
		_ = stdin.Close()
		return nil, err
	}
	stderr := &agentcli.LimitedBuffer{Remaining: 4 << 20}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		stop()
		lifecycle.close()
		_ = stdin.Close()
		return nil, err
	}
	c.command, c.stdin, c.encoder, c.stderr = command, stdin, json.NewEncoder(stdin), stderr
	go func() {
		decoder := json.NewDecoder(stdout)
		for {
			var message rpcMessage
			err := decoder.Decode(&message)
			select {
			case c.incoming <- rpcRead{message: message, err: err}:
			case <-processCtx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return c, nil
}
func (c *acpClient) close() {
	c.closeOnce.Do(func() {
		c.stop()
		_ = c.stdin.Close()
		_ = c.command.Wait()
		c.lifecycle.close()
	})
}
func (c *acpClient) send(value any) error { return c.encoder.Encode(value) }
func (c *acpClient) call(ctx context.Context, method string, params any) (map[string]any, error) {
	c.nextID++
	id := c.nextID
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case read := <-c.incoming:
			if read.err != nil {
				c.close()
				return nil, fmt.Errorf("relay antigravity: ACP stream ended awaiting %s: %w: %s", method, read.err, agentcli.Truncate(c.stderr.String(), 4096))
			}
			message := read.message
			if message.Method != "" {
				if err := c.handleRead(ctx, read); err != nil {
					return nil, err
				}
				continue
			}
			if string(message.ID) != fmt.Sprint(id) {
				return nil, errors.New("relay antigravity: unexpected ACP response ID")
			}
			if message.Error != nil {
				return nil, message.Error
			}
			var result map[string]any
			if len(message.Result) > 0 && string(message.Result) != "null" {
				if err := json.Unmarshal(message.Result, &result); err != nil {
					return nil, err
				}
			}
			return result, nil
		}
	}
}

func (c *acpClient) handleRead(ctx context.Context, read rpcRead) error {
	if read.hookAck != nil {
		defer close(read.hookAck)
	}
	return c.handle(ctx, read.message)
}

func (c *acpClient) handle(ctx context.Context, message rpcMessage) error {
	var params map[string]any
	if len(message.Params) > 0 && string(message.Params) != "null" {
		if err := json.Unmarshal(message.Params, &params); err != nil {
			return err
		}
	}
	if message.Method == "relay/antigravity-lifecycle" {
		if !c.active || stringField(params, "conversationId") != c.sessionID {
			return nil
		}
		c.hookSeen = true
		if c.updates != nil {
			c.updates.emit("runtime.antigravity.lifecycle", params)
		}
		switch stringField(params, "hookEventName") {
		case "PreInvocation":
			c.fullyIdle = false
		case "Stop":
			idle, present := params["fullyIdle"].(bool)
			c.fullyIdle = present && idle
			if !present {
				c.stopError = "relay antigravity: native Stop hook omitted fullyIdle; cannot confirm background completion"
			}
			reason := stringField(params, "terminationReason")
			if reason != "" && reason != "NO_TOOL_CALL" && reason != "INJECTED_RESPONSE" {
				c.stopError = "relay antigravity: native execution stopped with " + reason
			}
		}
		return nil
	}
	if message.Method == "session/update" {
		if c.active && stringField(params, "sessionId") != c.sessionID {
			return nil
		}
		update, _ := params["update"].(map[string]any)
		if c.snapshot != nil && stringField(params, "sessionId") == c.sessionID {
			switch stringField(update, "sessionUpdate") {
			case "user_message_chunk", "agent_thought_chunk", "tool_call", "tool_call_update":
				c.snapshot.Reset()
			case "agent_message_chunk":
				content, _ := update["content"].(map[string]any)
				c.snapshot.WriteString(stringField(content, "text"))
			}
		}
		if stringField(update, "sessionUpdate") == "session_info_update" && stringField(params, "sessionId") == c.replaySessionID {
			c.replayComplete = true
		}
		if stringField(update, "sessionUpdate") == "config_option_update" {
			c.updates.options, _ = update["configOptions"].([]any)
		}
		if c.active {
			c.updates.update(update)
		}
		return nil
	}
	if len(message.ID) == 0 {
		return nil
	}
	if message.Method == "session/request_permission" {
		if c.active {
			c.updates.hasActivity = true
		}
		outcome := map[string]any{"outcome": "cancelled"}
		if c.active && c.execution.Interactions != nil {
			tool, _ := params["toolCall"].(map[string]any)
			raw, _ := json.Marshal(params)
			response, err := c.execution.Interactions.Request(ctx, relay.InteractionRequest{Kind: relay.InteractionApproval, Prompt: "Allow Antigravity tool: " + stringField(tool, "title"), Data: raw})
			if err != nil {
				return err
			}
			outcome = permissionOutcome(params, response)
		}
		return c.send(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": map[string]any{"outcome": outcome}})
	}
	return c.send(map[string]any{"jsonrpc": "2.0", "id": message.ID, "error": map[string]any{"code": -32601, "message": "Relay does not provide this ACP client capability"}})
}
func permissionOutcome(params map[string]any, response json.RawMessage) map[string]any {
	denied := map[string]any{"outcome": "cancelled"}
	var answer map[string]any
	if json.Unmarshal(response, &answer) != nil {
		return denied
	}
	optionID := stringField(answer, "optionId")
	if outcome, ok := answer["outcome"].(map[string]any); ok {
		optionID = stringField(outcome, "optionId")
	}
	options, _ := params["options"].([]any)
	for _, value := range options {
		option, _ := value.(map[string]any)
		id := stringField(option, "optionId")
		if id != "" && id == optionID {
			return map[string]any{"outcome": "selected", "optionId": id}
		}
	}
	// Generic Relay approval grants only one use, never a persistent approval.
	approved, _ := answer["approved"].(bool)
	decision := stringField(answer, "decision")
	if approved || decision == "approved" || decision == "approve" || decision == "accept" {
		for _, value := range options {
			option, _ := value.(map[string]any)
			if stringField(option, "kind") == "allow_once" {
				return map[string]any{"outcome": "selected", "optionId": stringField(option, "optionId")}
			}
		}
	}
	return denied
}

func acpArgs(extra []string) (args []string, model, effort, mode string, err error) {
	args = []string{"acp"}
	for i := 0; i < len(extra); i++ {
		flag, value, hasValue := strings.Cut(extra[i], "=")
		switch flag {
		case "--mode", "--model", "--effort":
			if !hasValue {
				i++
				if i >= len(extra) {
					return nil, "", "", "", fmt.Errorf("%s requires a value", flag)
				}
				value = extra[i]
			}
			switch flag {
			case "--mode":
				mode = value
			case "--model":
				model = value
			case "--effort":
				effort = value
			}
		case "--input-format", "--output-format", "--conversation", "--print", "--prompt", "-p", "--disable-slash-commands":
			return nil, "", "", "", fmt.Errorf("relay antigravity: %s is a headless-only flag; configure protocol=stream-json for legacy execution", flag)
		default:
			args = append(args, extra[i])
		}
	}
	return
}

func (e Executor) executeACP(ctx context.Context, execution relay.Execution) (relay.Result, error) {
	config := e.Config
	if len(config.AllowedTools) > 0 || len(config.DisallowedTools) > 0 {
		return relay.Result{}, errors.New("relay antigravity: configure permissions in agy settings or args; allowed_tools and disallowed_tools are unsupported")
	}
	args, extraModel, extraEffort, mode, err := acpArgs(config.ExtraArgs)
	if err != nil {
		return relay.Result{}, err
	}
	model, effort := config.Model, config.ReasoningEffort
	if execution.Runtime.Model != "" {
		model = execution.Runtime.Model
	}
	if execution.Runtime.ReasoningEffort != "" {
		effort = execution.Runtime.ReasoningEffort
	}
	if extraModel != "" {
		model = extraModel
	}
	if extraEffort != "" {
		effort = extraEffort
	}
	run, err := agentcli.Prepare(execution, config.Config, "antigravity")
	if err != nil {
		return relay.Result{}, err
	}
	defer run.Close()
	run.WorkDir, err = filepath.Abs(run.WorkDir)
	if err != nil {
		return relay.Result{}, err
	}
	c, err := startACP(ctx, config, run, execution, args)
	if err != nil {
		return relay.Result{}, err
	}
	defer c.close()
	c.updates.emit("runtime.antigravity.started", map[string]string{"binary": ResolveBinary(config.Binary), "work_dir": run.WorkDir, "protocol": "acp"})
	init, err := c.call(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}, "clientInfo": map[string]string{"name": "relay", "version": relay.Version}})
	if err != nil {
		return relay.Result{}, err
	}
	if version, ok := init["protocolVersion"].(float64); !ok || version != 1 {
		return relay.Result{}, errors.New("relay antigravity: unsupported ACP protocol version")
	}
	caps, _ := init["agentCapabilities"].(map[string]any)
	sessionID := ""
	sessionParams := map[string]any{"cwd": run.WorkDir, "mcpServers": []any{}}
	resuming := execution.RuntimeSessionID != "" && !config.Ephemeral
	var session map[string]any
	if resuming {
		supportsLoad, _ := caps["loadSession"].(bool)
		if !supportsLoad {
			return relay.Result{}, errors.New("relay antigravity: ACP server does not support session/load")
		}
		c.replaySessionID = execution.RuntimeSessionID
		sessionParams["sessionId"] = execution.RuntimeSessionID
		session, err = c.call(ctx, "session/load", sessionParams)
		if err != nil {
			if ctx.Err() != nil {
				return relay.Result{}, ctx.Err()
			}
			if resumeUnavailable(err.Error()) || strings.Contains(strings.ToLower(err.Error()), "session not found") || strings.Contains(strings.ToLower(err.Error()), "unknown session") || strings.Contains(strings.ToLower(err.Error()), "session does not exist") {
				c.close()
				return e.recoverSession(ctx, execution, err.Error())
			}
			return relay.Result{}, err
		}
		sessionID = execution.RuntimeSessionID
		if returned := stringField(session, "sessionId"); returned != "" && returned != sessionID {
			c.close()
			return e.recoverSession(ctx, execution, "requested conversation was not loaded")
		}
		if err := c.waitForReplay(ctx); err != nil {
			if ctx.Err() != nil {
				return relay.Result{}, ctx.Err()
			}
			c.close()
			return e.recoverSession(ctx, execution, err.Error())
		}
	} else {
		session, err = c.call(ctx, "session/new", sessionParams)
		if err != nil {
			return relay.Result{}, err
		}
		sessionID = stringField(session, "sessionId")
	}
	if sessionID == "" {
		return relay.Result{}, errors.New("relay antigravity: ACP did not return a session ID")
	}
	if options, ok := session["configOptions"].([]any); ok {
		c.updates.options = options
	}
	if model != "" {
		if _, err = c.call(ctx, "session/set_model", map[string]any{"sessionId": sessionID, "modelId": model}); err != nil {
			return relay.Result{}, fmt.Errorf("select ACP model: %w", err)
		}
	}
	if effort != "" {
		configID := ""
		for _, value := range c.updates.options {
			option, _ := value.(map[string]any)
			if stringField(option, "category") == "thought_level" || stringField(option, "id") == "thought_level" {
				configID = stringField(option, "id")
				break
			}
		}
		if configID == "" {
			return relay.Result{}, errors.New("relay antigravity: selected model does not expose a thinking level option")
		}
		if _, err = c.call(ctx, "session/set_config_option", map[string]any{"sessionId": sessionID, "configId": configID, "value": effort}); err != nil {
			return relay.Result{}, fmt.Errorf("select ACP thinking level: %w", err)
		}
	}
	if mode != "" {
		if _, err = c.call(ctx, "session/set_mode", map[string]any{"sessionId": sessionID, "modeId": mode}); err != nil {
			return relay.Result{}, fmt.Errorf("select ACP mode: %w", err)
		}
	}
	prompt := []any{map[string]string{"type": "text", "text": execution.Instructions.Prompt}}
	if len(run.ImagePaths) > 0 {
		promptCaps, _ := caps["promptCapabilities"].(map[string]any)
		supported, _ := promptCaps["image"].(bool)
		if !supported {
			return relay.Result{}, errors.New("relay antigravity: ACP server does not support image attachments")
		}
		images, err := relay.InputImages(execution.Input)
		if err != nil {
			return relay.Result{}, err
		}
		for i, path := range run.ImagePaths {
			bytes, err := os.ReadFile(path)
			if err != nil {
				return relay.Result{}, err
			}
			prompt = append(prompt, map[string]string{"type": "image", "mimeType": images[i].ContentType, "data": base64.StdEncoding.EncodeToString(bytes)})
		}
	}
	c.sessionID, c.active = sessionID, true
	result, err := c.call(ctx, "session/prompt", map[string]any{"sessionId": sessionID, "prompt": prompt})
	if err != nil {
		// agy can acknowledge session/load for a deleted trajectory and report the
		// lookup failure only when it tries to submit the prompt. Recover solely on
		// this explicit rejection, before any thinking, reply, tool or approval.
		if resuming && !c.updates.hasActivity && strings.Contains(strings.ToLower(err.Error()), "trajectory not found") {
			c.close()
			return e.recoverSession(ctx, execution, err.Error())
		}
		return relay.Result{}, err
	}
	if c.updates.hasSubagents || (c.hookSeen && stringField(result, "stopReason") == "end_turn") {
		if err := c.waitForIdle(ctx, result, sessionParams); err != nil {
			_ = c.send(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]string{"sessionId": sessionID}})
			return relay.Result{}, err
		}
	}
	c.active = false
	c.updates.emit("runtime.antigravity.prompt.completed", result)
	c.updates.finish()
	stopReason := stringField(result, "stopReason")
	if stopReason != "end_turn" && stopReason != "max_tokens" && stopReason != "max_turn_requests" && stopReason != "refusal" {
		return relay.Result{}, fmt.Errorf("relay antigravity: ACP stopped with %q", stopReason)
	}
	finalMessage := strings.TrimSpace(c.updates.message.String())
	if finalMessage == "" && resuming && !c.updates.hasActivity && stopReason == "end_turn" && execution.FallbackPrompt != "" {
		_ = c.send(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]string{"sessionId": sessionID}})
		c.close()
		return e.recoverSession(ctx, execution, "resumed ACP session ended before current-turn execution")
	}
	if finalMessage == "" {
		return relay.Result{}, errors.New("relay antigravity: ACP completed without a final message")
	}
	artifacts, err := run.Artifacts(finalMessage)
	if err != nil {
		return relay.Result{}, err
	}
	c.updates.emit("assistant.final.completed", map[string]string{"text": finalMessage})
	c.updates.emit("runtime.antigravity.completed", map[string]string{"session_id": sessionID})
	output, _ := json.Marshal(map[string]string{"session_id": sessionID, "message": finalMessage, "work_dir": run.WorkDir, "protocol": "acp"})
	usage := acpUsage(result, model)
	if usage != nil && c.waitedBackground {
		// The foreground RPC's token snapshot excludes later background rounds.
		usage.Status = "partial"
	}
	answer := relay.Result{Summary: finalMessage, Output: output, Artifacts: artifacts, Usage: usage}
	if !config.Ephemeral {
		answer.RuntimeSessionID = sessionID
	}
	return answer, nil
}

// agy 1.5.14 acknowledges session/load before its asynchronous history replay
// finishes. session_info_update is emitted after that replay. Wait for that
// session's update before sending a prompt; a delay or user echo is not a fence.
func (c *acpClient) waitForReplay(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for !c.replayComplete {
		select {
		case <-ctx.Done():
			return fmt.Errorf("relay antigravity: session replay did not finish: %w", ctx.Err())
		case read := <-c.incoming:
			if read.err != nil {
				return fmt.Errorf("relay antigravity: session replay stream ended: %w", read.err)
			}
			if read.message.Method == "" {
				return errors.New("relay antigravity: unexpected response during session replay")
			}
			if err := c.handleRead(ctx, read); err != nil {
				return err
			}
		}
	}
	return nil
}
