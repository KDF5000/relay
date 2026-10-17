// Package agentcli contains shared process setup for native coding-agent CLIs.
package agentcli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/KDF5000/relay"
	runtimeartifacts "github.com/KDF5000/relay/runtime/artifacts"
	"github.com/KDF5000/relay/runtime/toolbridge"
)

type Config struct {
	Binary          string
	ToolDir         string
	PassEnv         []string
	Env             map[string]string
	Model           string
	WorkRoot        string
	WorkDir         string
	Ephemeral       bool
	AllowedTools    []string
	DisallowedTools []string
	ExtraArgs       []string
}

type Run struct {
	WorkDir         string
	Instruction     string
	FinalPath       string
	ImagePaths      []string
	ToolBridgeDir   string
	ToolBridgeToken string
	close           func()
}

func Prepare(execution relay.Execution, config Config, provider string) (Run, error) {
	workDir := execution.WorkDir
	if workDir == "" {
		workDir = config.WorkDir
	}
	if workDir == "" {
		root := config.WorkRoot
		if root == "" {
			root = filepath.Join(os.TempDir(), "relay-runs")
		}
		workDir = filepath.Join(root, execution.RunID)
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return Run{}, err
	}
	instruction, err := relay.MaterializeInstructions(workDir, provider, execution.Instructions)
	if err != nil {
		return Run{}, err
	}
	resultDir := filepath.Join(workDir, ".relay")
	if err := os.MkdirAll(resultDir, 0o700); err != nil {
		return Run{}, err
	}
	if err := runtimeartifacts.Reset(workDir); err != nil {
		return Run{}, err
	}
	images, cleanupImages, err := relay.MaterializeInputImages(workDir, execution.Input)
	if err != nil {
		return Run{}, err
	}
	bridge, err := toolbridge.StartFile(execution.Capabilities, filepath.Join(resultDir, "tool-bridge"))
	if err != nil {
		cleanupImages()
		return Run{}, err
	}
	return Run{
		WorkDir: workDir, Instruction: instruction,
		FinalPath:  filepath.Join(resultDir, provider+"-last-message.txt"),
		ImagePaths: images, ToolBridgeDir: bridge.Dir, ToolBridgeToken: bridge.Token,
		close: func() { bridge.Close(); cleanupImages() },
	}, nil
}

func (r Run) Close() {
	if r.close != nil {
		r.close()
	}
}

func (r Run) Artifacts(message string) ([]relay.Artifact, error) {
	if err := os.WriteFile(r.FinalPath, []byte(strings.TrimSpace(message)), 0o600); err != nil {
		return nil, err
	}
	return runtimeartifacts.Collect(r.WorkDir)
}

func Environment(config Config, run Run, providerEnv ...string) []string {
	allowed := []string{
		"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TMP", "TEMP",
		"LANG", "LC_ALL", "TERM", "COLORTERM",
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS",
		"SystemRoot", "ComSpec", "PATHEXT",
	}
	allowed = append(allowed, providerEnv...)
	allowed = append(allowed, config.PassEnv...)
	environment := make([]string, 0, len(allowed)+len(config.Env)+3)
	seen := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		if value, ok := os.LookupEnv(name); ok {
			environment = append(environment, name+"="+value)
		}
	}
	for name, value := range config.Env {
		environment = setEnv(environment, name, value)
	}
	if config.ToolDir != "" {
		toolDir := config.ToolDir
		if absolute, err := filepath.Abs(toolDir); err == nil {
			toolDir = absolute
		}
		environment = setEnv(environment, "PATH", toolDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	environment = setEnv(environment, "RELAY_TOOL_DIR", run.ToolBridgeDir)
	environment = setEnv(environment, "RELAY_TOOL_TOKEN", run.ToolBridgeToken)
	return environment
}

func setEnv(environment []string, name, value string) []string {
	prefix := name + "="
	for index, entry := range environment {
		if separator := strings.IndexByte(entry, '='); separator >= 0 && strings.EqualFold(entry[:separator], name) {
			environment[index] = prefix + value
			return environment
		}
	}
	return append(environment, prefix+value)
}

type LimitedBuffer struct {
	Buffer    bytes.Buffer
	Remaining int
}

func (w *LimitedBuffer) Write(value []byte) (int, error) {
	if len(value) > w.Remaining {
		return 0, errors.New("runtime stderr limit exceeded")
	}
	n, err := w.Buffer.Write(value)
	w.Remaining -= n
	return n, err
}

func (w *LimitedBuffer) String() string { return w.Buffer.String() }

func Truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

func ResumeUnavailable(stderr string) bool {
	value := strings.ToLower(stderr)
	for _, fragment := range []string{"session not found", "unknown session", "no saved session", "failed to load session", "could not find session", "invalid session id"} {
		if strings.Contains(value, fragment) {
			return true
		}
	}
	return false
}
