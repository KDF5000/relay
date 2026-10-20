package relay

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	instructionMarkerBegin = "<!-- BEGIN RELAY-RUNTIME (auto-managed; do not edit) -->"
	instructionMarkerEnd   = "<!-- END RELAY-RUNTIME -->"
)

type DefaultInstructionCompiler struct{}

func (DefaultInstructionCompiler) Compile(input Input, bundle InstructionBundle) (CompiledInstructions, error) {
	var stable strings.Builder
	stable.WriteString("# Relay Runtime\n\n")
	stable.WriteString("This execution is managed by Relay. Return durable results before the top-level agent process exits.\n")
	for _, layer := range [][]InstructionFragment{bundle.Runtime, bundle.Host, bundle.Workspace, bundle.Agent} {
		for _, fragment := range layer {
			appendInstruction(&stable, fragment)
		}
	}
	var prompt strings.Builder
	prompt.WriteString(strings.TrimSpace(input.Prompt))
	for _, fragment := range bundle.Turn {
		appendInstruction(&prompt, fragment)
	}
	return CompiledInstructions{
		Stable: strings.TrimRight(stable.String(), "\n") + "\n",
		Prompt: strings.TrimSpace(prompt.String()) + "\n",
	}, nil
}

func appendInstruction(builder *strings.Builder, fragment InstructionFragment) {
	content := strings.TrimSpace(fragment.Content)
	if content == "" {
		return
	}
	title := strings.TrimSpace(fragment.Title)
	if title == "" {
		title = strings.TrimSpace(fragment.ID)
	}
	builder.WriteString("\n\n## ")
	builder.WriteString(title)
	builder.WriteString("\n\n")
	builder.WriteString(content)
	builder.WriteByte('\n')
}

// CapabilityToolInstruction tells CLI-based runtimes how to reach only the
// capabilities granted to the current run.
func CapabilityToolInstruction(grants []CapabilityGrant) InstructionFragment {
	var builder strings.Builder
	builder.WriteString("Use `relay-tool call` when live host data or actions are needed. Do not invoke host-specific CLIs directly. Every call requires a unique, stable `--idempotency` value.\n")
	if len(grants) > 0 {
		builder.WriteString("\nGranted capabilities:\n")
		for _, grant := range grants {
			builder.WriteString("- `")
			builder.WriteString(grant.Name)
			builder.WriteString("@")
			builder.WriteString(grant.Version)
			builder.WriteString("`")
			if len(grant.Resources) > 0 {
				builder.WriteString(" resources: `")
				builder.WriteString(strings.Join(grant.Resources, "`, `"))
				builder.WriteString("`")
			}
			builder.WriteByte('\n')
		}
	}
	return InstructionFragment{ID: "relay-capability-tools", Version: "1", Title: "Relay capabilities", Content: builder.String()}
}

// MaterializeInstructions writes the stable instruction layer into the native file
// expected by a provider while preserving host-owned content outside Relay's block.
func MaterializeInstructions(workDir, provider string, compiled CompiledInstructions) (string, error) {
	name, err := instructionFileName(provider)
	if err != nil {
		return "", err
	}
	path := filepath.Join(workDir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	block := instructionMarkerBegin + "\n" + strings.TrimRight(compiled.Stable, "\n") + "\n" + instructionMarkerEnd + "\n"
	existing, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return path, os.WriteFile(path, []byte(block), 0o644)
	}
	if err != nil {
		return "", fmt.Errorf("relay: read instruction file: %w", err)
	}
	content := string(existing)
	start := strings.Index(content, instructionMarkerBegin)
	if start >= 0 {
		endRelative := strings.Index(content[start:], instructionMarkerEnd)
		if endRelative < 0 {
			return "", errors.New("relay: incomplete managed instruction marker")
		}
		end := start + endRelative + len(instructionMarkerEnd)
		if end < len(content) && content[end] == '\n' {
			end++
		}
		return path, os.WriteFile(path, []byte(content[:start]+block+content[end:]), 0o644)
	}
	separator := "\n\n"
	if len(content) == 0 {
		separator = ""
	}
	return path, os.WriteFile(path, []byte(content+separator+block), 0o644)
}

func instructionFileName(provider string) (string, error) {
	switch strings.ToLower(provider) {
	case "codex", "trae", "traex", "copilot", "cursor", "opencode", "hermes", "pi", "qoder", "qwenwork", "mock":
		return "AGENTS.md", nil
	case "claude":
		return "CLAUDE.md", nil
	case "qwen":
		return "QWEN.md", nil
	case "codebuddy":
		return "CODEBUDDY.md", nil
	default:
		return "", fmt.Errorf("relay: unsupported instruction provider %q", provider)
	}
}
