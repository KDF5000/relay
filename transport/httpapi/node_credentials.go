package httpapi

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"

	"github.com/KDF5000/relay/controlplane"
)

const nodeTokenPrefix = "relay_node_v1."

// NodeTokenAuthenticator accepts legacy static tokens and credentials restricted
// to one Node. The signing key stays on the server; Nodes only store a signature.
// Rotating the root secret invalidates all derived credentials.
type NodeTokenAuthenticator struct {
	static StaticTokens
	key    []byte
}

func NewNodeTokenAuthenticator(host Token, nodeSecret string) *NodeTokenAuthenticator {
	mac := hmac.New(sha256.New, []byte(nodeSecret))
	mac.Write([]byte("relay/node-credentials/v1"))
	auth := &NodeTokenAuthenticator{static: StaticTokens{host, {Value: nodeSecret, Scope: controlplane.AccessScope{Kind: controlplane.AccessNode}}}}
	if nodeSecret != "" {
		auth.key = mac.Sum(nil)
	}
	return auth
}

func validCredentialNodeID(id string) bool {
	if len(id) == 0 || len(id) > 256 || id == "." || id == ".." {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

func (a *NodeTokenAuthenticator) signature(payload string) []byte {
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(nodeTokenPrefix + payload))
	return mac.Sum(nil)
}

// IssueNodeToken creates a persistent credential. Enrollment expiration is
// handled by the owning application, not by this heartbeat credential.
func (a *NodeTokenAuthenticator) IssueNodeToken(nodeID string) (string, error) {
	if len(a.key) == 0 {
		return "", errors.New("node credential signing is not configured")
	}
	if !validCredentialNodeID(nodeID) {
		return "", errors.New("invalid node ID")
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString([]byte(nodeID)) + "." + base64.RawURLEncoding.EncodeToString(nonce)
	return nodeTokenPrefix + payload + "." + base64.RawURLEncoding.EncodeToString(a.signature(payload)), nil
}

func (a *NodeTokenAuthenticator) Authenticate(value string) (controlplane.AccessScope, bool) {
	if value == "" {
		return controlplane.AccessScope{}, false
	}
	if scope, ok := a.static.Authenticate(value); ok {
		return scope, true
	}
	if len(a.key) == 0 || len(value) > 1024 || !strings.HasPrefix(value, nodeTokenPrefix) {
		return controlplane.AccessScope{}, false
	}
	parts := strings.Split(strings.TrimPrefix(value, nodeTokenPrefix), ".")
	if len(parts) != 3 {
		return controlplane.AccessScope{}, false
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(signature, a.signature(parts[0]+"."+parts[1])) {
		return controlplane.AccessScope{}, false
	}
	id, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || !validCredentialNodeID(string(id)) {
		return controlplane.AccessScope{}, false
	}
	nonce, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(nonce) != 32 {
		return controlplane.AccessScope{}, false
	}
	return controlplane.AccessScope{Kind: controlplane.AccessNode, Subject: string(id)}, true
}

func (h *Handler) issueNodeCredential(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// Explicitly require authentication even for handlers configured without auth.
	scope, ok := controlplane.AccessFrom(r.Context())
	if !ok || scope.Kind != controlplane.AccessHost {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "host token is required"})
		return
	}
	issuer, ok := h.auth.(interface{ IssueNodeToken(string) (string, error) })
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "node credential signing is not configured"})
		return
	}
	var input struct {
		NodeID string `json:"nodeId"`
	}
	if !decode(w, r, &input) {
		return
	}
	if !validCredentialNodeID(input.NodeID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid node ID"})
		return
	}
	token, err := issuer.IssueNodeToken(input.NodeID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not issue node credential"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"nodeId": input.NodeID, "nodeToken": token})
}
