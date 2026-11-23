package antigravity

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The ACP prompt response ends a foreground turn, not necessarily the native
// conversation. AGY's Stop hook supplies the authoritative fullyIdle state.
// A private additional customization directory preserves all user hooks.
// PreToolUse also bridges subagent tool permissions that AGY ACP does not
// surface over session/request_permission.
type acpLifecycle struct {
	dir, url, token string
	server          *http.Server
}

type hookToolCall struct {
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

type lifecycleEvent struct {
	ConversationID    string        `json:"conversationId"`
	HookEventName     string        `json:"hookEventName"`
	FullyIdle         *bool         `json:"fullyIdle,omitempty"`
	TerminationReason string        `json:"terminationReason,omitempty"`
	ToolCall          *hookToolCall `json:"toolCall,omitempty"`
	WorkspacePaths    []string      `json:"workspacePaths,omitempty"`
	Cwd               string        `json:"cwd,omitempty"`
}

const subagentApprovalToolMatcher = "run_command|read_file|view_file|view_file_outline|view_code_item|write_file|edit_file|write_to_file|replace_file_content|multi_replace_file_content|list_dir|find_by_name|grep_search"

func startACPLifecycle(c *acpClient, toolDir string) (*acpLifecycle, error) {
	tool, err := lifecycleTool(toolDir)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "relay-antigravity-hooks-")
	if err != nil {
		return nil, err
	}
	b := &acpLifecycle{dir: dir}
	cleanup := true
	defer func() {
		if cleanup {
			b.close()
		}
	}()
	if err := os.Mkdir(filepath.Join(dir, ".agents"), 0700); err != nil {
		return nil, err
	}
	command := "'" + strings.ReplaceAll(tool, "'", "'\"'\"'") + "' antigravity-hook"
	if os.PathSeparator == '\\' {
		command = "\"" + tool + "\" antigravity-hook"
	}
	var secret [24]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return nil, err
	}
	b.token = hex.EncodeToString(secret[:])
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b.url = "http://" + listener.Addr().String()
	command += " --url " + b.url + " --token " + b.token
	handler := map[string]any{"type": "command", "command": command, "timeout": 15}
	hookSpec := map[string]any{
		"PreInvocation": []any{handler},
		"Stop":          []any{handler},
	}
	if !c.skipPermissions {
		// AGY ACP does not route subagent tool confirmations over
		// session/request_permission, causing self-type subagents to hang
		// indefinitely on privileged tools. Fail fast in PreToolUse for
		// subagent tools that would block on an unreachable prompt.
		hookSpec["PreToolUse"] = []any{
			map[string]any{
				"matcher": subagentApprovalToolMatcher,
				"hooks":   []any{handler},
			},
		}
	}
	hooks, _ := json.Marshal(map[string]any{
		"relay-lifecycle-" + b.token[:12]: hookSpec,
	})
	if err := os.WriteFile(filepath.Join(dir, ".agents", "hooks.json"), hooks, 0600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+b.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var event lifecycleEvent
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&event); err != nil || event.ConversationID == "" || (event.HookEventName != "PreInvocation" && event.HookEventName != "Stop" && event.HookEventName != "PreToolUse") {
			http.Error(w, "invalid lifecycle event", http.StatusBadRequest)
			return
		}
		if event.HookEventName == "PreToolUse" {
			decision := c.handleSubagentPreToolUse(event, b.dir)
			raw, _ := json.Marshal(decision)
			_, _ = w.Write(append(raw, '\n'))
			return
		}
		raw, _ := json.Marshal(event)
		ack := make(chan struct{})
		select {
		case c.incoming <- rpcRead{message: rpcMessage{Method: "relay/antigravity-lifecycle", Params: raw}, hookAck: ack}:
		case <-c.ctx.Done():
			return
		case <-r.Context().Done():
			return
		}
		// Process the lifecycle state before acknowledging the synchronous hook.
		select {
		case <-ack:
			_, _ = w.Write([]byte("{}\n"))
		case <-c.ctx.Done():
		case <-r.Context().Done():
		}
	})
	b.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = b.server.Serve(listener) }()
	cleanup = false
	return b, nil
}

func lifecycleTool(toolDir string) (string, error) {
	name := "relay-tool"
	if os.PathSeparator == '\\' {
		name += ".exe"
	}
	var candidates []string
	if toolDir != "" {
		candidates = append(candidates, filepath.Join(toolDir, name))
	}
	if executable, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(executable), name))
	}
	if path, err := exec.LookPath(name); err == nil {
		candidates = append(candidates, path)
	}
	for _, path := range candidates {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return filepath.Abs(path)
		}
	}
	return "", errors.New("relay antigravity: relay-tool is required for native lifecycle hooks; install the complete Relay bundle")
}

func (b *acpLifecycle) close() {
	if b.server != nil {
		_ = b.server.Close()
	}
	_ = os.RemoveAll(b.dir)
}

func (c *acpClient) waitForIdle(ctx context.Context, result, sessionParams map[string]any) error {
	if !c.hookSeen {
		return errors.New("relay antigravity: background tasks started without native lifecycle notifications; cannot confirm completion")
	}
	if c.stopError != "" {
		return errors.New(c.stopError)
	}
	if c.fullyIdle {
		return nil // The RPC response has already drained this foreground turn.
	}
	if reason := stringField(result, "stopReason"); reason != "end_turn" {
		return fmt.Errorf("relay antigravity: stopped with %q while background tasks were pending", reason)
	}
	c.updates.closeThought()
	c.waitedBackground = true
	c.updates.closeMessage("commentary")
	c.updates.emit("runtime.antigravity.background.waiting", map[string]string{"session_id": c.sessionID})
	for !c.fullyIdle {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case read := <-c.incoming:
			if read.err != nil {
				return fmt.Errorf("relay antigravity: stream ended while waiting for background tasks: %w", read.err)
			}
			if read.message.Method == "" {
				return errors.New("relay antigravity: unexpected response while waiting for background tasks")
			}
			if err := c.handleRead(ctx, read); err != nil {
				return err
			}
			if c.stopError != "" {
				return errors.New(c.stopError)
			}
		}
	}
	// Stop callbacks and ACP text notifications use different transports and
	// may arrive in either order. Reload only after authoritative completion and
	// consume its replay fence to obtain the full saved reply. Never resubmit a
	// user prompt or expose replayed tools/thinking as new current-turn activity.
	c.setActive(c.sessionID, false)
	c.replaySessionID, c.replayComplete = c.sessionID, false
	c.snapshot = &strings.Builder{}
	defer func() { c.snapshot = nil }()
	params := make(map[string]any, len(sessionParams)+1)
	for key, value := range sessionParams {
		params[key] = value
	}
	params["sessionId"] = c.sessionID
	loaded, err := c.call(ctx, "session/load", params)
	if err != nil {
		return fmt.Errorf("relay antigravity: load completed background reply: %w", err)
	}
	if id := stringField(loaded, "sessionId"); id != "" && id != c.sessionID {
		return errors.New("relay antigravity: completed background snapshot belongs to another session")
	}
	if err := c.waitForReplay(ctx); err != nil {
		return err
	}
	c.updates.message.Reset()
	c.updates.message.WriteString(c.snapshot.String())
	c.updates.emit("runtime.antigravity.background.completed", map[string]string{"session_id": c.sessionID})
	return nil
}

func hasSkipPermissionsFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--dangerously-skip-permissions" || arg == "--dangerously-skip-permissions=true" {
			return true
		}
	}
	return false
}

// handleSubagentPreToolUse prevents AGY subagents from deadlocking on internal
// tool permission prompts that AGY ACP never forwards over session/request_permission.
// Parent conversations return decision="allow" so they proceed directly to AGY's
// normal ACP session/request_permission flow without interference.
func (c *acpClient) handleSubagentPreToolUse(event lifecycleEvent, hooksDir string) map[string]any {
	sessionID, active := c.sessionState()
	if !active || event.ToolCall == nil || event.ConversationID == sessionID || c.skipPermissions {
		return map[string]any{"decision": "allow"}
	}
	if !subagentToolNeedsApproval(event, c.mode, hooksDir) {
		return map[string]any{"decision": "allow"}
	}
	return map[string]any{
		"decision": "deny",
		"reason": fmt.Sprintf(
			"AGY ACP cannot interactively approve subagent tool %q (%s); it would hang waiting for a prompt that cannot be shown. Report this to the parent conversation so the parent can run it directly.",
			event.ToolCall.Name,
			subagentToolTitle(event.ToolCall),
		),
	}
}

func subagentToolNeedsApproval(event lifecycleEvent, mode, hooksDir string) bool {
	if event.ToolCall == nil {
		return false
	}
	name := strings.ToLower(strings.TrimSpace(event.ToolCall.Name))
	switch name {
	case "run_command":
		return true
	case "write_file", "edit_file", "write_to_file", "replace_file_content", "multi_replace_file_content":
		if mode == "accept-edits" && pathWithinWorkspaces(toolTargetPath(event.ToolCall.Args), event.Cwd, event.WorkspacePaths, hooksDir) {
			return false
		}
		return true
	case "read_file", "view_file", "view_file_outline", "view_code_item", "list_dir", "find_by_name", "grep_search":
		target := toolTargetPath(event.ToolCall.Args)
		if target == "" {
			return false
		}
		return !pathWithinWorkspaces(target, event.Cwd, event.WorkspacePaths, hooksDir)
	default:
		return false
	}
}

func toolTargetPath(args map[string]any) string {
	for _, key := range []string{"Path", "AbsolutePath", "TargetFile", "DirectoryPath", "SearchPath", "SearchDirectory", "path", "file_path"} {
		if value := strings.TrimSpace(stringField(args, key)); value != "" {
			return value
		}
	}
	return ""
}

func pathWithinWorkspaces(target, cwd string, workspaces []string, hooksDir string) bool {
	if target == "" {
		return false
	}
	if !filepath.IsAbs(target) && cwd != "" {
		target = filepath.Join(cwd, target)
	}
	cleanTarget, err := filepath.Abs(target)
	if err != nil {
		cleanTarget = filepath.Clean(target)
	}
	cleanHooks := ""
	if hooksDir != "" {
		if abs, err := filepath.Abs(hooksDir); err == nil {
			cleanHooks = abs
		} else {
			cleanHooks = filepath.Clean(hooksDir)
		}
	}
	for _, ws := range workspaces {
		if strings.TrimSpace(ws) == "" {
			continue
		}
		cleanWS, err := filepath.Abs(ws)
		if err != nil {
			cleanWS = filepath.Clean(ws)
		}
		if cleanHooks != "" && cleanWS == cleanHooks {
			continue
		}
		rel, err := filepath.Rel(cleanWS, cleanTarget)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func subagentToolTitle(call *hookToolCall) string {
	if call == nil {
		return "tool"
	}
	if cmd := strings.TrimSpace(stringField(call.Args, "CommandLine")); cmd != "" {
		return cmd
	}
	if cmd := strings.TrimSpace(stringField(call.Args, "command")); cmd != "" {
		return cmd
	}
	if target := toolTargetPath(call.Args); target != "" {
		return call.Name + " " + target
	}
	if call.Name != "" {
		return call.Name
	}
	return "tool"
}

// ForwardLifecycleHook is used by the bundled relay-tool command. It forwards
// only lifecycle fields, leaving hook decisions and user customizations intact.
func ForwardLifecycleHook(ctx context.Context, input io.Reader, url, token string) error {
	return ForwardHook(ctx, input, io.Discard, url, token)
}

// ForwardHook forwards whitelisted Antigravity hook fields to Relay's local
// loopback endpoint and writes the JSON hook response to output.
func ForwardHook(ctx context.Context, input io.Reader, output io.Writer, url, token string) error {
	var event lifecycleEvent
	if err := json.NewDecoder(io.LimitReader(input, 4<<20)).Decode(&event); err != nil {
		return err
	}
	if url == "" || token == "" {
		return errors.New("missing Relay AGY hook connection")
	}
	parsed, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil || parsed.URL.Scheme != "http" || parsed.URL.Hostname() != "127.0.0.1" {
		return errors.New("Relay AGY hook connection must use loopback HTTP")
	}
	if event.HookEventName != "PreToolUse" {
		event.ToolCall = nil
		event.WorkspacePaths = nil
		event.Cwd = ""
	}
	raw, _ := json.Marshal(event)
	parsed.Body = io.NopCloser(bytes.NewReader(raw))
	parsed.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	response, err := client.Do(parsed)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Relay AGY hook returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return err
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		body = []byte("{}")
	}
	if output != nil {
		if _, err := output.Write(append(body, '\n')); err != nil {
			return err
		}
	}
	return nil
}
