package antigravity_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
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
	executor := antigravity.Executor{Config: antigravity.Config{Config: agentcli.Config{Binary: os.Getenv("RELAY_ANTIGRAVITY_BINARY"), ToolDir: localLifecycleTools(t), WorkRoot: root, Model: model, ExtraArgs: []string{"--mode", "accept-edits"}}}}
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
		delta, final, thinking := false, false, false
		for _, event := range events {
			delta = delta || event.Type == "assistant.message.delta"
			final = final || event.Type == "assistant.final.completed"
			thinking = thinking || event.Type == "runtime.antigravity.item.reasoning.textDelta"
		}
		if !delta || !final {
			t.Fatalf("missing stream/final: delta=%v final=%v", delta, final)
		}
		for _, event := range events {
			if event.Type == "runtime.antigravity.prompt.completed" {
				t.Logf("ACP terminal metadata: %s", string(event.Data))
			}
		}
		t.Logf("current turn: thinking=%v usage=%+v", thinking, completed.Result.Usage)
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

func TestLocalACPThinking(t *testing.T) {
	if os.Getenv("RELAY_ANTIGRAVITY_SMOKE") != "1" {
		t.Skip("set RELAY_ANTIGRAVITY_SMOKE=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	model := os.Getenv("RELAY_ANTIGRAVITY_MODEL")
	if model == "" {
		model = "gemini-4-neon-medium"
	}
	executor := antigravity.Executor{Config: antigravity.Config{Config: agentcli.Config{Model: model, ToolDir: localLifecycleTools(t)}}}
	execution := relay.Execution{RunID: "thinking-probe", WorkDir: t.TempDir(), Runtime: relay.RuntimeRequirement{Model: model, ReasoningEffort: "high"}, Instructions: relay.CompiledInstructions{Stable: "Do not use tools or edit files. Answer the user's question.", Prompt: "How many integer triples (a,b,c) satisfy 1 <= a,b,c <= 30 and a+b+c=47? Answer with only the count. Do not use tools."}, Capabilities: relay.NewCapabilityInvoker(relay.CapabilityInvokerOptions{})}
	thoughtCharacters := 0
	execution.Emit = func(_ context.Context, name string, data any) {
		if name == "runtime.antigravity.item.reasoning.textDelta" {
			thoughtCharacters += len(data.(map[string]string)["delta"])
		}
	}
	result, err := executor.Execute(ctx, execution)
	if err != nil {
		t.Fatal(err)
	}
	if thoughtCharacters == 0 || result.Summary == "" || result.Usage == nil || result.Usage.ReasoningTokens == nil || *result.Usage.ReasoningTokens == 0 {
		t.Fatalf("thinking chars=%d result=%+v", thoughtCharacters, result)
	}
	t.Logf("Actual ACP thinking streamed: %d bytes; reply=%q; model=%s reasoning tokens=%d", thoughtCharacters, result.Summary, model, *result.Usage.ReasoningTokens)
}

func localLifecycleTools(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	command := exec.Command("go", "build", "-o", filepath.Join(dir, "relay-tool"), "../../cmd/relay-tool")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build lifecycle helper: %v: %s", err, output)
	}
	return dir
}

func TestLocalACPSubagents(t *testing.T) {
	if os.Getenv("RELAY_ANTIGRAVITY_SMOKE") != "1" {
		t.Skip("set RELAY_ANTIGRAVITY_SMOKE=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	model := os.Getenv("RELAY_ANTIGRAVITY_MODEL")
	if model == "" {
		model = "gemini-4-neon-medium"
	}
	executor := antigravity.Executor{Config: antigravity.Config{Config: agentcli.Config{Model: model, ToolDir: localLifecycleTools(t)}}}
	execution := relay.Execution{RunID: "native-background-probe", WorkDir: t.TempDir(), Instructions: relay.CompiledInstructions{
		Stable: "This integration test explicitly authorizes exactly one native subagent. Do not use files, commands, skills or any other agents.",
		Prompt: "Invoke exactly one native subagent (invoke_subagent), role Tiny Probe, TypeName self, asking it to calculate 47*29 without tools and reply exactly RELAY_CHILD_1363. Wait for its completion, then reply exactly RELAY_PARENT_1363. Do not access files, run commands, use skills, or spawn any other agents.",
	}, Capabilities: relay.NewCapabilityInvoker(relay.CapabilityInvokerOptions{})}
	var spawned, final, nativeIdle bool
	execution.Emit = func(_ context.Context, name string, data any) {
		if name == "runtime.antigravity.session.update" {
			raw, _ := json.Marshal(data)
			spawned = spawned || strings.Contains(string(raw), `"name":"invoke_subagent"`)
			if !strings.Contains(string(raw), `"agent_thought_chunk"`) {
				t.Logf("native update: %.1000s", raw)
			}
		}
		if strings.HasPrefix(name, "runtime.antigravity.background.") || name == "runtime.antigravity.lifecycle" {
			t.Logf("native lifecycle: %s %+v", name, data)
		}
		if name == "runtime.antigravity.lifecycle" {
			state, _ := data.(map[string]any)
			idle, _ := state["fullyIdle"].(bool)
			if state["hookEventName"] == "Stop" {
				nativeIdle = idle
			}
		}
		if name == "assistant.final.completed" && !nativeIdle {
			t.Error("published final result before parent fullyIdle notification")
		}
		final = final || name == "assistant.final.completed"
	}
	result, err := executor.Execute(ctx, execution)
	if err != nil || !spawned || !final || !nativeIdle || !strings.Contains(result.Summary, "RELAY_PARENT_1363") {
		t.Fatalf("native background completion: result=%+v spawned=%v final=%v err=%v", result, spawned, final, err)
	}
	t.Logf("Native subagent result collected before completion: %q", result.Summary)
}
