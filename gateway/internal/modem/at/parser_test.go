package at

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestParserFragmentedReads(t *testing.T) {
	p := Parser{}
	var got []string
	got = append(got, p.Feed([]byte("ATI\r"))...)
	got = append(got, p.Feed([]byte("\nQuectel\r\nO"))...)
	got = append(got, p.Feed([]byte("K\r\n"))...)
	want := []string{"ATI", "Quectel", "OK"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestIsFinal(t *testing.T) {
	for _, line := range []string{"OK", "ERROR", "+CME ERROR: 3", "+CMS ERROR: 500"} {
		if !IsFinal(line) {
			t.Errorf("%q should be final", line)
		}
	}
	if IsFinal("+CSQ: 20,99") {
		t.Error("intermediate response marked final")
	}
}

type blockingPort struct {
	closed chan struct{}
	mu     sync.Mutex
	writes []string
}

func newBlockingPort() *blockingPort { return &blockingPort{closed: make(chan struct{})} }

func (p *blockingPort) Write(data []byte) (int, error) {
	p.mu.Lock()
	p.writes = append(p.writes, string(data))
	p.mu.Unlock()
	return len(data), nil
}

func (p *blockingPort) writesCopy() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.writes...)
}

func (p *blockingPort) Read([]byte) (int, error) {
	<-p.closed
	return 0, io.ErrClosedPipe
}

func (p *blockingPort) Close() error {
	select {
	case <-p.closed:
	default:
		close(p.closed)
	}
	return nil
}

func TestExchangeCancelsBlockedSerialRead(t *testing.T) {
	port := newBlockingPort()
	client := NewClient(port)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := client.Exchange(ctx, "AT")
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exchange error = %v, want deadline exceeded", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("blocked exchange did not cancel promptly")
	}
}

// TestExchangeDoesNotWriteOnCanceledContext covers the acquireIO race: the
// queue may be handed over even though ctx was already canceled, so the
// client must re-check the context before writing. A canceled ATD must never
// reach the modem because its side effect would be unknown.
func TestExchangeDoesNotWriteOnCanceledContext(t *testing.T) {
	for attempt := 0; attempt < 50; attempt++ {
		port := newBlockingPort()
		client := NewClient(port)
		client.ioCh <- struct{}{} // another exchange owns the queue
		released := make(chan struct{})
		go func() {
			<-client.ioCh
			close(released)
		}()
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // canceled before the queue is acquired
		if _, err := client.Exchange(ctx, "AT+CLCC"); err == nil {
			t.Fatal("exchange on canceled context succeeded")
		}
		<-released
		if writes := port.writesCopy(); len(writes) != 0 {
			t.Fatalf("wrote %#v on a canceled context", writes)
		}
	}
}

// chunkedPort answers each written command with a sequence of chunks
// delivered over separate reads after small delays, so exchanges and the
// idle Run() reader must reassemble fragmented URC/final responses.
type chunkedPort struct {
	mu      sync.Mutex
	writes  []string
	readBuf []byte
	chunks  map[string][]string
}

func newChunkedPort() *chunkedPort {
	return &chunkedPort{chunks: map[string][]string{}}
}

func (p *chunkedPort) script(command string, chunks ...string) {
	p.mu.Lock()
	p.chunks[command] = chunks
	p.mu.Unlock()
}

func (p *chunkedPort) Write(data []byte) (int, error) {
	p.mu.Lock()
	p.writes = append(p.writes, string(data))
	command := strings.TrimSpace(string(data))
	chunks := append([]string(nil), p.chunks[command]...)
	p.mu.Unlock()
	go func() {
		for _, chunk := range chunks {
			time.Sleep(5 * time.Millisecond)
			p.mu.Lock()
			p.readBuf = append(p.readBuf, chunk...)
			p.mu.Unlock()
		}
	}()
	return len(data), nil
}

func (p *chunkedPort) Read(buffer []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.readBuf) == 0 {
		return 0, syscall.EAGAIN
	}
	n := copy(buffer, p.readBuf)
	p.readBuf = p.readBuf[n:]
	return n, nil
}

func (p *chunkedPort) Close() error { return nil }

// TestExchangeTreatsFragmentedNoCarrierAsURC: a terminal URC split across
// reads mid-exchange must be collected as unsolicited and must NOT terminate
// the ATD exchange; the final OK still completes it.
func TestExchangeTreatsFragmentedNoCarrierAsURC(t *testing.T) {
	port := newChunkedPort()
	port.script("ATD10086;", "\r\nNO CAR", "RIER\r", "\nOK\r\n")
	client := NewClient(port)
	var urc []string
	client.SetLineHandler(func(line string) { urc = append(urc, line) })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	response, err := client.Exchange(ctx, "ATD10086;")
	if err != nil {
		t.Fatalf("exchange = %v", err)
	}
	if len(response) != 1 || response[0] != "OK" {
		t.Fatalf("response = %#v, want [OK]", response)
	}
	if len(urc) != 1 || !strings.Contains(urc[0], "NO CARRIER") {
		t.Fatalf("dispatched URCs = %#v, want the fragmented NO CARRIER", urc)
	}
}

// TestRunDoesNotStealExchangeBytes: the idle Run() reader and command
// exchanges share the ioCh queue. While a slow exchange owns the port, Run
// must skip its poll and never consume (or misdispatch) response bytes.
func TestRunDoesNotStealExchangeBytes(t *testing.T) {
	port := newChunkedPort()
	port.script("AT+CLCC", "\r\n+CLCC: 1,0,0,0,0,\"\",129\r\nOK\r\n")
	client := NewClient(port)
	var seen []string
	var seenMu sync.Mutex
	client.SetLineHandler(func(line string) {
		seenMu.Lock()
		seen = append(seen, line)
		seenMu.Unlock()
	})

	runCtx, runCancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- client.Run(runCtx) }()

	// Give Run a few poll ticks before the exchange starts.
	time.Sleep(40 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	response, err := client.Exchange(ctx, "AT+CLCC")
	if err != nil {
		t.Fatalf("exchange = %v", err)
	}
	want := []string{"+CLCC: 1,0,0,0,0,\"\",129", "OK"}
	if len(response) != len(want) || response[0] != want[0] || response[1] != want[1] {
		t.Fatalf("response = %#v, want %#v", response, want)
	}
	runCancel()
	<-runDone

	seenMu.Lock()
	defer seenMu.Unlock()
	for _, line := range seen {
		if strings.HasPrefix(line, "+CLCC") || line == "OK" {
			t.Fatalf("idle reader stole exchange bytes: %#v", seen)
		}
	}
}

// TestExchangeClassifiesWriteErrors pins write-side uncertainty: any write
// that flushed some or all of the command (short write with or without an
// error, or a full write with an error) means the modem may hold (part of)
// it, so it must classify as ErrResponseLost; a zero-byte write is a clean
// pre-write failure and must not.
func TestExchangeClassifiesWriteErrors(t *testing.T) {
	partial := &flakyWritePort{keep: 3, err: io.ErrClosedPipe}
	client := NewClient(partial)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := client.Exchange(ctx, "ATD10086;"); !errors.Is(err, ErrResponseLost) {
		t.Fatalf("partial write error = %v, want ErrResponseLost", err)
	}

	// io.WriteString does not synthesize ErrShortWrite: a writer may
	// return (n < len, nil). That must still be treated as uncertain.
	shortSilent := &flakyWritePort{keep: 3}
	client = NewClient(shortSilent)
	if _, err := client.Exchange(ctx, "ATD10086;"); !errors.Is(err, ErrResponseLost) {
		t.Fatalf("short write without error = %v, want ErrResponseLost", err)
	}

	zero := &flakyWritePort{keep: 0, err: io.ErrClosedPipe}
	client = NewClient(zero)
	if _, err := client.Exchange(ctx, "ATD10086;"); err == nil || errors.Is(err, ErrResponseLost) {
		t.Fatalf("zero-byte write error = %v, want plain write error", err)
	}
}

type flakyWritePort struct {
	mu     sync.Mutex
	writes []string
	keep   int
	err    error
}

func (p *flakyWritePort) Write(data []byte) (int, error) {
	p.mu.Lock()
	p.writes = append(p.writes, string(data))
	p.mu.Unlock()
	return p.keep, p.err
}

func (p *flakyWritePort) Read([]byte) (int, error) { return 0, syscall.EAGAIN }
func (p *flakyWritePort) Close() error             { return nil }
