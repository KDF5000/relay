// Package trae adapts TraeCode CLI's codex-compatible exec protocol to Relay.
package trae

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/KDF5000/relay"
	runtimecodex "github.com/KDF5000/relay/runtime/codex"
)

type Config = runtimecodex.Config
type ModelInfo = runtimecodex.ModelInfo
type Executor struct{ Config Config }

func (e Executor) Execute(ctx context.Context, execution relay.Execution) (relay.Result, error) {
	config := e.Config
	if config.Binary == "" {
		config.Binary = ResolveBinary("")
	}
	return runtimecodex.ExecuteFork(ctx, config, execution, runtimecodex.ForkOptions{Name: "trae", DefaultBinary: "traex", InstructionProvider: "trae", PassEnv: []string{"TRAE_HOME", "TRAE_API_KEY", "TRAE_BASE_URL"}})
}

func ProbeVersion(ctx context.Context, binary string) (string, error) {
	binary = ResolveBinary(binary)
	output, err := exec.CommandContext(ctx, binary, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("relay trae: probe version: %w: %s", err, output)
	}
	return runtimecodex.ParseVersionOutput(string(output)), nil
}

func ProbeModels(ctx context.Context, config Config) ([]ModelInfo, error) {
	if config.Binary == "" {
		config.Binary = ResolveBinary("")
	}
	return runtimecodex.ProbeModels(ctx, config, runtimecodex.ForkOptions{Name: "trae", DefaultBinary: "traex", PassEnv: []string{"TRAE_HOME", "TRAE_API_KEY", "TRAE_BASE_URL"}})
}

func ResolveBinary(binary string) string {
	if binary != "" {
		return binary
	}
	if _, err := exec.LookPath("traex"); err == nil {
		return "traex"
	}
	return "trae-cli"
}
