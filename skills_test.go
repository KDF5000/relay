package relay

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testSkill(t *testing.T) SkillPackage {
	t.Helper()
	skill := SkillPackage{ID: "review", Name: "Review", Files: []SkillFile{
		{Path: "SKILL.md", Content: "Read references/rules.md"},
		{Path: "references/rules.md", Content: "original rules"},
		{Path: "scripts/check", Content: "#!/bin/sh\necho ok\n", Executable: true},
		{Path: "assets/data.bin", Content: base64.StdEncoding.EncodeToString([]byte{0, 255, 10}), Encoding: "base64"},
	}}
	var err error
	skill.Version, err = SkillVersion(skill)
	if err != nil {
		t.Fatal(err)
	}
	return skill
}

func installedDir(t *testing.T, fragment InstructionFragment) string {
	t.Helper()
	start := strings.Index(fragment.Content, "\"")
	end := strings.Index(fragment.Content[start+1:], "\"")
	if start < 0 || end < 0 {
		t.Fatalf("missing installed path: %s", fragment.Content)
	}
	return filepath.Dir(fragment.Content[start+1 : start+1+end])
}

func TestInstallSkillsCacheAndUpdates(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	skill := testSkill(t)
	first, err := InstallSkills(ctx, root, "workspace-a", []SkillPackage{skill})
	if err != nil {
		t.Fatal(err)
	}
	dir := installedDir(t, first[0])
	rule := filepath.Join(dir, "references", "rules.md")
	original, err := os.Stat(rule)
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(filepath.Join(dir, "assets", "data.bin"))
	if err != nil || string(binary) != string([]byte{0, 255, 10}) {
		t.Fatalf("binary=%v err=%v", binary, err)
	}
	script, err := os.Stat(filepath.Join(dir, "scripts", "check"))
	if err != nil || script.Mode()&0100 == 0 {
		t.Fatalf("executable not preserved: %v %v", script, err)
	}
	again, err := InstallSkills(ctx, root, "workspace-a", []SkillPackage{skill})
	if err != nil {
		t.Fatal(err)
	}
	current, err := os.Stat(rule)
	if err != nil || !os.SameFile(original, current) || !original.ModTime().Equal(current.ModTime()) || again[0].Content != first[0].Content {
		t.Fatal("same version rewrote cached files")
	}
	skill.Files[1].Content = "new rules"
	skill.Version, _ = SkillVersion(skill)
	updated, err := InstallSkills(ctx, root, "workspace-a", []SkillPackage{skill})
	if err != nil {
		t.Fatal(err)
	}
	newDir := installedDir(t, updated[0])
	if newDir == dir {
		t.Fatal("updated skill reused old version")
	}
	if old, _ := os.ReadFile(rule); string(old) != "original rules" {
		t.Fatal("update changed active version")
	}
	if newRule, _ := os.ReadFile(filepath.Join(newDir, "references", "rules.md")); string(newRule) != "new rules" {
		t.Fatal("update not installed")
	}
	other, err := InstallSkills(ctx, root, "workspace-b", []SkillPackage{skill})
	if err != nil || installedDir(t, other[0]) == newDir {
		t.Fatal("tenants shared installation")
	}
}

func TestInstallSkillsConcurrent(t *testing.T) {
	root := t.TempDir()
	skill := testSkill(t)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			fragments, err := InstallSkills(context.Background(), root, "tenant", []SkillPackage{skill})
			if err != nil {
				t.Error(err)
				return
			}
			dir := installedDir(t, fragments[0])
			for _, file := range skill.Files {
				if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(file.Path))); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("cache identity directories=%v %v", entries, err)
	}
	versions, err := os.ReadDir(filepath.Join(root, entries[0].Name()))
	if err != nil || len(versions) != 1 || versions[0].Name() != skill.Version {
		t.Fatalf("incomplete installations left behind: %v %v", versions, err)
	}
}

func TestSkillVersionCanonicalAndValidation(t *testing.T) {
	original := testSkill(t)
	skill := testSkill(t)
	skill.Files[0], skill.Files[1] = skill.Files[1], skill.Files[0]
	for i := range skill.Files {
		if skill.Files[i].Encoding == "" {
			skill.Files[i].Content = base64.StdEncoding.EncodeToString([]byte(skill.Files[i].Content))
			skill.Files[i].Encoding = "base64"
		}
	}
	version, err := SkillVersion(skill)
	if err != nil || version != original.Version {
		t.Fatal("order/encoding changed version")
	}
	skill.Files[2].Executable = false
	version, err = SkillVersion(skill)
	if err != nil || version == original.Version {
		t.Fatal("mode did not change version")
	}
	if err := ValidateSkills([]SkillPackage{skill}); err == nil {
		t.Fatal("accepted mismatched digest")
	}
	for _, invalid := range []string{"../escape", "/absolute", "a/../escape", "a\\escape", "C:/escape", "references", "skill.md"} {
		t.Run(invalid, func(t *testing.T) {
			bad := testSkill(t)
			bad.Files = append(bad.Files, SkillFile{Path: invalid, Content: "bad"})
			if _, err := SkillVersion(bad); err == nil {
				t.Fatal("accepted invalid/conflicting path")
			}
		})
	}
	bad := testSkill(t)
	bad.Files = bad.Files[1:]
	if _, err := SkillVersion(bad); err == nil {
		t.Fatal("accepted missing entrypoint")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := InstallSkills(ctx, t.TempDir(), "t", []SkillPackage{original}); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestEngineInstallsSkillsBeforeExecution(t *testing.T) {
	skill := testSkill(t)
	engine, err := New(Options{SkillCacheRoot: t.TempDir(), Executor: ExecutorFunc(func(ctx context.Context, execution Execution) (Result, error) {
		dir := installedDir(t, InstructionFragment{Content: execution.Instructions.Stable})
		if _, err := os.ReadFile(filepath.Join(dir, "references", "rules.md")); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(execution.Instructions.Prompt, filepath.Join(dir, "SKILL.md")) {
			t.Fatal("current turn does not carry the installed skill location")
		}
		return Result{Summary: "ok"}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	run, err := engine.Execute(context.Background(), Request{TenantID: "workspace", AgentID: "agent", IdempotencyKey: "skill", Input: Input{Prompt: "check"}, Skills: []SkillPackage{skill}})
	if err != nil || run.Status != RunSucceeded {
		t.Fatalf("run=%+v err=%v", run, err)
	}
}
