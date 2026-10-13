package pi_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/KDF5000/relay"
	runtimepi "github.com/KDF5000/relay/runtime/pi"
)

func TestExecutorStreamsFinalMessageAndSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "pi")
	script := `#!/bin/sh
printf '%s\n' "$@" > .relay/pi-args.txt
printf '%s\n' '{"type":"session","version":3,"id":"pi-session"}'
printf '%s\n' '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"Pi completed"}}'
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"Pi completed the task"}],"stopReason":"stop"}}'
printf '%s\n' '{"type":"agent_settled","aborted":false}'
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var events []string
	executor := runtimepi.Executor{Config: runtimepi.Config{Binary: fake, WorkRoot: dir, Model: "anthropic/claude-test", AllowedTools: []string{"read", "bash"}}}
	result, err := executor.Execute(context.Background(), relay.Execution{
		RunID: "run-pi", RuntimeSessionID: "prior-session",
		Instructions: relay.CompiledInstructions{Stable: "stable rules\n", Prompt: "do work\n"},
		Capabilities: relay.NewCapabilityInvoker(relay.CapabilityInvokerOptions{}),
		Emit:         func(_ context.Context, event string, _ any) { events = append(events, event) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary != "Pi completed the task" || result.RuntimeSessionID != "pi-session" {
		t.Fatalf("result = %#v", result)
	}
	args, err := os.ReadFile(filepath.Join(dir, "run-pi", ".relay", "pi-args.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"--mode\njson", "--session\nprior-session", "--model\nanthropic/claude-test", "--tools\nread,bash", "do work"} {
		if !strings.Contains(string(args), expected) {
			t.Fatalf("args %q do not contain %q", args, expected)
		}
	}
	if !contains(events, "assistant.message.delta") || !contains(events, "runtime.pi.completed") {
		t.Fatalf("events = %#v", events)
	}
	content, err := os.ReadFile(filepath.Join(dir, "run-pi", "AGENTS.md"))
	if err != nil || !strings.Contains(string(content), "stable rules") {
		t.Fatalf("AGENTS.md = %q, err = %v", content, err)
	}
}

func TestProbeVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	fake := filepath.Join(t.TempDir(), "pi")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho 'pi 0.55.1'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	version, err := runtimepi.ProbeVersion(context.Background(), fake)
	if err != nil || version != "0.55.1" {
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
