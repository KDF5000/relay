package antigravity_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/runtime/agentcli"
	"github.com/KDF5000/relay/runtime/antigravity"
)

const initEvent = `printf '%s\n' '{"event":"init","conversation_id":"conversation-1"}'
`
const successEvent = `printf '%s\n' '{"event":"result","result":{"status":"SUCCESS","response":"done","conversation_id":"conversation-1"}}'
`

func fixture(t *testing.T, script string) (antigravity.Executor, relay.Execution) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix shell fixture")
	}
	root := t.TempDir()
	binary := filepath.Join(root, "agy")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	return antigravity.Executor{Config: antigravity.Config{Config: agentcli.Config{Binary: binary}}}, relay.Execution{
		RunID: "test", WorkDir: filepath.Join(root, "work"),
		Instructions: relay.CompiledInstructions{Stable: "stable instructions", Prompt: "prompt"},
		Capabilities: relay.NewCapabilityInvoker(relay.CapabilityInvokerOptions{}),
	}
}

func TestStreamingAndOverrides(t *testing.T) {
	executor, execution := fixture(t, `printf '%s\n' "$@" > .relay/args.txt
`+initEvent+`cat > .relay/input.json
printf '%s\n' '{"event":"step_update","step_update":{"step_type":"tool","text_delta":"hidden"}}'
printf '%s\n' '{"event":"step_update","step_update":{"step_type":"agent_response","state":"ACTIVE","text_delta":"do"}}'
printf '%s\n' '{"event":"step_update","step_update":{"step_type":"agent_response","state":"DONE","text_delta":"ne"}}'
`+successEvent)
	executor.Config.Model = "default-model"
	executor.Config.ReasoningEffort = "low"
	execution.Runtime = relay.RuntimeRequirement{Model: "selected-model", ReasoningEffort: "high"}
	execution.RuntimeSessionID = "conversation-1"
	execution.Instructions.Prompt = "--flag-like prompt\n" + strings.Repeat("long ", 50000)
	var deltas []string
	var final bool
	execution.Emit = func(_ context.Context, event string, value any) {
		if event == "assistant.message.delta" {
			deltas = append(deltas, value.(map[string]string)["delta"])
		}
		if event == "assistant.final.completed" {
			final = true
		}
	}
	result, err := executor.Execute(context.Background(), execution)
	if err != nil || result.Summary != "done" || result.RuntimeSessionID != "conversation-1" || len(result.Artifacts) != 0 || strings.Join(deltas, "") != "done" || !final {
		t.Fatalf("result=%+v deltas=%v final=%v err=%v", result, deltas, final, err)
	}
	args, _ := os.ReadFile(filepath.Join(execution.WorkDir, ".relay/args.txt"))
	for _, want := range []string{"--input-format\nstream-json", "--conversation\nconversation-1", "--model\nselected-model", "--effort\nhigh", "--disable-slash-commands"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("missing %q in %s", want, args)
		}
	}
	data, _ := os.ReadFile(filepath.Join(execution.WorkDir, ".relay/input.json"))
	var input struct {
		Event   string
		Message struct{ Content string }
	}
	if err := json.Unmarshal(data, &input); err != nil || input.Event != "user" || input.Message.Content != execution.Instructions.Prompt {
		t.Fatalf("stdin prompt not preserved: %v", err)
	}
	rules, _ := os.ReadFile(filepath.Join(execution.WorkDir, "AGENTS.md"))
	if !strings.Contains(string(rules), execution.Instructions.Stable) {
		t.Fatal("instructions missing")
	}
}

func TestFailedAndIncompleteStreams(t *testing.T) {
	for _, tc := range []struct{ name, script, want string }{
		{"partial", initEvent + `cat >/dev/null; printf '%s\n' '{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"partial"}}'`, "before the result"},
		{"runtime-error", initEvent + `cat >/dev/null; printf '%s\n' '{"event":"result","result":{"status":"ERROR","response":"partial","error":"location unavailable"}}'`, "location unavailable"},
		{"waiting", initEvent + `cat >/dev/null; printf '%s\n' '{"event":"result","result":{"status":"WAITING","response":"partial"}}'`, "WAITING"},
		{"empty", initEvent + `cat >/dev/null; printf '%s\n' '{"event":"result","result":{"status":"SUCCESS","response":""}}'`, "without a final message"},
		{"missing-init", successEvent, "before prompt initialization"},
		{"missing-id", `printf '%s\n' '{"event":"init"}'; cat >/dev/null; printf '%s\n' '{"event":"result","result":{"status":"SUCCESS","response":"done"}}'`, "without a conversation ID"},
		{"crash", initEvent + "cat >/dev/null; echo boom >&2; exit 7\n", "boom"},
		{"invalid", "echo not-json\n", "read JSON stream"},
		{"duplicate-result", initEvent + "cat >/dev/null\n" + successEvent + successEvent, "multiple result events"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor, execution := fixture(t, tc.script)
			_, err := executor.Execute(context.Background(), execution)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want=%s", err, tc.want)
			}
		})
	}
}

func TestMissingConversationRecoversBeforeSendingPrompt(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(map[bool]string{false: "without-fallback", true: "with-fallback"}[fallback], func(t *testing.T) {
			executor, execution := fixture(t, `case "$*" in
 *--conversation*)
  printf '%s\n' '{"event":"init","conversation_id":"wrong-new-conversation"}'
  if read -r prompt; then echo unexpected-prompt > .relay/unsafe.txt; fi
  exit 0 ;;
esac
`+initEvent+"cat > .relay/input.json\n"+successEvent)
			execution.RuntimeSessionID = "lost-id"
			if fallback {
				execution.FallbackPrompt = "full history"
			}
			var recovered bool
			execution.Emit = func(_ context.Context, event string, _ any) {
				recovered = recovered || event == "runtime.antigravity.session.resume_failed"
			}
			result, err := executor.Execute(context.Background(), execution)
			if !recovered {
				t.Fatal("missing recovery event")
			}
			if _, err := os.Stat(filepath.Join(execution.WorkDir, ".relay/unsafe.txt")); !os.IsNotExist(err) {
				t.Fatal("sent continuation into a fresh conversation")
			}
			if fallback {
				if err != nil || result.RuntimeSessionID != "conversation-1" {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				input, _ := os.ReadFile(filepath.Join(execution.WorkDir, ".relay/input.json"))
				if !strings.Contains(string(input), "full history") {
					t.Fatalf("fallback=%s", input)
				}
			} else if err == nil || !strings.Contains(err.Error(), "no fallback") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestEphemeralStartsFreshConversation(t *testing.T) {
	executor, execution := fixture(t, "printf '%s\\n' \"$@\" > .relay/args.txt\n"+initEvent+"cat >/dev/null\n"+successEvent)
	executor.Config.Ephemeral = true
	execution.RuntimeSessionID = "prior"
	result, err := executor.Execute(context.Background(), execution)
	args, _ := os.ReadFile(filepath.Join(execution.WorkDir, ".relay/args.txt"))
	if err != nil || result.RuntimeSessionID != "" || strings.Contains(string(args), "--conversation") {
		t.Fatalf("result=%+v err=%v args=%s", result, err, args)
	}
}

func TestPermissionListsRejected(t *testing.T) {
	executor, execution := fixture(t, successEvent)
	executor.Config.AllowedTools = []string{"Write"}
	if _, err := executor.Execute(context.Background(), execution); err == nil {
		t.Fatal("unsupported tool policy accepted")
	}
}

func TestCancellation(t *testing.T) {
	executor, execution := fixture(t, initEvent+"cat >/dev/null\nsleep 30\n")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := executor.Execute(ctx, execution)
	if err != context.DeadlineExceeded || time.Since(start) > 6*time.Second {
		t.Fatalf("elapsed=%s err=%v", time.Since(start), err)
	}
}

func TestModelDiscoveryAndVersion(t *testing.T) {
	executor, _ := fixture(t, `case "$1" in
 --version) echo '1.5.13'; exit 0 ;;
 models) printf '%s\n' 'Fetching available models...' 'gemini-test\tGemini Test' 'claude-test\tClaude Test' 'claude-test\tClaude Test' | sed 's/\\t/\t/g' ;;
 *) exit 2 ;;
esac
`)
	executor.Config.Model = "claude-test"
	version, err := antigravity.ProbeVersion(context.Background(), executor.Config.Binary)
	if err != nil || version != "1.5.13" {
		t.Fatalf("version=%s err=%v", version, err)
	}
	models, err := antigravity.ProbeModels(context.Background(), executor.Config)
	if err != nil || len(models) != 2 || models[0].Default || !models[1].Default || models[1].DisplayName != "Claude Test" {
		t.Fatalf("models=%+v err=%v", models, err)
	}
	executor.Config.Model = ""
	models, err = antigravity.ProbeModels(context.Background(), executor.Config)
	if err != nil || models[0].Default || models[1].Default {
		t.Fatalf("invented a default: models=%+v err=%v", models, err)
	}
}

func TestImagesAreRejectedBeforeStartingCLI(t *testing.T) {
	executor, execution := fixture(t, "touch .relay/started\n"+initEvent+"cat >/dev/null\n"+successEvent)
	data, err := relay.EncodeInputImages(relay.InputImage{Name: "screen.png", ContentType: "image/png", Data: []byte("\x89PNG\r\n\x1a\nfixture")})
	if err != nil {
		t.Fatal(err)
	}
	execution.Input.Data = data
	_, err = executor.Execute(context.Background(), execution)
	if err == nil || !strings.Contains(err.Error(), "image attachments are unsupported") {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(execution.WorkDir, ".relay/started")); !os.IsNotExist(err) {
		t.Fatal("CLI started with an unsupported attachment")
	}
}

func TestEmptyModelCatalogIsAnError(t *testing.T) {
	executor, _ := fixture(t, "echo 'Fetching available models...'\n")
	if _, err := antigravity.ProbeModels(context.Background(), executor.Config); err == nil {
		t.Fatal("empty catalog treated as successful discovery")
	}
}
