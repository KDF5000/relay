package antigravity

import (
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
type acpLifecycle struct {
	dir, url, token string
	server          *http.Server
}

type lifecycleEvent struct {
	ConversationID    string `json:"conversationId"`
	HookEventName     string `json:"hookEventName"`
	FullyIdle         *bool  `json:"fullyIdle,omitempty"`
	TerminationReason string `json:"terminationReason,omitempty"`
}

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
	hooks, _ := json.Marshal(map[string]any{"relay-lifecycle-" + b.token[:12]: map[string]any{"PreInvocation": []any{handler}, "Stop": []any{handler}}})
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
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&event); err != nil || event.ConversationID == "" || (event.HookEventName != "PreInvocation" && event.HookEventName != "Stop") {
			http.Error(w, "invalid lifecycle event", http.StatusBadRequest)
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
	c.active = false
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

// ForwardLifecycleHook is used by the bundled relay-tool command. It forwards
// only lifecycle fields, leaving hook decisions and user customizations intact.
func ForwardLifecycleHook(ctx context.Context, input io.Reader, url, token string) error {
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
	raw, _ := json.Marshal(event)
	parsed.Body = io.NopCloser(strings.NewReader(string(raw)))
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
	return nil
}
