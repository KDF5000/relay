package opencode

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/KDF5000/relay/runtime/agentcli"
	runtimecodex "github.com/KDF5000/relay/runtime/codex"
	runtimeprocess "github.com/KDF5000/relay/runtime/process"
)

// ProbeModels reads the provider/model IDs exposed by the Node user's OpenCode
// configuration. The CLI does not expose model display names or the default.
func ProbeModels(ctx context.Context, config Config) ([]runtimecodex.ModelInfo, error) {
	binary := resolveBinary(config.Binary)
	version, err := ProbeVersion(ctx, binary)
	if err != nil {
		return nil, err
	}
	args := []string{"models"}
	if isV2(version) {
		args = append(args, "--standalone")
	}
	command := exec.CommandContext(ctx, binary, args...)
	runtimeprocess.Configure(command)
	command.Dir = config.WorkDir
	command.Env = environment(config, agentcli.Run{})
	output := &agentcli.LimitedBuffer{Remaining: 8 << 20}
	command.Stdout = output
	// Provider diagnostics may contain credentials; omit them from probe errors.
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("relay opencode: discover models: %w", err)
	}
	seen := map[string]bool{}
	var models []runtimecodex.ModelInfo
	for _, line := range strings.Split(output.String(), "\n") {
		id := strings.TrimSpace(line)
		provider, model, ok := strings.Cut(id, "/")
		if !ok || provider == "" || model == "" || strings.ContainsAny(id, " \t\r\x1b") || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, runtimecodex.ModelInfo{ID: id, DisplayName: model + " (" + provider + ")", Default: id == config.Model})
	}
	return models, nil
}
