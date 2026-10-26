// Package usage contains provider-specific token normalization used by Node.
// It consumes native events and emits absolute, attempt-scoped Relay snapshots.
package usage

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/KDF5000/relay"
)

type Tracker struct {
	mu                            sync.Mutex
	provider, model, turn, thread string
	resumed                       bool
	baseline                      *relay.Usage
	last                          *relay.Usage
	parts                         map[string]*relay.Usage
	serial                        int
	final                         bool
	incomplete                    bool
}

func New(provider, model string, resumed bool, threadIDs ...string) *Tracker {
	thread := ""
	if len(threadIDs) > 0 {
		thread = threadIDs[0]
	}
	return &Tracker{provider: provider, model: model, resumed: resumed, thread: thread, parts: map[string]*relay.Usage{}}
}
func ptr(n int64) *int64          { return &n }
func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func num(m map[string]any, key string) *int64 {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	n, ok := v.(json.Number)
	if !ok {
		return nil
	}
	i, err := strconv.ParseInt(string(n), 10, 64)
	if err != nil || i < 0 {
		return nil
	}
	return ptr(i)
}
func add(a, b *int64) *int64 {
	if a == nil || b == nil || *a > math.MaxInt64-*b {
		return nil
	}
	return ptr(*a + *b)
}
func (t *Tracker) normalize(m map[string]any, style string) *relay.Usage {
	u := &relay.Usage{Status: "partial", Source: t.provider, Model: t.model}
	switch style {
	case "codex":
		u.InputTokens = num(m, "input_tokens")
		u.OutputTokens = num(m, "output_tokens")
		u.CacheReadTokens = num(m, "cached_input_tokens")
	case "app":
		u.InputTokens = num(m, "inputTokens")
		u.OutputTokens = num(m, "outputTokens")
		u.CacheReadTokens = num(m, "cachedInputTokens")
		u.CacheWriteTokens = num(m, "cacheWriteInputTokens")
		if u.CacheWriteTokens == nil {
			u.CacheWriteTokens = num(m, "cacheCreationInputTokens")
		}
		u.ReasoningTokens = num(m, "reasoningOutputTokens")
	case "claude":
		u.InputTokens = num(m, "input_tokens")
		u.OutputTokens = num(m, "output_tokens")
		u.CacheReadTokens = num(m, "cache_read_input_tokens")
		u.CacheWriteTokens = num(m, "cache_creation_input_tokens")
		// Anthropic input_tokens excludes cache reads/writes. Only account them
		// when supplied; absent cache fields make this a partial measurement.
		if u.CacheReadTokens == nil || u.CacheWriteTokens == nil {
			t.incomplete = true
		}
		for _, n := range []*int64{u.CacheReadTokens, u.CacheWriteTokens} {
			if n != nil {
				u.InputTokens = add(u.InputTokens, n)
			}
		}
	case "pi":
		u.InputTokens = num(m, "input")
		u.OutputTokens = num(m, "output")
		u.CacheReadTokens = num(m, "cacheRead")
		u.CacheWriteTokens = num(m, "cacheWrite")
		if u.CacheReadTokens == nil || u.CacheWriteTokens == nil {
			t.incomplete = true
		}
		for _, n := range []*int64{u.CacheReadTokens, u.CacheWriteTokens} {
			if n != nil {
				u.InputTokens = add(u.InputTokens, n)
			}
		}
	case "opencode":
		u.InputTokens = num(m, "input")
		u.OutputTokens = num(m, "output")
		u.ReasoningTokens = num(m, "reasoning")
		cache := object(m["cache"])
		u.CacheReadTokens = num(cache, "read")
		u.CacheWriteTokens = num(cache, "write")
		// OpenCode step tokens separate cache and reasoning from input/output.
		if u.CacheReadTokens == nil || u.CacheWriteTokens == nil || u.ReasoningTokens == nil {
			t.incomplete = true
		}
		for _, n := range []*int64{u.CacheReadTokens, u.CacheWriteTokens} {
			if n != nil {
				u.InputTokens = add(u.InputTokens, n)
			}
		}
		if u.ReasoningTokens != nil {
			u.OutputTokens = add(u.OutputTokens, u.ReasoningTokens)
		}
	}
	if u.InputTokens == nil || u.OutputTokens == nil {
		return nil
	}
	u.TotalTokens = add(u.InputTokens, u.OutputTokens)
	if !u.Valid() {
		return nil
	}
	return u
}
func subtract(a, b *relay.Usage) *relay.Usage {
	out := *a
	sub := func(x, y *int64) *int64 {
		if x == nil {
			return nil
		}
		if y == nil {
			return nil
		}
		if *x < *y {
			return nil
		}
		return ptr(*x - *y)
	}
	out.InputTokens = sub(a.InputTokens, b.InputTokens)
	out.OutputTokens = sub(a.OutputTokens, b.OutputTokens)
	out.CacheReadTokens = sub(a.CacheReadTokens, b.CacheReadTokens)
	out.CacheWriteTokens = sub(a.CacheWriteTokens, b.CacheWriteTokens)
	out.ReasoningTokens = sub(a.ReasoningTokens, b.ReasoningTokens)
	out.TotalTokens = add(out.InputTokens, out.OutputTokens)
	if !out.Valid() {
		return nil
	}
	return &out
}
func (t *Tracker) Observe(kind string, data any) *relay.Usage {
	t.mu.Lock()
	defer t.mu.Unlock()
	prefix := "runtime." + t.provider + "."
	if !strings.HasPrefix(kind, prefix) {
		return nil
	}
	kind = strings.TrimPrefix(kind, prefix)
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var m map[string]any
	if decoder.Decode(&m) != nil {
		return nil
	}
	switch t.provider {
	case "codex", "trae":
		if kind == "usage.scope" {
			id, _ := m["thread_id"].(string)
			if id != "" {
				t.thread = id
			}
			return nil
		}
		if threadID, _ := m["threadId"].(string); threadID != "" && t.thread != "" && threadID != t.thread {
			return nil
		}
		if kind == "thread.resume_failed" {
			t.resumed = false
			t.baseline = nil
			t.turn = ""
			t.thread = ""
			return nil
		}
		if kind == "turn.started" {
			if id, _ := object(m["turn"])["id"].(string); id != "" {
				t.turn = id
			}
			return nil
		}
		if kind == "turn.completed" {
			if raw := object(m["usage"]); raw != nil {
				t.last = t.normalize(raw, "codex")
			}
			t.final = true
			return t.snapshot()
		}
		if kind == "thread.tokenUsage.updated" {
			usage := object(m["tokenUsage"])
			total := t.normalize(object(usage["total"]), "app")
			last := t.normalize(object(usage["last"]), "app")
			if total == nil {
				return nil
			}
			id, _ := m["turnId"].(string)
			if t.resumed && (t.turn == "" || id != t.turn) {
				if t.last == nil {
					t.baseline = total
				}
				return nil
			}
			if id != "" && t.turn != "" && id != t.turn {
				return nil
			}
			if t.baseline == nil {
				if t.resumed {
					if last == nil {
						t.incomplete = true
						return nil
					}
					t.baseline = subtract(total, last)
					if t.baseline == nil {
						t.incomplete = true
						return nil
					}
				} else {
					t.baseline = &relay.Usage{InputTokens: ptr(0), OutputTokens: ptr(0), CacheReadTokens: ptr(0), CacheWriteTokens: ptr(0), ReasoningTokens: ptr(0)}
				}
			}
			delta := subtract(total, t.baseline)
			if delta == nil {
				t.incomplete = true
				return t.snapshot()
			}
			t.last = delta
			return t.snapshot()
		}
		// Exec JSONL turn.completed carries invocation usage, not thread totals.
	case "claude":
		if kind == "result" {
			t.incomplete = false
			if u := t.normalize(object(m["usage"]), "claude"); u != nil {
				t.last = u
				t.final = true
				return t.snapshot()
			}
			t.incomplete = true
			return nil
		}
		if kind == "assistant" {
			message := object(m["message"])
			id, _ := message["id"].(string)
			u := t.normalize(object(message["usage"]), "claude")
			if id != "" && u != nil {
				t.parts[id] = u
				t.sumParts()
				return t.snapshot()
			}
		}
	case "pi":
		if kind == "message_end" {
			message := object(m["message"])
			if message["role"] != "assistant" {
				return nil
			}
			u := t.normalize(object(message["usage"]), "pi")
			if u == nil {
				t.incomplete = true
				return nil
			}
			key := fmtKey(message)
			if key == "" {
				t.serial++
				key = strconv.Itoa(t.serial)
			}
			t.parts[key] = u
			t.sumParts()
			return t.snapshot()
		}
		if kind == "agent_end" {
			t.final = true
			return t.snapshot()
		}
	case "opencode":
		if kind == "step_finish" {
			part := object(m["part"])
			id, _ := part["id"].(string)
			u := t.normalize(object(part["tokens"]), "opencode")
			if id == "" || u == nil {
				t.incomplete = true
				return nil
			}
			t.parts[id] = u
			t.sumParts()
			reason, _ := part["reason"].(string)
			t.final = reason == "stop" || reason == "end_turn"
			return t.snapshot()
		}
	}
	return nil
}
func fmtKey(m map[string]any) string {
	if id, _ := m["id"].(string); id != "" {
		return id
	}
	if n, ok := m["timestamp"].(json.Number); ok {
		return string(n)
	}
	return ""
}
func (t *Tracker) sumParts() {
	events := []relay.Event{}
	i := 0
	for id, u := range t.parts {
		i++
		raw, _ := json.Marshal(u)
		events = append(events, relay.Event{AttemptID: id, Sequence: i, Type: "usage.updated", Data: raw})
	}
	t.last = relay.AggregateUsage(events, len(t.parts))
}
func (t *Tracker) snapshot() *relay.Usage {
	if t.last == nil {
		return nil
	}
	out := *t.last
	out.Source = t.provider
	out.Model = t.model
	out.Status = "partial"
	if t.final && !t.incomplete && out.InputTokens != nil && out.OutputTokens != nil {
		out.Status = "complete"
	}
	return &out
}
func (t *Tracker) Finish(success bool) *relay.Usage {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !success {
		t.final = false
	}
	return t.snapshot()
}
