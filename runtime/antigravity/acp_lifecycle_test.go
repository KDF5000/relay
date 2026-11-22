package antigravity

import (
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
