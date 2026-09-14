package codex

import (
	"encoding/json"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/KDF5000/relay"
)

const artifactManifestPath = ".relay/artifacts.json"

type artifactManifest struct {
	Artifacts []declaredArtifact `json:"artifacts"`
}

type declaredArtifact struct {
	Path        string `json:"path"`
	Type        string `json:"type"`
	Name        string `json:"name,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

func resetArtifactManifest(workDir string) error {
	err := os.Remove(filepath.Join(workDir, artifactManifestPath))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reset artifact manifest: %w", err)
	}
	return nil
}

func collectDeclaredArtifacts(workDir string) ([]relay.Artifact, error) {
	manifestFile := filepath.Join(workDir, artifactManifestPath)
	content, err := os.ReadFile(manifestFile)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read artifact manifest: %w", err)
	}
	if len(content) > 256<<10 {
		return nil, fmt.Errorf("artifact manifest exceeds 256 KiB")
	}
	var manifest artifactManifest
	if err := json.Unmarshal(content, &manifest); err != nil {
		return nil, fmt.Errorf("parse artifact manifest: %w", err)
	}
	if len(manifest.Artifacts) > 32 {
		return nil, fmt.Errorf("artifact manifest declares more than 32 files")
	}
	root, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolve work directory: %w", err)
	}
	result := make([]relay.Artifact, 0, len(manifest.Artifacts))
	seen := make(map[string]struct{}, len(manifest.Artifacts))
	for index, declared := range manifest.Artifacts {
		path := filepath.Clean(strings.TrimSpace(declared.Path))
		if path == "." || filepath.IsAbs(path) {
			return nil, fmt.Errorf("artifact %d path must be relative to the work directory", index+1)
		}
		if path == ".relay" || strings.HasPrefix(path, ".relay"+string(os.PathSeparator)) {
			return nil, fmt.Errorf("artifact %d cannot reference Relay's internal directory", index+1)
		}
		resolved, err := filepath.EvalSymlinks(filepath.Join(root, path))
		if err != nil {
			return nil, fmt.Errorf("resolve artifact %d path: %w", index+1, err)
		}
		relative, err := filepath.Rel(root, resolved)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
			return nil, fmt.Errorf("artifact %d path escapes the work directory", index+1)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return nil, fmt.Errorf("stat artifact %d: %w", index+1, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("artifact %d is not a regular file", index+1)
		}
		if _, duplicate := seen[resolved]; duplicate {
			continue
		}
		seen[resolved] = struct{}{}
		typ := strings.TrimSpace(declared.Type)
		if typ == "" {
			typ = "deliverable"
		}
		name := strings.TrimSpace(declared.Name)
		if name == "" {
			name = filepath.Base(resolved)
		}
		contentType := strings.TrimSpace(declared.ContentType)
		if contentType == "" {
			extension := strings.ToLower(filepath.Ext(name))
			if extension == ".md" || extension == ".markdown" {
				contentType = "text/markdown; charset=utf-8"
			} else {
				contentType = mime.TypeByExtension(extension)
			}
			if contentType == "" {
				contentType = "application/octet-stream"
			}
		}
		result = append(result, relay.Artifact{
			Type:        typ,
			Ref:         resolved,
			Name:        name,
			ContentType: contentType,
			Size:        info.Size(),
		})
	}
	return result, nil
}
