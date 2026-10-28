package opencode_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/runtime/opencode"
)

const unfinishedV2Stream = `
cat >/dev/null
printf '%s\n' '{"type":"step_start","sessionID":"ses_current","part":{"messageID":"msg_current"}}'
printf '%s\n' '{"type":"text","sessionID":"ses_current","part":{"messageID":"msg_current","text":"Done"}}'
`

func completionFixture(t *testing.T, export, tail string) (opencode.Executor, relay.Execution) {
	t.Helper()
	executor, execution := fixture(t, "2.0.32", `
if [ "$1" = 'session' ]; then
 test "$2" = 'export' && test "$3" = '--standalone' && test "$4" = 'ses_current' || exit 4
 printf '%s\n' '`+export+`'
 `+tail+`
 exit 0
fi
`+unfinishedV2Stream)
	return executor, execution
}

func TestV2VerifiesNativeCompletionWithoutStepFinish(t *testing.T) {
	for _, export := range []string{
		`{"info":{"id":"ses_current"},"messages":[{"id":"msg_current","type":"assistant","finish":"stop","time":{"completed":123}},{"id":"idle","type":"idle"}]}`,
		`{"messages":[{"info":{"id":"msg_current","role":"assistant","finish":"end_turn","time":{"completed":123},"error":null},"parts":[]}],"info":{"id":"ses_current"}}`,
	} {
		executor, execution := completionFixture(t, export, "")
		var verified, final bool
		execution.Emit = func(_ context.Context, name string, value any) {
			if name == "runtime.opencode.completion.verified" {
				b, _ := json.Marshal(value)
				verified = strings.Contains(string(b), "msg_current") && strings.Contains(string(b), "ses_current")
			}
			if name == "assistant.final.completed" {
				final = true
			}
		}
		result, err := executor.Execute(context.Background(), execution)
		if err != nil || result.Summary != "Done" || result.RuntimeSessionID != "ses_current" || !verified || !final {
			t.Fatalf("result=%+v verified=%v final=%v err=%v", result, verified, final, err)
		}
	}
}

func TestV2DoesNotUsePreviousOrIncompleteNativeReplies(t *testing.T) {
	cases := []struct{ name, export, want string }{
		{"old reply", `{"info":{"id":"ses_current"},"messages":[{"id":"msg_old","type":"assistant","finish":"stop","time":{"completed":123}}]}`, "absent"},
		{"unfinished", `{"info":{"id":"ses_current"},"messages":[{"id":"msg_current","type":"assistant","finish":"stop","time":{}}]}`, "not a successfully completed"},
		{"tool turn", `{"info":{"id":"ses_current"},"messages":[{"id":"msg_current","type":"assistant","finish":"tool-calls","time":{"completed":123}}]}`, "not a successfully completed"},
		{"errored", `{"info":{"id":"ses_current"},"messages":[{"id":"msg_current","type":"assistant","finish":"stop","time":{"completed":123},"error":{"message":"failed"}}]}`, "not a successfully completed"},
		{"wrong role", `{"info":{"id":"ses_current"},"messages":[{"id":"msg_current","type":"user","finish":"stop","time":{"completed":123}}]}`, "not a successfully completed"},
		{"wrong session", `{"info":{"id":"ses_other"},"messages":[{"id":"msg_current","type":"assistant","finish":"stop","time":{"completed":123}}]}`, "identity mismatch"},
		{"duplicate message", `{"info":{"id":"ses_current"},"messages":[{"id":"msg_current"},{"id":"msg_current","type":"assistant","finish":"stop","time":{"completed":123}}]}`, "duplicate"},
		{"malformed", `{`, "verify completion"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			executor, execution := completionFixture(t, tc.export, "")
			var final bool
			execution.Emit = func(_ context.Context, name string, _ any) { final = final || name == "assistant.final.completed" }
			_, err := executor.Execute(context.Background(), execution)
			if err == nil || !strings.Contains(err.Error(), tc.want) || final {
				t.Fatalf("err=%v final=%v", err, final)
			}
		})
	}
}

func TestV2ExportFailureAndCancellationRemainFailures(t *testing.T) {
	export := `{"info":{"id":"ses_current"},"messages":[{"id":"msg_current","type":"assistant","finish":"stop","time":{"completed":123}}]}`
	executor, execution := completionFixture(t, export, "exit 2")
	if _, err := executor.Execute(context.Background(), execution); err == nil || !strings.Contains(err.Error(), "export failed") {
		t.Fatalf("err=%v", err)
	}
	executor, execution = completionFixture(t, export, "sleep 30")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := executor.Execute(ctx, execution)
	if err == nil || time.Since(start) > 4*time.Second {
		t.Fatalf("cancellation took %s: %v", time.Since(start), err)
	}
}

func TestExplicitFailedFinishIsNotOverriddenByExport(t *testing.T) {
	executor, execution := fixture(t, "2.0.32", `
if [ "$1" = 'session' ]; then touch .relay/unexpected-export; exit 1; fi
`+unfinishedV2Stream+`printf '%s\n' '{"type":"step_finish","part":{"reason":"tool-calls"}}'
`)
	_, err := executor.Execute(context.Background(), execution)
	if err == nil || !strings.Contains(err.Error(), "successful final step") {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(execution.WorkDir, ".relay/unexpected-export")); !os.IsNotExist(err) {
		t.Fatal("explicit failure triggered export")
	}
}

func TestV2CompletionExportDiscardsLargeHistory(t *testing.T) {
	executor, execution := fixture(t, "2.0.32", `
if [ "$1" = 'session' ]; then cat export.json; exit 0; fi
`+unfinishedV2Stream)
	if err := os.MkdirAll(execution.WorkDir, 0700); err != nil {
		t.Fatal(err)
	}
	history := strings.Repeat("private history ", 100000)
	exported, _ := json.Marshal(map[string]any{"info": map[string]string{"id": "ses_current"}, "messages": []any{
		map[string]any{"id": "msg_old", "type": "assistant", "content": history, "finish": "stop", "time": map[string]int64{"completed": 1}},
		map[string]any{"id": "msg_current", "type": "assistant", "finish": "stop", "time": map[string]int64{"completed": 2}},
	}})
	if err := os.WriteFile(filepath.Join(execution.WorkDir, "export.json"), exported, 0600); err != nil {
		t.Fatal(err)
	}
	execution.Emit = func(_ context.Context, _ string, value any) {
		b, _ := json.Marshal(value)
		if strings.Contains(string(b), "private history") {
			t.Error("session transcript leaked into events")
		}
	}
	result, err := executor.Execute(context.Background(), execution)
	if err != nil || result.Summary != "Done" || strings.Contains(string(result.Output), "private history") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestV2ResetsMessageIdentityForEachStep(t *testing.T) {
	executor, execution := fixture(t, "2.0.32", `
if [ "$1" = 'session' ]; then
 printf '%s\n' '{"info":{"id":"ses_current"},"messages":[{"id":"msg_old","type":"assistant","finish":"stop","time":{"completed":123}}]}'
 exit 0
fi
printf '%s\n' '{"type":"step_start","sessionID":"ses_current","part":{"messageID":"msg_old"}}'
printf '%s\n' '{"type":"text","part":{"messageID":"msg_old","text":"Old answer"}}'
printf '%s\n' '{"type":"step_finish","part":{"reason":"tool-calls"}}'
printf '%s\n' '{"type":"step_start","sessionID":"ses_current","part":{"messageID":"msg_current"}}'
printf '%s\n' '{"type":"text","part":{"messageID":"msg_current","text":"Partial"}}'
`)
	if _, err := executor.Execute(context.Background(), execution); err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("err=%v", err)
	}
}

func TestV2RejectsTextFromAnotherMessage(t *testing.T) {
	executor, execution := fixture(t, "2.0.32", unfinishedV2Stream+`printf '%s\n' '{"type":"text","part":{"messageID":"msg_other","text":"Unexpected"}}'
`)
	if _, err := executor.Execute(context.Background(), execution); err == nil || !strings.Contains(err.Error(), "different assistant message") {
		t.Fatalf("err=%v", err)
	}
}
