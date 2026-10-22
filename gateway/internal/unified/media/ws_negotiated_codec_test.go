package media

import (
	"testing"
	"time"
)

// TestReadyCallbackReceivesNegotiatedCodec pins the contract that the
// ready greeting reports the codec ACTUALLY created locally — not the
// codec requested in the URL. Announcing Opus while the host fell back
// to PCMU silently wedged every frame exchange (uplink framed Opus
// rejected by the exact-160 PCMU gate, bare PCMU sent to an Opus app).
func TestReadyCallbackReceivesNegotiatedCodec(t *testing.T) {
	_, opusServer, opusCleanup := wsTestPair(t)
	defer opusCleanup()
	opusEngine, err := NewEngine()
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	opusSession := NewSession(opusEngine, "neg-opus", "call")
	defer opusSession.Close()
	negotiated := ""
	gen, err := opusSession.AttachWSHost(opusServer, codecOpus, func(got string, _ AttachInfo) error {
		negotiated = got
		return nil
	})
	if err != nil {
		t.Fatalf("attach opus: %v", err)
	}
	opusSession.DetachWSHost(gen, opusSession.ws)
	if negotiated != codecOpus {
		t.Fatalf("negotiated = %q, want %q", negotiated, codecOpus)
	}

	// A PCMU attach must report PCMU even when the app asks for no codec.
	_, pcmuServer, pcmuCleanup := wsTestPair(t)
	defer pcmuCleanup()
	pcmuEngine, err := NewEngine()
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	pcmuSession := NewSession(pcmuEngine, "neg-pcmu", "call")
	defer pcmuSession.Close()
	negotiated = ""
	if _, err := pcmuSession.AttachWSHost(pcmuServer, "", func(got string, _ AttachInfo) error {
		negotiated = got
		return nil
	}); err != nil {
		t.Fatalf("attach pcmu: %v", err)
	}
	if negotiated != codecPCMU {
		t.Fatalf("negotiated = %q, want %q", negotiated, codecPCMU)
	}
}

// guard against unused import if timing constants shift.
var _ = time.Millisecond
