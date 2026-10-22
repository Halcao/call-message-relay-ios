// Package events provides the durable global event stream. Every event is
// persisted with a monotonically increasing global sequence before fan-out,
// so clients can resume after any disconnection with ?after=<seq>.
package events

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"callrelay/gateway/internal/unified/store"
)

type Event struct {
	Seq       int64           `json:"seq"`
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	LineID    string          `json:"lineId,omitempty"`
	DeviceID  string          `json:"-"`
	CreatedAt int64           `json:"createdAt"`
	Data      json.RawMessage `json:"data"`
}

type Filter func(Event) bool

type subscriber struct {
	ch      chan Event
	filter  Filter
	ready   bool
	pending []Event
	closed  bool
	mu      sync.Mutex
}

type Bus struct {
	store *store.Store
	mu    sync.Mutex
	next  int
	subs  map[int]*subscriber
}

func NewBus(store *store.Store) *Bus {
	return &Bus{store: store, subs: make(map[int]*subscriber)}
}

// Publish persists and fans out one event. deviceID is empty for broadcast
// events; targeted events are only delivered to that device.
func (b *Bus) Publish(ctx context.Context, lineID, deviceID, eventType string, data any) (Event, error) {
	payload, err := json.Marshal(data)
	if err != nil {
		return Event{}, err
	}
	raw := make([]byte, 9)
	if _, err := rand.Read(raw); err != nil {
		return Event{}, err
	}
	event := Event{
		ID:        "evt_" + base64.RawURLEncoding.EncodeToString(raw),
		Type:      eventType,
		LineID:    lineID,
		DeviceID:  deviceID,
		CreatedAt: time.Now().UTC().UnixMilli(),
		Data:      payload,
	}
	seq, err := b.store.InsertEvent(ctx, store.Event{
		ID: event.ID, LineID: lineID, DeviceID: deviceID, Type: eventType,
		Payload: payload, CreatedAt: time.UnixMilli(event.CreatedAt).UTC(),
	})
	if err != nil {
		return Event{}, err
	}
	event.Seq = seq

	b.mu.Lock()
	for _, sub := range b.subs {
		sub.enqueue(event)
	}
	b.mu.Unlock()
	return event, nil
}

// enqueue appends to the replay backlog while the subscriber is still
// catching up, otherwise delivers live. The subscriber fields are guarded
// solely by s.mu: Publish holds b.mu only for the subscriber-table iteration.
func (s *subscriber) enqueue(event Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if !s.ready {
		s.pending = append(s.pending, event)
		return
	}
	s.deliverLocked(event)
}

func (s *subscriber) deliver(event Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deliverLocked(event)
}

// deliverLocked requires s.mu to be held.
func (s *subscriber) deliverLocked(event Event) {
	if s.closed {
		return
	}
	select {
	case s.ch <- event:
	default:
		// A slow subscriber must not block the modem path; close it so the
		// client reconnects with ?after= and replays from the durable log.
		s.closed = true
		close(s.ch)
	}
}

func (s *subscriber) fail() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
}

// Subscribe replays persisted events after the given sequence, filtered by
// the caller (permission-scoped), then streams live events. The returned
// channel is closed if the subscriber falls too far behind.
func (b *Bus) Subscribe(ctx context.Context, after int64, filter Filter) (<-chan Event, func()) {
	b.mu.Lock()
	b.next++
	id := b.next
	sub := &subscriber{ch: make(chan Event, 256), filter: filter}
	b.subs[id] = sub
	b.mu.Unlock()

	go func() {
		highWater, err := b.store.LatestEventSeq(context.Background())
		if err == nil {
			// Durable replay is a resume, not a live source: a call that has
			// already ended must never re-ring a phone because its historical
			// `call.incoming` sits in the log. Terminal call events are still
			// replayed so clients can clear a call they saw before dropping.
			terminalCalls, terminalErr := b.store.TerminalCallIDs(context.Background())
			if terminalErr != nil {
				terminalCalls = nil // fail open: never suppress without fresh state
			}
			cursor := after
			for {
				events, loadErr := b.store.EventsAfter(context.Background(), cursor, 500)
				if loadErr != nil {
					break
				}
				if len(events) == 0 {
					break
				}
				for _, stored := range events {
					cursor = stored.Seq
					event := fromStore(stored)
					if !filter(event) || suppressReplayedCallEvent(event, terminalCalls) {
						continue
					}
					sub.deliver(event)
				}
				if len(events) < 500 {
					break
				}
			}
		}
		sub.mu.Lock()
		sub.ready = true
		pending := sub.pending
		sub.pending = nil
		sub.mu.Unlock()
		for _, event := range pending {
			if event.Seq > highWater && filter(event) {
				sub.deliver(event)
			}
		}
	}()

	cancel := func() {
		// The subscriber fields are guarded by s.mu (see enqueue/deliver);
		// b.mu only protects the subscriber TABLE.
		b.mu.Lock()
		delete(b.subs, id)
		b.mu.Unlock()
		sub.mu.Lock()
		if !sub.closed {
			sub.closed = true
			close(sub.ch)
		}
		sub.mu.Unlock()
	}
	return sub.ch, cancel
}

// suppressReplayedCallEvent reports whether one persisted call event must be
// withheld during replay because its call has already ended. Only events that
// would present or update a live call are withheld; terminal states are kept
// so a client that saw the call before disconnecting can still clear it.
func suppressReplayedCallEvent(event Event, terminal map[string]struct{}) bool {
	if len(terminal) == 0 || !strings.HasPrefix(event.Type, "call.") {
		return false
	}
	var view struct {
		ID      string `json:"id"`
		State   string `json:"state"`
		EndedAt *int64 `json:"endedAt"`
	}
	if json.Unmarshal(event.Data, &view) != nil || view.ID == "" {
		return false
	}
	if _, ended := terminal[view.ID]; !ended {
		return false
	}
	if view.EndedAt != nil {
		return false
	}
	switch view.State {
	case "ended", "ending", "failed":
		return false
	}
	return true
}

func fromStore(stored store.Event) Event {
	return Event{
		Seq: stored.Seq, ID: stored.ID, Type: stored.Type, LineID: stored.LineID,
		DeviceID: stored.DeviceID, CreatedAt: stored.CreatedAt.UnixMilli(), Data: stored.Payload,
	}
}

// Replay loads persisted events after a sequence for REST-based catch-up.
func (b *Bus) Replay(ctx context.Context, after int64, limit int, filter Filter) ([]Event, error) {
	stored, err := b.store.EventsAfter(ctx, after, limit)
	if err != nil {
		return nil, err
	}
	events := make([]Event, 0, len(stored))
	for _, item := range stored {
		event := fromStore(item)
		if filter == nil || filter(event) {
			events = append(events, event)
		}
	}
	return events, nil
}
