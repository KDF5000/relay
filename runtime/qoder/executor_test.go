package qoder_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/runtime/qoder"
)

func fixture(t *testing.T, variant qoder.Variant, body string) (qoder.Executor, relay.Execution) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, variant.Binary)
	script := "#!/bin/sh\nif [ \"$1\" = '--version' ]; then echo '1.1.26'; exit 0; fi\n" + body
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return qoder.Executor{Config: qoder.Config{Binary: binary}, Variant: variant}, relay.Execution{
		RunID: "run-qoder", WorkDir: filepath.Join(dir, "work"),
		Instructions: relay.CompiledInstructions{Stable: "stable rules\n", Prompt: "--prompt with `literal` $characters\n"},
		Capabilities: relay.NewCapabilityInvoker(relay.CapabilityInvokerOptions{}),
	}
}

// success mirrors a real qodercli stream: hook noise, init, a thinking block and
// a text block for the same message, then the result.
const success = `
printf '%s\n' '{"type":"system","subtype":"hook_started","hook_name":"SessionStart","session_id":"sess-new"}'
printf '%s\n' '{"type":"system","subtype":"hook_response","stdout":"secret hook output","session_id":"sess-new"}'
printf '%s\n' '{"type":"system","subtype":"init","model":"Auto","session_id":"sess-new"}'
printf '%s\n' '{"type":"assistant","message":{"id":"m1","content":[{"type":"thinking","thinking":"internal"}]},"session_id":"sess-new"}'
printf '%s\n' '{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"Done successfully"}]},"session_id":"sess-new"}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"Done successfully","session_id":"sess-new"}'
`

func TestExecuteStreamsResultResumesAndCollectsArtifacts(t *testing.T) {
	executor, execution := fixture(t, qoder.Qoder, `
printf '%s\n' "$@" > .relay/args.txt
cat > .relay/prompt.txt
test -n "$RELAY_TOOL_DIR" && test -n "$RELAY_TOOL_TOKEN" || exit 2
test -z "$UNRELATED_SECRET" || exit 3
echo deliverable > report.txt
echo '{"artifacts":[{"path":"report.txt","type":"report"}]}' > .relay/artifacts.json
`+success)
	t.Setenv("UNRELATED_SECRET", "leak")
	execution.RuntimeSessionID = "sess-prior"
	execution.Runtime.Model = "Qwen3.8-Max"
	executor.Config.Model = "Auto"
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
	if result.Summary != "Done successfully" || result.RuntimeSessionID != "sess-new" || len(result.Artifacts) != 1 {
		t.Fatalf("result = %+v", result)
	}
	args, _ := os.ReadFile(filepath.Join(execution.WorkDir, ".relay/args.txt"))
	for _, want := range []string{"--print\n--output-format\nstream-json", "--resume\nsess-prior", "--model\nQwen3.8-Max", "--attachment\n"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("args %q missing %q", args, want)
		}
	}
	if strings.Contains(string(args), "--no-session-persistence") {
		t.Fatalf("persistent run disabled sessions: %q", args)
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
	for _, want := range []string{"runtime.qoder.started", "runtime.qoder.system", "runtime.qoder.result", "assistant.message.completed", "assistant.final.completed", "runtime.qoder.completed"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("events %v missing %s", events, want)
		}
	}
	if count(events, "runtime.qoder.system") != 1 || count(events, "assistant.message.completed") != 1 {
		t.Fatalf("hook or thinking events leaked into the stream: %v", events)
	}
}

func TestQwenWorkVariantUsesItsOwnNamespace(t *testing.T) {
	executor, execution := fixture(t, qoder.QwenWork, success)
	var events []string
	execution.Emit = func(_ context.Context, event string, _ any) { events = append(events, event) }
	result, err := executor.Execute(context.Background(), execution)
	if err != nil || result.RuntimeSessionID != "sess-new" {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	joined := strings.Join(events, ",")
	if !strings.Contains(joined, "runtime.qwenwork.completed") || strings.Contains(joined, "runtime.qoder.") {
		t.Fatalf("events = %v", events)
	}
	if _, err := os.Stat(filepath.Join(execution.WorkDir, "AGENTS.md")); err != nil {
		t.Fatalf("AGENTS.md: %v", err)
	}
}

func TestExecuteRejectsIncompleteAndFailedStreams(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"partial", `printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"partial"}]},"session_id":"s"}'`, "before the result event"},
		{"empty", `printf '%s\n' '{"type":"result","is_error":false,"result":"","session_id":"s"}'`, "without a final message"},
		{"error", `printf '%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"quota exhausted","session_id":"s"}'`, "quota exhausted"},
		{"login", `printf '%s\n' '{"type":"result","subtype":"success","is_error":true,"result":"Not logged in · Please run /login"}'; exit 1`, "qoderclicn login"},
		{"crash", `echo 'boom' >&2; exit 7`, "boom"},
		{"invalid", `echo 'not JSON'`, "read JSON stream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor, execution := fixture(t, qoder.QwenWork, tc.body+"\n")
			_, err := executor.Execute(context.Background(), execution)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %s", err, tc.want)
			}
		})
	}
}

func TestLostSessionUsesFallbackPrompt(t *testing.T) {
	executor, execution := fixture(t, qoder.Qoder, `
case "$*" in
  *--resume*) echo 'Error resuming session: Invalid session identifier "sess-lost"' >&2; exit 42 ;;
esac
cat > .relay/prompt.txt
`+success)
	execution.RuntimeSessionID = "sess-lost"
	execution.FallbackPrompt = "full conversation history"
	var recovered bool
	execution.Emit = func(_ context.Context, event string, _ any) {
		recovered = recovered || event == "runtime.qoder.session.resume_failed"
	}
	result, err := executor.Execute(context.Background(), execution)
	if err != nil || result.RuntimeSessionID != "sess-new" || !recovered {
		t.Fatalf("result = %+v, recovered = %v, err = %v", result, recovered, err)
	}
	prompt, _ := os.ReadFile(filepath.Join(execution.WorkDir, ".relay/prompt.txt"))
	if string(prompt) != execution.FallbackPrompt {
		t.Fatalf("fallback prompt = %q", prompt)
	}
}

func TestEphemeralDisablesPersistenceAndOmitsSession(t *testing.T) {
	executor, execution := fixture(t, qoder.Qoder, "printf '%s\\n' \"$@\" > .relay/args.txt\n"+success)
	executor.Config.Ephemeral = true
	execution.RuntimeSessionID = "sess-prior"
	result, err := executor.Execute(context.Background(), execution)
	if err != nil || result.RuntimeSessionID != "" || len(result.Artifacts) != 0 {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	args, _ := os.ReadFile(filepath.Join(execution.WorkDir, ".relay/args.txt"))
	if strings.Contains(string(args), "--resume") || !strings.Contains(string(args), "--no-session-persistence") {
		t.Fatalf("ephemeral args = %q", args)
	}
}

func TestToolListsAreRejected(t *testing.T) {
	executor, execution := fixture(t, qoder.Qoder, success)
	executor.Config.AllowedTools = []string{"Write"}
	if _, err := executor.Execute(context.Background(), execution); err == nil || !strings.Contains(err.Error(), "--permission-mode") {
		t.Fatalf("err = %v", err)
	}
}

func TestCancellationTerminatesProcess(t *testing.T) {
	executor, execution := fixture(t, qoder.Qoder, "sleep 30\n"+success)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := executor.Execute(ctx, execution)
	if err == nil || time.Since(start) > 6*time.Second {
		t.Fatalf("cancellation took %s, err = %v", time.Since(start), err)
	}
}

func TestProbeVersionIgnoresEmbeddingSDKEnvironment(t *testing.T) {
	executor, _ := fixture(t, qoder.Qoder, "")
	script := "#!/bin/sh\ntest -z \"$QODER_AGENT_SDK_ENTRYPOINT\" || { echo 'sdk_invalid_args' >&2; exit 1; }\necho '1.1.26'\n"
	if err := os.WriteFile(executor.Config.Binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QODER_AGENT_SDK_ENTRYPOINT", "desktop")
	version, err := qoder.ProbeVersion(context.Background(), executor.Config.Binary)
	if err != nil || version != "1.1.26" {
		t.Fatalf("version = %q, err = %v", version, err)
	}
}

func TestModelDiscovery(t *testing.T) {
	executor, _ := fixture(t, qoder.Qoder, `
if [ "$1" != '--list-models' ]; then exit 2; fi
printf '%s\n' 'MODEL' 'Auto' 'Ultimate' 'Qwen3.8-Max' 'Auto' 'Team Model (mode-abc123)' ''
`)
	models, err := qoder.ProbeModels(context.Background(), executor.Config, qoder.Qoder)
	if err != nil || len(models) != 4 {
		t.Fatalf("models = %+v, err = %v", models, err)
	}
	if models[0].ID != "Auto" || !models[0].Default || models[3].ID != "mode-abc123" || models[3].DisplayName != "Team Model" {
		t.Fatalf("models = %+v", models)
	}
	executor.Config.Model = "Qwen3.8-Max"
	models, _ = qoder.ProbeModels(context.Background(), executor.Config, qoder.Qoder)
	if models[0].Default || !models[2].Default {
		t.Fatalf("configured default ignored: %+v", models)
	}
}

func TestModelDiscoveryReportsLogin(t *testing.T) {
	executor, _ := fixture(t, qoder.QwenWork, "echo 'Not logged in. Run `qoderclicn login` to authenticate.' >&2; exit 1\n")
	_, err := qoder.ProbeModels(context.Background(), executor.Config, qoder.QwenWork)
	if err == nil || !strings.Contains(err.Error(), "relay qwenwork") || !strings.Contains(err.Error(), "qoderclicn login") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveBinaryPrefersConfiguredThenPath(t *testing.T) {
	if got := qoder.ResolveBinary(qoder.QwenWork, "/opt/custom"); got != "/opt/custom" {
		t.Fatalf("configured = %q", got)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "qoderclicn"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if got := qoder.ResolveBinary(qoder.QwenWork, ""); got != "qoderclicn" {
		t.Fatalf("PATH lookup = %q", got)
	}
}

func count(values []string, expected string) int {
	total := 0
	for _, value := range values {
		if value == expected {
			total++
		}
	}
	return total
}
