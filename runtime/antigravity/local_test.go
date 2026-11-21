package antigravity_test

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/binding"
	"github.com/KDF5000/relay/controlplane"
	"github.com/KDF5000/relay/node"
	"github.com/KDF5000/relay/runtime/agentcli"
	"github.com/KDF5000/relay/runtime/antigravity"
	"github.com/KDF5000/relay/sdk"
	"github.com/KDF5000/relay/transport/httpapi"
)

// Opt in because this uses the locally authenticated CLI and model quota.
func TestLocalCLI(t *testing.T) {
	if os.Getenv("RELAY_ANTIGRAVITY_SMOKE") != "1" {
		t.Skip("set RELAY_ANTIGRAVITY_SMOKE=1 to test the installed agy CLI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	model := os.Getenv("RELAY_ANTIGRAVITY_MODEL")
	root := t.TempDir()
	executor := antigravity.Executor{Config: antigravity.Config{Config: agentcli.Config{Binary: os.Getenv("RELAY_ANTIGRAVITY_BINARY"), WorkRoot: root, Model: model, ExtraArgs: []string{"--mode", "accept-edits"}}}}
	version, err := antigravity.ProbeVersion(ctx, executor.Config.Binary)
	if err != nil {
		t.Fatal(err)
	}
	models, err := antigravity.ProbeModels(ctx, executor.Config)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("agy=%s catalog=%d models configured=%s", version, len(models), model)
	service := controlplane.New(time.Minute)
	server := httptest.NewServer(httpapi.NewHandler(service))
	defer server.Close()
	transport := httpapi.NewClient(server.URL)
	host := sdk.New(transport)
	worker := &node.Worker{Registration: controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "antigravity-smoke", Capacity: 1, Runtimes: []controlplane.Runtime{{Provider: "antigravity", Version: version, State: "healthy"}}}, ControlPlane: transport, Bindings: binding.NewRegistry(), Executors: node.ExecutorMap{"antigravity": executor}}
	if _, err := worker.Register(ctx); err != nil {
		t.Fatal(err)
	}
	submit := func(key, prompt string) relay.Run {
		t.Helper()
		queued, err := host.Submit(ctx, relay.Request{SessionID: "antigravity-smoke-session", AgentID: "antigravity-smoke", IdempotencyKey: key, Runtime: relay.RuntimeRequirement{Provider: "antigravity", Model: model}, Input: relay.Input{Type: "task", Version: "1", Prompt: prompt}, Instructions: relay.InstructionBundle{Runtime: []relay.InstructionFragment{{ID: "smoke-rule", Version: "1", Title: "Test rule", Content: "For this test, the secret from the workspace instructions is RELAY_RULES_7164. When asked for the secret, reply with only that value. Do not use tools unless the prompt explicitly requests them."}}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := worker.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		completed, err := host.Run(ctx, queued.ID)
		if err != nil {
			t.Fatal(err)
		}
		if completed.Status != relay.RunSucceeded {
			t.Fatalf("run=%+v", completed)
		}
		events, err := host.Events(ctx, queued.ID)
		if err != nil {
			t.Fatal(err)
		}
		delta, final := false, false
		for _, event := range events {
			delta = delta || event.Type == "assistant.message.delta"
			final = final || event.Type == "assistant.final.completed"
		}
		if !delta || !final {
			t.Fatalf("missing stream/final: events=%+v", events)
		}
		return completed
	}
	first := submit("first", "Remember the conversation token RELAY_MEMORY_8372. Reply with only the secret defined by the workspace instructions.")
	if strings.TrimSpace(first.Result.Summary) != "RELAY_RULES_7164" || first.Result.RuntimeSessionID == "" {
		t.Fatalf("first=%+v", first)
	}
	second := submit("second", "Reply with only the conversation token I told you in my previous message.")
	if strings.TrimSpace(second.Result.Summary) != "RELAY_MEMORY_8372" || second.Result.RuntimeSessionID != first.Result.RuntimeSessionID {
		t.Fatalf("resume-result=%+v first-session=%s", second.Result, first.Result.RuntimeSessionID)
	}
	t.Logf("Relay execution, instruction loading, streaming and native resume passed; conversation=%s", first.Result.RuntimeSessionID)
	// Exercise the missing-session path against the real CLI, including the
	// complete-history prompt chosen before the first user message is sent.
	execution := relay.Execution{RunID: "recovery", WorkDir: filepath.Join(root, "recovery"), RuntimeSessionID: "00000000-0000-0000-0000-000000000000", Instructions: relay.CompiledInstructions{Stable: "Do not use tools.", Prompt: "Reply with exactly WRONG_CONTINUATION."}, FallbackPrompt: "Reply with exactly RELAY_RECOVERY_OK. Do not use tools.", Capabilities: relay.NewCapabilityInvoker(relay.CapabilityInvokerOptions{})}
	var recovered bool
	execution.Emit = func(_ context.Context, event string, _ any) {
		recovered = recovered || event == "runtime.antigravity.session.resume_failed"
	}
	result, err := executor.Execute(ctx, execution)
	if err != nil || result.Summary != "RELAY_RECOVERY_OK" || !recovered {
		t.Fatalf("recovery=%+v event=%v err=%v", result, recovered, err)
	}
	t.Log("missing conversation recovered with the fallback prompt")
	// A file edit verifies that tool policy, work directory and process lifetime
	// are usable for coding tasks, beyond a text-only connectivity check.
	execution.RunID = "edit"
	execution.RuntimeSessionID = ""
	execution.FallbackPrompt = ""
	execution.Instructions.Stable = "Follow the prompt exactly. File tools may be used to create smoke.txt in this test workspace."
	execution.Instructions.Prompt = "Create smoke.txt in the current workspace containing exactly RELAY_EDIT_OK. Then reply exactly RELAY_EDIT_DONE."
	result, err = executor.Execute(ctx, execution)
	content, readErr := os.ReadFile(filepath.Join(execution.WorkDir, "smoke.txt"))
	if err != nil || readErr != nil || strings.TrimSpace(string(content)) != "RELAY_EDIT_OK" || result.Summary != "RELAY_EDIT_DONE" {
		t.Fatalf("edit-summary=%q err=%v read=%v content=%q", result.Summary, err, readErr, content)
	}
	t.Log("workspace file edit passed under --mode accept-edits")
}
