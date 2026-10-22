// Package opus wraps the reference libopus C library (Xiph.Org, BSD-3-Clause;
// static builds vendored under gateway/third_party/opus, see
// tools/build-libopus.sh) for the CallRelay gateway's network-hop codec.
//
// The gateway PCM domain is 8 kHz mono (the modem leg is fixed-rate G.711),
// so the encoder/decoder run narrowband at 8 kHz with 20 ms frames. RTP
// packetization uses the RFC 7587 48 kHz clock (960 ticks per frame) at the
// RTP layer — that lives in the media package, not here.
package opus

/*
// Vendored static archives are pinned for the gateway host targets
// (darwin/arm64 development host and linux/arm64-musl production host).
// On other Linux architectures (linux/amd64 development boxes) the build
// links the SYSTEM libopus instead: install libopus-dev plus
// build-essential; the wrapper only uses the stable libopus encoder,
// decoder and opus_*_ctl API, which the distro library provides.
#cgo darwin,arm64 CFLAGS: -I${SRCDIR}/../../third_party/opus/include
#cgo linux,arm64 CFLAGS: -I${SRCDIR}/../../third_party/opus/include
#cgo darwin,arm64 LDFLAGS: ${SRCDIR}/../../third_party/opus/lib-darwin-arm64/libopus.a
#cgo linux,arm64 LDFLAGS: ${SRCDIR}/../../third_party/opus/lib-linux-arm64-musl/libopus.a -lm
#cgo linux,amd64 LDFLAGS: -lopus -lm

#include <stdlib.h>
#include <opus/opus.h>

static int set_int_ctl(OpusEncoder* st, int req, int value) {
	return opus_encoder_ctl(st, req, value);
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// Narrowband 8 kHz, mono, 20 ms — matches the gateway PCM domain.
const (
	SampleRate   = 8000
	Channels     = 1
	FrameMillis  = 20
	FrameSamples = SampleRate * FrameMillis / 1000 // 160

	// MaxFrameBytes bounds one narrowband frame at the maximum supported
	// bitrate (64 kbps -> 160 bytes) plus generous headroom for FEC
	// redundancy and decoder excursions. libopus rejects oversized buffers
	// cleanly, this is only the scratch buffer size.
	MaxFrameBytes = 400
)

// Reference libopus version string (e.g. "libopus 1.5.2") for diagnostics.
func Version() string {
	return C.GoString(C.opus_get_version_string())
}

// Encoder is a narrowband Opus encoder. NOT safe for concurrent use; the
// media session calls it from the single mixer goroutine only.
type Encoder struct {
	st *C.OpusEncoder
}

// NewEncoder creates a VOIP-application encoder at 8 kHz mono with the
// gateway defaults: 32 kbps, inband FEC on, DTX on (advertised in SDP as
// usedtx=1), 0% expected loss.
func NewEncoder() (*Encoder, error) {
	var err C.int
	st := C.opus_encoder_create(C.opus_int32(SampleRate), C.int(Channels), C.int(C.OPUS_APPLICATION_VOIP), &err)
	if err != C.OPUS_OK || st == nil {
		return nil, fmt.Errorf("opus_encoder_create: %s", C.GoString(C.opus_strerror(err)))
	}
	enc := &Encoder{st: st}
	if err := enc.SetBitrate(32000); err != nil {
		C.opus_encoder_destroy(st)
		return nil, err
	}
	if err := enc.SetInbandFEC(true); err != nil {
		C.opus_encoder_destroy(st)
		return nil, err
	}
	if err := enc.SetDTX(true); err != nil {
		C.opus_encoder_destroy(st)
		return nil, err
	}
	return enc, nil
}

// Encode produces exactly one 20 ms Opus frame. The output slice is reused
// across calls: copy it if it must outlive the next Encode.
func (e *Encoder) Encode(pcm []int16) ([]byte, error) {
	if len(pcm) != FrameSamples {
		return nil, fmt.Errorf("opus encode: got %d samples, want %d", len(pcm), FrameSamples)
	}
	var buf [MaxFrameBytes]byte
	n := C.opus_encode(e.st,
		(*C.opus_int16)(unsafe.Pointer(&pcm[0])), C.int(FrameSamples),
		(*C.uchar)(unsafe.Pointer(&buf[0])), C.opus_int32(len(buf)))
	if n < 0 {
		return nil, fmt.Errorf("opus_encode: %s", C.GoString(C.opus_strerror(n)))
	}
	return buf[:int(n)], nil
}

// SetBitrate bounds the encoder bitrate in bits per second. libopus clamps
// to its own supported range; the media controller clamps harder
// (16-64 kbps) before calling.
func (e *Encoder) SetBitrate(bps int) error {
	if bps <= 0 || bps > 512000 {
		return fmt.Errorf("opus bitrate %d out of range", bps)
	}
	if rc := C.set_int_ctl(e.st, C.OPUS_SET_BITRATE_REQUEST, C.int(bps)); rc != C.OPUS_OK {
		return fmt.Errorf("OPUS_SET_BITRATE: %s", C.GoString(C.opus_strerror(rc)))
	}
	return nil
}

// SetPacketLossPerc informs the encoder of the expected packet loss
// percentage (0-100; the controller clamps to 0-30) so it sizes FEC
// redundancy. With inband FEC enabled libopus uses this to protect the
// most sensitive frames; it does not replace NACK retransmission.
func (e *Encoder) SetPacketLossPerc(p int) error {
	if p < 0 || p > 100 {
		return fmt.Errorf("opus packet loss perc %d out of range", p)
	}
	if rc := C.set_int_ctl(e.st, C.OPUS_SET_PACKET_LOSS_PERC_REQUEST, C.int(p)); rc != C.OPUS_OK {
		return fmt.Errorf("OPUS_SET_PACKET_LOSS_PERC: %s", C.GoString(C.opus_strerror(rc)))
	}
	return nil
}

// SetInbandFEC enables or disables RFC 7587 inband FEC (redundancy for the
// previous frame inside the current packet).
func (e *Encoder) SetInbandFEC(enabled bool) error {
	v := C.int(0)
	if enabled {
		v = 1
	}
	if rc := C.set_int_ctl(e.st, C.OPUS_SET_INBAND_FEC_REQUEST, v); rc != C.OPUS_OK {
		return fmt.Errorf("OPUS_SET_INBAND_FEC: %s", C.GoString(C.opus_strerror(rc)))
	}
	return nil
}

// SetDTX enables or disables discontinuous transmission (silence sends
// tiny comfort-only packets). Advertised in SDP with usedtx=1.
func (e *Encoder) SetDTX(enabled bool) error {
	v := C.int(0)
	if enabled {
		v = 1
	}
	if rc := C.set_int_ctl(e.st, C.OPUS_SET_DTX_REQUEST, v); rc != C.OPUS_OK {
		return fmt.Errorf("OPUS_SET_DTX: %s", C.GoString(C.opus_strerror(rc)))
	}
	return nil
}

// SetComplexity sets the encoder complexity 0-10. The controller lowers it
// (bounded) under sustained CPU pressure; libopus default is used until
// then.
func (e *Encoder) SetComplexity(c int) error {
	if c < 0 || c > 10 {
		return fmt.Errorf("opus complexity %d out of range", c)
	}
	if rc := C.set_int_ctl(e.st, C.OPUS_SET_COMPLEXITY_REQUEST, C.int(c)); rc != C.OPUS_OK {
		return fmt.Errorf("OPUS_SET_COMPLEXITY: %s", C.GoString(C.opus_strerror(rc)))
	}
	return nil
}

// Close releases the encoder. Idempotent.
func (e *Encoder) Close() {
	if e.st != nil {
		C.opus_encoder_destroy(e.st)
		e.st = nil
	}
}

// Decoder is a narrowband Opus decoder. NOT safe for concurrent use; the
// media session calls it from the track read goroutine only.
type Decoder struct {
	st *C.OpusDecoder
}

// NewDecoder creates an 8 kHz mono decoder.
func NewDecoder() (*Decoder, error) {
	var err C.int
	st := C.opus_decoder_create(C.opus_int32(SampleRate), C.int(Channels), &err)
	if err != C.OPUS_OK || st == nil {
		return nil, fmt.Errorf("opus_decoder_create: %s", C.GoString(C.opus_strerror(err)))
	}
	return &Decoder{st: st}, nil
}

// Decode decodes exactly one frame. A nil packet performs packet-loss
// concealment (the libopus mature PLC the primitive app-side fallback is
// not). The output slice is reused across calls.
func (d *Decoder) Decode(packet []byte) ([]int16, error) {
	var buf [FrameSamples]C.opus_int16
	var data *C.uchar
	if len(packet) > 0 {
		data = (*C.uchar)(unsafe.Pointer(&packet[0]))
	}
	n := C.opus_decode(d.st, data, C.opus_int32(len(packet)),
		(*C.opus_int16)(unsafe.Pointer(&buf[0])), C.int(FrameSamples), 0)
	if n < 0 {
		return nil, fmt.Errorf("opus_decode: %s", C.GoString(C.opus_strerror(n)))
	}
	out := make([]int16, n)
	for i := 0; i < int(n); i++ {
		out[i] = int16(buf[i])
	}
	return out, nil
}

// DecodeFEC recovers the PREVIOUS frame from the given packet's RFC 7587
// inband FEC redundancy (decode_fec=1) without consuming the packet: the
// carrier decodes normally at its own playout slot afterwards. Returns an
// error when the packet carries no recoverable redundancy (older than one
// frame) — the caller then falls back to plain PLC.
func (d *Decoder) DecodeFEC(packet []byte) ([]int16, error) {
	if len(packet) == 0 {
		return nil, fmt.Errorf("opus decode fec: empty packet")
	}
	var buf [FrameSamples]C.opus_int16
	n := C.opus_decode(d.st, (*C.uchar)(unsafe.Pointer(&packet[0])), C.opus_int32(len(packet)),
		(*C.opus_int16)(unsafe.Pointer(&buf[0])), C.int(FrameSamples), 1)
	if n < 0 {
		return nil, fmt.Errorf("opus_decode_fec: %s", C.GoString(C.opus_strerror(n)))
	}
	out := make([]int16, n)
	for i := 0; i < int(n); i++ {
		out[i] = int16(buf[i])
	}
	return out, nil
}

// Close releases the decoder. Idempotent.
func (d *Decoder) Close() {
	if d.st != nil {
		C.opus_decoder_destroy(d.st)
		d.st = nil
	}
}
