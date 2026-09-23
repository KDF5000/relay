package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/controlplane"
	"github.com/KDF5000/relay/transport/httpapi"
)

func TestConsoleAssetsAndHostSession(t *testing.T) {
	service := controlplane.New(time.Minute)
	auth := httpapi.StaticTokens{{Value: "host-secret", Scope: controlplane.AccessScope{Kind: controlplane.AccessHost, TenantID: "tenant", ProjectID: "project"}}}
	server := httptest.NewServer(httpapi.NewHandlerWithAuth(service, auth))
	defer server.Close()

	response, err := http.Get(server.URL + "/console/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("Relay Playground")) {
		t.Fatalf("console response status=%d body=%q", response.StatusCode, body)
	}

	unauthorized, err := http.Get(server.URL + "/v1/nodes")
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", unauthorized.StatusCode)
	}

	sessionResponse, err := http.Post(server.URL+"/v1/console/session", "application/json", bytes.NewBufferString(`{"token":"host-secret"}`))
	if err != nil {
		t.Fatal(err)
	}
	sessionResponse.Body.Close()
	if sessionResponse.StatusCode != http.StatusNoContent || len(sessionResponse.Cookies()) != 1 {
		t.Fatalf("session status=%d cookies=%v", sessionResponse.StatusCode, sessionResponse.Cookies())
	}
	cookie := sessionResponse.Cookies()[0]
	if cookie.Name != "relay_host_token" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("unexpected session cookie: %+v", cookie)
	}

	request, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/nodes", nil)
	request.AddCookie(cookie)
	authorized, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	authorized.Body.Close()
	if authorized.StatusCode != http.StatusOK {
		t.Fatalf("cookie-authenticated status = %d, want 200", authorized.StatusCode)
	}
}

func TestVersionAndProtocolMismatch(t *testing.T) {
	server := httptest.NewServer(httpapi.NewHandler(controlplane.New(time.Minute)))
	defer server.Close()
	client := httpapi.NewClient(server.URL)
	info, err := client.BuildInfo(context.Background())
	if err != nil || info.ProtocolVersion != relay.ProtocolVersion {
		t.Fatalf("build info = %+v, %v", info, err)
	}
	_, err = client.RegisterNode(context.Background(), controlplane.NodeRegistration{
		ID: "old-node", ProtocolVersion: "0", Capacity: 1,
		Runtimes: []controlplane.Runtime{{Provider: "test"}},
	})
	if !errors.Is(err, controlplane.ErrIncompatibleProtocol) {
		t.Fatalf("register error = %v, want incompatible protocol", err)
	}
}

func TestHostUpdatesNodeCapacity(t *testing.T) {
	service := controlplane.New(time.Minute)
	auth := httpapi.StaticTokens{{Value: "host", Scope: controlplane.AccessScope{Kind: controlplane.AccessHost}}, {Value: "node", Scope: controlplane.AccessScope{Kind: controlplane.AccessNode, Subject: "node-a"}}}
	server := httptest.NewServer(httpapi.NewHandlerWithAuth(service, auth))
	defer server.Close()
	ctx := context.Background()
	host := httpapi.NewAuthenticatedClient(server.URL, "host")
	node := httpapi.NewAuthenticatedClient(server.URL, "node")
	if _, err := node.RegisterNode(ctx, controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "node-a", Capacity: 2, Runtimes: []controlplane.Runtime{{Provider: "mock"}}}); err != nil {
		t.Fatal(err)
	}
	updated, err := host.UpdateNodeCapacity(ctx, "node-a", 4)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Capacity != 2 || updated.DesiredCapacity != 4 {
		t.Fatalf("updated node = %+v", updated)
	}
	heartbeat, err := node.Heartbeat(ctx, "node-a")
	if err != nil || heartbeat.DesiredCapacity != 4 {
		t.Fatalf("heartbeat = %+v, %v", heartbeat, err)
	}
}

func TestListEndpointsEncodeEmptyArrays(t *testing.T) {
	service := controlplane.New(time.Minute)
	server := httptest.NewServer(httpapi.NewHandler(service))
	defer server.Close()

	run, err := httpapi.NewClient(server.URL).Submit(context.Background(), relay.Request{
		AgentID:        "agent",
		IdempotencyKey: "empty-lists",
		Runtime:        relay.RuntimeRequirement{Provider: "mock"},
		Input:          relay.Input{Prompt: "work"},
	})
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{
		"/v1/nodes",
		"/v1/sessions/missing/runs",
		"/v1/runs/" + run.ID + "/artifacts",
		"/v1/runs/" + run.ID + "/interactions",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			response, err := http.Get(server.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK || string(bytes.TrimSpace(body)) != "[]" {
				t.Fatalf("GET %s: status=%d body=%q, want 200 []", path, response.StatusCode, body)
			}
		})
	}
}

func TestAuthenticatedIsolationArtifactsAndInteractions(t *testing.T) {
	service := controlplane.New(time.Minute)
	auth := httpapi.StaticTokens{{Value: "host-a", Scope: controlplane.AccessScope{Kind: controlplane.AccessHost, TenantID: "tenant-a", ProjectID: "project"}}, {Value: "host-b", Scope: controlplane.AccessScope{Kind: controlplane.AccessHost, TenantID: "tenant-b", ProjectID: "project"}}, {Value: "node", Scope: controlplane.AccessScope{Kind: controlplane.AccessNode, Subject: "node-a"}}}
	server := httptest.NewServer(httpapi.NewHandlerWithAuth(service, auth))
	defer server.Close()
	ctx := context.Background()
	hostA := httpapi.NewAuthenticatedClient(server.URL, "host-a")
	hostB := httpapi.NewAuthenticatedClient(server.URL, "host-b")
	node := httpapi.NewAuthenticatedClient(server.URL, "node")
	if _, err := node.RegisterNode(ctx, controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "node-a", Capacity: 1, Runtimes: []controlplane.Runtime{{Provider: "mock", State: "healthy"}}}); err != nil {
		t.Fatal(err)
	}
	request := relay.Request{AgentID: "agent", IdempotencyKey: "same-key", Runtime: relay.RuntimeRequirement{Provider: "mock"}, Input: relay.Input{Prompt: "work"}, SessionID: "session-1"}
	runA, err := hostA.Submit(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hostB.GetRun(ctx, runA.ID); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("cross-tenant read = %v", err)
	}
	a, err := node.Claim(ctx, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(ctx, a); err != nil {
		t.Fatal(err)
	}
	artifact, err := node.UploadArtifact(ctx, a, relay.Artifact{Type: "log", Name: "output.txt", ContentType: "text/plain"}, bytes.NewBufferString("hello artifact"))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := hostA.OpenArtifact(ctx, artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(reader)
	reader.Close()
	if string(body) != "hello artifact" {
		t.Fatalf("artifact = %q", body)
	}
	interaction, err := node.CreateInteraction(ctx, a, relay.InteractionRequest{Kind: relay.InteractionApproval, Prompt: "deploy?"})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := hostA.ResolveInteraction(ctx, interaction.ID, json.RawMessage(`{"approved":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if resolved.State != "resolved" {
		t.Fatalf("interaction = %+v", resolved)
	}
	runB, err := hostB.Submit(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if runA.ID == runB.ID || runA.TenantID != "tenant-a" || runB.TenantID != "tenant-b" {
		t.Fatalf("scope failure: %+v %+v", runA, runB)
	}
	sessionRuns, err := hostA.SessionRuns(ctx, "session-1", 10)
	if err != nil || len(sessionRuns) != 1 || sessionRuns[0].ID != runA.ID {
		t.Fatalf("session runs = %+v, %v", sessionRuns, err)
	}
}

func TestHostAndNodeProtocol(t *testing.T) {
	server := httptest.NewServer(httpapi.NewHandler(controlplane.New(time.Minute)))
	defer server.Close()
	client := httpapi.NewClient(server.URL)
	ctx := context.Background()
	if _, err := client.RegisterNode(ctx, controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "node", Runtimes: []controlplane.Runtime{{Provider: "mock"}}, Capacity: 1}); err != nil {
		t.Fatal(err)
	}
	run, err := client.Submit(ctx, relay.Request{AgentID: "agent", IdempotencyKey: "one", Runtime: relay.RuntimeRequirement{Provider: "mock", Model: "model-a"}, Input: relay.Input{Prompt: "work"}})
	if err != nil {
		t.Fatal(err)
	}
	assignment, err := client.Claim(ctx, "node")
	if err != nil {
		t.Fatal(err)
	}
	if assignment.Request.Runtime.Model != "model-a" || run.Runtime.Model != "model-a" {
		t.Fatalf("model override did not survive HTTP protocol: run=%q assignment=%q", run.Runtime.Model, assignment.Request.Runtime.Model)
	}
	if err := client.Start(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	renewal, err := client.Renew(ctx, assignment)
	if err != nil {
		t.Fatal(err)
	}
	if !renewal.LeaseExpiresAt.After(assignment.LeaseExpiresAt) {
		t.Fatalf("renewed lease %s did not advance beyond %s", renewal.LeaseExpiresAt, assignment.LeaseExpiresAt)
	}
	stale := assignment
	stale.LeaseToken = "stale"
	if _, err := client.Renew(ctx, stale); !errors.Is(err, controlplane.ErrInvalidLease) {
		t.Fatalf("stale HTTP renewal = %v, want invalid lease", err)
	}
	if err := client.AppendEvent(ctx, assignment.RunID, assignment.AttemptID, assignment.LeaseToken, "executor.output", map[string]string{"text": "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := client.Complete(ctx, assignment, relay.Result{Summary: "done"}); err != nil {
		t.Fatal(err)
	}
	completed, err := client.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != relay.RunSucceeded {
		t.Fatalf("unexpected status: %s", completed.Status)
	}
	events, err := client.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 8 {
		t.Fatalf("expected 8 events, got %d", len(events))
	}
	var streamed []relay.Event
	if err := client.StreamEvents(ctx, run.ID, 2, func(event relay.Event) error {
		streamed = append(streamed, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(streamed) != 6 || streamed[0].Sequence != 3 || streamed[len(streamed)-1].Sequence != 8 {
		t.Fatalf("unexpected resumed stream: %+v", streamed)
	}
}

func TestCancellationProtocol(t *testing.T) {
	service := controlplane.New(time.Second)
	server := httptest.NewServer(httpapi.NewHandler(service))
	defer server.Close()
	client := httpapi.NewClient(server.URL)
	ctx := context.Background()
	_, _ = client.RegisterNode(ctx, controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "node", Runtimes: []controlplane.Runtime{{Provider: "mock"}}, Capacity: 1})
	run, err := client.Submit(ctx, relay.Request{AgentID: "agent", IdempotencyKey: "cancel-http", Runtime: relay.RuntimeRequirement{Provider: "mock"}, Input: relay.Input{Prompt: "work"}})
	if err != nil {
		t.Fatal(err)
	}
	assignment, _ := client.Claim(ctx, "node")
	if err := client.Start(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	pending, err := client.CancelRun(ctx, run.ID, controlplane.CancelRequest{Reason: "stop", RequestedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != relay.RunCancelling {
		t.Fatalf("status = %s, want cancelling", pending.Status)
	}
	update, err := client.Renew(ctx, assignment)
	if err != nil {
		t.Fatal(err)
	}
	if !update.CancelRequested {
		t.Fatal("renewal did not deliver cancellation")
	}
	if err := client.AcknowledgeCancellation(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	cancelled, _ := client.GetRun(ctx, run.ID)
	if cancelled.Status != relay.RunCancelled {
		t.Fatalf("status = %s, want cancelled", cancelled.Status)
	}
}

func TestEventStreamDeliversNewEventsUntilTerminalState(t *testing.T) {
	service := controlplane.New(time.Second)
	server := httptest.NewServer(httpapi.NewHandler(service))
	defer server.Close()
	client := httpapi.NewClient(server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = client.RegisterNode(ctx, controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: "node", Runtimes: []controlplane.Runtime{{Provider: "mock"}}, Capacity: 1})
	run, err := client.Submit(ctx, relay.Request{AgentID: "agent", IdempotencyKey: "stream-live", Runtime: relay.RuntimeRequirement{Provider: "mock"}, Input: relay.Input{Prompt: "work"}})
	if err != nil {
		t.Fatal(err)
	}
	producer := make(chan error, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		assignment, err := client.Claim(ctx, "node")
		if err == nil {
			err = client.Start(ctx, assignment)
		}
		if err == nil {
			err = client.Complete(ctx, assignment, relay.Result{Summary: "streamed"})
		}
		producer <- err
	}()
	var sequences []int
	if err := client.StreamEvents(ctx, run.ID, 0, func(event relay.Event) error {
		sequences = append(sequences, event.Sequence)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-producer; err != nil {
		t.Fatal(fmt.Errorf("produce streamed events: %w", err))
	}
	if len(sequences) != 7 {
		t.Fatalf("streamed sequences = %v, want 1..7", sequences)
	}
	for index, sequence := range sequences {
		if sequence != index+1 {
			t.Fatalf("streamed sequences out of order: %v", sequences)
		}
	}
}

func TestEventStreamReportsCleanDisconnectBeforeTerminal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "id: 1\nevent: relay.event\ndata: {\"id\":\"event-1\",\"sequence\":1,\"type\":\"run.started\"}\n\n")
	}))
	defer server.Close()
	var count int
	err := httpapi.NewClient(server.URL).StreamEvents(context.Background(), "run", 0, func(relay.Event) error { count++; return nil })
	if !errors.Is(err, httpapi.ErrEventStreamInterrupted) || count != 1 {
		t.Fatalf("count=%d error=%v", count, err)
	}
}
