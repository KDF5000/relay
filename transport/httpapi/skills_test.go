package httpapi_test

import (
	"context"
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/controlplane"
	"github.com/KDF5000/relay/transport/httpapi"
)

func TestLargeSkillSnapshotAndCompactLeaseReports(t *testing.T) {
	ctx := context.Background()
	service := controlplane.New(time.Minute)
	server := httptest.NewServer(httpapi.NewHandler(service))
	defer server.Close()
	client := httpapi.NewClient(server.URL)
	if _, err := client.RegisterNode(ctx, controlplane.NodeRegistration{ID: "skills", ProtocolVersion: relay.ProtocolVersion, Capacity: 1, Labels: map[string]string{relay.SkillSupportLabel: relay.SkillSupportVersion}, Runtimes: []controlplane.Runtime{{Provider: "test"}}}); err != nil {
		t.Fatal(err)
	}
	request := relay.Request{AgentID: "agent", IdempotencyKey: "large-skills", Runtime: relay.RuntimeRequirement{Provider: "test"}, Input: relay.Input{Prompt: "use skills"}}
	// Two valid packages produce a request larger than the former 8 MiB limit.
	for _, id := range []string{"one", "two"} {
		skill := relay.SkillPackage{ID: id, Name: id, Files: []relay.SkillFile{{Path: "SKILL.md", Content: "Read assets/data"}, {Path: "assets/data", Content: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 4<<20))), Encoding: "base64"}}}
		skill.Version, _ = relay.SkillVersion(skill)
		request.Skills = append(request.Skills, skill)
	}
	run, err := client.Submit(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	assignment, err := client.Claim(ctx, "skills")
	if err != nil || len(assignment.Request.Skills) != 2 {
		t.Fatalf("assignment=%s err=%v", assignment.RunID, err)
	}
	if err := client.Start(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Renew(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	if err := client.Complete(ctx, assignment, relay.Result{Summary: "ok"}); err != nil {
		t.Fatal(err)
	}
	finished, err := client.GetRun(ctx, run.ID)
	if err != nil || finished.Status != relay.RunSucceeded {
		t.Fatalf("run=%+v err=%v", finished, err)
	}
}
