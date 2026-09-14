package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KDF5000/relay/controlplane"
	"github.com/KDF5000/relay/transport/httpapi"
)

func TestAuthenticationFailureIsNotAnExpiredLease(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid bearer token"}`))
	}))
	defer server.Close()
	err := httpapi.NewClient(server.URL).AppendEvent(context.Background(), "run", "attempt", "lease", "delta", nil, "1")
	if err == nil || errors.Is(err, controlplane.ErrInvalidLease) {
		t.Fatalf("authentication error must preserve pending events: %v", err)
	}
}

func TestClientAcceptsLargeJSONResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"version":          strings.Repeat("v", 9<<20),
			"protocol_version": "1",
		})
	}))
	defer server.Close()

	info, err := httpapi.NewClient(server.URL).BuildInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Version) != 9<<20 {
		t.Fatalf("version length=%d", len(info.Version))
	}
}
