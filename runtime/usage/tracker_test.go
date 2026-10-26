package usage

import (
	"encoding/json"
	"testing"
)

func event(t *testing.T, tr *Tracker, kind, body string) {
	t.Helper()
	u := tr.Observe("runtime."+tr.provider+"."+kind, json.RawMessage(body))
	if u != nil && !u.Valid() {
		t.Fatalf("invalid snapshot %+v", u)
	}
}
func TestCodexAndTraeResumeAccounting(t *testing.T) {
	for _, provider := range []string{"codex", "trae"} {
		t.Run(provider, func(t *testing.T) {
			tr := New(provider, "model", true)
			event(t, tr, "thread.tokenUsage.updated", `{"turnId":"old","tokenUsage":{"total":{"inputTokens":1000,"outputTokens":100,"cachedInputTokens":800,"reasoningOutputTokens":20},"last":{"inputTokens":500,"outputTokens":50,"cachedInputTokens":400,"reasoningOutputTokens":10}}}`)
			if tr.Finish(false) != nil {
				t.Fatal("restored history counted")
			}
			event(t, tr, "turn.started", `{"turn":{"id":"new"}}`)
			body := `{"turnId":"new","tokenUsage":{"total":{"inputTokens":1300,"outputTokens":150,"cachedInputTokens":1000,"reasoningOutputTokens":30},"last":{"inputTokens":300,"outputTokens":50,"cachedInputTokens":200,"reasoningOutputTokens":10}}}`
			event(t, tr, "thread.tokenUsage.updated", body)
			event(t, tr, "thread.tokenUsage.updated", body)
			event(t, tr, "thread.tokenUsage.updated", `{"turnId":"new","tokenUsage":{"total":{"inputTokens":1500,"outputTokens":180,"cachedInputTokens":1100,"reasoningOutputTokens":35},"last":{"inputTokens":200,"outputTokens":30,"cachedInputTokens":100,"reasoningOutputTokens":5}}}`)
			event(t, tr, "turn.completed", `{"turn":{"status":"completed"}}`)
			u := tr.Finish(true)
			if u == nil || *u.InputTokens != 500 || *u.OutputTokens != 80 || *u.TotalTokens != 580 || *u.CacheReadTokens != 300 || *u.ReasoningTokens != 15 || u.Status != "complete" {
				t.Fatalf("wrong delta %+v", u)
			}
		})
	}
}
func TestAppServerResumeWithoutRestoredNotification(t *testing.T) {
	tr := New("codex", "", true)
	event(t, tr, "turn.started", `{"turn":{"id":"new"}}`)
	event(t, tr, "thread.tokenUsage.updated", `{"turnId":"new","tokenUsage":{"total":{"inputTokens":1500,"outputTokens":120},"last":{"inputTokens":500,"outputTokens":20}}}`)
	event(t, tr, "turn.completed", `{}`)
	u := tr.Finish(true)
	if u == nil || *u.TotalTokens != 520 {
		t.Fatalf("thread history counted %+v", u)
	}
}
func TestJSONLTurnUsageReplacesDuplicateSnapshot(t *testing.T) {
	tr := New("trae", "", true)
	for range 2 {
		event(t, tr, "turn.completed", `{"usage":{"input_tokens":300,"output_tokens":50,"cached_input_tokens":200}}`)
	}
	u := tr.Finish(true)
	if u == nil || *u.TotalTokens != 350 || *u.CacheReadTokens != 200 {
		t.Fatalf("duplicate/cache counted %+v", u)
	}
}
func TestClaudeResultOverridesAssistantUsage(t *testing.T) {
	tr := New("claude", "", false)
	for range 2 {
		event(t, tr, "assistant", `{"message":{"id":"m1","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100,"cache_creation_input_tokens":20}}}`)
	}
	event(t, tr, "result", `{"usage":{"input_tokens":30,"output_tokens":15,"cache_read_input_tokens":200,"cache_creation_input_tokens":40}}`)
	u := tr.Finish(true)
	if u == nil || *u.InputTokens != 270 || *u.TotalTokens != 285 || u.Status != "complete" {
		t.Fatalf("wrong aggregate %+v", u)
	}
}
func TestMessageAndStepUsage(t *testing.T) {
	tests := []struct {
		provider, kind, body, finish string
		total                        int64
	}{
		{"pi", "message_end", `{"message":{"role":"assistant","timestamp":1,"usage":{"input":10,"output":5,"cacheRead":100,"cacheWrite":20}}}`, "agent_end", 135},
		{"opencode", "step_finish", `{"part":{"id":"p1","reason":"stop","tokens":{"input":10,"output":5,"reasoning":2,"cache":{"read":100,"write":20}}}}`, "", 137},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			tr := New(tt.provider, "", true)
			for range 2 {
				event(t, tr, tt.kind, tt.body)
			}
			if tt.finish != "" {
				event(t, tr, tt.finish, `{}`)
			}
			u := tr.Finish(true)
			if u == nil || *u.TotalTokens != tt.total || u.Status != "complete" {
				t.Fatalf("wrong usage %+v", u)
			}
			u = tr.Finish(false)
			if u.Status != "partial" {
				t.Fatal("failed run should preserve partial use")
			}
		})
	}
}
func TestUnknownMalformedAndMissingUsage(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "qoder", "qwenwork"} {
		tr := New(provider, "", false)
		event(t, tr, "turn.completed", `{"usage":{"input_tokens":-1,"output_tokens":5}}`)
		if tr.Finish(true) != nil {
			t.Fatalf("fabricated %s usage", provider)
		}
	}
	tr := New("claude", "", false)
	event(t, tr, "result", `{"usage":{"input_tokens":10,"output_tokens":5}}`)
	u := tr.Finish(true)
	if u == nil || u.Status != "partial" {
		t.Fatal("missing cache semantics must be partial")
	}
}

func TestForeignThreadUsageIsIgnored(t *testing.T) {
	tr := New("codex", "", false, "main")
	event(t, tr, "turn.started", `{"threadId":"main","turn":{"id":"turn-main"}}`)
	event(t, tr, "turn.started", `{"threadId":"other","turn":{"id":"turn-other"}}`)
	event(t, tr, "thread.tokenUsage.updated", `{"threadId":"other","turnId":"turn-other","tokenUsage":{"total":{"inputTokens":1000,"outputTokens":100},"last":{"inputTokens":1000,"outputTokens":100}}}`)
	if tr.Finish(true) != nil {
		t.Fatal("foreign thread counted")
	}
	event(t, tr, "thread.tokenUsage.updated", `{"threadId":"main","turnId":"turn-main","tokenUsage":{"total":{"inputTokens":100,"outputTokens":20},"last":{"inputTokens":100,"outputTokens":20}}}`)
	event(t, tr, "turn.completed", `{"threadId":"main"}`)
	if u := tr.Finish(true); u == nil || *u.TotalTokens != 120 {
		t.Fatalf("lost primary usage %+v", u)
	}
}

func TestCounterResetPreservesKnownPartialUsage(t *testing.T) {
	tr := New("trae", "", true)
	event(t, tr, "thread.tokenUsage.updated", `{"turnId":"old","tokenUsage":{"total":{"inputTokens":1000,"outputTokens":100},"last":{"inputTokens":100,"outputTokens":20}}}`)
	event(t, tr, "turn.started", `{"turn":{"id":"new"}}`)
	event(t, tr, "thread.tokenUsage.updated", `{"turnId":"new","tokenUsage":{"total":{"inputTokens":1100,"outputTokens":120},"last":{"inputTokens":100,"outputTokens":20}}}`)
	event(t, tr, "thread.tokenUsage.updated", `{"turnId":"new","tokenUsage":{"total":{"inputTokens":10,"outputTokens":2},"last":{"inputTokens":10,"outputTokens":2}}}`)
	event(t, tr, "turn.completed", `{}`)
	if u := tr.Finish(true); u == nil || *u.TotalTokens != 120 || u.Status != "partial" {
		t.Fatalf("reset lost known usage %+v", u)
	}
}
