package antigravity

import (
	"fmt"
	"strings"
)

// toolEvents projects confirmed agy step updates into Relay's shared item
// protocol. The original step_update remains available unchanged. agy's current
// stream-json protocol does not expose thinking text; do not invent it from
// agent_response state/usage updates.
type toolEvents struct {
	started   map[string]bool
	completed map[string]bool
}

func stringField(value map[string]any, key string) string {
	result, _ := value[key].(string)
	return result
}

func (events *toolEvents) emit(runID string, step map[string]any, emit func(string, any)) {
	if stringField(step, "step_type") != "tool" || step["step_index"] == nil {
		return
	}
	info, _ := step["tool_info"].(map[string]any)
	name := stringField(step, "tool_name")
	if name == "" {
		name = stringField(info, "name")
	}
	if name == "" {
		return
	}
	state := strings.ToUpper(stringField(step, "state"))
	if state != "ACTIVE" && state != "DONE" && state != "ERROR" && state != "CANCELLED" {
		return
	}
	if events.started == nil {
		events.started = map[string]bool{}
		events.completed = map[string]bool{}
	}
	id := fmt.Sprintf("antigravity:%s:%s:%v", runID, stringField(step, "conversation_id"), step["step_index"])
	if events.completed[id] {
		return
	}
	parameters, _ := info["parameters"].(map[string]any)
	item := map[string]any{"id": id, "type": "toolCall", "name": name, "arguments": parameters}
	if path := stringField(parameters, "Path"); path != "" {
		item["path"] = path
		item["name"] = name + " " + path
	}
	if name == "run_command" {
		item["type"] = "commandExecution"
		item["command"] = stringField(parameters, "CommandLine")
	}
	if state == "ACTIVE" {
		if events.started[id] {
			return
		}
		events.started[id] = true
		emit("runtime.antigravity.item.started", map[string]any{"item": item})
		return
	}
	events.completed[id] = true
	switch state {
	case "ERROR":
		item["status"] = "failed"
	case "CANCELLED":
		item["status"] = "cancelled"
	default:
		item["status"] = "completed"
	}
	if output, ok := info["output"]; ok {
		item["output"] = output
	}
	if errorText := stringField(step, "error"); errorText != "" {
		item["error"] = errorText
	}
	emit("runtime.antigravity.item.completed", map[string]any{"item": item})
}
