package media

import (
	"testing"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/report"
	"github.com/pion/rtcp"
)

// TestRRObserverParsesReceiverReports: the congestion-feedback observer
// consumes real Receiver Report packets (what the client SDK emits) and
// accumulates per-SSRC fraction-lost/jitter windows.
func TestRRObserverParsesReceiverReports(t *testing.T) {
	observer := newRRObserver()
	pkt := &rtcp.ReceiverReport{
		SSRC: 42,
		Reports: []rtcp.ReceptionReport{{
			SSRC:               42,
			FractionLost:       51, // 20%
			TotalLost:          4,
			LastSequenceNumber: 99,
			Jitter:             120,
		}},
	}
	raw, err := pkt.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	observer.observe(raw)
	windows := observer.observedWindows()
	stats, ok := windows[42]
	if !ok {
		t.Fatalf("observer did not record the receiver report")
	}
	if stats.reports != 1 || stats.fractionSum != 51 || stats.jitterMax != 120 {
		t.Fatalf("unexpected window: %+v", stats)
	}
}

// TestRRObserverConsumesReportsThroughChain: the observer sits in the REAL
// interceptor composition (registry.Build, exactly what pion composes per
// peer connection), and a Receiver Report fed through the chain's bound
// RTCP reader reaches it. The wire half runs in production, where the
// client is the Google WebRTC SDK — RR emission is the SDK's documented
// baseline, and the session's OnTrack path drives receiver.ReadRTCP so the
// chain (and this observer) actually sees the packets.
func TestRRObserverConsumesReportsThroughChain(t *testing.T) {
	observer := newRRObserver()
	receiver, err := report.NewReceiverInterceptor()
	if err != nil {
		t.Fatalf("receiver: %v", err)
	}
	registry := &interceptor.Registry{}
	registry.Add(receiver)
	registry.Add(observerFactory{observer: observer})
	chain, err := registry.Build("test")
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	rtcpReader := chain.BindRTCPReader(interceptor.RTCPReaderFunc(func(buf []byte, _ interceptor.Attributes) (int, interceptor.Attributes, error) {
		pkt := &rtcp.ReceiverReport{SSRC: 7, Reports: []rtcp.ReceptionReport{{SSRC: 7, FractionLost: 25, Jitter: 40}}}
		raw, marshalErr := pkt.Marshal()
		if marshalErr != nil {
			return 0, nil, marshalErr
		}
		copy(buf, raw)
		return len(raw), nil, nil
	}))
	buf := make([]byte, 1500)
	if _, _, err := rtcpReader.Read(buf, nil); err != nil {
		t.Fatalf("read: %v", err)
	}
	windows := observer.observedWindows()
	stats, ok := windows[7]
	if !ok {
		t.Fatalf("observer did not consume the report through the chain")
	}
	if stats.fractionSum != 25 || stats.jitterMax != 40 {
		t.Fatalf("wrong evidence through chain: %+v", stats)
	}
}
