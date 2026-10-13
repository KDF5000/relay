package claude_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/KDF5000/relay"
	runtimeclaude "github.com/KDF5000/relay/runtime/claude"
)

func TestExecutorStreamsFinalMessageAndSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "claude")
	script := `#!/bin/sh
printf '%s\n' "$@" > .relay/claude-args.txt
cat > .relay/claude-stdin.txt
printf '%s\n' '{"type":"system","subtype":"init","session_id":"claude-session"}'
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"Claude completed the task"}]},"session_id":"claude-session"}'
printf '%s\n' '{"type":"result","is_error":false,"result":"Claude completed the task","session_id":"claude-session"}'
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var events []string
	executor := runtimeclaude.Executor{Config: runtimeclaude.Config{Binary: fake, WorkRoot: dir, Model: "claude-test"}}
	result, err := executor.Execute(context.Background(), relay.Execution{
		RunID: "run-claude", RuntimeSessionID: "prior-session",
		Instructions: relay.CompiledInstructions{Stable: "stable rules\n", Prompt: "do work\n"},
		Capabilities: relay.NewCapabilityInvoker(relay.CapabilityInvokerOptions{}),
		Emit:         func(_ context.Context, event string, _ any) { events = append(events, event) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary != "Claude completed the task" || result.RuntimeSessionID != "claude-session" {
		t.Fatalf("result = %#v", result)
	}
	args, err := os.ReadFile(filepath.Join(dir, "run-claude", ".relay", "claude-args.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"--output-format\nstream-json", "--resume\nprior-session", "--model\nclaude-test"} {
		if !strings.Contains(string(args), expected) {
			t.Fatalf("args %q do not contain %q", args, expected)
		}
	}
	if !contains(events, "assistant.final.completed") || !contains(events, "runtime.claude.completed") {
		t.Fatalf("events = %#v", events)
	}
	content, err := os.ReadFile(filepath.Join(dir, "run-claude", "CLAUDE.md"))
	if err != nil || !strings.Contains(string(content), "stable rules") {
		t.Fatalf("CLAUDE.md = %q, err = %v", content, err)
	}
}

func TestProbeVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	fake := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho '2.1.30 (Claude Code)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	version, err := runtimeclaude.ProbeVersion(context.Background(), fake)
	if err != nil || version != "2.1.30" {
		t.Fatalf("version = %q, err = %v", version, err)
	}
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
