package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLifecycleHookForwardsOnlyStateWithoutProxy(t *testing.T) {
	var received lifecycleEvent
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing authentication")
		}
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "PRIVATE_PROMPT") {
			t.Error("forwarded unrelated hook data")
		}
		_ = json.Unmarshal(raw, &received)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	input := `{"conversationId":"parent","hookEventName":"Stop","fullyIdle":true,"terminationReason":"NO_TOOL_CALL","lastUserInput":"PRIVATE_PROMPT"}`
	if err := ForwardLifecycleHook(context.Background(), strings.NewReader(input), server.URL, "secret"); err != nil {
		t.Fatal(err)
	}
	if received.FullyIdle == nil || !*received.FullyIdle || received.ConversationID != "parent" {
		t.Fatalf("missing terminal state: %+v", received)
	}
	if err := ForwardLifecycleHook(context.Background(), strings.NewReader(input), "https://example.com", "secret"); err == nil {
		t.Fatal("accepted an external hook connection")
	}
}

func TestLifecycleAcknowledgmentFencesNativeFinalOutput(t *testing.T) {
	tools := t.TempDir()
	if err := os.WriteFile(filepath.Join(tools, "relay-tool"), []byte("stub"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c := &acpClient{ctx: ctx, active: true, sessionID: "parent", incoming: make(chan rpcRead, 1)}
	bridge, err := startACPLifecycle(c, tools)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.close()
	completed := make(chan error, 1)
	go func() {
		completed <- ForwardLifecycleHook(ctx, strings.NewReader(`{"conversationId":"parent","hookEventName":"Stop","fullyIdle":true,"terminationReason":"NO_TOOL_CALL"}`), bridge.url, bridge.token)
	}()
	select {
	case read := <-c.incoming:
		select {
		case <-completed:
			t.Fatal("released AGY before recording fullyIdle")
		default:
		}
		if err := c.handleRead(ctx, read); err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := <-completed; err != nil || !c.fullyIdle {
		t.Fatalf("idle=%v error=%v", c.fullyIdle, err)
	}
	bridge.close()
	if _, err := os.Stat(bridge.dir); !os.IsNotExist(err) {
		t.Fatal("temporary customization directory was retained")
	}
}

func TestSubagentPreToolUseFailsFastOnPrivilegedTools(t *testing.T) {
	tools := t.TempDir()
	if err := os.WriteFile(filepath.Join(tools, "relay-tool"), []byte("stub"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := &acpClient{
		ctx:       ctx,
		active:    true,
		sessionID: "parent-conv",
		incoming:  make(chan rpcRead, 4),
	}
	bridge, err := startACPLifecycle(c, tools)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.close()

	forward := func(input string) map[string]any {
		t.Helper()
		var out bytes.Buffer
		if err := ForwardHook(ctx, strings.NewReader(input), &out, bridge.url, bridge.token); err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatalf("unmarshal hook output %q: %v", out.String(), err)
		}
		return result
	}

	// 1. Parent tool calls pass Stage 1 so ACP session/request_permission handles them.
	parentOut := forward(`{"conversationId":"parent-conv","hookEventName":"PreToolUse","toolCall":{"id":"t1","name":"run_command","args":{"CommandLine":"echo hi"}}}`)
	if parentOut["decision"] != "allow" {
		t.Fatalf("expected allow for parent tool call, got %+v", parentOut)
	}

	// 2. Subagent in-workspace read_file passes through without blocking.
	inWSOut := forward(`{"conversationId":"child-conv","hookEventName":"PreToolUse","workspacePaths":["/repo"],"cwd":"/repo","toolCall":{"id":"t2","name":"read_file","args":{"Path":"/repo/main.go"}}}`)
	if inWSOut["decision"] != "allow" {
		t.Fatalf("in-workspace read_file should be allowed, got %+v", inWSOut)
	}

	// 3. Subagent run_command fails fast with deny instead of deadlocking.
	childCmd := forward(`{"conversationId":"child-conv","hookEventName":"PreToolUse","workspacePaths":["/repo"],"cwd":"/repo","toolCall":{"id":"t3","name":"run_command","args":{"CommandLine":"impeccable detect --json"}}}`)
	if childCmd["decision"] != "deny" || !strings.Contains(stringField(childCmd, "reason"), "parent conversation") {
		t.Fatalf("expected deny with parent fallback guidance, got %+v", childCmd)
	}

	// 4. Subagent out-of-workspace read_file fails fast with deny.
	childRead := forward(`{"conversationId":"child-conv","hookEventName":"PreToolUse","workspacePaths":["/repo"],"cwd":"/repo","toolCall":{"id":"t4","name":"read_file","args":{"Path":"/etc/hosts"}}}`)
	if childRead["decision"] != "deny" {
		t.Fatalf("expected deny for out-of-workspace subagent read, got %+v", childRead)
	}

	// 5. Subagent in-workspace write_file is allowed under --mode accept-edits, denied otherwise.
	childWrite := forward(`{"conversationId":"child-conv","hookEventName":"PreToolUse","workspacePaths":["/repo"],"cwd":"/repo","toolCall":{"id":"t5","name":"write_file","args":{"Path":"/repo/out.txt"}}}`)
	if childWrite["decision"] != "deny" {
		t.Fatalf("expected deny for subagent write_file in default mode, got %+v", childWrite)
	}
	c.mode = "accept-edits"
	childWriteAccept := forward(`{"conversationId":"child-conv","hookEventName":"PreToolUse","workspacePaths":["/repo"],"cwd":"/repo","toolCall":{"id":"t6","name":"write_file","args":{"Path":"/repo/out.txt"}}}`)
	if childWriteAccept["decision"] != "allow" {
		t.Fatalf("expected allow for subagent write_file in accept-edits mode, got %+v", childWriteAccept)
	}
}
