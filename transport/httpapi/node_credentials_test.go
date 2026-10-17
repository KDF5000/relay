package httpapi_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/controlplane"
	"github.com/KDF5000/relay/transport/httpapi"
)

func TestNodeCredentialSignatures(t *testing.T) {
	host := httpapi.Token{Value: "host-secret", Scope: controlplane.AccessScope{Kind: controlplane.AccessHost}}
	auth := httpapi.NewNodeTokenAuthenticator(host, "root-secret")
	token, err := auth.IssueNodeToken("node-a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := auth.IssueNodeToken("node-a")
	if err != nil || second == token {
		t.Fatal("credentials must include a fresh nonce")
	}
	// A restart or another server with the same root preserves valid credentials.
	restarted := httpapi.NewNodeTokenAuthenticator(host, "root-secret")
	scope, ok := restarted.Authenticate(token)
	if !ok || scope.Kind != controlplane.AccessNode || scope.Subject != "node-a" {
		t.Fatalf("wrong credential scope: %+v", scope)
	}
	tampered := strings.Replace(token, base64.RawURLEncoding.EncodeToString([]byte("node-a")), base64.RawURLEncoding.EncodeToString([]byte("node-b")), 1)
	for _, value := range []string{"", "relay_node_v1.", token + ".extra", tampered, token[:len(token)-5]} {
		if _, ok := auth.Authenticate(value); ok {
			t.Fatal("accepted malformed or forged credential")
		}
	}
	if _, ok := httpapi.NewNodeTokenAuthenticator(host, "rotated-root").Authenticate(token); ok {
		t.Fatal("credential survived root rotation")
	}
	for _, id := range []string{"", "..", "a/b", "a b", "a\n", strings.Repeat("x", 257)} {
		if _, err := auth.IssueNodeToken(id); err == nil {
			t.Fatalf("accepted invalid ID %q", id)
		}
	}
	if _, err := httpapi.NewNodeTokenAuthenticator(host, "").IssueNodeToken("node-a"); err == nil {
		t.Fatal("issued with missing signing root")
	}
	if scope, ok := auth.Authenticate("root-secret"); !ok || scope.Kind != controlplane.AccessNode || scope.Subject != "" {
		t.Fatal("legacy node token no longer works")
	}
}

func TestScopedNodeCredentialProtocol(t *testing.T) {
	auth := httpapi.NewNodeTokenAuthenticator(httpapi.Token{Value: "host-secret", Scope: controlplane.AccessScope{Kind: controlplane.AccessHost}}, "root-secret")
	server := httptest.NewServer(httpapi.NewHandlerWithAuth(controlplane.New(time.Minute), auth))
	defer server.Close()
	call := func(token, method, path, body string, status int) *http.Response {
		t.Helper()
		request, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != status {
			response.Body.Close()
			t.Fatalf("%s %s: got %d want %d", method, path, response.StatusCode, status)
		}
		return response
	}
	call("", "POST", "/v1/node-credentials", `{"nodeId":"node-a"}`, 401).Body.Close()
	call("root-secret", "POST", "/v1/node-credentials", `{"nodeId":"node-a"}`, 403).Body.Close()
	call("host-secret", "POST", "/v1/node-credentials", `{"nodeId":"../node-a"}`, 400).Body.Close()
	issue := func(id string) string {
		t.Helper()
		response := call("host-secret", "POST", "/v1/node-credentials", `{"nodeId":"`+id+`"}`, 201)
		defer response.Body.Close()
		if response.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("credential response can be cached")
		}
		var result struct {
			NodeID    string `json:"nodeId"`
			NodeToken string `json:"nodeToken"`
		}
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		if result.NodeID != id || result.NodeToken == "root-secret" {
			t.Fatal("invalid scoped issuance")
		}
		return result.NodeToken
	}
	tokenA, tokenB := issue("node-a"), issue("node-b")
	for _, path := range []string{"/v1/node-credentials", "/v1/nodes/node-b/heartbeat", "/v1/nodes/node-b/claim", "/v1/nodes/node-b/inspections/claim", "/v1/nodes/node-b/inspections/fake"} {
		call(tokenA, "POST", path, `{}`, 403).Body.Close()
	}
	call(tokenA, "GET", "/v1/nodes", "", 403).Body.Close()
	call(tokenA, "POST", "/v1/nodes/register", `{"id":"node-b"}`, 403).Body.Close()
	call(tokenA, "POST", "/v1/nodes/register", `{"id":"node-a","protocol_version":"`+relay.ProtocolVersion+`","runtimes":[{"id":"node-b/mock","provider":"mock"}]}`, 400).Body.Close()
	ctx := context.Background()
	nodeA := httpapi.NewAuthenticatedClient(server.URL, tokenA)
	nodeB := httpapi.NewAuthenticatedClient(server.URL, tokenB)
	for id, client := range map[string]*httpapi.Client{"node-a": nodeA, "node-b": nodeB} {
		if _, err := client.RegisterNode(ctx, controlplane.NodeRegistration{ProtocolVersion: relay.ProtocolVersion, ID: id, Runtimes: []controlplane.Runtime{{Provider: "mock"}}, Capacity: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := client.Heartbeat(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	host := httpapi.NewAuthenticatedClient(server.URL, "host-secret")
	run, err := host.Submit(ctx, relay.Request{AgentID: "agent", IdempotencyKey: "scoped", Runtime: relay.RuntimeRequirement{ID: "node-a/mock", Provider: "mock"}, Input: relay.Input{Prompt: "work"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nodeB.Claim(ctx, "node-b"); !errors.Is(err, controlplane.ErrNoAssignment) {
		t.Fatalf("wrong Node claimed work: %v", err)
	}
	assignment, err := nodeA.Claim(ctx, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	// Even with a known valid lease, another Node cannot write to this attempt.
	checks := []struct {
		name string
		call func() error
	}{
		{"start", func() error { return nodeB.Start(ctx, assignment) }},
		{"renew", func() error { _, err := nodeB.Renew(ctx, assignment); return err }},
		{"event", func() error {
			return nodeB.AppendEvent(ctx, run.ID, assignment.AttemptID, assignment.LeaseToken, "executor.output", nil)
		}},
		{"complete", func() error { return nodeB.Complete(ctx, assignment, relay.Result{}) }},
		{"fail", func() error { return nodeB.Fail(ctx, assignment, "failure") }},
		{"cancel", func() error { return nodeB.AcknowledgeCancellation(ctx, assignment) }},
		{"artifact", func() error {
			_, err := nodeB.UploadArtifact(ctx, assignment, relay.Artifact{Type: "text", Name: "test"}, strings.NewReader("test"))
			return err
		}},
		{"reserve", func() error {
			_, err := nodeB.ReserveCapability(ctx, assignment, "key", "hash", relay.CapabilityRequest{})
			return err
		}},
		{"finish", func() error {
			return nodeB.FinishCapability(ctx, assignment, relay.CapabilityReservation{CallID: "call"}, relay.CapabilityResult{}, "")
		}},
		{"interaction", func() error {
			_, err := nodeB.CreateInteraction(ctx, assignment, relay.InteractionRequest{Kind: relay.InteractionInput})
			return err
		}},
		{"read interaction", func() error { _, err := nodeB.GetInteraction(ctx, assignment, "interaction"); return err }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.call(); !errors.Is(err, controlplane.ErrInvalidLease) && (err == nil || !strings.Contains(err.Error(), "relay HTTP 401 Unauthorized")) {
				t.Fatalf("wrong Node access = %v", err)
			}
		})
	}
	if err := nodeA.Start(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	if _, err := nodeA.Renew(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	if err := nodeA.AppendEvent(ctx, run.ID, assignment.AttemptID, assignment.LeaseToken, "executor.output", map[string]string{"text": "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := nodeA.Complete(ctx, assignment, relay.Result{Summary: "done"}); err != nil {
		t.Fatal(err)
	}
}

func TestNodeCredentialIssuanceDisabledWithoutSigner(t *testing.T) {
	for _, auth := range []httpapi.Authenticator{nil, httpapi.StaticTokens{{Value: "host", Scope: controlplane.AccessScope{Kind: controlplane.AccessHost}}}} {
		handler := httpapi.NewHandlerWithAuth(controlplane.New(time.Minute), auth)
		request := httptest.NewRequest("POST", "/v1/node-credentials", strings.NewReader(`{"nodeId":"node-a"}`))
		request.Header.Set("Authorization", "Bearer host")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 403 && response.Code != 503 {
			t.Fatalf("unsigned issuance returned %d", response.Code)
		}
	}
}
