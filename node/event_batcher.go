package node

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/KDF5000/relay/controlplane"
)

const (
	eventQueueCapacity = 256
	eventBatchDelay    = 20 * time.Millisecond
	eventBatchMaxBytes = 4 << 20
)

type queuedEvent struct {
	event controlplane.ProducerEvent
	path  string
}

// eventBatcher keeps runtime reads independent from cross-region event ACKs.
// One sender preserves ordering; Close waits for every acknowledged batch so a
// terminal run transition can never overtake its output.
type eventBatcher struct {
	ctx        context.Context
	cp         ControlPlane
	assignment controlplane.Assignment
	outbox     *Outbox
	cancel     context.CancelFunc

	queue  chan queuedEvent
	failed chan struct{}
	done   chan struct{}

	emitMu   sync.Mutex
	stateMu  sync.Mutex
	sequence int
	closed   bool
	err      error
}

func newEventBatcher(ctx context.Context, cp ControlPlane, assignment controlplane.Assignment, outbox *Outbox, cancel context.CancelFunc) *eventBatcher {
	b := &eventBatcher{
		ctx: ctx, cp: cp, assignment: assignment, outbox: outbox, cancel: cancel,
		queue: make(chan queuedEvent, eventQueueCapacity), failed: make(chan struct{}), done: make(chan struct{}),
	}
	go b.run()
	return b
}

func (b *eventBatcher) Emit(eventCtx context.Context, eventType string, data any) {
	b.emitMu.Lock()
	defer b.emitMu.Unlock()
	if b.closed || b.Err() != nil {
		return
	}
	payload, err := marshalEventData(data)
	if err != nil {
		b.fail(fmt.Errorf("relay node: encode event %s: %w", eventType, err))
		return
	}
	b.sequence++
	id := fmt.Sprint(b.sequence)
	item := queuedEvent{event: controlplane.ProducerEvent{EventID: id, Type: eventType, Data: json.RawMessage(payload)}}
	if b.outbox != nil {
		item.path, err = b.outbox.save(b.assignment, id, eventType, item.event.Data)
		if err != nil {
			b.fail(fmt.Errorf("relay node: persist event %s: %w", eventType, err))
			return
		}
	}
	select {
	case b.queue <- item:
	case <-b.failed:
	case <-eventCtx.Done():
		b.fail(fmt.Errorf("relay node: queue event %s: %w", eventType, eventCtx.Err()))
	}
}

func (b *eventBatcher) Close() error {
	b.emitMu.Lock()
	if !b.closed {
		b.closed = true
		close(b.queue)
	}
	b.emitMu.Unlock()
	<-b.done
	return b.Err()
}

func (b *eventBatcher) Err() error {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	return b.err
}

func (b *eventBatcher) fail(err error) {
	b.stateMu.Lock()
	if b.err == nil {
		b.err = err
		close(b.failed)
		b.cancel()
	}
	b.stateMu.Unlock()
}

func (b *eventBatcher) run() {
	defer close(b.done)
	for {
		first, ok := <-b.queue
		if !ok {
			return
		}
		items := []queuedEvent{first}
		bytes := eventSize(first.event)
		timer := time.NewTimer(eventBatchDelay)
		closed := false
	collect:
		for len(items) < controlplane.MaxProducerEventBatch && bytes < eventBatchMaxBytes {
			select {
			case item, ok := <-b.queue:
				if !ok {
					closed = true
					break collect
				}
				size := eventSize(item.event)
				if bytes+size > eventBatchMaxBytes {
					// The queue is intentionally larger than a batch. Preserve the
					// item for the next send without reordering it.
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					if err := b.send(items); err != nil {
						b.fail(err)
						return
					}
					items = []queuedEvent{item}
					bytes = size
					timer.Reset(eventBatchDelay)
					continue
				}
				items = append(items, item)
				bytes += size
			case <-timer.C:
				break collect
			}
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		if err := b.send(items); err != nil {
			b.fail(err)
			return
		}
		if closed {
			return
		}
	}
}

func eventSize(event controlplane.ProducerEvent) int {
	return len(event.EventID) + len(event.Type) + len(event.Data) + 128
}

func (b *eventBatcher) send(items []queuedEvent) error {
	events := make([]controlplane.ProducerEvent, len(items))
	paths := make([]string, 0, len(items))
	for index, item := range items {
		events[index] = item.event
		if item.path != "" {
			paths = append(paths, item.path)
		}
	}
	if err := deliverEvents(b.ctx, b.cp, b.assignment, events); err != nil {
		return fmt.Errorf("relay node: deliver %d events: %w", len(events), err)
	}
	if b.outbox != nil && len(paths) > 0 {
		if err := b.outbox.removeBatch(paths); err != nil {
			return fmt.Errorf("relay node: remove acknowledged event batch: %w", err)
		}
	}
	return nil
}
