package antigravity

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/KDF5000/relay"
)

type acpEvents struct {
	ctx           context.Context
	execution     relay.Execution
	options       []any
	counter       int
	hasActivity   bool
	hasSubagents  bool
	thoughtID     string
	thought       strings.Builder
	messageID     string
	message       strings.Builder
	tools         map[string]map[string]any
	toolStarted   map[string]bool
	toolCompleted map[string]bool
}

func newACPEvents(ctx context.Context, execution relay.Execution) *acpEvents {
	return &acpEvents{ctx: ctx, execution: execution, tools: map[string]map[string]any{}, toolStarted: map[string]bool{}, toolCompleted: map[string]bool{}}
}
func (e *acpEvents) emit(name string, data any) {
	if e.execution.Emit != nil {
		e.execution.Emit(e.ctx, name, data)
	}
}
func (e *acpEvents) id(kind string) string {
	e.counter++
	return fmt.Sprintf("antigravity:%s:%s:%d", e.execution.RunID, kind, e.counter)
}
func (e *acpEvents) closeThought() {
	if e.thoughtID == "" {
		return
	}
	e.emit("runtime.antigravity.item.completed", map[string]any{"item": map[string]any{"id": e.thoughtID, "type": "reasoning", "text": e.thought.String()}})
	e.thoughtID = ""
	e.thought.Reset()
}
func (e *acpEvents) closeMessage(phase string) {
	if e.messageID == "" {
		return
	}
	e.emit("runtime.antigravity.item.completed", map[string]any{"item": map[string]any{"id": e.messageID, "type": "agentMessage", "phase": phase, "text": e.message.String()}})
	e.emit("assistant.message.completed", map[string]string{"text": e.message.String(), "phase": phase})
	e.messageID = ""
	if phase == "commentary" {
		e.message.Reset()
	}
}
func (e *acpEvents) finish() { e.closeThought(); e.closeMessage("final_answer") }
func (e *acpEvents) update(update map[string]any) {
	e.emit("runtime.antigravity.session.update", map[string]any{"update": update})
	switch stringField(update, "sessionUpdate") {
	case "agent_thought_chunk":
		e.hasActivity = true
		content, _ := update["content"].(map[string]any)
		text := stringField(content, "text")
		if text == "" {
			return
		}
		if e.messageID != "" {
			e.closeMessage("commentary")
		}
		if e.thoughtID == "" {
			e.thoughtID = e.id("reasoning")
			e.emit("runtime.antigravity.item.started", map[string]any{"item": map[string]any{"id": e.thoughtID, "type": "reasoning"}})
		}
		e.thought.WriteString(text)
		e.emit("runtime.antigravity.item.reasoning.textDelta", map[string]string{"itemId": e.thoughtID, "delta": text})
	case "agent_message_chunk":
		e.hasActivity = true
		content, _ := update["content"].(map[string]any)
		text := stringField(content, "text")
		if text == "" {
			return
		}
		e.closeThought()
		if e.messageID == "" {
			e.message.Reset()
			e.messageID = e.id("message")
			e.emit("runtime.antigravity.item.started", map[string]any{"item": map[string]any{"id": e.messageID, "type": "agentMessage", "phase": "commentary"}})
		}
		e.message.WriteString(text)
		e.emit("runtime.antigravity.item.agentMessage.delta", map[string]string{"itemId": e.messageID, "delta": text})
		e.emit("assistant.message.delta", map[string]string{"delta": text})
	case "tool_call", "tool_call_update":
		e.hasActivity = true
		if stringField(update, "name") == "invoke_subagent" {
			e.hasSubagents = true
		}
		e.closeThought()
		e.closeMessage("commentary")
		e.tool(update)
	}
}
func (e *acpEvents) tool(update map[string]any) {
	callID := stringField(update, "toolCallId")
	if callID == "" {
		return
	}
	item := e.tools[callID]
	if item == nil {
		item = map[string]any{"id": "antigravity:" + e.execution.RunID + ":tool:" + callID, "type": "toolCall"}
		e.tools[callID] = item
	}
	if title := stringField(update, "title"); title != "" {
		item["name"] = title
	}
	if input, ok := update["rawInput"]; ok {
		item["arguments"] = input
	}
	input, _ := item["arguments"].(map[string]any)
	if stringField(update, "kind") == "execute" || stringField(input, "CommandLine") != "" {
		item["type"] = "commandExecution"
		item["command"] = stringField(input, "CommandLine")
	}
	if path := stringField(input, "Path"); path != "" {
		item["path"] = path
	}
	if locations, ok := update["locations"].([]any); ok && len(locations) > 0 {
		location, _ := locations[0].(map[string]any)
		if path := stringField(location, "path"); path != "" {
			item["path"] = path
		}
	}
	if raw, ok := update["rawOutput"]; ok {
		item["output"] = raw
	}
	if blocks, ok := update["content"].([]any); ok {
		var output []string
		for _, value := range blocks {
			block, _ := value.(map[string]any)
			content, _ := block["content"].(map[string]any)
			if text := stringField(content, "text"); text != "" {
				output = append(output, text)
			}
		}
		if len(output) > 0 {
			item["output"] = strings.Join(output, "\n")
		}
	}
	status := stringField(update, "status")
	switch status {
	case "completed":
		item["status"] = "completed"
	case "failed":
		item["status"] = "failed"
	case "cancelled":
		item["status"] = "cancelled"
	default:
		item["status"] = "inProgress"
	}
	// Emit immutable snapshots: buffered observers may consume them later.
	snapshot := make(map[string]any, len(item))
	for key, value := range item {
		snapshot[key] = value
	}
	if e.toolCompleted[callID] {
		return
	}
	if status == "completed" || status == "failed" || status == "cancelled" {
		e.toolCompleted[callID] = true
		e.emit("runtime.antigravity.item.completed", map[string]any{"item": snapshot})
	} else if !e.toolStarted[callID] {
		e.toolStarted[callID] = true
		e.emit("runtime.antigravity.item.started", map[string]any{"item": snapshot})
	}
}
func acpUsage(result map[string]any, model string) *relay.Usage {
	data, _ := result["usage"].(map[string]any)
	if data == nil {
		return nil
	}
	// Reuse Relay's bounded integer decoding/validation, without double-counting
	// reasoning: ACP outputTokens already includes thoughtTokens.
	raw, _ := json.Marshal(map[string]any{"input_tokens": data["inputTokens"], "output_tokens": data["outputTokens"], "total_tokens": data["totalTokens"], "cache_read_tokens": data["cachedReadTokens"], "cache_write_tokens": data["cachedWriteTokens"], "reasoning_tokens": data["thoughtTokens"], "status": "complete", "source": "antigravity.acp", "model": model})
	var usage relay.Usage
	if json.Unmarshal(raw, &usage) != nil {
		return nil
	}
	// agy reports uncached input separately. Older ACP builds may already
	// include cache tokens; use the reported total to distinguish the layouts.
	if usage.InputTokens != nil && usage.OutputTokens != nil && usage.TotalTokens != nil && *usage.InputTokens >= 0 && *usage.OutputTokens >= 0 && *usage.InputTokens <= math.MaxInt64-*usage.OutputTokens {
		if *usage.TotalTokens != *usage.InputTokens+*usage.OutputTokens {
			input := *usage.InputTokens
			for _, cache := range []*int64{usage.CacheReadTokens, usage.CacheWriteTokens} {
				if cache != nil {
					if *cache < 0 || input > math.MaxInt64-*cache {
						return nil
					}
					input += *cache
				}
			}
			if input <= math.MaxInt64-*usage.OutputTokens && *usage.TotalTokens == input+*usage.OutputTokens {
				usage.InputTokens = &input
			}
		}
	}
	if !usage.Valid() {
		return nil
	}
	return &usage
}
