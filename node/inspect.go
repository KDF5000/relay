package node

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/KDF5000/relay/transport/httpapi"
	"github.com/KDF5000/relay/workspace"
)

type inspectionTransport interface {
	ClaimInspection(context.Context, string) (httpapi.Inspection, error)
	FinishInspection(context.Context, string, string, httpapi.InspectionResult) error
}

func (w *Worker) serveInspections(ctx context.Context) {
	client, ok := w.ControlPlane.(inspectionTransport)
	if !ok {
		return
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		request, err := client.ClaimInspection(ctx, w.Registration.ID)
		if err != nil || request.ID == "" {
			continue
		}
		result := httpapi.InspectionResult{}
		if root := w.inspectionRoot(request.RunID); root != "" {
			inspectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			result, err = inspectDirectory(inspectCtx, root, request.Operation, request.Path)
			cancel()
		} else {
			err = errors.New("workspace unavailable on this node for this run")
		}
		if err != nil {
			result.Error = err.Error()
		}
		_ = client.FinishInspection(ctx, w.Registration.ID, request.ID, result)
	}
}
func (w *Worker) inspectionIndex(runID string) string {
	if manager, ok := w.Workspaces.(*workspace.Manager); ok && manager.Root != "" {
		return filepath.Join(manager.Root, "inspection", fmt.Sprintf("%x", sha256.Sum256([]byte(runID))))
	}
	return ""
}
func (w *Worker) rememberInspection(runID, root string) {
	w.inspectionDirs.Store(runID, root)
	if index := w.inspectionIndex(runID); index != "" {
		if os.MkdirAll(filepath.Dir(index), 0700) == nil {
			_ = os.WriteFile(index, []byte(root), 0600)
		}
	}
}
func (w *Worker) inspectionRoot(runID string) string {
	if root, ok := w.inspectionDirs.Load(runID); ok {
		return root.(string)
	}
	if index := w.inspectionIndex(runID); index != "" {
		if data, err := os.ReadFile(index); err == nil {
			return string(data)
		}
	}
	return ""
}
func inspectDirectory(ctx context.Context, root, operation, path string) (httpapi.InspectionResult, error) {
	result := httpapi.InspectionResult{Root: root, Path: path, Entries: []httpapi.FileEntry{}}
	if filepath.IsAbs(path) || !filepath.IsLocal(path) && path != "" {
		return result, errors.New("path must stay inside the workspace")
	}
	// os.Root rejects symlink escapes and traversal, including concurrent path changes.
	dir, err := os.OpenRoot(root)
	if err != nil {
		return result, err
	}
	defer dir.Close()
	target := path
	if target == "" {
		target = "."
	}
	if operation == "diff" {
		if path != "" {
			return result, errors.New("diff accepts only the workspace root")
		}
		command := exec.CommandContext(ctx, "git", "diff", "--no-ext-diff", "--no-textconv", "HEAD", "--", ".")
		command.Dir = root
		pipe, err := command.StdoutPipe()
		if err != nil {
			return result, err
		}
		var stderr strings.Builder
		command.Stderr = &stderr
		if err = command.Start(); err != nil {
			return result, err
		}
		data, err := io.ReadAll(io.LimitReader(pipe, (2<<20)+1))
		if len(data) > 2<<20 {
			_ = command.Process.Kill()
			_ = command.Wait()
			return result, errors.New("diff exceeds 2 MiB")
		}
		waitErr := command.Wait()
		if err != nil {
			return result, err
		}
		if waitErr != nil {
			message := strings.TrimSpace(stderr.String())
			if message == "" {
				message = waitErr.Error()
			}
			return result, fmt.Errorf("Git diff unavailable for this workspace: %s", message)
		}
		result.Content = string(data)
		return result, nil
	}
	if operation == "git-status" {
		if path != "" {
			return result, errors.New("git-status accepts only the workspace root")
		}
		result.Git = inspectGitStatus(ctx, root)
		return result, nil
	}
	file, err := dir.Open(target)
	if err != nil {
		return result, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return result, err
	}
	switch operation {
	case "list":
		if !info.IsDir() {
			return result, errors.New("not a directory")
		}
		entries, err := file.ReadDir(1001)
		if err != nil && err != io.EOF {
			return result, err
		}
		if len(entries) > 1000 {
			return result, errors.New("directory exceeds 1000 entries")
		}
		for _, entry := range entries {
			if entry.Name() == ".git" {
				continue
			}
			if entry.Type()&os.ModeSymlink != 0 {
				continue
			}
			stat, err := entry.Info()
			if err != nil {
				continue
			}
			result.Entries = append(result.Entries, httpapi.FileEntry{Name: entry.Name(), Directory: entry.IsDir(), Size: stat.Size()})
		}
	case "read":
		if !info.Mode().IsRegular() {
			return result, errors.New("not a regular file")
		}
		data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
		if err != nil {
			return result, err
		}
		if len(data) > 1<<20 {
			return result, errors.New("file exceeds 1 MiB")
		}
		if !utf8.Valid(data) || strings.ContainsRune(string(data), 0) {
			return result, errors.New("binary file preview is unavailable")
		}
		result.Content = string(data)
	default:
		return result, errors.New("unsupported operation")
	}
	return result, nil
}

func inspectGitStatus(ctx context.Context, root string) *httpapi.GitStatus {
	status := &httpapi.GitStatus{}
	inside := exec.CommandContext(ctx, "git", "--no-optional-locks", "rev-parse", "--is-inside-work-tree")
	inside.Dir = root
	inside.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	output, err := inside.Output()
	if err != nil || strings.TrimSpace(string(output)) != "true" {
		return status
	}
	status.Repository = true
	branch := exec.CommandContext(ctx, "git", "--no-optional-locks", "symbolic-ref", "--quiet", "--short", "HEAD")
	branch.Dir = root
	branch.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	if output, branchErr := branch.Output(); branchErr == nil {
		status.Branch = strings.TrimSpace(string(output))
	} else {
		status.Detached = true
	}
	commit := exec.CommandContext(ctx, "git", "--no-optional-locks", "rev-parse", "--short=7", "HEAD")
	commit.Dir = root
	commit.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	if output, commitErr := commit.Output(); commitErr == nil {
		status.Commit = strings.TrimSpace(string(output))
	}
	return status
}
