package at

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"callrelay/gateway/internal/modem"
	"callrelay/gateway/internal/sms"
)

type scriptedPort struct {
	mu           sync.Mutex
	read         bytes.Buffer
	writes       []string
	incomingURC  bool
	incomingClip bool
	dryRun       bool   // +CMGW responses instead of +CMGS
	failOn       string // exact command answered with ERROR instead of OK
}

func (p *scriptedPort) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	value := string(data)
	p.writes = append(p.writes, value)
	command := strings.TrimSpace(value)
	if p.failOn != "" && command == p.failOn {
		p.read.WriteString("\r\nERROR\r\n")
		return len(data), nil
	}
	switch {
	case strings.Contains(value, "\x1a"):
		if p.dryRun {
			p.read.WriteString("\r\n+CMGW: 1\r\nOK\r\n")
		} else {
			p.read.WriteString("\r\n+CMGS: 1\r\nOK\r\n")
		}
	case strings.HasPrefix(command, "AT+CMGS="), strings.HasPrefix(command, "AT+CMGW="):
		p.read.WriteString("\r\n> ")
	case strings.HasPrefix(command, "AT+CMGL"):
		p.read.WriteString("\r\n+CMGL: 2,0,,23\r\n00000D916831553322F10000FF\r\nOK\r\n")
	case strings.HasPrefix(command, "AT+CEER"):
		p.read.WriteString("\r\n+CEER: 16,\"Normal call clearing\"\r\nOK\r\n")
	case strings.HasPrefix(command, "AT+CPIN"):
		p.read.WriteString("\r\n+CPIN: READY\r\nOK\r\n")
	case strings.HasPrefix(command, "AT+CEREG"):
		p.read.WriteString("\r\n+CEREG: 0,1\r\nOK\r\n")
	case strings.HasPrefix(command, "AT+CSQ"):
		if p.incomingURC {
			p.read.WriteString("\r\nRING\r\n")
		}
		if p.incomingClip {
			p.read.WriteString("\r\n+CLIP: \"18712345678\",161\r\n")
		}
		p.read.WriteString("\r\n+CSQ: 20,99\r\nOK\r\n")
	case strings.HasPrefix(command, "AT+CGMI"):
		p.read.WriteString("\r\nQuectel\r\nOK\r\n")
	case strings.HasPrefix(command, "AT+CGMM"):
		p.read.WriteString("\r\nEC25\r\nOK\r\n")
	case strings.HasPrefix(command, "AT+CGMR"):
		p.read.WriteString("\r\nEC25EFAR06A01M4G\r\nOK\r\n")
	default:
		p.read.WriteString("\r\nOK\r\n")
	}
	return len(data), nil
}

func (p *scriptedPort) Read(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.read.Len() == 0 {
		return 0, io.EOF
	}
	return p.read.Read(data)
}

func (p *scriptedPort) Close() error { return nil }

func TestAdapterControlsCallsAndSMS(t *testing.T) {
	port := &scriptedPort{}
	adapter := NewAdapter(NewClient(port))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	callID, err := adapter.Dial(ctx, "+8613800138000")
	if err != nil || callID == "" {
		t.Fatalf("dial = %q, %v", callID, err)
	}
	if err := adapter.Answer(ctx, callID); err != nil {
		t.Fatal(err)
	}
	if err := adapter.SendDTMF(ctx, callID, '5'); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Hangup(ctx, callID); err != nil {
		t.Fatal(err)
	}
	segments, err := sms.EncodeSubmitSegments("10086", "hello", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.SendSMS(ctx, "10086", modem.SMSPayload{PDU: segments[0].PDU}); err != nil {
		t.Fatal(err)
	}
	status, err := adapter.Status(ctx)
	if err != nil || status.SIM != "ready" || status.Registration != "registered" || status.Signal.Bars != 4 {
		t.Fatalf("status = %#v, %v", status, err)
	}
	if len(port.writes) < 8 {
		t.Fatalf("writes = %#v", port.writes)
	}
}

func TestClientSendPDUWaitsForPrompt(t *testing.T) {
	port := &scriptedPort{}
	client := NewClient(port)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	lines, err := client.SendPDU(ctx, 5, "00010005912143F50000FF")
	if err != nil || len(lines) == 0 || lines[len(lines)-1] != "OK" {
		t.Fatalf("send PDU = %#v, %v", lines, err)
	}
}

func TestClientDispatchesInterleavedCallURC(t *testing.T) {
	port := &scriptedPort{incomingURC: true}
	client := NewClient(port)
	var received []string
	client.SetLineHandler(func(line string) { received = append(received, line) })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := client.Exchange(ctx, "AT+CSQ"); err != nil {
		t.Fatal(err)
	}
	if len(received) != 1 || received[0] != "RING" {
		t.Fatalf("interleaved URC = %#v", received)
	}
}

// A +CLIP that arrives during a command exchange must be treated as an
// unsolicited event (not appended to the command response) and dispatched to
// the line handler.
func TestClientDispatchesInterleavedCallerIDURC(t *testing.T) {
	port := &scriptedPort{incomingURC: true, incomingClip: true}
	client := NewClient(port)
	var received []string
	client.SetLineHandler(func(line string) { received = append(received, line) })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := client.Exchange(ctx, "AT+CSQ"); err != nil {
		t.Fatal(err)
	}
	if len(received) != 2 || received[0] != "RING" || received[1] != `+CLIP: "18712345678",161` {
		t.Fatalf("interleaved URCs = %#v", received)
	}
}

func waitAdapterEvent(t *testing.T, adapter *Adapter) modem.ModemEvent {
	t.Helper()
	select {
	case event, open := <-adapter.Events():
		if !open {
			t.Fatal("adapter events closed")
		}
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for adapter event")
	}
	return modem.ModemEvent{}
}

func assertNoAdapterEvent(t *testing.T, adapter *Adapter) {
	t.Helper()
	select {
	case event := <-adapter.Events():
		t.Fatalf("unexpected adapter event %#v", event)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestAdapterCallerIDAfterRingUpdatesActiveCall(t *testing.T) {
	adapter := NewAdapter(NewClient(&scriptedPort{}))
	defer adapter.Close()
	adapter.HandleURC("RING")
	incoming := waitAdapterEvent(t, adapter)
	if incoming.Kind != "incoming" || incoming.Peer != "" || incoming.CallID == "" {
		t.Fatalf("incoming = %#v", incoming)
	}
	adapter.HandleURC(`+CLIP: "+8618712345678",145`)
	peerEvent := waitAdapterEvent(t, adapter)
	if peerEvent.Kind != "peer" || peerEvent.Peer != "+8618712345678" || peerEvent.CallID != incoming.CallID {
		t.Fatalf("peer event = %#v", peerEvent)
	}
	// A repeated +CLIP for the same call is not re-emitted.
	adapter.HandleURC(`+CLIP: "+8618712345678",145`)
	assertNoAdapterEvent(t, adapter)
	// A withheld number does not erase the known caller.
	adapter.HandleURC(`+CLIP: "",129`)
	assertNoAdapterEvent(t, adapter)
}

func TestAdapterCallerIDBeforeRingAttachesToNextRing(t *testing.T) {
	adapter := NewAdapter(NewClient(&scriptedPort{}))
	defer adapter.Close()
	adapter.HandleURC(`+CLIP: "18712345678",129`)
	adapter.HandleURC("RING")
	incoming := waitAdapterEvent(t, adapter)
	if incoming.Kind != "incoming" || incoming.Peer != "18712345678" {
		t.Fatalf("incoming = %#v", incoming)
	}
}

func TestAdapterStaleCallerIDAfterEndDoesNotLeak(t *testing.T) {
	adapter := NewAdapter(NewClient(&scriptedPort{}))
	defer adapter.Close()
	adapter.HandleURC("RING")
	if event := waitAdapterEvent(t, adapter); event.Kind != "incoming" {
		t.Fatalf("incoming = %#v", event)
	}
	adapter.HandleURC(`+CLIP: "18712345678",129`)
	if event := waitAdapterEvent(t, adapter); event.Kind != "peer" {
		t.Fatalf("peer = %#v", event)
	}
	adapter.HandleURC("NO CARRIER")
	if event := waitAdapterEvent(t, adapter); event.Kind != "ended" {
		t.Fatalf("ended = %#v", event)
	}
	// A late +CLIP in the cooldown window belongs to the ended call and must
	// not be carried into the next ring.
	adapter.HandleURC(`+CLIP: "18712345678",129`)
	adapter.HandleURC("RING")
	next := waitAdapterEvent(t, adapter)
	if next.Kind != "incoming" || next.Peer != "" {
		t.Fatalf("next incoming = %#v", next)
	}
}

func TestSignalBarsNeverFabricatesOnUnknownCSQ(t *testing.T) {
	// AT+CSQ 99 means "not known or not detectable". It must render as no
	// bars, never as maximum signal.
	if got := parseRSSI("+CSQ: 99,99"); got != 0 {
		t.Fatalf("unknown CSQ must parse to the 0 sentinel, got %d", got)
	}
	if got := signalBars(parseRSSI("+CSQ: 99,99")); got != 0 {
		t.Fatalf("unknown CSQ must yield 0 bars, got %d", got)
	}
	if got := signalBars(0); got != 0 {
		t.Fatalf("zero/unknown RSSI must yield 0 bars, got %d", got)
	}
	for rssi, want := range map[int]int{-73: 4, -85: 3, -100: 2, -110: 1, -120: 0} {
		if got := signalBars(rssi); got != want {
			t.Fatalf("signalBars(%d) = %d, want %d", rssi, got, want)
		}
	}
}

// TestAdapterDiagnosticsSendsOnlyTheFixedReadOnlyAllowlist proves the
// diagnostics path can never inject arbitrary AT commands and never opens a
// second serial reader: every byte written is one of the fixed allowlisted
// queries, each result is attributed to its command, and a command that the
// modem rejects is reported per command without aborting the remaining
// queries. The release-cause line (AT+CEER) is preserved for field analysis.
func TestAdapterDiagnosticsSendsOnlyTheFixedReadOnlyAllowlist(t *testing.T) {
	port := &scriptedPort{failOn: "AT+CIREG?"}
	adapter := NewAdapter(NewClient(port))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	results, err := adapter.Diagnostics(ctx)
	if err != nil {
		t.Fatalf("diagnostics = %v", err)
	}
	if len(results) != len(diagnosticCommands) {
		t.Fatalf("results = %d, want %d", len(results), len(diagnosticCommands))
	}
	allowlist := make(map[string]bool, len(diagnosticCommands))
	for _, command := range diagnosticCommands {
		allowlist[command] = true
	}
	port.mu.Lock()
	writes := append([]string(nil), port.writes...)
	port.mu.Unlock()
	if len(writes) != len(diagnosticCommands) {
		t.Fatalf("writes = %d, want %d", len(writes), len(diagnosticCommands))
	}
	for index, raw := range writes {
		command := strings.TrimSpace(raw)
		if !allowlist[command] {
			t.Fatalf("non-allowlisted command written: %q", command)
		}
		if command != results[index].Command {
			t.Fatalf("result %d command = %q, want %q", index, results[index].Command, command)
		}
	}
	// The rejected CIREG query is reported without aborting the rest.
	var ciregFailed, cerLines, laterSucceeded bool
	for index, result := range results {
		if result.Command == "AT+CIREG?" {
			ciregFailed = result.Error != ""
		}
		if result.Command == "AT+CEER" {
			for _, line := range result.Lines {
				if strings.Contains(line, "+CEER:") {
					cerLines = true
				}
			}
		}
		if index > 1 && result.Error == "" {
			laterSucceeded = true
		}
	}
	if !ciregFailed {
		t.Fatalf("AT+CIREG? rejection not reported: %#v", results)
	}
	if !cerLines {
		t.Fatalf("AT+CEER release cause missing: %#v", results)
	}
	if !laterSucceeded {
		t.Fatalf("a rejected query aborted the remaining allowlist: %#v", results)
	}
}

// uncertainPort blocks the first (ATD) read until its context is canceled
// and then serves scripted AT+CLCC responses, so the fail-closed quarantine
// and its resolution can be exercised without any real modem.
type uncertainPort struct {
	mu        sync.Mutex
	writes    []string
	readN     int
	closed    chan struct{}
	closeOnce sync.Once
	clccLines []string
	clccErr   bool
}

func newUncertainPort() *uncertainPort {
	return &uncertainPort{closed: make(chan struct{})}
}

func (p *uncertainPort) setCLCC(lines []string, fail bool) {
	p.mu.Lock()
	p.clccLines = append([]string(nil), lines...)
	p.clccErr = fail
	p.mu.Unlock()
}

func (p *uncertainPort) Write(data []byte) (int, error) {
	p.mu.Lock()
	p.writes = append(p.writes, string(data))
	p.mu.Unlock()
	return len(data), nil
}

func (p *uncertainPort) Read(buffer []byte) (int, error) {
	p.mu.Lock()
	p.readN++
	read := p.readN
	p.mu.Unlock()
	if read == 1 {
		<-p.closed // the canceled ATD final response never arrives
		return 0, io.ErrClosedPipe
	}
	p.mu.Lock()
	last := ""
	if len(p.writes) > 0 {
		last = strings.TrimSpace(p.writes[len(p.writes)-1])
	}
	lines := append([]string(nil), p.clccLines...)
	clccErr := p.clccErr
	p.mu.Unlock()
	if strings.HasPrefix(last, "AT+CLCC") {
		if clccErr {
			return copy(buffer, "\r\nERROR\r\n"), nil
		}
		response := "\r\n"
		for _, line := range lines {
			response += line + "\r\n"
		}
		response += "OK\r\n"
		return copy(buffer, response), nil
	}
	return copy(buffer, "\r\nOK\r\n"), nil
}

func (p *uncertainPort) Close() error {
	p.closeOnce.Do(func() { close(p.closed) })
	return nil
}

func (p *uncertainPort) commands() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	commands := make([]string, 0, len(p.writes))
	for _, write := range p.writes {
		commands = append(commands, strings.TrimSpace(write))
	}
	return commands
}

func countPrefix(commands []string, prefix string) int {
	count := 0
	for _, command := range commands {
		if strings.HasPrefix(command, prefix) {
			count++
		}
	}
	return count
}

func assertNoHangup(t *testing.T, commands []string) {
	t.Helper()
	for _, command := range commands {
		if command == "ATH" || command == "AT+CHUP" {
			t.Fatalf("automatic hangup written during quarantine: %q (all %#v)", command, commands)
		}
	}
}

func loseFirstDial(t *testing.T, adapter *Adapter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := adapter.Dial(ctx, "10086")
	if err == nil || !errors.Is(err, ErrResponseLost) {
		t.Fatalf("lost dial error = %v, want ErrResponseLost", err)
	}
}

// TestDialLostResponseQuarantinesLineWithoutHangup: a written ATD whose
// final response is lost must fail closed. The immediate path writes nothing
// beyond the ATD, and the next dial is refused while the line is quarantined.
func TestDialLostResponseQuarantinesLineWithoutHangup(t *testing.T) {
	port := newUncertainPort()
	adapter := NewAdapter(NewClient(port))
	loseFirstDial(t, adapter)

	commands := port.commands()
	if len(commands) != 1 || commands[0] != "ATD10086;" {
		t.Fatalf("commands after lost dial = %#v, want only the ATD", commands)
	}
	adapter.mu.Lock()
	uncertain := adapter.uncertain
	adapter.mu.Unlock()
	if !uncertain {
		t.Fatal("lost dial did not set the fail-closed quarantine")
	}
	// Make the confirmation unavailable (CLCC rejected) so the quarantine
	// must refuse the next dial without writing another ATD.
	port.setCLCC(nil, true)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := adapter.Dial(ctx, "10086"); !errors.Is(err, modem.ErrDialUncertain) {
		t.Fatalf("dial during quarantine = %v, want ErrDialUncertain", err)
	}
	commands = port.commands()
	if countPrefix(commands, "ATD") != 1 {
		t.Fatalf("quarantined dial wrote another ATD: %#v", commands)
	}
	assertNoHangup(t, commands)
}

// TestDialQuarantineClearsAfterFreshCleanCLCC covers the happy recovery: the
// first post-cancel CLCC is untrusted (it may consume the stale final
// response), and only two consecutive trusted clean exchanges clear the
// quarantine. No command other than ATD/AT+CLCC is written.
func TestDialQuarantineClearsAfterFreshCleanCLCC(t *testing.T) {
	port := newUncertainPort()
	adapter := NewAdapter(NewClient(port))
	loseFirstDial(t, adapter)

	// No calls: every CLCC answers a bare OK. The first exchange may be the
	// stale ATD final response, so three exchanges are required.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	callID, err := adapter.Dial(ctx, "10086")
	if err != nil || callID == "" {
		t.Fatalf("recovered dial = %q, %v", callID, err)
	}
	commands := port.commands()
	want := []string{"ATD10086;", "AT+CLCC", "AT+CLCC", "AT+CLCC", "ATD10086;"}
	if len(commands) != len(want) {
		t.Fatalf("commands = %#v, want %#v", commands, want)
	}
	for i := range want {
		if commands[i] != want[i] {
			t.Fatalf("commands = %#v, want %#v", commands, want)
		}
	}
	adapter.mu.Lock()
	uncertain, active := adapter.uncertain, adapter.active
	adapter.mu.Unlock()
	if uncertain || active != callID {
		t.Fatalf("state after recovery uncertain=%v active=%q", uncertain, active)
	}
}

// TestDialQuarantineIgnoresPersistentDataSession: this fleet permanently
// reports a mobile-terminated data-session row (mode 1). It is not a voice
// call, so it must neither block recovery nor trigger any hangup.
func TestDialQuarantineIgnoresPersistentDataSession(t *testing.T) {
	port := newUncertainPort()
	port.setCLCC([]string{`+CLCC: 2,1,0,1,0,"",128`}, false)
	adapter := NewAdapter(NewClient(port))
	loseFirstDial(t, adapter)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := adapter.Dial(ctx, "10086"); err != nil {
		t.Fatalf("dial with data-only CLCC = %v", err)
	}
	commands := port.commands()
	assertNoHangup(t, commands)
	if countPrefix(commands, "AT+CLCC") != 3 {
		t.Fatalf("CLCC confirmations = %#v, want three (first untrusted)", commands)
	}
}

// TestDialQuarantineHoldsForOutgoingVoiceLegWithoutHangup: an unreleased
// outgoing voice leg keeps the line quarantined. The adapter records the leg
// id but never sends ATH/CHUP, so it cannot tear down a call it does not own.
func TestDialQuarantineHoldsForOutgoingVoiceLegWithoutHangup(t *testing.T) {
	port := newUncertainPort()
	port.setCLCC([]string{`+CLCC: 3,0,2,0,0,"10086",129`}, false)
	adapter := NewAdapter(NewClient(port))
	loseFirstDial(t, adapter)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if _, err := adapter.Dial(ctx, "10086"); !errors.Is(err, modem.ErrDialUncertain) {
		t.Fatalf("dial with live outgoing leg = %v, want ErrDialUncertain", err)
	}
	commands := port.commands()
	assertNoHangup(t, commands)
	if countPrefix(commands, "ATD") != 1 {
		t.Fatalf("quarantined dial wrote ATD: %#v", commands)
	}
	adapter.mu.Lock()
	bound := adapter.uncertainCallID
	adapter.mu.Unlock()
	if bound != "3" {
		t.Fatalf("bound outgoing call id = %q, want 3", bound)
	}

	// Once the leg is gone, two trusted clean exchanges clear the quarantine.
	port.setCLCC(nil, false)
	recoverCtx, recoverCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer recoverCancel()
	if _, err := adapter.Dial(recoverCtx, "10086"); err != nil {
		t.Fatalf("dial after outgoing leg cleared = %v", err)
	}
	assertNoHangup(t, port.commands())
}

// TestDialQuarantineNeverTouchesIncomingVoice: an incoming voice leg means
// the line is owned by a call this adapter did not originate. The quarantine
// waits it out and never hangs it up.
func TestDialQuarantineNeverTouchesIncomingVoice(t *testing.T) {
	port := newUncertainPort()
	port.setCLCC([]string{`+CLCC: 7,1,4,0,0,"+8613800138000",129`}, false)
	adapter := NewAdapter(NewClient(port))
	loseFirstDial(t, adapter)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if _, err := adapter.Dial(ctx, "10086"); !errors.Is(err, modem.ErrDialUncertain) {
		t.Fatalf("dial with incoming voice = %v, want ErrDialUncertain", err)
	}
	assertNoHangup(t, port.commands())

	port.setCLCC(nil, false)
	recoverCtx, recoverCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer recoverCancel()
	if _, err := adapter.Dial(recoverCtx, "10086"); err != nil {
		t.Fatalf("dial after incoming cleared = %v", err)
	}
	assertNoHangup(t, port.commands())
}

// TestDialQuarantineSurvivesCLCCFailure: failed or rejected CLCC exchanges
// leave the quarantine in place and never trigger a fallback hangup.
func TestDialQuarantineSurvivesCLCCFailure(t *testing.T) {
	port := newUncertainPort()
	port.setCLCC(nil, true)
	adapter := NewAdapter(NewClient(port))
	loseFirstDial(t, adapter)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if _, err := adapter.Dial(ctx, "10086"); !errors.Is(err, modem.ErrDialUncertain) {
		t.Fatalf("dial with failing CLCC = %v, want ErrDialUncertain", err)
	}
	assertNoHangup(t, port.commands())

	port.setCLCC(nil, false)
	recoverCtx, recoverCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer recoverCancel()
	if _, err := adapter.Dial(recoverCtx, "10086"); err != nil {
		t.Fatalf("dial after CLCC recovered = %v", err)
	}
	assertNoHangup(t, port.commands())
}

// TestDialPrewriteCancelDoesNotQuarantine proves a dial canceled before ATD
// reached the modem (the port lock is held) writes nothing and never enters
// the quarantine: there is no unknown side effect to guard against.
func TestDialPrewriteCancelDoesNotQuarantine(t *testing.T) {
	port := &scriptedPort{}
	client := NewClient(port)
	adapter := NewAdapter(client)
	// Simulate another exchange owning the serial queue.
	client.ioCh <- struct{}{}
	defer func() { <-client.ioCh }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := adapter.Dial(ctx, "10086")
	if err == nil || errors.Is(err, ErrResponseLost) || errors.Is(err, modem.ErrDialUncertain) {
		t.Fatalf("pre-write cancel error = %v, want a plain context error", err)
	}
	port.mu.Lock()
	writes := append([]string(nil), port.writes...)
	port.mu.Unlock()
	if len(writes) != 0 {
		t.Fatalf("writes = %#v, want none for a pre-write cancel", writes)
	}
	adapter.mu.Lock()
	uncertain := adapter.uncertain
	adapter.mu.Unlock()
	if uncertain {
		t.Fatal("pre-write cancel set the quarantine")
	}
}

// TestDialDoesNotQuarantineWhenModemRejectsExplicitly: an ERROR final
// response is a known rejection; no quarantine and no hangup may follow.
func TestDialDoesNotQuarantineWhenModemRejectsExplicitly(t *testing.T) {
	port := &scriptedPort{failOn: "ATD10086;"}
	adapter := NewAdapter(NewClient(port))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := adapter.Dial(ctx, "10086")
	if err == nil || !errors.Is(err, ErrCommandFailed) {
		t.Fatalf("dial error = %v, want ErrCommandFailed", err)
	}
	if errors.Is(err, ErrResponseLost) {
		t.Fatalf("explicit modem rejection misclassified as ErrResponseLost: %v", err)
	}
	port.mu.Lock()
	writes := append([]string(nil), port.writes...)
	port.mu.Unlock()
	if len(writes) != 1 || strings.TrimSpace(writes[0]) != "ATD10086;" {
		t.Fatalf("writes = %#v, want only the rejected ATD", writes)
	}
	adapter.mu.Lock()
	uncertain, active := adapter.uncertain, adapter.active
	adapter.mu.Unlock()
	if uncertain || active != "" {
		t.Fatalf("state after rejection uncertain=%v active=%q", uncertain, active)
	}
}

// TestDialQuarantineMalformedCLCCFailsClosed: a truncated or unclassifiable
// CLCC row must read back as UNKNOWN, never as "no calls". The quarantine
// holds (no ATD, no ATH) until well-formed clean exchanges confirm the line.
func TestDialQuarantineMalformedCLCCFailsClosed(t *testing.T) {
	port := newUncertainPort()
	port.setCLCC([]string{`+CLCC: 1,0,0`}, false) // truncated row
	adapter := NewAdapter(NewClient(port))
	loseFirstDial(t, adapter)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if _, err := adapter.Dial(ctx, "10086"); !errors.Is(err, modem.ErrDialUncertain) {
		t.Fatalf("dial with malformed CLCC = %v, want ErrDialUncertain", err)
	}
	assertNoHangup(t, port.commands())
	if countPrefix(port.commands(), "ATD") != 1 {
		t.Fatalf("quarantined dial wrote ATD: %#v", port.commands())
	}
	adapter.mu.Lock()
	uncertain := adapter.uncertain
	adapter.mu.Unlock()
	if !uncertain {
		t.Fatal("malformed CLCC cleared the quarantine (fail open)")
	}

	// Unknown modes (3GPP reserves 9) fail closed the same way.
	port.setCLCC([]string{`+CLCC: 1,0,0,9,0,"",128`}, false)
	if _, err := adapter.Dial(ctx, "10086"); !errors.Is(err, modem.ErrDialUncertain) {
		t.Fatalf("dial with unknown-mode CLCC = %v, want ErrDialUncertain", err)
	}
	assertNoHangup(t, port.commands())

	// Well-formed empty reads clear it as before.
	port.setCLCC(nil, false)
	recoverCtx, recoverCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer recoverCancel()
	if _, err := adapter.Dial(recoverCtx, "10086"); err != nil {
		t.Fatalf("dial after CLCC recovered = %v", err)
	}
	assertNoHangup(t, port.commands())
}

// gatedOKPort answers the first read only after release, then OK. It models
// a modem whose ATD response is still in flight while URCs race in.
type gatedOKPort struct {
	mu     sync.Mutex
	writes []string
	release chan struct{}
}

func newGatedOKPort() *gatedOKPort { return &gatedOKPort{release: make(chan struct{})} }

func (p *gatedOKPort) Write(data []byte) (int, error) {
	p.mu.Lock()
	p.writes = append(p.writes, string(data))
	p.mu.Unlock()
	return len(data), nil
}

func (p *gatedOKPort) Read(buffer []byte) (int, error) {
	select {
	case <-p.release:
		return copy(buffer, "\r\nOK\r\n"), nil
	default:
		return 0, syscall.EAGAIN
	}
}

func (p *gatedOKPort) Close() error { return nil }

func (p *gatedOKPort) wrote(prefix string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, write := range p.writes {
		if strings.HasPrefix(strings.TrimSpace(write), prefix) {
			return true
		}
	}
	return false
}

func (p *gatedOKPort) commands() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	commands := make([]string, 0, len(p.writes))
	for _, write := range p.writes {
		commands = append(commands, strings.TrimSpace(write))
	}
	return commands
}

// TestRingDuringDialClaimsInboundAndQuarantinesOutgoing covers the
// mid-exchange attribution race: a RING dispatched while the ATD is in
// flight must claim the line and emit its incoming event (never dropped),
// and the dial's success path must preserve that inbound call, quarantine
// the unknown outgoing outcome, and write no hangup.
func TestRingDuringDialClaimsInboundAndQuarantinesOutgoing(t *testing.T) {
	port := newGatedOKPort()
	adapter := NewAdapter(NewClient(port))

	type dialResult struct {
		id  modem.CallID
		err error
	}
	result := make(chan dialResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		id, err := adapter.Dial(ctx, "10086")
		result <- dialResult{id, err}
	}()

	// Wait until the ATD is on the wire, then race a RING into the dispatch
	// path exactly as dispatchUnsolicited would.
	deadline := time.Now().Add(2 * time.Second)
	for !port.wrote("ATD") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !port.wrote("ATD") {
		t.Fatal("ATD was never written")
	}
	adapter.HandleURC("RING")

	var inbound modem.CallID
	select {
	case event := <-adapter.Events():
		if event.Kind != "incoming" || event.CallID == "" {
			t.Fatalf("incoming event = %#v, want claimed inbound", event)
		}
		inbound = event.CallID
	case <-time.After(time.Second):
		t.Fatal("RING during dial was not claimed")
	}
	adapter.mu.Lock()
	active := adapter.active
	adapter.mu.Unlock()
	if active != inbound {
		t.Fatalf("active = %q, want inbound %q", active, inbound)
	}

	close(port.release)
	got := <-result
	if got.err == nil || !errors.Is(got.err, modem.ErrDialUncertain) {
		t.Fatalf("dial = %q, %v; want ErrDialUncertain", got.id, got.err)
	}
	adapter.mu.Lock()
	active, uncertain := adapter.active, adapter.uncertain
	adapter.mu.Unlock()
	if active != inbound {
		t.Fatalf("inbound call overwritten: active = %q, want %q", active, inbound)
	}
	if !uncertain {
		t.Fatal("outcome of the in-flight ATD was not quarantined")
	}
	if got.id != "" {
		t.Fatalf("dial returned a call id %q although the line stayed inbound", got.id)
	}
	assertNoHangup(t, port.commands())

	// The inbound call still works end to end: its terminal URC ends it,
	// and once it is gone the quarantine resolves from fresh clean CLCC.
	adapter.HandleURC("NO CARRIER")
	deadline = time.Now().Add(2 * time.Second)
	for {
		adapter.mu.Lock()
		active, uncertain = adapter.active, adapter.uncertain
		adapter.mu.Unlock()
		if active == "" || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case event := <-adapter.Events():
		if event.Kind != "ended" || event.CallID != inbound {
			t.Fatalf("inbound end event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("inbound end event missing")
	}
	if !uncertain {
		t.Fatal("quarantine cleared before fresh CLCC proof")
	}
}

// TestRingBeforeDialSubmissionBlocksATD: a RING dispatched while the dial
// is still waiting for the serial queue (before ATD was written) must claim
// the line, emit the inbound event, and make the dial refuse to write ATD
// at all — a busy line, not a quarantine.
func TestRingBeforeDialSubmissionBlocksATD(t *testing.T) {
	port := &scriptedPort{}
	client := NewClient(port)
	adapter := NewAdapter(client)
	// Simulate another exchange owning the serial queue so the dial waits
	// between its busy check and the actual submission. The explicit release
	// below lets the dial through; ExchangeGuarded's releaseIO balances it,
	// so no deferred drain is needed (a second receive would hang).
	client.ioCh <- struct{}{}

	type dialResult struct {
		id  modem.CallID
		err error
	}
	result := make(chan dialResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		id, err := adapter.Dial(ctx, "10086")
		result <- dialResult{id, err}
	}()

	// Give the dial a moment to pass its busy check and park on the queue,
	// then deliver the inbound call.
	time.Sleep(50 * time.Millisecond)
	adapter.HandleURC("RING")

	var inbound modem.CallID
	select {
	case event := <-adapter.Events():
		if event.Kind != "incoming" {
			t.Fatalf("event = %#v, want incoming", event)
		}
		inbound = event.CallID
	case <-time.After(time.Second):
		t.Fatal("RING before submission was not claimed")
	}
	<-client.ioCh // release the queue so the dial can reach the guard
	got := <-result
	if got.err == nil || !errors.Is(got.err, modem.ErrActiveCall) {
		t.Fatalf("dial = %q, %v; want ErrActiveCall", got.id, got.err)
	}
	port.mu.Lock()
	writes := append([]string(nil), port.writes...)
	port.mu.Unlock()
	for _, write := range writes {
		if strings.HasPrefix(strings.TrimSpace(write), "ATD") {
			t.Fatalf("ATD written over an inbound call: %#v", writes)
		}
	}
	adapter.mu.Lock()
	active, uncertain := adapter.active, adapter.uncertain
	adapter.mu.Unlock()
	if active != inbound {
		t.Fatalf("active = %q, want inbound %q", active, inbound)
	}
	if uncertain {
		t.Fatal("nothing was written, but the line was quarantined")
	}
}

// TestRingDuringQuarantineResolveClaimsLine: while tryResolveUncertain
// holds the dial slot, a RING must still claim the line and emit its
// inbound event; the dial then refuses as a busy line (nothing was written)
// and must NOT quarantine.
func TestRingDuringQuarantineResolveClaimsLine(t *testing.T) {
	port := newUncertainPort()
	adapter := NewAdapter(NewClient(port))
	loseFirstDial(t, adapter)

	type dialResult struct {
		id  modem.CallID
		err error
	}
	result := make(chan dialResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		id, err := adapter.Dial(ctx, "10086")
		result <- dialResult{id, err}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for countPrefix(port.commands(), "AT+CLCC") == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	adapter.HandleURC("RING")
	var inbound modem.CallID
	select {
	case event := <-adapter.Events():
		if event.Kind != "incoming" {
			t.Fatalf("event = %#v, want incoming", event)
		}
		inbound = event.CallID
	case <-time.After(time.Second):
		t.Fatal("RING during resolve was not claimed")
	}
	adapter.mu.Lock()
	active := adapter.active
	adapter.mu.Unlock()
	if active != inbound {
		t.Fatalf("active = %q, want inbound %q", active, inbound)
	}

	got := <-result
	if got.err == nil || !errors.Is(got.err, modem.ErrActiveCall) {
		t.Fatalf("dial = %q, %v; want ErrActiveCall", got.id, got.err)
	}
	assertNoHangup(t, port.commands())
	if countPrefix(port.commands(), "ATD") != 1 {
		t.Fatalf("resolve wrote an ATD over an inbound call: %#v", port.commands())
	}
	adapter.mu.Lock()
	active = adapter.active
	adapter.mu.Unlock()
	if active != inbound {
		t.Fatalf("inbound lost after refused dial: active = %q, want %q", active, inbound)
	}
	// The pre-existing quarantine from the lost dial is untouched by this
	// refused dial: it stays set (self-healing later), and nothing new was
	// written. End the inbound call; the next dial then resolves the
	// quarantine from fresh clean CLCC and succeeds.
	adapter.HandleURC("NO CARRIER")
	port.setCLCC(nil, false)
	recoverCtx, recoverCancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer recoverCancel()
	if _, err := adapter.Dial(recoverCtx, "10086"); err != nil {
		t.Fatalf("dial after inbound ended = %v", err)
	}
	assertNoHangup(t, port.commands())
	adapter.mu.Lock()
	active, uncertain := adapter.active, adapter.uncertain
	adapter.mu.Unlock()
	if uncertain || active == "" {
		t.Fatalf("state after recovery active=%q uncertain=%v; want active dial, quarantine cleared", active, uncertain)
	}
}

// partialWritePort reports a short write for ATD, with or without an error,
// modeling a serial flush that dies (or lies) mid-command.
type partialWritePort struct {
	mu     sync.Mutex
	writes []string
	err    error
}

func (p *partialWritePort) Write(data []byte) (int, error) {
	p.mu.Lock()
	p.writes = append(p.writes, string(data))
	p.mu.Unlock()
	return len(data) / 2, p.err
}

func (p *partialWritePort) Read([]byte) (int, error) { return 0, syscall.EAGAIN }
func (p *partialWritePort) Close() error             { return nil }

func (p *partialWritePort) commands() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	commands := make([]string, 0, len(p.writes))
	for _, write := range p.writes {
		commands = append(commands, strings.TrimSpace(write))
	}
	return commands
}

// TestDialPartialWriteQuarantines: a write that flushed part of the ATD —
// reported either with an error or as a silent short write — leaves the
// modem side effect unknown, exactly like a lost final response. It must
// quarantine (no ATH/CHUP) instead of returning a plain error that would
// let the next dial pile onto an unknown leg.
func TestDialPartialWriteQuarantines(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "error", err: io.ErrClosedPipe},
		{name: "silent short write", err: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port := &partialWritePort{err: tc.err}
			adapter := NewAdapter(NewClient(port))
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err := adapter.Dial(ctx, "10086")
			if err == nil || !errors.Is(err, ErrResponseLost) {
				t.Fatalf("partial write dial error = %v, want ErrResponseLost", err)
			}
			adapter.mu.Lock()
			uncertain := adapter.uncertain
			adapter.mu.Unlock()
			if !uncertain {
				t.Fatal("partial write did not set the fail-closed quarantine")
			}
			commands := port.commands()
			if len(commands) != 1 || commands[0] != "ATD10086;" {
				t.Fatalf("writes after partial write = %#v, want only the ATD", commands)
			}
			assertNoHangup(t, commands)
		})
	}
}

// clccScriptPort serves a switchable CLCC row set, then OK.
type clccScriptPort struct {
	mu     sync.Mutex
	writes []string
	rows   []string
}

func (p *clccScriptPort) Write(data []byte) (int, error) {
	p.mu.Lock()
	p.writes = append(p.writes, string(data))
	p.mu.Unlock()
	return len(data), nil
}

func (p *clccScriptPort) setRows(rows ...string) {
	p.mu.Lock()
	p.rows = rows
	p.mu.Unlock()
}

func (p *clccScriptPort) Read(buffer []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.writes) == 0 {
		return 0, syscall.EAGAIN
	}
	last := strings.TrimSpace(p.writes[len(p.writes)-1])
	if !strings.HasPrefix(last, "AT+CLCC") {
		return 0, syscall.EAGAIN
	}
	// Consume the pending command so a retry doesn't double-serve.
	p.writes = p.writes[:len(p.writes)-1]
	response := "\r\n"
	for _, row := range p.rows {
		response += row + "\r\n"
	}
	response += "OK\r\n"
	return copy(buffer, response), nil
}

func (p *clccScriptPort) Close() error { return nil }

// TestWaitActiveRequiresVoiceMode: an active mobile-originated DATA session
// (dir=0, state=0, mode=1) must not satisfy WaitActive — only an answered
// voice leg may release the SIP bridge's audio path.
func TestWaitActiveRequiresVoiceMode(t *testing.T) {
	port := &clccScriptPort{}
	adapter := NewAdapter(NewClient(port))
	port.setRows(`+CLCC: 2,0,0,1,0,"",128`)

	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	answered, err := adapter.WaitActive(ctx)
	if err == nil || answered {
		t.Fatalf("WaitActive satisfied by active data session: answered=%v err=%v", answered, err)
	}

	port.setRows(`+CLCC: 2,0,0,0,0,"",129`)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	answered, err = adapter.WaitActive(ctx2)
	if err != nil || !answered {
		t.Fatalf("WaitActive missed answered voice leg: answered=%v err=%v", answered, err)
	}
}

// TestWaitAnyActiveRequiresVoiceMode: the fleet permanently reports an
// active mobile-terminated DATA row (+CLCC: 2,1,0,1,0). WaitAnyActive must
// not treat it as an active call, or the internal media path would open
// UAC capture on an idle line.
func TestWaitAnyActiveRequiresVoiceMode(t *testing.T) {
	port := &clccScriptPort{}
	adapter := NewAdapter(NewClient(port))
	port.setRows(`+CLCC: 2,1,0,1,0,"",128`)

	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	active, err := adapter.WaitAnyActive(ctx)
	if err == nil || active {
		t.Fatalf("WaitAnyActive satisfied by persistent data row: active=%v err=%v", active, err)
	}

	port.setRows(`+CLCC: 7,1,0,0,0,"+8613800138000",129`)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	active, err = adapter.WaitAnyActive(ctx2)
	if err != nil || !active {
		t.Fatalf("WaitAnyActive missed active inbound voice: active=%v err=%v", active, err)
	}
}

// TestStatusReportsDialUncertain makes the quarantine observable through
// the regular line status instead of only through refused dials.
func TestStatusReportsDialUncertain(t *testing.T) {
	port := newUncertainPort()
	adapter := NewAdapter(NewClient(port))
	loseFirstDial(t, adapter)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	status, err := adapter.Status(ctx)
	if err != nil {
		t.Fatalf("status = %v", err)
	}
	if !status.DialUncertain {
		t.Fatal("status did not report the fail-closed quarantine")
	}
	port.setCLCC(nil, false)
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	if _, err := adapter.Dial(dialCtx, "10086"); err != nil {
		t.Fatalf("recovery dial = %v", err)
	}
	status, err = adapter.Status(ctx)
	if err != nil {
		t.Fatalf("status after recovery = %v", err)
	}
	if status.DialUncertain {
		t.Fatal("status still reports quarantine after recovery")
	}
}
// is a voice call; the fleet's permanent data rows (mode 1) and disconnected
// legs (state 6) are ignored. Malformed rows and unknown modes must mark the
// whole response untrusted so callers fail closed.
func TestVoiceCallsCLCCParsing(t *testing.T) {
	lines := []string{
		`+CLCC: 2,1,0,1,0,"",128`,               // persistent data session
		`+CLCC: 9,0,6,0,0,"+8613800138000",129`, // disconnected voice
		`+CLCC: 3,0,2,0,0,"10086",129`,          // outgoing voice dialing
		"OK",
	}
	calls, trusted := voiceCallsCLCC(lines)
	if !trusted || len(calls) != 1 || calls[0].id != "3" || calls[0].dir != "0" {
		t.Fatalf("voiceCallsCLCC = %#v, %v; want only call 3, trusted", calls, trusted)
	}
	if got, ok := voiceCallsCLCC([]string{`+CLCC: 2,1,0,1,0,"",128`, "OK"}); ok && len(got) != 0 {
		t.Fatalf("data-only session reported as voice: %#v", got)
	}
	if got, ok := voiceCallsCLCC([]string{`+CLCC: 7,1,4,0,0,"+8613800138000",129`, "OK"}); !ok || len(got) != 1 || got[0].dir != "1" {
		t.Fatalf("incoming voice not detected: %#v, %v", got, ok)
	}
	// Fail-closed cases: these must be reported untrusted, never as clean.
	for _, tc := range [][]string{
		{`+CLCC: 1,0,0`, "OK"},           // truncated row
		{`+CLCC: 1,0,x,0,0,"",128`, "OK"}, // non-numeric state
		{`+CLCC: 1,0,0,x,0,"",128`, "OK"}, // non-numeric mode
		{`+CLCC: 1,0,0,9,0,"",128`, "OK"}, // unknown mode (27.007: 9)
		{ // valid voice row followed by garbage taints the whole read
			`+CLCC: 1,0,0,0,0,"",128`,
			`+CLCC: 1,0,0`,
			"OK",
		},
	} {
		if calls, ok := voiceCallsCLCC(tc); ok {
			t.Fatalf("voiceCallsCLCC(%#v) trusted = true, calls %#v; want fail closed", tc, calls)
		}
	}
}
