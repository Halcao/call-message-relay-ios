package media

import "testing"

func TestCountLocalCandidates(t *testing.T) {
	sdp := "v=0\r\n" +
		"o=- 1 1 IN IP4 0.0.0.0\r\n" +
		"a=candidate:1 1 udp 2130706431 192.0.2.10 40123 typ host\r\n" +
		"a=candidate:2 1 udp 1694498815 203.0.113.7 5000 typ srflx\r\n" +
		"a=candidate:3 1 tcp 1518280447 192.0.2.10 9 typ host tcptype passive\r\n" +
		"m=audio 9 UDP/TLS/RTP/SAVPF 0\r\n"

	udp, tcp, host, srflx, relay := countLocalCandidates(sdp)
	if udp != 2 || tcp != 1 {
		t.Fatalf("transport counts udp=%d tcp=%d, want 2/1", udp, tcp)
	}
	if host != 2 || srflx != 1 || relay != 0 {
		t.Fatalf("type counts host=%d srflx=%d relay=%d, want 2/1/0", host, srflx, relay)
	}
}
