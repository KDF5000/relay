package controlplane

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const MaxProducerEventBatch = 64

// ProducerEvent is an immutable runtime event submitted by a fenced Node
// attempt. EventID makes retries safe when a response is lost.
type ProducerEvent struct {
	EventID string          `json:"event_id"`
	Type    string          `json:"type"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// EventIdentity scopes producer identities to one fenced attempt.
func EventIdentity(attempt, lease string, ids []string) string {
	if len(ids) == 0 || ids[0] == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(attempt + "\x00" + lease + "\x00" + ids[0]))
	return "producer_" + hex.EncodeToString(sum[:])
}

func SameEvent(oldType string, oldData []byte, newType string, data any) error {
	encoded, err := json.Marshal(data)
	if err != nil {
		return err
	}
	var old, current any
	if len(oldData) == 0 {
		oldData = []byte("null")
	}
	oldDecoder := json.NewDecoder(bytes.NewReader(oldData))
	oldDecoder.UseNumber()
	if err := oldDecoder.Decode(&old); err != nil {
		return err
	}
	newDecoder := json.NewDecoder(bytes.NewReader(encoded))
	newDecoder.UseNumber()
	if err := newDecoder.Decode(&current); err != nil {
		return err
	}
	// Canonical JSON ignores object-key order and JSONB whitespace.
	a, _ := json.Marshal(old)
	b, _ := json.Marshal(current)
	if oldType != newType || string(a) != string(b) {
		return fmt.Errorf("%w: event identity reused with different content", ErrInvalidTransition)
	}
	return nil
}

// SameValue compares JSON-shaped protocol values without depending on object
// key order or insignificant whitespace.
func SameValue(previous, current any) bool {
	oldData, oldErr := json.Marshal(previous)
	newData, newErr := json.Marshal(current)
	if oldErr != nil || newErr != nil {
		return false
	}
	var oldValue, newValue any
	oldDecoder := json.NewDecoder(bytes.NewReader(oldData))
	oldDecoder.UseNumber()
	newDecoder := json.NewDecoder(bytes.NewReader(newData))
	newDecoder.UseNumber()
	if oldDecoder.Decode(&oldValue) != nil || newDecoder.Decode(&newValue) != nil {
		return false
	}
	oldCanonical, _ := json.Marshal(oldValue)
	newCanonical, _ := json.Marshal(newValue)
	return bytes.Equal(oldCanonical, newCanonical)
}
