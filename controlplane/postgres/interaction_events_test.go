package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/controlplane"
)

func TestApprovalResolutionSerializesWithRuntimeEvents(t *testing.T) {
	store := openTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := store.RegisterNode(ctx, controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "approval-node", Capacity: 1, Runtimes: []controlplane.Runtime{{Provider: "mock", State: "healthy"}}}); err != nil {
		t.Fatal(err)
	}
	run, err := store.Submit(ctx, relay.Request{AgentID: "agent", TenantID: "tenant", ProjectID: "project", Runtime: relay.RuntimeRequirement{Provider: "mock"}, Input: relay.Input{Prompt: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	assignment, err := store.Claim(ctx, "approval-node", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Start(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	var requests []relay.Interaction
	for i := 0; i < 50; i++ {
		item, err := store.CreateInteraction(ctx, assignment, relay.InteractionRequest{Kind: relay.InteractionApproval, Prompt: "Allow command"})
		if err != nil {
			t.Fatal(err)
		}
		requests = append(requests, item)
	}
	if _, err := store.ResolveInteraction(ctx, requests[0].ID, json.RawMessage(`{"approved":true}`), "other-tenant", "project"); err != controlplane.ErrNotFound {
		t.Fatalf("tenant isolation: %v", err)
	}
	start := make(chan struct{})
	failures := make(chan error, 52)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 100; i++ {
			events := []controlplane.ProducerEvent{{EventID: fmt.Sprintf("event-%d-a", i), Type: "runtime.test.delta", Data: json.RawMessage(fmt.Sprintf(`{"index":%d}`, i))}, {EventID: fmt.Sprintf("event-%d-b", i), Type: "runtime.test.delta", Data: json.RawMessage(fmt.Sprintf(`{"index":%d}`, i))}}
			if err := store.AppendEvents(ctx, run.ID, assignment.AttemptID, assignment.LeaseToken, events); err != nil {
				failures <- err
				return
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 100; i++ {
			if _, err := store.GetInteraction(ctx, assignment, requests[i%len(requests)].ID); err != nil {
				failures <- err
				return
			}
		}
	}()
	for _, item := range requests {
		wg.Add(1)
		go func(item relay.Interaction) {
			defer wg.Done()
			<-start
			if _, err := store.ResolveInteraction(ctx, item.ID, json.RawMessage(`{"approved":true}`), "tenant", "project"); err != nil {
				failures <- err
			}
		}(item)
	}
	close(start)
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if t.Failed() {
		return
	}
	events, err := store.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	resolved, delta := 0, 0
	for i, event := range events {
		if event.Sequence != i+1 {
			t.Fatalf("sequence %d: %d", i, event.Sequence)
		}
		if event.Type == "interaction.resolved" {
			resolved++
		}
		if event.Type == "runtime.test.delta" {
			delta++
		}
	}
	if resolved != 50 || delta != 200 {
		t.Fatalf("lost events: resolutions=%d deltas=%d", resolved, delta)
	}
}
