package opencode_test

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
	"github.com/KDF5000/relay/runtime/opencode"
)

func fixture(t *testing.T, version, body string) (opencode.Executor, relay.Execution) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "opencode")
	script := "#!/bin/sh\nif [ \"$1\" = '--version' ]; then echo 'opencode " + version + "'; exit 0; fi\n" + body
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return opencode.Executor{Config: opencode.Config{Binary: binary}}, relay.Execution{
		RunID: "run-opencode", WorkDir: filepath.Join(dir, "work"),
		Instructions: relay.CompiledInstructions{Stable: "stable rules\n", Prompt: "--prompt with `literal` $characters\n"},
		Capabilities: relay.NewCapabilityInvoker(relay.CapabilityInvokerOptions{}),
	}
}

const success = `
printf '%s\n' '{"type":"step_start","sessionID":"ses_test","part":{"messageID":"msg_final"}}'
printf '%s\n' '{"type":"text","sessionID":"ses_test","part":{"id":"part1","text":"Done "}}'
printf '%s\n' '{"type":"text","sessionID":"ses_test","part":{"id":"part2","text":"successfully"}}'
printf '%s\n' '{"type":"step_finish","sessionID":"ses_test","part":{"reason":"stop"}}'
`

func TestExecuteStreamsBlocksResumesAndCollectsDeclaredArtifacts(t *testing.T) {
	for _, version := range []string{"1.2.27", "v2.0.32"} {
		t.Run(version, func(t *testing.T) {
			executor, execution := fixture(t, version, `
printf '%s\n' "$@" > .relay/args.txt
cat > .relay/prompt.txt
test -n "$RELAY_TOOL_DIR" && test -n "$RELAY_TOOL_TOKEN" || exit 2
printf '%s\n' '{"type":"step_start","sessionID":"ses_test"}'
printf '%s\n' '{"type":"text","sessionID":"ses_test","part":{"text":"Working on it"}}'
printf '%s\n' '{"type":"tool_use","sessionID":"ses_test","part":{"tool":"bash","state":{"status":"completed"}}}'
printf '%s\n' '{"type":"step_finish","sessionID":"ses_test","part":{"reason":"tool-calls"}}'
echo deliverable > report.txt
echo '{"artifacts":[{"path":"report.txt","type":"report"}]}' > .relay/artifacts.json
`+success)
			execution.RuntimeSessionID = "ses_prior"
			execution.Runtime.Model = "provider/override"
			executor.Config.Model = "provider/default"
			imageData, err := relay.EncodeInputImages(relay.InputImage{Name: "screen.png", ContentType: "image/png", Data: []byte("\x89PNG\r\n\x1a\nimage fixture")})
			if err != nil {
				t.Fatal(err)
			}
			execution.Input.Data = imageData
			var events []string
			execution.Emit = func(_ context.Context, event string, _ any) { events = append(events, event) }
			result, err := executor.Execute(context.Background(), execution)
			if err != nil {
				t.Fatal(err)
			}
			if result.Summary != "Done successfully" || result.RuntimeSessionID != "ses_test" || len(result.Artifacts) != 1 {
				t.Fatalf("result = %+v", result)
			}
			args, _ := os.ReadFile(filepath.Join(execution.WorkDir, ".relay/args.txt"))
			for _, want := range []string{"run\n--format\njson", "--session\nses_prior", "--model\nprovider/override", "--file\n"} {
				if !strings.Contains(string(args), want) {
					t.Fatalf("args %q missing %q", args, want)
				}
			}
			if strings.Contains(string(args), "--standalone") != strings.HasPrefix(version, "v2") {
				t.Fatalf("incorrect version-specific flags: %q", args)
			}
			prompt, _ := os.ReadFile(filepath.Join(execution.WorkDir, ".relay/prompt.txt"))
			if string(prompt) != execution.Instructions.Prompt {
				t.Fatalf("prompt corrupted: %q", prompt)
			}
			instructions, _ := os.ReadFile(filepath.Join(execution.WorkDir, "AGENTS.md"))
			if !strings.Contains(string(instructions), "stable rules") {
				t.Fatalf("instructions = %q", instructions)
			}
			joined := strings.Join(events, ",")
			if !strings.Contains(joined, "assistant.message.completed") || !strings.Contains(joined, "assistant.final.completed") || !strings.Contains(joined, "runtime.opencode.tool_use") || strings.Contains(joined, "runtime.opencode.text") {
				t.Fatalf("events = %v", events)
			}
		})
	}
}

func TestExecuteRejectsIncompleteAndFailedStreams(t *testing.T) {
	for _, tc := range []struct{ name, stream, want string }{
		{"partial", `{"type":"text","part":{"text":"partial"}}`, "successful final step"},
		{"tool step", `{"type":"step_finish","part":{"reason":"tool-calls"}}`, "successful final step"},
		{"empty", `{"type":"step_finish","part":{"reason":"stop"}}`, "without a final message"},
		{"error v1", `{"type":"error","error":{"name":"APIError","data":{"message":"quota exhausted"}}}`, "quota exhausted"},
		{"error v2", `{"type":"error","error":{"type":"provider.quota","message":"rate limited"}}`, "rate limited"},
		{"invalid", `not JSON`, "read JSON stream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor, execution := fixture(t, "2.0.32", "printf '%s\\n' '"+tc.stream+"'\n")
			_, err := executor.Execute(context.Background(), execution)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %s", err, tc.want)
			}
		})
	}
}

func TestLostSessionUsesFallbackPrompt(t *testing.T) {
	for _, version := range []string{"1.2.27", "2.0.32"} {
		t.Run(version, func(t *testing.T) {
			executor, execution := fixture(t, version, `
if [ "$1" = 'session' ]; then echo 'Session not found: ses_lost' >&2; exit 1; fi
case "$*" in
  *--session*) echo '{"type":"error","error":{"name":"SessionNotFoundError","data":{"message":"Session not found"}}}'; exit 0 ;;
esac
cat > .relay/prompt.txt
`+success)
			execution.RuntimeSessionID = "ses_lost"
			execution.FallbackPrompt = "full conversation history"
			var recovered bool
			execution.Emit = func(_ context.Context, event string, _ any) {
				recovered = recovered || event == "runtime.opencode.session.resume_failed"
			}
			result, err := executor.Execute(context.Background(), execution)
			if err != nil || result.RuntimeSessionID != "ses_test" || !recovered {
				t.Fatalf("result = %+v, recovered = %v, err = %v", result, recovered, err)
			}
			prompt, _ := os.ReadFile(filepath.Join(execution.WorkDir, ".relay/prompt.txt"))
			if string(prompt) != execution.FallbackPrompt {
				t.Fatalf("fallback prompt = %q", prompt)
			}
		})
	}
}

func TestEphemeralStartsFreshAndPlainAnswerHasNoArtifacts(t *testing.T) {
	executor, execution := fixture(t, "2.0.32", "printf '%s\\n' \"$@\" > .relay/args.txt\n"+success)
	executor.Config.Ephemeral = true
	execution.RuntimeSessionID = "ses_prior"
	result, err := executor.Execute(context.Background(), execution)
	if err != nil || result.RuntimeSessionID != "" || len(result.Artifacts) != 0 {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	args, _ := os.ReadFile(filepath.Join(execution.WorkDir, ".relay/args.txt"))
	if strings.Contains(string(args), "--session") {
		t.Fatalf("ephemeral run resumed session: %q", args)
	}
}

func TestLargeJSONTextBlock(t *testing.T) {
	executor, execution := fixture(t, "2.0.32", "cat stream.json\n")
	if err := os.MkdirAll(execution.WorkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	message := strings.Repeat("x", 5<<20)
	text, _ := json.Marshal(map[string]any{"type": "text", "sessionID": "ses_large", "part": map[string]string{"text": message}})
	stream := append(text, []byte("\n{\"type\":\"step_finish\",\"part\":{\"reason\":\"stop\"}}\n")...)
	if err := os.WriteFile(filepath.Join(execution.WorkDir, "stream.json"), stream, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), execution)
	if err != nil || len(result.Summary) != len(message) {
		t.Fatalf("summary length = %d, err = %v", len(result.Summary), err)
	}
}

func TestCancellationTerminatesProcess(t *testing.T) {
	executor, execution := fixture(t, "2.0.32", "sleep 30\n"+success)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := executor.Execute(ctx, execution)
	if err == nil || time.Since(start) > 6*time.Second {
		t.Fatalf("cancellation took %s, err = %v", time.Since(start), err)
	}
}

func TestModelDiscovery(t *testing.T) {
	for _, version := range []string{"1.2.27", "2.0.32"} {
		t.Run(version, func(t *testing.T) {
			executor, execution := fixture(t, version, `
if [ "$1" != 'models' ]; then exit 2; fi
printf '%s\n' "$@" > args.txt
printf '%s\n' 'provider/model-a' 'provider/model-b' 'provider/model-a' 'diagnostic line' '/invalid'
`)
			executor.Config.WorkDir = filepath.Dir(execution.WorkDir)
			executor.Config.Model = "provider/model-b"
			models, err := opencode.ProbeModels(context.Background(), executor.Config)
			if err != nil || len(models) != 2 || models[0].ID != "provider/model-a" || !models[1].Default {
				t.Fatalf("models = %+v, err = %v", models, err)
			}
			args, _ := os.ReadFile(filepath.Join(executor.Config.WorkDir, "args.txt"))
			if strings.Contains(string(args), "--standalone") != strings.HasPrefix(version, "2") {
				t.Fatalf("args = %q", args)
			}
		})
	}
}
