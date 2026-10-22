package media

import (
	"fmt"
	"time"
)

// hostSocket is the transport seam the WSS host (wsHost) speaks. The
// concrete *websocket.Conn satisfies it unchanged; the QUIC DATAGRAM
// transport (build 61) provides a second implementation, so the entire
// proven session machinery — standby slots, fenced promotion, recovery
// grace, jitter/mix routing — serves QUIC without a parallel state machine.
//
// Message types reuse the gorilla/websocket constants so the pump protocol
// (text = JSON control, binary = one 20 ms audio frame) is identical on
// both transports:
//
//   - text   (websocket.TextMessage): JSON control lines (ready/ping/pong/
//     denoise/promote/bye). On QUIC these ride the single bidirectional
//     control stream, newline-delimited.
//   - binary (websocket.BinaryMessage): one audio frame. On QUIC these ride
//     unreliable DATAGRAM frames carrying the QUIC envelope
//     (version||epoch||seq16 frame — see quicsock.go); the envelope is
//     added on send and validated+stripped on receive HERE, so the codec
//     and depacketizer below this seam stay byte-identical to WSS.
//
// There is deliberately no cross-type ordering guarantee: WebSocket ordered
// its single pipe, QUIC deliberately does not (control is a reliable
// stream, audio is datagrams). The pump protocol never relied on
// text/binary interleaving order.
type hostSocket interface {
	// ReadMessage returns the next control line (text) or audio frame
	// (binary). Implementations drop malformed/epoch-stale datagrams
	// internally so callers only ever see valid frames.
	ReadMessage() (messageType int, payload []byte, err error)
	// WriteMessage writes one control line (text) or audio frame (binary).
	WriteMessage(messageType int, payload []byte) error
	SetWriteDeadline(t time.Time) error
	// Close converges this exact socket; the reader unblocks with an error
	// and the host detaches exactly like the WebSocket path.
	Close() error
}

// ErrQUICNoControlStream is returned by the QUIC socket adapter when the
// peer never opened the control stream within the auth window.
var ErrQUICNoControlStream = fmt.Errorf("quic: control stream not opened")

// ErrQUICSocketClosed is returned by reads after Close.
var ErrQUICSocketClosed = fmt.Errorf("quic: socket closed")
