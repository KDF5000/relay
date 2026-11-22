package antigravity_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
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

const acpFixture = `#!/usr/bin/env python3
import json,sys,os,time
scenario=os.environ.get('ACP_TEST_CASE','normal')
loaded=False
native_id='native-session'
open('.relay/args.txt','w').write('\n'.join(sys.argv[1:]))
def send(v): print(json.dumps(v),flush=True)
def update(u): send({'jsonrpc':'2.0','method':'session/update','params':{'sessionId':native_id,'update':u}})
def text(kind,value): update({'sessionUpdate':kind,'content':{'type':'text','text':value}})
for line in sys.stdin:
 r=json.loads(line)
 if not r.get('method'): continue
 method=r['method'];p=r.get('params',{})
 with open('.relay/rpc.jsonl','a') as f:f.write(json.dumps(r)+'\n')
 answer={}
 if method=='initialize': answer={'protocolVersion':1,'agentCapabilities':{'loadSession':True,'promptCapabilities':{'image':True}}}
 elif method=='session/load':
  loaded=True;native_id=p['sessionId']
  if p['sessionId']=='missing' and scenario not in ('late-missing','late-failure'):
   send({'jsonrpc':'2.0','id':r['id'],'error':{'code':-32000,'message':'conversation not found'}});continue
  text('agent_thought_chunk','HISTORICAL THINKING')
  text('agent_message_chunk','HISTORICAL ANSWER')
  answer={'sessionId':'different' if scenario=='mismatch' else p['sessionId'],'configOptions':[{'id':'thought_level','category':'thought_level'}]}
 elif method=='session/new': answer={'sessionId':'native-session','configOptions':[{'id':'thought_level','category':'thought_level'}]}
 elif method=='session/set_model' and scenario=='invalid-model':
  send({'jsonrpc':'2.0','id':r['id'],'error':{'code':-32602,'message':'unknown model'}});continue
 elif method=='session/prompt':
  if loaded and scenario=='echo-only':
   send({'jsonrpc':'2.0','id':r['id'],'result':{'stopReason':'end_turn'}});continue
  if scenario in ('late-missing','late-failure') and p['sessionId']=='missing':
   if scenario=='late-failure':text('agent_thought_chunk','Execution already began')
   send({'jsonrpc':'2.0','id':r['id'],'error':{'code':-32603,'message':'Failed to send user message: not_found: trajectory not found'}});continue
  if scenario=='hang': time.sleep(60);continue
  if scenario=='eof':sys.exit(0)
  text('agent_thought_chunk','Think ')
  if loaded and scenario=='active-empty':
   send({'jsonrpc':'2.0','id':r['id'],'result':{'stopReason':'end_turn'}});continue
  text('agent_thought_chunk','carefully.')
  text('agent_message_chunk','Checking the workspace.')
  update({'sessionUpdate':'tool_call','toolCallId':'cmd','kind':'execute','title':'Run command','status':'in_progress','rawInput':{'CommandLine':'git status --short'}})
  update({'sessionUpdate':'tool_call_update','toolCallId':'cmd','status':'completed','content':[{'type':'content','content':{'type':'text','text':'clean'}}]})
  text('agent_thought_chunk','Now answer.')
  if scenario=='permission':
   send({'jsonrpc':'2.0','id':'permission-id','method':'session/request_permission','params':{'toolCall':{'title':'Edit file'},'options':[{'optionId':'once','kind':'allow_once'},{'optionId':'always','kind':'allow_always'}]}})
   decision=json.loads(sys.stdin.readline())
   with open('.relay/permission.json','w') as f:json.dump(decision,f)
  if scenario!='empty':
   text('agent_message_chunk','final ')
   text('agent_message_chunk','answer')
  answer={'stopReason':'cancelled' if scenario=='cancelled' else 'end_turn','usage':{'inputTokens':100,'outputTokens':20,'thoughtTokens':10,'cachedReadTokens':5,'totalTokens':120}}
 send({'jsonrpc':'2.0','id':r['id'],'result':answer})
 if method=='session/load':
  if scenario=='late-history':
   time.sleep(.1)
   send({'jsonrpc':'2.0','method':'session/update','params':{'sessionId':'another-session','update':{'sessionUpdate':'session_info_update'}}})
   text('agent_thought_chunk','DELAYED HISTORICAL THINKING')
   text('agent_message_chunk','DELAYED HISTORICAL ANSWER')
   update({'sessionUpdate':'tool_call','toolCallId':'old-tool','kind':'execute','title':'OLD COMMAND','status':'completed'})
  if scenario!='missing-replay-end':update({'sessionUpdate':'session_info_update'})
`

func acpTestFixture(t *testing.T, scenario string) (antigravity.Executor, relay.Execution) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix fixture")
	}
	root := t.TempDir()
	binary := filepath.Join(root, "agy")
	if err := os.WriteFile(binary, []byte(acpFixture), 0700); err != nil {
		t.Fatal(err)
	}
	return antigravity.Executor{Config: antigravity.Config{Config: agentcli.Config{Binary: binary, Env: map[string]string{"ACP_TEST_CASE": scenario}}}}, relay.Execution{RunID: "acp-test", WorkDir: filepath.Join(root, "work"), Instructions: relay.CompiledInstructions{Stable: "workspace rules", Prompt: "current user request"}, Capabilities: relay.NewCapabilityInvoker(relay.CapabilityInvokerOptions{})}
}
func calls(t *testing.T, execution relay.Execution) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(execution.WorkDir, ".relay/rpc.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var result []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var value map[string]any
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			t.Fatal(err)
		}
		result = append(result, value)
	}
	return result
}
func TestACPThinkingToolsResumeAndOptions(t *testing.T) {
	executor, execution := acpTestFixture(t, "normal")
	execution.RuntimeSessionID = "existing-conversation"
	execution.Runtime = relay.RuntimeRequirement{Model: "selected-model", ReasoningEffort: "high"}
	executor.Config.ExtraArgs = []string{"--mode", "accept-edits"}
	var events []relay.Event
	execution.Emit = func(_ context.Context, name string, data any) {
		raw, _ := json.Marshal(data)
		events = append(events, relay.Event{Type: name, Data: raw})
	}
	result, err := executor.Execute(context.Background(), execution)
	if err != nil || result.Summary != "final answer" || result.RuntimeSessionID != "existing-conversation" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if result.Usage == nil || !result.Usage.Valid() || *result.Usage.ReasoningTokens != 10 || *result.Usage.TotalTokens != 120 {
		t.Fatalf("usage=%+v", result.Usage)
	}
	thought, command, final := false, false, false
	for _, event := range events {
		if strings.Contains(string(event.Data), "HISTORICAL") {
			t.Fatal("replayed history leaked into current execution")
		}
		if strings.Contains(event.Type, "reasoning.textDelta") {
			thought = true
		}
		if strings.Contains(string(event.Data), "commandExecution") {
			command = true
		}
		if event.Type == "assistant.final.completed" {
			final = true
			if strings.Contains(string(event.Data), "Think") {
				t.Fatal("thinking leaked into final response")
			}
		}
	}
	if !thought || !command || !final {
		t.Fatalf("thought=%v command=%v final=%v", thought, command, final)
	}
	methods := map[string]map[string]any{}
	for _, call := range calls(t, execution) {
		methods[call["method"].(string)] = call["params"].(map[string]any)
	}
	if _, ok := methods["session/new"]; ok {
		t.Fatal("resumed session was replaced")
	}
	if methods["session/set_model"]["modelId"] != "selected-model" || methods["session/set_config_option"]["value"] != "high" || methods["session/set_mode"]["modeId"] != "accept-edits" {
		t.Fatalf("options=%v", methods)
	}
}
func TestACPRecoveryBeforePrompt(t *testing.T) {
	executor, execution := acpTestFixture(t, "normal")
	execution.RuntimeSessionID = "missing"
	execution.FallbackPrompt = "full context recovery"
	result, err := executor.Execute(context.Background(), execution)
	if err != nil || result.Summary != "final answer" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	promptCount := 0
	for _, call := range calls(t, execution) {
		if call["method"] == "session/prompt" {
			promptCount++
			params := call["params"].(map[string]any)
			block := params["prompt"].([]any)[0].(map[string]any)
			if block["text"] != "full context recovery" {
				t.Fatal("recovery sent the wrong prompt")
			}
		}
	}
	if promptCount != 1 {
		t.Fatalf("prompt sent %d times", promptCount)
	}
}

type approvalBroker struct{ approved bool }

func (b approvalBroker) Request(context.Context, relay.InteractionRequest) (json.RawMessage, error) {
	if b.approved {
		return json.RawMessage(`{"approved":true}`), nil
	}
	return json.RawMessage(`{"optionId":"not-offered"}`), nil
}
func TestACPPermissionsAreExplicit(t *testing.T) {
	for _, scenario := range []string{"no-broker", "approved", "invalid-option"} {
		t.Run(scenario, func(t *testing.T) {
			executor, execution := acpTestFixture(t, "permission")
			if scenario != "no-broker" {
				execution.Interactions = approvalBroker{approved: scenario == "approved"}
			}
			if _, err := executor.Execute(context.Background(), execution); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(execution.WorkDir, ".relay/permission.json"))
			if err != nil {
				t.Fatal(err)
			}
			var value map[string]any
			if err := json.Unmarshal(raw, &value); err != nil {
				t.Fatal(err)
			}
			outcome := value["result"].(map[string]any)["outcome"].(map[string]any)
			if scenario == "approved" {
				if outcome["optionId"] != "once" {
					t.Fatalf("approval=%v", outcome)
				}
			} else if outcome["outcome"] != "cancelled" {
				t.Fatalf("unapproved=%v", outcome)
			}
		})
	}
}
func TestACPErrorsAndCancellation(t *testing.T) {
	for _, scenario := range []string{"invalid-model", "eof", "cancelled", "empty", "hang"} {
		t.Run(scenario, func(t *testing.T) {
			executor, execution := acpTestFixture(t, scenario)
			execution.Runtime.Model = "model"
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			started := time.Now()
			_, err := executor.Execute(ctx, execution)
			if err == nil {
				t.Fatal("invalid execution succeeded")
			}
			if scenario == "hang" && (time.Since(started) > 7*time.Second || !strings.Contains(err.Error(), "deadline")) {
				t.Fatalf("cancel took %s: %v", time.Since(started), err)
			}
		})
	}
}
func TestACPImagePrompt(t *testing.T) {
	executor, execution := acpTestFixture(t, "normal")
	var imageBytes bytes.Buffer
	if err := png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	data, err := relay.EncodeInputImages(relay.InputImage{Name: "screen.png", ContentType: "image/png", Data: imageBytes.Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	execution.Input.Data = data
	if _, err := executor.Execute(context.Background(), execution); err != nil {
		t.Fatal(err)
	}
	for _, call := range calls(t, execution) {
		if call["method"] == "session/prompt" {
			blocks := call["params"].(map[string]any)["prompt"].([]any)
			if len(blocks) != 2 || blocks[1].(map[string]any)["type"] != "image" || blocks[1].(map[string]any)["mimeType"] != "image/png" {
				t.Fatalf("blocks=%v", blocks)
			}
		}
	}
}

func TestACPLateMissingSessionRecoveryDoesNotRepeatActiveWork(t *testing.T) {
	for _, scenario := range []string{"late-missing", "late-failure"} {
		t.Run(scenario, func(t *testing.T) {
			executor, execution := acpTestFixture(t, scenario)
			execution.RuntimeSessionID = "missing"
			execution.FallbackPrompt = "restore full history"
			result, err := executor.Execute(context.Background(), execution)
			if scenario == "late-missing" && (err != nil || result.Summary != "final answer") {
				t.Fatalf("safe recovery=%+v %v", result, err)
			}
			if scenario == "late-failure" && err == nil {
				t.Fatal("repeated work after execution began")
			}
			count := 0
			for _, call := range calls(t, execution) {
				if call["method"] == "session/prompt" {
					count++
				}
			}
			expected := 1
			if scenario == "late-missing" {
				expected = 2
			}
			if count != expected {
				t.Fatalf("prompt count=%d want=%d", count, expected)
			}
		})
	}
}

func TestACPLateReplayAndEmptyResume(t *testing.T) {
	for _, scenario := range []string{"late-history", "echo-only", "active-empty"} {
		t.Run(scenario, func(t *testing.T) {
			executor, execution := acpTestFixture(t, scenario)
			execution.RuntimeSessionID = "existing-conversation"
			execution.FallbackPrompt = "restore current user request from shared history"
			var recorded []string
			execution.Emit = func(_ context.Context, event string, data any) {
				raw, _ := json.Marshal(data)
				recorded = append(recorded, event+string(raw))
			}
			result, err := executor.Execute(context.Background(), execution)
			if scenario == "active-empty" {
				if err == nil {
					t.Fatal("current activity with no answer succeeded")
				}
			} else if err != nil || result.Summary != "final answer" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			joined := strings.Join(recorded, "\n")
			if strings.Contains(joined, "HISTORICAL") || strings.Contains(joined, "OLD COMMAND") {
				t.Fatal("late history leaked into current turn")
			}
			promptCount := 0
			for _, call := range calls(t, execution) {
				if call["method"] == "session/prompt" {
					promptCount++
				}
			}
			if scenario == "echo-only" {
				if promptCount != 2 || result.RuntimeSessionID == execution.RuntimeSessionID {
					t.Fatal("empty resume not replaced with recovery context")
				}
			} else if promptCount != 1 {
				t.Fatal("repeated a live turn")
			}
		})
	}
}

func TestACPReplayWaitIsCancellable(t *testing.T) {
	executor, execution := acpTestFixture(t, "missing-replay-end")
	execution.RuntimeSessionID = "existing-conversation"
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := executor.Execute(ctx, execution); err == nil {
		t.Fatal("ignored incomplete replay")
	}
	for _, call := range calls(t, execution) {
		if call["method"] == "session/prompt" {
			t.Fatal("submitted prompt before replay completed")
		}
	}
}
