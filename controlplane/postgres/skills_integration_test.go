package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/controlplane"
)

func TestSkillSnapshotPersistenceAndNodeCapability(t *testing.T) {
	store := openTestStore(t)
	service := controlplane.NewWithStorage(store, time.Minute)
	ctx := context.Background()
	for _, id := range []string{"old", "new"} {
		labels := map[string]string{}
		if id == "new" {
			labels[relay.SkillSupportLabel] = relay.SkillSupportVersion
		}
		if _, err := service.RegisterNode(ctx, controlplane.NodeRegistration{ID: id, ProtocolVersion: relay.ProtocolVersion, Labels: labels, Capacity: 1, Runtimes: []controlplane.Runtime{{Provider: "test"}}}); err != nil {
			t.Fatal(err)
		}
	}
	skill := relay.SkillPackage{ID: "review", Name: "Review", Files: []relay.SkillFile{{Path: "SKILL.md", Content: "Read references/rules.md"}, {Path: "references/rules.md", Content: "complete rules"}}}
	skill.Version, _ = relay.SkillVersion(skill)
	request := relay.Request{AgentID: "agent", IdempotencyKey: "snapshot", Runtime: relay.RuntimeRequirement{Provider: "test"}, Input: relay.Input{Prompt: "review"}, Skills: []relay.SkillPackage{skill}}
	if _, err := service.Submit(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Claim(ctx, "old"); err == nil {
		t.Fatal("legacy node claimed skill task")
	}
	assignment, err := service.Claim(ctx, "new")
	if err != nil || len(assignment.Request.Skills) != 1 {
		t.Fatalf("missing persisted skill: %v", err)
	}
	persisted := assignment.Request.Skills[0]
	if persisted.Version != skill.Version || len(persisted.Files) != 2 || persisted.Files[1].Content != "complete rules" {
		t.Fatalf("snapshot changed: %+v", persisted)
	}
	if err := relay.ValidateSkills(assignment.Request.Skills); err != nil {
		t.Fatal(err)
	}
}
