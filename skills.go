package relay

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// SkillPackage is a complete, content-addressed skill snapshot. Version must
// equal SkillVersion; timestamps and display metadata do not affect it.
type SkillPackage struct {
	ID      string      `json:"id"`
	Name    string      `json:"name"`
	Version string      `json:"version"`
	Files   []SkillFile `json:"files"`
}

type SkillFile struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	Encoding   string `json:"encoding,omitempty"`
	Executable bool   `json:"executable,omitempty"`
}

const SkillSupportLabel = "relay.skills"
const SkillSupportVersion = "1"
const MaxSkillBytes = 8 << 20
const MaxRequestSkillBytes = 160 << 20

type canonicalSkillFile struct {
	Path       string
	Data       []byte
	Executable bool
}

func skillFiles(skill SkillPackage) ([]canonicalSkillFile, int, error) {
	if skill.ID == "" || len(skill.Files) == 0 || len(skill.Files) > 200 {
		return nil, 0, errors.New("skill ID and 1–200 files are required")
	}
	files := make([]canonicalSkillFile, 0, len(skill.Files))
	seen := map[string]bool{}
	total := 0
	entrypoint := false
	for _, file := range skill.Files {
		p := file.Path
		if p == "" || path.IsAbs(p) || path.Clean(p) != p || p == "." || p == ".." || strings.HasPrefix(p, "../") || strings.ContainsAny(p, "\\\x00:") {
			return nil, 0, fmt.Errorf("invalid skill path %q", p)
		}
		for _, r := range p {
			if r < 0x20 || r == 0x7f {
				return nil, 0, fmt.Errorf("invalid skill path %q", p)
			}
		}
		if len(file.Content) > (MaxSkillBytes+2)/3*4 {
			return nil, 0, errors.New("encoded skill file exceeds 8 MiB")
		}
		key := strings.ToLower(p)
		if seen[key] {
			return nil, 0, fmt.Errorf("duplicate skill path %q", p)
		}
		seen[key] = true
		var data []byte
		switch file.Encoding {
		case "", "text":
			data = []byte(file.Content)
		case "base64":
			var err error
			data, err = base64.StdEncoding.DecodeString(file.Content)
			if err != nil {
				return nil, 0, fmt.Errorf("skill file %s: %w", p, err)
			}
		default:
			return nil, 0, fmt.Errorf("unsupported skill encoding %q", file.Encoding)
		}
		total += len(data)
		if total > MaxSkillBytes {
			return nil, 0, errors.New("skill exceeds 8 MiB")
		}
		if p == "SKILL.md" {
			entrypoint = true
		}
		files = append(files, canonicalSkillFile{p, data, file.Executable})
	}
	if !entrypoint {
		return nil, 0, errors.New("skill requires SKILL.md")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	// A file must never be another file's parent, including on case-insensitive hosts.
	for _, file := range files {
		for parent := path.Dir(file.Path); parent != "."; parent = path.Dir(parent) {
			if seen[strings.ToLower(parent)] {
				return nil, 0, fmt.Errorf("skill path has file parent: %s", file.Path)
			}
		}
	}
	return files, total, nil
}

func SkillVersion(skill SkillPackage) (string, error) {
	files, _, err := skillFiles(skill)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(files)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// ValidateSkills also guards mismatched snapshots and excessive aggregate size.
func ValidateSkills(skills []SkillPackage) error {
	seen := map[string]bool{}
	total := 0
	for _, skill := range skills {
		if seen[skill.ID] {
			return fmt.Errorf("duplicate skill ID %s", skill.ID)
		}
		seen[skill.ID] = true
		_, size, err := skillFiles(skill)
		if err != nil {
			return fmt.Errorf("skill %s: %w", skill.ID, err)
		}
		total += size
		if total > MaxRequestSkillBytes {
			return errors.New("request skills exceed 160 MiB")
		}
		version, err := SkillVersion(skill)
		if err != nil {
			return err
		}
		if skill.Version != version {
			return fmt.Errorf("skill %s content version mismatch", skill.ID)
		}
	}
	return nil
}

func DefaultSkillCacheRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "relay", "skills"), nil
}

// InstallSkills publishes immutable versions atomically. A completed version is
// reused without rewriting files. Old versions remain available to active runs.
func InstallSkills(ctx context.Context, root, tenant string, skills []SkillPackage) ([]InstructionFragment, error) {
	if err := ValidateSkills(skills); err != nil {
		return nil, err
	}
	if len(skills) == 0 {
		return nil, nil
	}
	if root == "" {
		var err error
		root, err = DefaultSkillCacheRoot()
		if err != nil {
			return nil, err
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	fragments := make([]InstructionFragment, 0, len(skills))
	for _, skill := range skills {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		identityInput, _ := json.Marshal([]string{tenant, skill.ID})
		identity := sha256.Sum256(identityInput)
		parent := filepath.Join(root, hex.EncodeToString(identity[:]))
		dest := filepath.Join(parent, skill.Version)
		if err := os.MkdirAll(parent, 0700); err != nil {
			return nil, err
		}
		marker := filepath.Join(dest, "complete")
		if raw, readErr := os.ReadFile(marker); readErr != nil || string(raw) != skill.Version {
			temp, err := os.MkdirTemp(parent, ".install-")
			if err != nil {
				return nil, err
			}
			installErr := func() error {
				defer os.RemoveAll(temp)
				files, _, err := skillFiles(skill)
				if err != nil {
					return err
				}
				for _, file := range files {
					if err := ctx.Err(); err != nil {
						return err
					}
					target := filepath.Join(temp, "bundle", filepath.FromSlash(file.Path))
					if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
						return err
					}
					mode := os.FileMode(0400)
					if file.Executable {
						mode = 0500
					}
					if err := os.WriteFile(target, file.Data, mode); err != nil {
						return err
					}
				}
				if err := os.WriteFile(filepath.Join(temp, "complete"), []byte(skill.Version), 0400); err != nil {
					return err
				}
				if err := os.Rename(temp, dest); err != nil {
					// Another process may have installed the same version concurrently.
					if raw, e := os.ReadFile(marker); e == nil && string(raw) == skill.Version {
						return nil
					}
					return err
				}
				return nil
			}()
			if installErr != nil {
				return nil, fmt.Errorf("install skill %s: %w", skill.ID, installErr)
			}
		}
		entrypoint := filepath.Join(dest, "bundle", "SKILL.md")
		fragments = append(fragments, InstructionFragment{ID: "relay-skill-" + skill.ID, Version: skill.Version, Title: "Assigned skill: " + skill.Name, Content: fmt.Sprintf("This skill was selected for this Agent. Apply it whenever it is relevant to the user's request. Read the complete skill instructions at %q before using it. Resolve all referenced files relative to that SKILL.md directory; the full skill bundle is installed there.", entrypoint)})
	}
	return fragments, nil
}
