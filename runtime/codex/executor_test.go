package codex_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KDF5000/relay"
	runtimecodex "github.com/KDF5000/relay/runtime/codex"
)

func TestExecutorUsesCodexExecJSONLAndFinalMessage(t *testing.T) {
	dir := t.TempDir()
	toolDir := filepath.Join(dir, "tools")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(toolDir, "relay-tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "codex")
	script := `#!/bin/sh
out=""
model=""
all_args=" $* "
case "$all_args" in *" --sandbox "*|*" --permission-mode "*) exit 15;; esac
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--output-last-message" ]; then out="$2"; shift 2; continue; fi
  if [ "$1" = "--model" ]; then model="$2"; shift 2; continue; fi
  shift
done
cat >/dev/null
command -v relay-tool >/dev/null || exit 9
[ -n "$RELAY_TOOL_DIR" ] || exit 10
[ -n "$RELAY_TOOL_TOKEN" ] || exit 11
[ -z "$MULTICA_TOKEN" ] || exit 12
[ "$RELAY_TEST_ALLOWED" = "allowed" ] || exit 13
[ "$model" = "per-agent-model" ] || exit 14
printf '%s\n' '{"type":"thread.started","thread_id":"thread-real"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"Let me inspect the adapter."}}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"Codex completed the real adapter contract"}}'
printf '%s' 'Let me inspect the adapter.Codex completed the real adapter contract' > "$out"
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	events := []string{}
	t.Setenv("MULTICA_TOKEN", "must-not-reach-agent")
	t.Setenv("RELAY_TEST_ALLOWED", "allowed")
	executor := runtimecodex.Executor{Config: runtimecodex.Config{Binary: fake, ToolDir: toolDir, PassEnv: []string{"RELAY_TEST_ALLOWED"}, Model: "node-default", WorkRoot: dir, Ephemeral: true}}
	result, err := executor.Execute(context.Background(), relay.Execution{RunID: "run-1", AgentID: "agent", Runtime: relay.RuntimeRequirement{Provider: "codex", Model: "per-agent-model"}, Input: relay.Input{Prompt: "do work"}, Instructions: relay.CompiledInstructions{Stable: "stable rules\n", Prompt: "do work\n"}, Capabilities: relay.NewCapabilityInvoker(relay.CapabilityInvokerOptions{}), Emit: func(_ context.Context, eventType string, _ any) { events = append(events, eventType) }})
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary != "Codex completed the real adapter contract" {
		t.Fatalf("unexpected summary: %q", result.Summary)
	}
	if len(events) != 8 || events[1] != "runtime.codex.thread.started" || events[2] != "runtime.codex.item.completed" || events[3] != "assistant.message.completed" || events[6] != "assistant.final.completed" {
		t.Fatalf("unexpected events: %v", events)
	}
	content, err := os.ReadFile(filepath.Join(dir, "run-1", "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "stable rules") {
		t.Fatalf("instructions not materialized: %s", content)
	}
}

func TestExecutorPassesMaterializedImagesToCodex(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "codex")
	script := `#!/bin/sh
out=""
image=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--output-last-message" ]; then out="$2"; shift 2; continue; fi
  if [ "$1" = "--image" ]; then image="$2"; shift 2; continue; fi
  shift
done
[ -f "$image" ] || exit 20
[ "$(wc -c < "$image" | tr -d ' ')" = "8" ] || exit 21
cat >/dev/null
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"image received"}}'
printf '%s' 'image received' > "$out"
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	executor := runtimecodex.Executor{Config: runtimecodex.Config{Binary: fake, WorkRoot: dir, Ephemeral: true}}
	result, err := executor.Execute(context.Background(), relay.Execution{
		RunID:        "run-image",
		Input:        relay.Input{Prompt: "inspect", Data: []byte(`{"images":[{"name":"screen.png","content_type":"image/png","data":"iVBORw0KGgo="}]}`)},
		Instructions: relay.CompiledInstructions{Prompt: "inspect"},
		Capabilities: relay.NewCapabilityInvoker(relay.CapabilityInvokerOptions{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary != "image received" {
		t.Fatalf("summary = %q", result.Summary)
	}
}

func TestExecutorResumesPersistedThread(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "codex")
	script := `#!/bin/sh
[ "$1" = "exec" ] || exit 20
[ "$2" = "resume" ] || exit 21
out=""
session=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--output-last-message" ]; then out="$2"; shift 2; continue; fi
  if [ "$1" = "native-thread" ]; then session="$1"; fi
  shift
done
[ "$session" = "native-thread" ] || exit 22
[ "$(cat)" = "current turn" ] || exit 23
printf '%s\n' '{"type":"thread.started","thread_id":"native-thread"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"continued"}}'
printf '%s' 'continued' > "$out"
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	executor := runtimecodex.Executor{Config: runtimecodex.Config{Binary: fake, WorkRoot: dir}}
	result, err := executor.Execute(context.Background(), relay.Execution{
		RunID: "run-resume", RuntimeSessionID: "native-thread", Instructions: relay.CompiledInstructions{Prompt: "current turn"},
		Capabilities: relay.NewCapabilityInvoker(relay.CapabilityInvokerOptions{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary != "continued" || result.RuntimeSessionID != "native-thread" {
		t.Fatalf("result = %+v", result)
	}
}

func TestExecutorRecoversWhenPersistedThreadIsMissing(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "codex")
	script := `#!/bin/sh
if [ "$1" = "exec" ] && [ "$2" = "resume" ]; then
  cat >/dev/null
  echo 'Error: no rollout found for thread id missing-thread' >&2
  exit 1
fi
out=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--output-last-message" ]; then out="$2"; shift 2; continue; fi
  shift
done
[ "$(cat)" = "recovery transcript" ] || exit 24
printf '%s\n' '{"type":"thread.started","thread_id":"replacement-thread"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"recovered"}}'
printf '%s' 'recovered' > "$out"
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var resumeFailed bool
	executor := runtimecodex.Executor{Config: runtimecodex.Config{Binary: fake, WorkRoot: dir}}
	result, err := executor.Execute(context.Background(), relay.Execution{
		RunID: "run-recover", RuntimeSessionID: "missing-thread", FallbackPrompt: "recovery transcript", Instructions: relay.CompiledInstructions{Prompt: "current turn"},
		Capabilities: relay.NewCapabilityInvoker(relay.CapabilityInvokerOptions{}), Emit: func(_ context.Context, event string, _ any) {
			resumeFailed = resumeFailed || event == "runtime.codex.thread.resume_failed"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary != "recovered" || result.RuntimeSessionID != "replacement-thread" || !resumeFailed {
		t.Fatalf("result=%+v resumeFailed=%v", result, resumeFailed)
	}
}

func TestProbeModelsReadsAppServerCatalog(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "codex")
	script := `#!/bin/sh
[ "$1" = "app-server" ] || exit 8
read -r initialize
printf '%s\n' '{"id":1,"result":{"userAgent":"fake"}}'
read -r initialized
read -r model_list
printf '%s\n' '{"id":2,"result":{"data":[{"id":"model-a","model":"model-a","displayName":"Model A","description":"Fast model","hidden":false,"isDefault":true}],"nextCursor":null}}'
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	models, err := runtimecodex.ProbeModels(context.Background(), runtimecodex.Config{Binary: fake}, runtimecodex.ForkOptions{Name: "codex", DefaultBinary: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "model-a" || models[0].DisplayName != "Model A" || !models[0].Default {
		t.Fatalf("models = %#v", models)
	}
}

func TestAppServerStreamsAssistantDeltas(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "codex")
	script := `#!/bin/sh
[ "$1" = "app-server" ] || exit 8
read -r initialize
printf '%s\n' '{"id":1,"result":{"userAgent":"fake"}}'
read -r initialized
read -r thread_start
case "$thread_start" in *'"sandbox"'*|*'"approvalPolicy"'*) exit 9;; esac
printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-stream"}}}'
read -r turn_start
printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-1"}}}'
printf '%s\n' '{"method":"item/agentMessage/delta","params":{"threadId":"thread-stream","turnId":"turn-1","itemId":"progress-1","delta":"Let me inspect this."}}'
printf '%s\n' '{"method":"item/agentMessage/delta","params":{"threadId":"thread-stream","turnId":"turn-1","itemId":"final-1","delta":"hello "}}'
printf '%s\n' '{"method":"item/agentMessage/delta","params":{"threadId":"thread-stream","turnId":"turn-1","itemId":"final-1","delta":"world"}}'
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-stream","turn":{"id":"turn-1","items":[],"status":"completed"}}}'
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var deltas []string
	var final string
	executor := runtimecodex.Executor{Config: runtimecodex.Config{Binary: fake, Protocol: "app-server", WorkRoot: dir, Ephemeral: true}}
	result, err := executor.Execute(context.Background(), relay.Execution{RunID: "run-stream", Instructions: relay.CompiledInstructions{Stable: "rules", Prompt: "say hello"}, Capabilities: relay.NewCapabilityInvoker(relay.CapabilityInvokerOptions{}), Emit: func(_ context.Context, event string, data any) {
		if event == "assistant.message.delta" {
			value := data.(map[string]string)
			deltas = append(deltas, value["delta"])
		}
		if event == "assistant.final.completed" {
			final = data.(map[string]string)["text"]
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary != "hello world" || final != "hello world" || strings.Join(deltas, "") != "Let me inspect this.hello world" {
		t.Fatalf("result=%q final=%q deltas=%q", result.Summary, final, deltas)
	}
}

func TestAppServerFallsBackWhenPersistedThreadCannotResume(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "codex")
	script := `#!/bin/sh
[ "$1" = "app-server" ] || exit 8
read -r initialize
printf '%s\n' '{"id":1,"result":{"userAgent":"fake"}}'
read -r initialized
read -r thread_resume
case "$thread_resume" in *'"method":"thread/resume"'*'"threadId":"missing-thread"'*) ;; *) exit 20;; esac
printf '%s\n' '{"id":2,"error":{"code":-32000,"message":"thread not found"}}'
read -r thread_start
case "$thread_start" in *'"method":"thread/start"'*) ;; *) exit 21;; esac
printf '%s\n' '{"id":4,"result":{"thread":{"id":"replacement-thread"}}}'
read -r turn_start
case "$turn_start" in *'recovery context'*) ;; *) exit 22;; esac
printf '%s\n' '{"id":5,"result":{"turn":{"id":"turn-2"}}}'
printf '%s\n' '{"method":"item/agentMessage/delta","params":{"threadId":"replacement-thread","turnId":"turn-2","itemId":"final-1","delta":"recovered"}}'
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"replacement-thread","turn":{"id":"turn-2","items":[],"status":"completed"}}}'
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var resumeFailed bool
	executor := runtimecodex.Executor{Config: runtimecodex.Config{Binary: fake, Protocol: "app-server", WorkRoot: dir}}
	result, err := executor.Execute(context.Background(), relay.Execution{
		RunID: "run-recover", RuntimeSessionID: "missing-thread", FallbackPrompt: "recovery context", Instructions: relay.CompiledInstructions{Prompt: "current turn"},
		Capabilities: relay.NewCapabilityInvoker(relay.CapabilityInvokerOptions{}), Emit: func(_ context.Context, event string, _ any) {
			resumeFailed = resumeFailed || event == "runtime.codex.thread.resume_failed"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary != "recovered" || result.RuntimeSessionID != "replacement-thread" || !resumeFailed {
		t.Fatalf("result=%+v resumeFailed=%v", result, resumeFailed)
	}
}

func TestAppServerRejectsPartialMessageWithoutTurnCompletion(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "codex")
	script := `#!/bin/sh
[ "$1" = "app-server" ] || exit 8
read -r initialize
printf '%s\n' '{"id":1,"result":{"userAgent":"fake"}}'
read -r initialized
read -r thread_start
printf '%s\n' '{"id":2,"result":{"thread":{"id":"thread-partial"}}}'
read -r turn_start
printf '%s\n' '{"id":3,"result":{"turn":{"id":"turn-partial"}}}'
printf '%s\n' '{"method":"item/agentMessage/delta","params":{"delta":"partial output"}}'
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var partial string
	executor := runtimecodex.Executor{Config: runtimecodex.Config{Binary: fake, Protocol: "app-server", WorkRoot: dir, Ephemeral: true}}
	_, err := executor.Execute(context.Background(), relay.Execution{RunID: "run-partial", Instructions: relay.CompiledInstructions{Prompt: "work"}, Capabilities: relay.NewCapabilityInvoker(relay.CapabilityInvokerOptions{}), Emit: func(_ context.Context, event string, data any) {
		if event == "assistant.message.delta" {
			partial += data.(map[string]string)["delta"]
		}
	}})
	if err == nil || !strings.Contains(err.Error(), "before turn/completed") {
		t.Fatalf("expected incomplete turn error, got %v", err)
	}
	if partial != "partial output" {
		t.Fatalf("partial output = %q", partial)
	}
}
