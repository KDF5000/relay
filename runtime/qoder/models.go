package qoder

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/KDF5000/relay/runtime/agentcli"
	runtimecodex "github.com/KDF5000/relay/runtime/codex"
	runtimeprocess "github.com/KDF5000/relay/runtime/process"
)

// defaultModel is the CLI's own selection when --model is omitted.
const defaultModel = "Auto"

// ProbeModels reads `--list-models`. Built-in models are addressed by name;
// custom models are listed as "Name (model-id)" and addressed by their ID.
func ProbeModels(ctx context.Context, config Config, variant Variant) ([]runtimecodex.ModelInfo, error) {
	if variant.Provider == "" {
		variant = Qoder
	}
	binary := ResolveBinary(variant, config.Binary)
	command := exec.CommandContext(ctx, binary, "--list-models")
	runtimeprocess.Configure(command)
	command.Dir = config.WorkDir
	command.Env = agentcli.Environment(config, agentcli.Run{}, variant.Env...)
	output := &agentcli.LimitedBuffer{Remaining: 4 << 20}
	command.Stdout = output
	// Account diagnostics may contain credentials; only the login state is surfaced.
	diagnostics := &agentcli.LimitedBuffer{Remaining: 64 << 10}
	command.Stderr = diagnostics
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("relay %s: discover models: %w%s", variant.Provider, err, loginHint(binary, output.String()+diagnostics.String()))
	}
	return parseModels(output.String(), config.Model), nil
}

func parseModels(output, configured string) []runtimecodex.ModelInfo {
	selected := configured
	if selected == "" {
		selected = defaultModel
	}
	seen := map[string]bool{}
	var models []runtimecodex.ModelInfo
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "MODEL" || strings.ContainsAny(line, "\t\x1b") {
			continue
		}
		id, display := line, line
		if open := strings.LastIndex(line, " ("); open > 0 && strings.HasSuffix(line, ")") {
			display = strings.TrimSpace(line[:open])
			id = strings.TrimSpace(line[open+2 : len(line)-1])
		}
		if id == "" || strings.ContainsAny(id, " ") || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, runtimecodex.ModelInfo{ID: id, DisplayName: display, Default: id == selected})
	}
	return models
}

// probeEnvironment drops embedding-SDK variables: a Relay Node started from a
// Qoder-hosted shell would otherwise make the CLI refuse to run standalone.
func probeEnvironment() []string {
	var environment []string
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "QODER_AGENT_SDK_") || strings.HasPrefix(entry, "QODER_SDK_") {
			continue
		}
		environment = append(environment, entry)
	}
	return environment
}
