// Package speexpp wraps the SpeexDSP preprocessor (Xiph.Org, BSD-3-Clause;
// static builds vendored under gateway/third_party/speexdsp, see
// tools/build-speexdsp.sh) for the gateway's per-call directional denoise.
//
// The gateway PCM domain is 8 kHz mono / 20 ms = 160 samples, exactly the
// frame the preprocessor is initialised for. Policy (the user-approved
// build-48 plan): denoise ON with SPEEX_PREPROCESS_SET_NOISE_SUPPRESS at
// -6 dB; AGC, VAD gating and dereverb stay OFF so the phone's own AEC /
// voice-processing is not double-driver. Process never blocks a call: on
// any error or while disabled it returns the raw frame untouched.
package speexpp

/*
// Vendored static archives are pinned for the gateway host targets
// (darwin/arm64 development host and linux/arm64-musl production host).
// On other Linux architectures (linux/amd64 development boxes) the build
// links the SYSTEM libspeexdsp instead: install libspeexdsp-dev plus
// build-essential; the wrapper only uses the stable speex_preprocess_*
// API, which the distro library provides.
#cgo darwin,arm64 CFLAGS: -I${SRCDIR}/../../third_party/speexdsp/include
#cgo linux,arm64 CFLAGS: -I${SRCDIR}/../../third_party/speexdsp/include
#cgo darwin,arm64 LDFLAGS: ${SRCDIR}/../../third_party/speexdsp/lib-darwin-arm64/libspeexdsp.a
#cgo linux,arm64 LDFLAGS: ${SRCDIR}/../../third_party/speexdsp/lib-linux-arm64-musl/libspeexdsp.a -lm
#cgo linux,amd64 LDFLAGS: -lspeexdsp

#include <stdlib.h>
#include <speex/speex_preprocess.h>

static int pp_ctl_int(SpeexPreprocessState* st, int req, int value) {
	return speex_preprocess_ctl(st, req, &value);
}
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"
)

const (
	// SampleRate is the narrowband telephony domain.
	SampleRate = 8000
	// FrameSamples is one 20 ms frame at 8 kHz.
	FrameSamples = 160
	// DefaultSuppressDB is the approved light floor: -6 dB maximum
	// attenuation. Speex takes the value as a negative dB integer.
	DefaultSuppressDB = -6
)

// Preprocessor is one isolated, stateful SpeexDSP direction. NOT safe for
// concurrent use by itself; the owning call serialises frames and the
// enable/disable hot toggle takes the mutex so a switch never tears the
// state mid-frame.
type Preprocessor struct {
	mu     sync.Mutex
	st     *C.SpeexPreprocessState
	active bool
}

// New creates one preprocessor initialised exactly per the approved policy
// (denoise on, -6 dB; AGC/VAD-gating/dereverb off). enabled controls the
// starting gate; the state is allocated even when disabled so a hot toggle
// at a frame boundary needs no (re)allocation.
func New(enabled bool) (*Preprocessor, error) {
	st := C.speex_preprocess_state_init(C.int(FrameSamples), C.int(SampleRate))
	if st == nil {
		return nil, fmt.Errorf("speex_preprocess_state_init returned nil")
	}
	p := &Preprocessor{st: st, active: enabled}
	// All CTLs are best-effort with raw bypass on failure.
	if rc := C.pp_ctl_int(st, C.SPEEX_PREPROCESS_SET_DENOISE, 1); rc != 0 {
		C.speex_preprocess_state_destroy(st)
		return nil, fmt.Errorf("SET_DENOISE rc=%d", int(rc))
	}
	if rc := C.pp_ctl_int(st, C.SPEEX_PREPROCESS_SET_NOISE_SUPPRESS, C.int(DefaultSuppressDB)); rc != 0 {
		C.speex_preprocess_state_destroy(st)
		return nil, fmt.Errorf("SET_NOISE_SUPPRESS rc=%d", int(rc))
	}
	// Explicitly OFF: do not fight the phone's existing AEC/AGC/voice
	// processing and never gate frames on VAD (genuine quiet must stay a
	// valid signal, not a failure/closure).
	_ = C.pp_ctl_int(st, C.SPEEX_PREPROCESS_SET_AGC, 0)
	_ = C.pp_ctl_int(st, C.SPEEX_PREPROCESS_SET_VAD, 0)
	_ = C.pp_ctl_int(st, C.SPEEX_PREPROCESS_SET_DEREVERB, 0)
	return p, nil
}

// SetEnabled hot-toggles processing at the next frame boundary. State is
// preserved across toggles (the noise estimate is not cold-started).
func (p *Preprocessor) SetEnabled(enabled bool) {
	p.mu.Lock()
	p.active = enabled
	p.mu.Unlock()
}

// Enabled reports the current gate.
func (p *Preprocessor) Enabled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active
}

// Process runs the preprocessor on exactly one 160-sample frame. On any
// non-ready state (nil state, wrong frame size, disabled) it returns the
// input unchanged: denoise must NEVER block or drop audio.
//
// The speex_preprocess_run return value is the VAD decision (1 = speech,
// 0 = noise/non-voice; with VAD disabled it normally returns 1) — it is NOT
// an error status, so it must never be interpreted as failure or cause a
// bypass latch.
func (p *Preprocessor) Process(pcm []int16) []int16 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.st == nil || !p.active || len(pcm) != FrameSamples {
		return pcm
	}
	// speex_preprocess_run mutates the vector in place; copy first so the
	// caller's buffer (which may be a shared decode scratch) is never
	// altered as a side effect.
	frame := make([]int16, FrameSamples)
	copy(frame, pcm)
	_ = C.speex_preprocess_run(p.st, (*C.spx_int16_t)(unsafe.Pointer(&frame[0])))
	return frame
}

// Close destroys the native state. Idempotent; safe at call end and after
// handover (each call owns isolated preprocessors).
func (p *Preprocessor) Close() {
	p.mu.Lock()
	if p.st != nil {
		C.speex_preprocess_state_destroy(p.st)
		p.st = nil
	}
	p.mu.Unlock()
}
