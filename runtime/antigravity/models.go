package antigravity

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/KDF5000/relay/runtime/agentcli"
	runtimecodex "github.com/KDF5000/relay/runtime/codex"
	runtimeprocess "github.com/KDF5000/relay/runtime/process"
)

// ProbeModels reads agy's tab-separated slug/display-name catalog. The CLI
// does not expose the selected default here, so only mark a configured default.
func ProbeModels(ctx context.Context, config Config) ([]runtimecodex.ModelInfo, error) {
	command := exec.CommandContext(ctx, ResolveBinary(config.Binary), "models")
	runtimeprocess.Configure(command)
	command.Env = environment(config, agentcli.Run{})
	command.Dir = config.WorkDir
	output := &agentcli.LimitedBuffer{Remaining: 4 << 20}
	command.Stdout = output
	diagnostics := &agentcli.LimitedBuffer{Remaining: 64 << 10}
	command.Stderr = diagnostics
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("relay antigravity: discover models: %w", err)
	}
	seen := map[string]bool{}
	var models []runtimecodex.ModelInfo
	for _, line := range strings.Split(output.String(), "\n") {
		id, display, ok := strings.Cut(strings.TrimSpace(line), "\t")
		id, display = strings.TrimSpace(id), strings.TrimSpace(display)
		if !ok || id == "" || display == "" || strings.ContainsAny(id, " \x1b") || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, runtimecodex.ModelInfo{ID: id, DisplayName: display, Default: id == config.Model})
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("relay antigravity: model discovery returned no models")
	}
	return models, nil
}
