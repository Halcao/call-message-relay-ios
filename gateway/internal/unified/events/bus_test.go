package events

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"callrelay/gateway/internal/unified/store"
)

func newBusStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "events.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func collectReplay(t *testing.T, bus *Bus, after int64, sentinel func() error) []Event {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, unsubscribe := bus.Subscribe(ctx, after, func(Event) bool { return true })
	defer unsubscribe()

	if err := sentinel(); err != nil {
		t.Fatalf("sentinel publish: %v", err)
	}

	var events []Event
	timeout := time.After(5 * time.Second)
	for {
		select {
		case event, ok := <-stream:
			if !ok {
				return events
			}
			events = append(events, event)
			if event.Type == "sentinel" {
				return events
			}
		case <-timeout:
			t.Fatalf("replay timed out after %d events", len(events))
		}
	}
}

// A durable replay from sequence zero must not resurrect an incoming call that
// has already ended, while still delivering the terminal events and genuine
// live call state. This is the backend half of the phantom-ringing fix.
func TestReplaySuppressesNonTerminalEventsOfEndedCalls(t *testing.T) {
	ctx := context.Background()
	st := newBusStore(t)
	bus := NewBus(st)

	// Calls table: call-a has ended, call-b is still ringing.
	started := time.Now().UTC().Add(-time.Minute)
	ended := time.Now().UTC()
	if err := st.UpsertCall(ctx, store.Call{
		ID: "line1:call-a", LineID: "line1", WorkerCallID: "call-a",
		Direction: "inbound", State: "ended", CreatedAt: started, EndedAt: &ended,
	}); err != nil {
		t.Fatalf("upsert ended call: %v", err)
	}
	if err := st.UpsertCall(ctx, store.Call{
		ID: "line2:call-b", LineID: "line2", WorkerCallID: "call-b",
		Direction: "inbound", State: "incoming_ringing", CreatedAt: ended,
	}); err != nil {
		t.Fatalf("upsert ringing call: %v", err)
	}

	incomingA := map[string]any{
		"id": "line1:call-a", "lineId": "line1", "direction": "inbound",
		"peer": "10086", "state": "incoming_ringing", "startedAt": started.UnixMilli(),
	}
	endedA := map[string]any{
		"id": "line1:call-a", "lineId": "line1", "direction": "inbound",
		"peer": "10086", "state": "ended", "startedAt": started.UnixMilli(),
		"endedAt": ended.UnixMilli(),
	}
	incomingB := map[string]any{
		"id": "line2:call-b", "lineId": "line2", "direction": "inbound",
		"peer": "10010", "state": "incoming_ringing", "startedAt": ended.UnixMilli(),
	}
	activeA := map[string]any{
		"id": "line1:call-a", "lineId": "line1", "direction": "inbound",
		"peer": "10086", "state": "active", "startedAt": started.UnixMilli(),
	}

	if _, err := bus.Publish(ctx, "line1", "", "call.incoming", incomingA); err != nil {
		t.Fatalf("publish incoming A: %v", err)
	}
	if _, err := bus.Publish(ctx, "line1", "", "call.updated", activeA); err != nil {
		t.Fatalf("publish active A: %v", err)
	}
	if _, err := bus.Publish(ctx, "line1", "", "call.ended", endedA); err != nil {
		t.Fatalf("publish ended A: %v", err)
	}
	if _, err := bus.Publish(ctx, "line2", "", "call.incoming", incomingB); err != nil {
		t.Fatalf("publish incoming B: %v", err)
	}

	events := collectReplay(t, bus, 0, func() error {
		_, err := bus.Publish(ctx, "", "", "sentinel", map[string]any{})
		return err
	})

	var sawIncomingA, sawActiveA, sawEndedA, sawIncomingB, sawSentinel bool
	for _, event := range events {
		data := string(event.Data)
		switch {
		case event.Type == "call.incoming" && strings.Contains(data, "call-a"):
			sawIncomingA = true
		case event.Type == "call.updated" && strings.Contains(data, "call-a"):
			sawActiveA = true
		case event.Type == "call.ended" && strings.Contains(data, "call-a"):
			sawEndedA = true
		case event.Type == "call.incoming" && strings.Contains(data, "call-b"):
			sawIncomingB = true
		case event.Type == "sentinel":
			sawSentinel = true
		}
	}
	if sawIncomingA || sawActiveA {
		t.Fatalf("ended call was replayed as live state: incoming=%v active=%v", sawIncomingA, sawActiveA)
	}
	if !sawEndedA {
		t.Fatalf("terminal call.ended event must stay deliverable for clearing")
	}
	if !sawIncomingB {
		t.Fatalf("active ringing call must still replay")
	}
	if !sawSentinel {
		t.Fatalf("live event after replay was not delivered")
	}
}

// A resume cursor must only replay events above it, even for call state.
func TestReplayHonorsAfterCursor(t *testing.T) {
	ctx := context.Background()
	st := newBusStore(t)
	bus := NewBus(st)

	first, err := bus.Publish(ctx, "line1", "", "call.incoming", map[string]any{
		"id": "line1:call-a", "lineId": "line1", "direction": "inbound",
		"state": "incoming_ringing", "startedAt": time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatalf("publish first: %v", err)
	}
	second, err := bus.Publish(ctx, "line1", "", "call.ended", map[string]any{
		"id": "line1:call-a", "lineId": "line1", "direction": "inbound",
		"state": "ended", "endedAt": time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatalf("publish second: %v", err)
	}

	events := collectReplay(t, bus, first.Seq, func() error {
		_, err := bus.Publish(ctx, "", "", "sentinel", map[string]any{})
		return err
	})
	for _, event := range events {
		if event.Seq <= first.Seq && event.Type != "sentinel" {
			t.Fatalf("event %d replayed despite after=%d", event.Seq, first.Seq)
		}
	}
	if len(events) < 2 || events[0].Seq != second.Seq {
		t.Fatalf("expected replay to start at seq %d, got %+v", second.Seq, events)
	}
}
