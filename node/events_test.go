package node

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/KDF5000/relay/controlplane"
	"github.com/KDF5000/relay/transport/httpapi"
)

type capturedEventDelivery struct {
	ControlPlane
	data any
}

func (c *capturedEventDelivery) AppendEvent(_ context.Context, _, _, _, _ string, data any, _ ...string) error {
	c.data = data
	return nil
}

type rejectedEventDelivery struct {
	ControlPlane
	calls int
	cause error
}

func (c *rejectedEventDelivery) AppendEvent(context.Context, string, string, string, string, any, ...string) error {
	c.calls++
	return c.cause
}

func TestInvalidEventIsNotRetried(t *testing.T) {
	cause := &httpapi.HTTPError{StatusCode: 400, Status: "400 Bad Request", Message: "invalid event"}
	cp := &rejectedEventDelivery{cause: cause}
	err := deliverEvent(context.Background(), cp, controlplane.Assignment{}, "1", "output", map[string]string{"delta": "hello"})
	if !errors.Is(err, cause) || cp.calls != 1 {
		t.Fatalf("error=%v, delivery attempts=%d", err, cp.calls)
	}
}

func TestOversizedEventIsDeliveredAsBoundedPreview(t *testing.T) {
	cp := &capturedEventDelivery{}
	err := deliverEvent(context.Background(), cp, controlplane.Assignment{}, "1", "runtime.trae.item.completed", map[string]any{
		"item": map[string]string{"type": "toolCall", "output": strings.Repeat("x", maxEventPayloadBytes)},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, ok := cp.data.(json.RawMessage)
	if !ok {
		t.Fatalf("event data type = %T", cp.data)
	}
	if len(payload) > maxEventPayloadBytes {
		t.Fatalf("payload size = %d", len(payload))
	}
	var data truncatedEventData
	if err := json.Unmarshal(payload, &data); err != nil {
		t.Fatal(err)
	}
	if !data.Truncated || data.OriginalBytes <= maxEventPayloadBytes || !strings.Contains(data.Preview, `"item"`) {
		t.Fatalf("truncated=%v original_bytes=%d preview_bytes=%d", data.Truncated, data.OriginalBytes, len(data.Preview))
	}
}
