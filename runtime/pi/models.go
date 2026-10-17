package pi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/KDF5000/relay/runtime/agentcli"
	runtimecodex "github.com/KDF5000/relay/runtime/codex"
	runtimeprocess "github.com/KDF5000/relay/runtime/process"
)

// ProbeModels uses Pi's RPC catalog rather than scraping its terminal table.
// This reads the models available with the Node user's credentials, without
// submitting a prompt or creating a persistent session.
func ProbeModels(ctx context.Context, config Config) ([]runtimecodex.ModelInfo, error) {
	binary := config.Binary
	if binary == "" {
		binary = "pi"
	}
	processCtx, stop := context.WithCancel(ctx)
	defer stop()
	command := exec.CommandContext(processCtx, binary, "--mode", "rpc", "--no-session")
	runtimeprocess.Configure(command)
	command.Dir = config.WorkDir
	command.Env = piEnvironment(config, agentcli.Run{})
	command.Stdin = strings.NewReader("{\"id\":\"relay-models\",\"type\":\"get_available_models\"}\n{\"id\":\"relay-state\",\"type\":\"get_state\"}\n")
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// Pi extensions may log credentials or provider diagnostics. Do not surface
	// their stderr through model-discovery logs.
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("relay pi: start model discovery: %w", err)
	}
	defer func() { stop(); _ = command.Wait() }()
	type model struct {
		ID       string `json:"id"`
		Provider string `json:"provider"`
		Name     string `json:"name"`
	}
	var available []model
	var current model
	catalogReceived, stateReceived := false, false
	decoder := json.NewDecoder(io.LimitReader(stdout, 8<<20))
	for !catalogReceived || !stateReceived {
		var response struct {
			ID      string          `json:"id"`
			Type    string          `json:"type"`
			Success bool            `json:"success"`
			Data    json.RawMessage `json:"data"`
		}
		if err := decoder.Decode(&response); err != nil {
			if processCtx.Err() != nil {
				return nil, processCtx.Err()
			}
			// Older Pi versions may provide the catalog without get_state support.
			if err == io.EOF && catalogReceived {
				break
			}
			return nil, fmt.Errorf("relay pi: read model catalog: %w", err)
		}
		if response.Type != "response" {
			continue
		}
		switch response.ID {
		case "relay-models":
			if !response.Success {
				return nil, errors.New("relay pi: model catalog request failed")
			}
			var data struct {
				Models []model `json:"models"`
			}
			if err := json.Unmarshal(response.Data, &data); err != nil {
				return nil, errors.New("relay pi: invalid model catalog response")
			}
			available = data.Models
			catalogReceived = true
		case "relay-state":
			stateReceived = true
			if response.Success {
				var data struct {
					Model model `json:"model"`
				}
				if json.Unmarshal(response.Data, &data) == nil {
					current = data.Model
				}
			}
		}
	}
	result := make([]runtimecodex.ModelInfo, 0, len(available))
	seen := make(map[string]bool)
	for _, m := range available {
		if m.ID == "" || m.Provider == "" {
			continue
		}
		id := m.Provider + "/" + m.ID
		if seen[id] {
			continue
		}
		seen[id] = true
		name := m.Name
		if name == "" {
			name = m.ID
		}
		result = append(result, runtimecodex.ModelInfo{ID: id, DisplayName: name + " (" + m.Provider + ")", Default: m.ID == current.ID && m.Provider == current.Provider})
	}
	return result, nil
}

func piEnvironment(config Config, run agentcli.Run) []string {
	return agentcli.Environment(config, run,
		"PI_CODING_AGENT_DIR", "PI_CODING_AGENT_SESSION_DIR",
		"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GOOGLE_API_KEY", "GEMINI_API_KEY",
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_REGION", "AWS_PROFILE")
}
