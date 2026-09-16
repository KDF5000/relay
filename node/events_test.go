package node

import (
	"context"
	"errors"
	"testing"

	"github.com/KDF5000/relay/controlplane"
	"github.com/KDF5000/relay/transport/httpapi"
)

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
