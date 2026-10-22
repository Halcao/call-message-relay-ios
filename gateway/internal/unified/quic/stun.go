package quic

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/pion/stun/v3"
)

// stunClient runs serialized RFC 8489 binding queries on the shared UDP
// socket. Only one transaction is in flight at a time (queries are
// cheap and rare), which keeps the transaction table trivially bounded
// and immune to spoofed-response races: a response is accepted only when
// its transaction ID matches the outstanding request AND its source
// address is the queried STUN server.
//
// Lock discipline (build 64): queryMu serializes whole transactions but
// is NEVER held across the network wait; mu guards only the short
// pending-state read/write. maybeHandle (the demux read loop) takes mu
// for its state read and then publishes to the buffered result channel
// without ever blocking on a lock — holding mu across the wait would
// deadlock every real response (and stall the demux loop behind it).
type stunClient struct {
	udp *net.UDPConn

	queryMu sync.Mutex

	mu      sync.Mutex
	pending chan stunBindingResult
	txID    []byte // 12 bytes, valid only while pending != nil
	server  net.Addr
}

type stunBindingResult struct {
	addr netip.AddrPort
	err  error
}

func newSTUNClient(udp *net.UDPConn) *stunClient {
	return &stunClient{udp: udp}
}

// maybeHandle inspects an inbound datagram that looks like STUN. It
// returns true IFF the packet was consumed (matched an outstanding
// transaction from the expected server).
func (c *stunClient) maybeHandle(pkt []byte, from net.Addr) bool {
	c.mu.Lock()
	pending := c.pending
	server := c.server
	txID := c.txID
	c.mu.Unlock()
	if pending == nil || server == nil {
		return false
	}
	if from.String() != server.String() {
		return false
	}
	msg := &stun.Message{Raw: pkt}
	if err := msg.Decode(); err != nil {
		return false
	}
	if msg.Type != (stun.MessageType{Method: stun.MethodBinding, Class: stun.ClassSuccessResponse}) {
		return false
	}
	if string(msg.TransactionID[:]) != string(txID) {
		return false
	}
	var xorAddr stun.XORMappedAddress
	if err := xorAddr.GetFrom(msg); err != nil {
		return false
	}
	if xorAddr.IP == nil {
		return false
	}
	addr, ok := netip.AddrFromSlice(xorAddr.IP)
	if !ok {
		return false
	}
	select {
	case pending <- stunBindingResult{addr: netip.AddrPortFrom(addr.Unmap(), uint16(xorAddr.Port))}:
	default:
	}
	return true
}

// ErrSTUNNoResponse indicates the server did not answer (timeout).
var ErrSTUNNoResponse = errors.New("stun: no response")

// query performs one binding request against server (host:port) with one
// retry, serialized across callers (queryMu; one transaction in flight).
// The response's XOR-MAPPED-ADDRESS is the server-reflexive candidate.
func (c *stunClient) query(ctx context.Context, server string) (netip.AddrPort, error) {
	c.queryMu.Lock()
	defer c.queryMu.Unlock()

	res, err := c.queryOnce(ctx, server)
	if err != nil {
		res, err = c.queryOnce(ctx, server)
	}
	return res, err
}

func (c *stunClient) queryOnce(ctx context.Context, server string) (netip.AddrPort, error) {
	serverAddr, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("stun: resolve %q: %w", server, err)
	}
	var txID [12]byte
	if _, err := rand.Read(txID[:]); err != nil {
		return netip.AddrPort{}, err
	}
	tid := stun.NewTransactionIDSetter(txID)
	msg, err := stun.Build(stun.NewType(stun.MethodBinding, stun.ClassRequest), tid)
	if err != nil {
		return netip.AddrPort{}, err
	}
	result := make(chan stunBindingResult, 1)
	c.mu.Lock()
	c.pending = result
	c.server = serverAddr
	c.txID = txID[:]
	c.mu.Unlock()
	defer func() {
		// Retire the pending state with a SHORT lock; clear only if it is
		// still THIS transaction's (a late response after timeout or
		// cancellation must never disturb a newer transaction).
		c.mu.Lock()
		if c.pending == result {
			c.pending = nil
			c.server = nil
			c.txID = nil
		}
		c.mu.Unlock()
	}()

	if _, err := c.udp.WriteTo(msg.Raw, serverAddr); err != nil {
		return netip.AddrPort{}, fmt.Errorf("stun: write: %w", err)
	}
	timeout := DefaultSTUNTimeout
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-result:
		return res.addr, res.err
	case <-timer.C:
		return netip.AddrPort{}, ErrSTUNNoResponse
	case <-ctx.Done():
		return netip.AddrPort{}, ctx.Err()
	}
}
