package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
)

// ModelInfo is the model metadata exposed by a Codex-compatible app server.
type ModelInfo struct {
	ID          string
	DisplayName string
	Description string
	Default     bool
}

// ProbeModels reads the visible model catalog from a Codex-compatible app server.
func ProbeModels(ctx context.Context, config Config, fork ForkOptions) ([]ModelInfo, error) {
	binary := config.Binary
	if binary == "" {
		binary = fork.DefaultBinary
	}
	if binary == "" {
		binary = "codex"
	}
	processCtx, stop := context.WithCancel(ctx)
	defer stop()
	command := exec.CommandContext(processCtx, binary, "app-server")
	command.Env = runtimeEnv(config, "", "", fork.PassEnv)
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	command.Stderr = &limitedBuffer{buffer: &stderr, remaining: 1 << 20}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("relay %s: start model discovery: %w", fork.Name, err)
	}
	defer func() {
		stop()
		_ = stdin.Close()
		_ = command.Wait()
	}()

	decoder := json.NewDecoder(stdout)
	write := func(value any) error { return json.NewEncoder(stdin).Encode(value) }
	if err := write(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]string{"name": "relay-model-probe", "version": "0.1"}, "capabilities": map[string]bool{"experimentalApi": true}}}); err != nil {
		return nil, err
	}
	if _, err := waitRPCResponse(decoder, 1, nil); err != nil {
		return nil, appServerError(fork.Name, err, stderr.String())
	}
	if err := write(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return nil, err
	}

	models := make([]ModelInfo, 0, 16)
	var cursor string
	for page := 0; page < 10; page++ {
		id := page + 2
		params := map[string]any{"limit": 100, "includeHidden": false}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if err := write(map[string]any{"id": id, "method": "model/list", "params": params}); err != nil {
			return nil, err
		}
		data, err := waitRPCResponse(decoder, id, nil)
		if err != nil {
			return nil, appServerError(fork.Name, err, stderr.String())
		}
		var response struct {
			Data []struct {
				ID          string `json:"id"`
				Model       string `json:"model"`
				DisplayName string `json:"displayName"`
				Description string `json:"description"`
				Hidden      bool   `json:"hidden"`
				Default     bool   `json:"isDefault"`
			} `json:"data"`
			NextCursor *string `json:"nextCursor"`
		}
		if err := json.Unmarshal(data, &response); err != nil {
			return nil, fmt.Errorf("relay %s: decode model catalog: %w", fork.Name, err)
		}
		for _, item := range response.Data {
			modelID := item.Model
			if modelID == "" {
				modelID = item.ID
			}
			if modelID == "" || item.Hidden {
				continue
			}
			models = append(models, ModelInfo{ID: modelID, DisplayName: item.DisplayName, Description: item.Description, Default: item.Default})
		}
		if response.NextCursor == nil || *response.NextCursor == "" {
			break
		}
		cursor = *response.NextCursor
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("relay %s: model discovery returned no visible models", fork.Name)
	}
	return models, nil
}
