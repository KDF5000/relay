package httpapi

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/KDF5000/relay"
	"github.com/KDF5000/relay/controlplane"
)

// NodeFailoverClient presents multiple URLs for one logical Relay control
// plane as a single Node client. All URLs must reach the same durable store.
// It keeps one active route, fails over on transport/gateway failures, and
// periodically probes the preferred route while using a fallback.
type NodeFailoverClient struct {
	clients []*Client
	urls    []string

	mu                 sync.Mutex
	active             int
	lastPreferredProbe time.Time
	recoveryInterval   time.Duration
	probeTimeout       time.Duration
	onRouteChange      func(string, string)
}

func NewNodeFailoverClient(baseURLs []string, token string) *NodeFailoverClient {
	seen := make(map[string]bool)
	urls := make([]string, 0, len(baseURLs))
	clients := make([]*Client, 0, len(baseURLs))
	for _, raw := range baseURLs {
		value := strings.TrimRight(strings.TrimSpace(raw), "/")
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		urls = append(urls, value)
		clients = append(clients, NewAuthenticatedClient(value, token))
	}
	return &NodeFailoverClient{
		clients:          clients,
		urls:             urls,
		recoveryInterval: time.Minute,
		probeTimeout:     2 * time.Second,
	}
}

func (c *NodeFailoverClient) URLs() []string {
	return append([]string(nil), c.urls...)
}

func (c *NodeFailoverClient) SetRouteChangeHandler(handler func(string, string)) {
	c.mu.Lock()
	c.onRouteChange = handler
	c.mu.Unlock()
}

func (c *NodeFailoverClient) current(ctx context.Context) int {
	c.mu.Lock()
	if len(c.clients) < 2 || c.active == 0 || time.Since(c.lastPreferredProbe) < c.recoveryInterval {
		active := c.active
		c.mu.Unlock()
		return active
	}
	c.lastPreferredProbe = time.Now()
	c.mu.Unlock()

	probeCtx, cancel := context.WithTimeout(ctx, c.probeTimeout)
	err := c.clients[0].Health(probeCtx)
	cancel()
	if err == nil {
		c.mu.Lock()
		previous := c.active
		c.active = 0
		handler := c.onRouteChange
		c.mu.Unlock()
		if previous != 0 && handler != nil {
			handler(c.urls[previous], c.urls[0])
		}
		return 0
	}
	c.mu.Lock()
	active := c.active
	c.mu.Unlock()
	return active
}

func (c *NodeFailoverClient) switchFrom(failed int) int {
	c.mu.Lock()
	if len(c.clients) < 2 {
		active := c.active
		c.mu.Unlock()
		return active
	}
	previous := c.active
	if c.active == failed {
		c.active = (failed + 1) % len(c.clients)
		if c.active != 0 {
			c.lastPreferredProbe = time.Now()
		}
	}
	active := c.active
	handler := c.onRouteChange
	c.mu.Unlock()
	if active != previous && handler != nil {
		handler(c.urls[previous], c.urls[active])
	}
	return active
}

func routeFailure(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == http.StatusBadGateway || httpErr.StatusCode == http.StatusServiceUnavailable || httpErr.StatusCode == http.StatusGatewayTimeout
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return errors.Is(err, io.ErrUnexpectedEOF)
}

func failoverValue[T any](ctx context.Context, c *NodeFailoverClient, retry bool, call func(*Client) (T, error)) (T, error) {
	var zero T
	if len(c.clients) == 0 {
		return zero, errors.New("relay HTTP: at least one Node server URL is required")
	}
	active := c.current(ctx)
	value, err := call(c.clients[active])
	if !routeFailure(ctx, err) {
		return value, err
	}
	next := c.switchFrom(active)
	if !retry || next == active {
		return value, err
	}
	return call(c.clients[next])
}

func failoverError(ctx context.Context, c *NodeFailoverClient, retry bool, call func(*Client) error) error {
	_, err := failoverValue(ctx, c, retry, func(client *Client) (struct{}, error) {
		return struct{}{}, call(client)
	})
	return err
}

func (c *NodeFailoverClient) RegisterNode(ctx context.Context, value controlplane.NodeRegistration) (controlplane.Node, error) {
	return failoverValue(ctx, c, true, func(client *Client) (controlplane.Node, error) { return client.RegisterNode(ctx, value) })
}

func (c *NodeFailoverClient) Heartbeat(ctx context.Context, nodeID string) (controlplane.Node, error) {
	return failoverValue(ctx, c, true, func(client *Client) (controlplane.Node, error) { return client.Heartbeat(ctx, nodeID) })
}

func (c *NodeFailoverClient) Claim(ctx context.Context, nodeID string) (controlplane.Assignment, error) {
	// A failed response does not prove the server did not grant a lease. Select
	// the fallback for the next poll, but never replay this Claim immediately.
	return failoverValue(ctx, c, false, func(client *Client) (controlplane.Assignment, error) { return client.Claim(ctx, nodeID) })
}

func (c *NodeFailoverClient) Start(ctx context.Context, value controlplane.Assignment) error {
	return failoverError(ctx, c, true, func(client *Client) error { return client.Start(ctx, value) })
}

func (c *NodeFailoverClient) Renew(ctx context.Context, value controlplane.Assignment) (controlplane.LeaseUpdate, error) {
	return failoverValue(ctx, c, true, func(client *Client) (controlplane.LeaseUpdate, error) { return client.Renew(ctx, value) })
}

func (c *NodeFailoverClient) AppendEvent(ctx context.Context, runID, attemptID, lease, eventType string, data any, eventIDs ...string) error {
	return failoverError(ctx, c, true, func(client *Client) error {
		return client.AppendEvent(ctx, runID, attemptID, lease, eventType, data, eventIDs...)
	})
}

func (c *NodeFailoverClient) AppendEvents(ctx context.Context, runID, attemptID, lease string, events []controlplane.ProducerEvent) error {
	return failoverError(ctx, c, true, func(client *Client) error {
		return client.AppendEvents(ctx, runID, attemptID, lease, events)
	})
}

func (c *NodeFailoverClient) Complete(ctx context.Context, value controlplane.Assignment, result relay.Result) error {
	return failoverError(ctx, c, true, func(client *Client) error { return client.Complete(ctx, value, result) })
}

func (c *NodeFailoverClient) Fail(ctx context.Context, value controlplane.Assignment, cause string) error {
	return failoverError(ctx, c, true, func(client *Client) error { return client.Fail(ctx, value, cause) })
}

func (c *NodeFailoverClient) AcknowledgeCancellation(ctx context.Context, value controlplane.Assignment) error {
	return failoverError(ctx, c, true, func(client *Client) error { return client.AcknowledgeCancellation(ctx, value) })
}

func (c *NodeFailoverClient) UploadArtifact(ctx context.Context, assignment controlplane.Assignment, artifact relay.Artifact, reader io.Reader) (relay.Artifact, error) {
	seeker, retryable := reader.(io.Seeker)
	var offset int64
	if retryable {
		var err error
		offset, err = seeker.Seek(0, io.SeekCurrent)
		if err != nil {
			retryable = false
		}
	}
	return failoverValue(ctx, c, retryable, func(client *Client) (relay.Artifact, error) {
		if retryable {
			if _, err := seeker.Seek(offset, io.SeekStart); err != nil {
				return relay.Artifact{}, err
			}
		}
		return client.UploadArtifact(ctx, assignment, artifact, reader)
	})
}

func (c *NodeFailoverClient) ReserveCapability(ctx context.Context, assignment controlplane.Assignment, key, hash string, request relay.CapabilityRequest) (relay.CapabilityReservation, error) {
	return failoverValue(ctx, c, false, func(client *Client) (relay.CapabilityReservation, error) {
		return client.ReserveCapability(ctx, assignment, key, hash, request)
	})
}

func (c *NodeFailoverClient) FinishCapability(ctx context.Context, assignment controlplane.Assignment, reservation relay.CapabilityReservation, result relay.CapabilityResult, cause string) error {
	return failoverError(ctx, c, true, func(client *Client) error {
		return client.FinishCapability(ctx, assignment, reservation, result, cause)
	})
}

func (c *NodeFailoverClient) CreateInteraction(ctx context.Context, assignment controlplane.Assignment, request relay.InteractionRequest) (relay.Interaction, error) {
	return failoverValue(ctx, c, false, func(client *Client) (relay.Interaction, error) {
		return client.CreateInteraction(ctx, assignment, request)
	})
}

func (c *NodeFailoverClient) GetInteraction(ctx context.Context, assignment controlplane.Assignment, id string) (relay.Interaction, error) {
	return failoverValue(ctx, c, true, func(client *Client) (relay.Interaction, error) {
		return client.GetInteraction(ctx, assignment, id)
	})
}

func (c *NodeFailoverClient) ClaimInspection(ctx context.Context, nodeID string) (Inspection, error) {
	return failoverValue(ctx, c, false, func(client *Client) (Inspection, error) {
		return client.ClaimInspection(ctx, nodeID)
	})
}

func (c *NodeFailoverClient) FinishInspection(ctx context.Context, nodeID, id string, result InspectionResult) error {
	return failoverError(ctx, c, true, func(client *Client) error {
		return client.FinishInspection(ctx, nodeID, id, result)
	})
}
