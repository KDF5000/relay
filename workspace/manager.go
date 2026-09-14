package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/KDF5000/relay"
	runtimeprocess "github.com/KDF5000/relay/runtime/process"
)

type Prepared struct {
	Dir     string
	Cleanup func(context.Context) error
}

type Provider interface {
	Prepare(context.Context, string, string, relay.WorkspaceSpec) (Prepared, error)
}

// Releaser removes a retained workspace when its caller-owned lifecycle ends.
// Implementations must not silently discard uncommitted work.
type Releaser interface {
	Release(context.Context, relay.WorkspaceSpec) error
}

// Manager prepares isolated workspaces and keeps bare Git mirrors for efficient
// reuse across attempts. It does not attach business semantics to repositories.
type Manager struct {
	Root string
	mu   sync.Mutex
	keys map[string]*sync.Mutex
}

func (m *Manager) Prepare(ctx context.Context, runID, attemptID string, spec relay.WorkspaceSpec) (Prepared, error) {
	root := m.Root
	if root == "" {
		root = filepath.Join(os.TempDir(), "relay-workspaces")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return Prepared{}, err
	}
	if err := validateSpec(spec); err != nil {
		return Prepared{}, err
	}
	switch spec.Kind {
	case "", "temp":
		dir := filepath.Join(root, "runs", safeSegment(runID), safeSegment(attemptID))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return Prepared{}, err
		}
		return preparedDirectory(root, dir, spec.Subdir, spec.Ephemeral || spec.Kind == "temp")
	case "local":
		if spec.Source == "" || !filepath.IsAbs(spec.Source) {
			return Prepared{}, errors.New("relay workspace: local source must be an absolute path")
		}
		info, err := os.Stat(spec.Source)
		if err != nil || !info.IsDir() {
			return Prepared{}, fmt.Errorf("relay workspace: local source is not a directory: %s", spec.Source)
		}
		return preparedDirectory("", spec.Source, spec.Subdir, false)
	case "git":
		if spec.Source == "" {
			return Prepared{}, errors.New("relay workspace: git source is required")
		}
		return m.prepareGit(ctx, root, runID, attemptID, spec)
	default:
		return Prepared{}, fmt.Errorf("relay workspace: unsupported kind %q", spec.Kind)
	}
}

// Release removes a reusable Git worktree while preserving its branch in the
// mirror. Git refuses the removal when the worktree contains uncommitted work.
func (m *Manager) Release(ctx context.Context, spec relay.WorkspaceSpec) error {
	if err := validateSpec(spec); err != nil {
		return err
	}
	if spec.Lifecycle != "reusable" {
		return errors.New("relay workspace: release requires reusable lifecycle")
	}
	root := m.Root
	if root == "" {
		root = filepath.Join(os.TempDir(), "relay-workspaces")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	sourceDigest := sha256.Sum256([]byte(spec.Source))
	mirror := filepath.Join(root, "git", hex.EncodeToString(sourceDigest[:12])+".git")
	keyDigest := sha256.Sum256([]byte(spec.Source + "\x00" + spec.ReuseKey))
	worktree := filepath.Join(root, "reusable", hex.EncodeToString(keyDigest[:16]))
	release := m.lockKey(hex.EncodeToString(keyDigest[:]))
	defer release()
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := os.Stat(worktree); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := runGit(ctx, mirror, "worktree", "remove", worktree); err != nil {
		return fmt.Errorf("relay workspace: release refused; commit, discard, or preserve pending changes first: %w", err)
	}
	return nil
}

func (m *Manager) prepareGit(ctx context.Context, root, runID, attemptID string, spec relay.WorkspaceSpec) (Prepared, error) {
	digest := sha256.Sum256([]byte(spec.Source))
	mirror := filepath.Join(root, "git", hex.EncodeToString(digest[:12])+".git")
	worktree := filepath.Join(root, "runs", safeSegment(runID), safeSegment(attemptID))
	reusable := spec.Lifecycle == "reusable"
	release := func() {}
	if reusable {
		keyDigest := sha256.Sum256([]byte(spec.Source + "\x00" + spec.ReuseKey))
		worktree = filepath.Join(root, "reusable", hex.EncodeToString(keyDigest[:16]))
		release = m.lockKey(hex.EncodeToString(keyDigest[:]))
	}
	keepLock := false
	defer func() {
		if !keepLock {
			release()
		}
	}()
	ref := spec.Ref
	if ref == "" {
		ref = "HEAD"
	}
	m.mu.Lock()
	locked := true
	defer func() {
		if locked {
			m.mu.Unlock()
		}
	}()
	if _, err := os.Stat(mirror); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(mirror), 0o700); err != nil {
			return Prepared{}, err
		}
		if err := runGit(ctx, "", "clone", "--mirror", "--", spec.Source, mirror); err != nil {
			return Prepared{}, err
		}
	} else if err != nil {
		return Prepared{}, err
	} else if err := runGit(ctx, mirror, "fetch", "--prune", "origin"); err != nil {
		return Prepared{}, err
	}
	if reusable {
		if info, err := os.Stat(worktree); err == nil {
			if !info.IsDir() {
				return Prepared{}, fmt.Errorf("relay workspace: reusable workspace is not a directory: %s", worktree)
			}
			if err := runGit(ctx, worktree, "rev-parse", "--is-inside-work-tree"); err != nil {
				return Prepared{}, fmt.Errorf("relay workspace: reusable workspace is invalid: %w", err)
			}
			m.mu.Unlock()
			locked = false
			prepared, err := preparedDirectory(root, worktree, spec.Subdir, false)
			if err != nil {
				return Prepared{}, err
			}
			keepLock = true
			prepared.Cleanup = func(context.Context) error {
				release()
				return nil
			}
			return prepared, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return Prepared{}, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(worktree), 0o700); err != nil {
		return Prepared{}, err
	}
	if !reusable {
		_ = os.RemoveAll(worktree)
	}
	args := []string{"worktree", "add", "--detach", worktree, ref}
	if reusable && spec.Branch != "" {
		if err := runGit(ctx, mirror, "check-ref-format", "--branch", spec.Branch); err != nil {
			return Prepared{}, fmt.Errorf("relay workspace: invalid branch %q: %w", spec.Branch, err)
		}
		if gitRefExists(ctx, mirror, "refs/heads/"+spec.Branch) {
			args = []string{"worktree", "add", worktree, spec.Branch}
		} else {
			args = []string{"worktree", "add", "-b", spec.Branch, worktree, ref}
		}
	}
	if err := runGit(ctx, mirror, args...); err != nil {
		return Prepared{}, err
	}
	m.mu.Unlock()
	locked = false
	prepared, err := preparedDirectory(root, worktree, spec.Subdir, false)
	if err != nil {
		m.mu.Lock()
		_ = runGit(context.Background(), mirror, "worktree", "remove", "--force", worktree)
		m.mu.Unlock()
		return Prepared{}, err
	}
	if reusable {
		keepLock = true
		prepared.Cleanup = func(context.Context) error {
			release()
			return nil
		}
		return prepared, nil
	}
	prepared.Cleanup = func(cleanupCtx context.Context) error {
		m.mu.Lock()
		defer m.mu.Unlock()
		return runGit(cleanupCtx, mirror, "worktree", "remove", "--force", worktree)
	}
	return prepared, nil
}

func validateSpec(spec relay.WorkspaceSpec) error {
	lifecycle := spec.Lifecycle
	if lifecycle == "" {
		lifecycle = "attempt"
	}
	if lifecycle != "attempt" && lifecycle != "reusable" {
		return fmt.Errorf("relay workspace: unsupported lifecycle %q", spec.Lifecycle)
	}
	if lifecycle == "reusable" {
		if spec.Kind != "git" {
			return errors.New("relay workspace: reusable lifecycle currently requires the git provider")
		}
		if strings.TrimSpace(spec.ReuseKey) == "" {
			return errors.New("relay workspace: reusable lifecycle requires reuse_key")
		}
		if spec.Ephemeral {
			return errors.New("relay workspace: reusable lifecycle cannot be ephemeral")
		}
	} else if spec.ReuseKey != "" || spec.Branch != "" {
		return errors.New("relay workspace: reuse_key and branch require reusable lifecycle")
	}
	return nil
}

func (m *Manager) lockKey(key string) func() {
	m.mu.Lock()
	if m.keys == nil {
		m.keys = map[string]*sync.Mutex{}
	}
	lock := m.keys[key]
	if lock == nil {
		lock = &sync.Mutex{}
		m.keys[key] = lock
	}
	m.mu.Unlock()
	lock.Lock()
	return lock.Unlock
}

func gitRefExists(ctx context.Context, dir, ref string) bool {
	command := exec.CommandContext(ctx, "git", "show-ref", "--verify", "--quiet", ref)
	runtimeprocess.Configure(command)
	command.Dir = dir
	return command.Run() == nil
}

func preparedDirectory(root, dir, subdir string, ephemeral bool) (Prepared, error) {
	target := filepath.Join(dir, filepath.Clean(subdir))
	if subdir == "" {
		target = dir
	}
	target, err := filepath.Abs(target)
	if err != nil {
		return Prepared{}, err
	}
	base, err := filepath.Abs(dir)
	if err != nil {
		return Prepared{}, err
	}
	if target != base && !strings.HasPrefix(target, base+string(os.PathSeparator)) {
		return Prepared{}, errors.New("relay workspace: subdir escapes workspace")
	}
	info, err := os.Stat(target)
	if err != nil || !info.IsDir() {
		return Prepared{}, fmt.Errorf("relay workspace: subdir is not a directory: %s", subdir)
	}
	cleanup := func(context.Context) error { return nil }
	if ephemeral && root != "" {
		cleanup = func(context.Context) error { return os.RemoveAll(base) }
	}
	return Prepared{Dir: target, Cleanup: cleanup}, nil
}

func runGit(ctx context.Context, dir string, args ...string) error {
	command := exec.CommandContext(ctx, "git", args...)
	runtimeprocess.Configure(command)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("relay workspace: git %s: %w: %s", args[0], err, strings.TrimSpace(string(output)))
	}
	return nil
}

func safeSegment(value string) string {
	value = strings.ReplaceAll(value, string(os.PathSeparator), "_")
	value = strings.ReplaceAll(value, "..", "_")
	return value
}
