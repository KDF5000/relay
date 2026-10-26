package relay

import (
	"encoding/json"
	"math"
)

// Usage is an absolute snapshot for ONE execution attempt, not an increment.
// Input includes cache reads/writes; output includes reasoning. Cache and
// reasoning fields are subsets and must never be added to TotalTokens.
// Nil means unreported. Status is partial until the adapter has a final snapshot.
type Usage struct {
	InputTokens      *int64 `json:"input_tokens,omitempty"`
	OutputTokens     *int64 `json:"output_tokens,omitempty"`
	TotalTokens      *int64 `json:"total_tokens,omitempty"`
	CacheReadTokens  *int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens *int64 `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  *int64 `json:"reasoning_tokens,omitempty"`
	Status           string `json:"status"`
	Model            string `json:"model,omitempty"`
	Source           string `json:"source,omitempty"`
}

func (u *Usage) Valid() bool {
	if u == nil || (u.Status != "partial" && u.Status != "complete") {
		return false
	}
	found := false
	for _, n := range []*int64{u.InputTokens, u.OutputTokens, u.TotalTokens, u.CacheReadTokens, u.CacheWriteTokens, u.ReasoningTokens} {
		if n != nil {
			if *n < 0 {
				return false
			}
			found = true
		}
	}
	if !found {
		return false
	}
	if u.InputTokens != nil && u.OutputTokens != nil {
		if *u.InputTokens > math.MaxInt64-*u.OutputTokens {
			return false
		}
		if u.TotalTokens == nil || *u.TotalTokens != *u.InputTokens+*u.OutputTokens {
			return false
		}
	}
	if u.InputTokens != nil {
		if u.CacheReadTokens != nil && *u.CacheReadTokens > *u.InputTokens {
			return false
		}
		if u.CacheWriteTokens != nil && *u.CacheWriteTokens > *u.InputTokens {
			return false
		}
	}
	if u.OutputTokens != nil && u.ReasoningTokens != nil && *u.ReasoningTokens > *u.OutputTokens {
		return false
	}
	return u.Status != "complete" || (u.InputTokens != nil && u.OutputTokens != nil && u.TotalTokens != nil)
}

// AggregateUsage selects the last valid snapshot per attempt before summing.
// It also works on a compact list of DISTINCT ON(attempt_id) events from SQL.
func AggregateUsage(events []Event, attempts int) *Usage {
	latest := map[string]struct {
		sequence int
		usage    Usage
	}{}
	for _, e := range events {
		if e.Type != "usage.updated" {
			continue
		}
		var u Usage
		if json.Unmarshal(e.Data, &u) != nil || !u.Valid() {
			continue
		}
		old, ok := latest[e.AttemptID]
		if !ok || e.Sequence > old.sequence {
			latest[e.AttemptID] = struct {
				sequence int
				usage    Usage
			}{e.Sequence, u}
		}
	}
	if len(latest) == 0 {
		return nil
	}
	total := &Usage{Status: "complete", Source: "runtime"}
	var input, output, count, read, write, reasoning int64
	haveInput, haveOutput, haveTotal, haveRead, haveWrite, haveReasoning := false, false, false, false, false, false
	add := func(sum *int64, n *int64, present *bool) bool {
		if n == nil {
			return true
		}
		if *sum > math.MaxInt64-*n {
			return false
		}
		*sum += *n
		*present = true
		return true
	}
	for _, item := range latest {
		u := item.usage
		if !add(&input, u.InputTokens, &haveInput) || !add(&output, u.OutputTokens, &haveOutput) || !add(&count, u.TotalTokens, &haveTotal) || !add(&read, u.CacheReadTokens, &haveRead) || !add(&write, u.CacheWriteTokens, &haveWrite) || !add(&reasoning, u.ReasoningTokens, &haveReasoning) {
			return nil
		}
		if u.Status != "complete" {
			total.Status = "partial"
		}
	}
	if len(latest) < attempts {
		total.Status = "partial"
	}
	if haveInput {
		total.InputTokens = &input
	}
	if haveOutput {
		total.OutputTokens = &output
	}
	if haveTotal {
		total.TotalTokens = &count
	}
	if haveRead {
		total.CacheReadTokens = &read
	}
	if haveWrite {
		total.CacheWriteTokens = &write
	}
	if haveReasoning {
		total.ReasoningTokens = &reasoning
	}
	// Missing fields on one attempt remain lower bounds. Only derive total when
	// every attempt supplied input+output, as indicated by complete status.
	if total.Status == "complete" {
		if input > math.MaxInt64-output {
			return nil
		}
		count = input + output
		total.TotalTokens = &count
	}
	return total
}
