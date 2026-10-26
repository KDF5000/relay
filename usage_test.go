package relay

import (
	"encoding/json"
	"testing"
)

func TestUsageSnapshotsAndAttempts(t *testing.T) {
	num := func(n int64) *int64 { return &n }
	event := func(attempt string, seq int, input int64, status string) Event {
		u := Usage{InputTokens: num(input), OutputTokens: num(10), TotalTokens: num(input + 10), CacheReadTokens: num(input / 2), Status: status}
		data, _ := json.Marshal(u)
		return Event{AttemptID: attempt, Sequence: seq, Type: "usage.updated", Data: data}
	}
	events := []Event{event("a", 1, 100, "partial"), event("a", 2, 200, "complete"), event("a", 2, 200, "complete"), event("b", 3, 300, "partial")}
	u := AggregateUsage(events, 2)
	if u == nil || *u.TotalTokens != 520 || *u.CacheReadTokens != 250 || u.Status != "partial" {
		t.Fatalf("bad retry/snapshot totals %+v", u)
	}
	events = append(events, event("b", 4, 300, "complete"))
	u = AggregateUsage(events, 2)
	if u.Status != "complete" {
		t.Fatal("final snapshot ignored")
	}
	if AggregateUsage(nil, 1) != nil {
		t.Fatal("missing usage should be unknown")
	}
	if AggregateUsage(events, 3).Status != "partial" {
		t.Fatal("missing attempt must lower coverage")
	}
}
