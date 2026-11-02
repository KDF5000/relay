package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KDF5000/relay/controlplane"
)

func nodeEndpoint(t *testing.T, status *atomic.Int64, claims *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code := int(status.Load()); code != 0 && code != http.StatusOK {
			http.Error(w, "gateway unavailable", code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case r.URL.Path == "/v1/nodes/node-1/claim":
			claims.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			_, _ = w.Write([]byte(`{"id":"node-1","capacity":1}`))
		}
	}))
}

func TestNodeFailoverClientRetriesSafeCallsAndStaysOnFallback(t *testing.T) {
	var primaryStatus atomic.Int64
	var primaryClaims atomic.Int64
	primaryStatus.Store(http.StatusServiceUnavailable)
	primary := nodeEndpoint(t, &primaryStatus, &primaryClaims)
	defer primary.Close()
	var fallbackStatus atomic.Int64
	var fallbackClaims atomic.Int64
	fallback := nodeEndpoint(t, &fallbackStatus, &fallbackClaims)
	defer fallback.Close()

	client := NewNodeFailoverClient([]string{primary.URL, fallback.URL}, "token")
	client.recoveryInterval = time.Hour
	if _, err := client.RegisterNode(context.Background(), controlplane.NodeRegistration{ID: "node-1", Capacity: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Heartbeat(context.Background(), "node-1"); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	active := client.active
	client.mu.Unlock()
	if active != 1 {
		t.Fatalf("active route = %d, want fallback", active)
	}
}

func TestNodeFailoverClientDoesNotReplayAmbiguousClaim(t *testing.T) {
	var primaryStatus atomic.Int64
	var primaryClaims atomic.Int64
	primaryStatus.Store(http.StatusGatewayTimeout)
	primary := nodeEndpoint(t, &primaryStatus, &primaryClaims)
	defer primary.Close()
	var fallbackStatus atomic.Int64
	var fallbackClaims atomic.Int64
	fallback := nodeEndpoint(t, &fallbackStatus, &fallbackClaims)
	defer fallback.Close()

	client := NewNodeFailoverClient([]string{primary.URL, fallback.URL}, "token")
	client.recoveryInterval = time.Hour
	if _, err := client.Claim(context.Background(), "node-1"); err == nil {
		t.Fatal("ambiguous primary claim unexpectedly succeeded")
	}
	if fallbackClaims.Load() != 0 {
		t.Fatalf("fallback claim count = %d, want 0", fallbackClaims.Load())
	}
	if _, err := client.Claim(context.Background(), "node-1"); err != controlplane.ErrNoAssignment {
		t.Fatalf("fallback claim = %v, want no assignment", err)
	}
	if fallbackClaims.Load() != 1 {
		t.Fatalf("fallback claim count = %d, want 1", fallbackClaims.Load())
	}
}

func TestNodeFailoverClientReturnsToPreferredRoute(t *testing.T) {
	var primaryStatus atomic.Int64
	var primaryClaims atomic.Int64
	primaryStatus.Store(http.StatusServiceUnavailable)
	primary := nodeEndpoint(t, &primaryStatus, &primaryClaims)
	defer primary.Close()
	var fallbackStatus atomic.Int64
	var fallbackClaims atomic.Int64
	fallback := nodeEndpoint(t, &fallbackStatus, &fallbackClaims)
	defer fallback.Close()

	client := NewNodeFailoverClient([]string{primary.URL, fallback.URL}, "token")
	client.recoveryInterval = time.Nanosecond
	if _, err := client.Heartbeat(context.Background(), "node-1"); err != nil {
		t.Fatal(err)
	}
	primaryStatus.Store(http.StatusOK)
	if _, err := client.Heartbeat(context.Background(), "node-1"); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	active := client.active
	client.mu.Unlock()
	if active != 0 {
		t.Fatalf("active route = %d, want preferred", active)
	}
}

func TestNodeHTTPClientAllowsHighLatencyFallbackResponses(t *testing.T) {
	client := NewClient("https://relay.example.test")
	if client.HTTP.Timeout != 60*time.Second {
		t.Fatalf("request timeout = %s, want 60s", client.HTTP.Timeout)
	}
	transport, ok := client.HTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", client.HTTP.Transport)
	}
	if transport.TLSHandshakeTimeout != 20*time.Second {
		t.Fatalf("TLS handshake timeout = %s, want 20s", transport.TLSHandshakeTimeout)
	}
	if transport.ResponseHeaderTimeout != 30*time.Second {
		t.Fatalf("response header timeout = %s, want 30s", transport.ResponseHeaderTimeout)
	}
}
