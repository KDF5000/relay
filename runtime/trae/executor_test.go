package trae_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KDF5000/relay"
	runtimetrae "github.com/KDF5000/relay/runtime/trae"
)

func TestExecutorUsesTraeExecContract(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "traex")
	script := `#!/bin/sh
out=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--output-last-message" ]; then out="$2"; shift 2; continue; fi
  shift
done
cat >/dev/null
[ -n "$RELAY_TOOL_DIR" ] || exit 10
[ -n "$RELAY_TOOL_TOKEN" ] || exit 11
[ -n "$TRAE_HOME" ] || exit 12
[ -z "$MULTICA_TOKEN" ] || exit 13
printf '%s\n' '{"type":"thread.started","thread_id":"trae-thread"}'
printf '%s' 'TRAE_RUNTIME_OK' > "$out"
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TRAE_HOME", filepath.Join(dir, "trae-home"))
	t.Setenv("MULTICA_TOKEN", "secret")
	var events []string
	executor := runtimetrae.Executor{Config: runtimetrae.Config{Binary: fake, WorkRoot: dir, Ephemeral: true, PermissionMode: "default"}}
	result, err := executor.Execute(context.Background(), relay.Execution{RunID: "run-trae", Instructions: relay.CompiledInstructions{Stable: "rules", Prompt: "work"}, Capabilities: relay.NewCapabilityInvoker(relay.CapabilityInvokerOptions{}), Emit: func(_ context.Context, event string, _ any) { events = append(events, event) }})
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary != "TRAE_RUNTIME_OK" {
		t.Fatalf("summary=%q", result.Summary)
	}
	if len(events) != 4 || events[1] != "runtime.trae.thread.started" || events[2] != "assistant.final.completed" {
		t.Fatalf("events=%v", events)
	}
	if result.Artifacts[1].Type != "trae_final_message" {
		t.Fatalf("artifact=%+v", result.Artifacts[1])
	}
	content, err := os.ReadFile(filepath.Join(dir, "run-trae", "AGENTS.md"))
	if err != nil || !strings.Contains(string(content), "rules") {
		t.Fatalf("AGENTS.md=%q, %v", content, err)
	}
}

func TestProbeVersionNormalizesInternalEdition(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "traex")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho 'traecli 0.202.3(internal edition)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	version, err := runtimetrae.ProbeVersion(context.Background(), fake)
	if err != nil || version != "0.202.3" {
		t.Fatalf("version=%q err=%v", version, err)
	}
}

func TestExecutorRejectsInteractivePermissionModes(t *testing.T) {
	executor := runtimetrae.Executor{Config: runtimetrae.Config{PermissionMode: "auto"}}
	_, err := executor.Execute(context.Background(), relay.Execution{})
	if err == nil || !strings.Contains(err.Error(), "headless permission mode") {
		t.Fatalf("error=%v", err)
	}
}
