package worker

import "testing"

// TestOfferMediaFrameBoundsBacklog keeps the worker->daemon audio bridge
// latency-bounded: a stalled consumer may lose the oldest frames, but the
// channel must never grow past its capacity and the newest audio must win.
func TestOfferMediaFrameBoundsBacklog(t *testing.T) {
	frames := make(chan []byte, 3)
	for i := 0; i < 5; i++ {
		offerMediaFrame(frames, []byte{byte(i)})
	}
	if got := len(frames); got != 3 {
		t.Fatalf("buffered frames = %d, want 3 (bounded)", got)
	}
	for want := byte(2); want <= 4; want++ {
		select {
		case frame := <-frames:
			if frame[0] != want {
				t.Fatalf("frame = %d, want %d (oldest dropped first)", frame[0], want)
			}
		default:
			t.Fatalf("missing frame %d", want)
		}
	}
	if len(frames) != 0 {
		t.Fatalf("channel not drained: %d", len(frames))
	}
}

// TestOfferMediaFrameCopiesPayload makes sure the frame handed to the mixer
// never aliases the websocket read buffer (gorilla reuses it on the next read).
func TestOfferMediaFrameCopiesPayload(t *testing.T) {
	frames := make(chan []byte, 1)
	payload := []byte{7, 7}
	offerMediaFrame(frames, payload)
	payload[0] = 9
	frame := <-frames
	if frame[0] != 7 {
		t.Fatalf("frame aliases the read buffer: %d", frame[0])
	}
}
