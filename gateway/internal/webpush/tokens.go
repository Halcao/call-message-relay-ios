package webpush

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"callrelay/gateway/internal/unified/store"
)

// Token lifecycle:
//
//   - BindCode: minted by the paired app through the authenticated device
//     API, shown to the user as a short code and/or a #fragment link/QR.
//     Short (9 chars, 45 bits) so it can be TYPED inside the installed PWA —
//     iOS isolates Home-Screen web-app storage from Safari, so the code must
//     be entered in the PWA itself after install (install-then-bind). The
//     code is reusable until ONE successful redemption (or expiry), so a
//     failed first attempt in the wrong browser never bricks the flow. It is
//     never a long-term app key: the native pairing credential never appears
//     in any URL or web page. Rate-limited per source IP at the HTTP layer.
//   - WebSession: derived browser credential, stored hashed, sliding TTL,
//     revocable. Scope-limited to the webpush endpoints (subscription
//     management, test, notify mode, minimal call status, handoff redeem).
//   - HandoffToken: in-memory, single use, ~120s, bound to (device, call).
//     Carried in the notification's navigate URL as a #fragment (fragments
//     never reach server or proxy access logs, so bots and link previews
//     cannot consume it). Only authorizes reading minimal metadata of that
//     one still-ringing call; stale/replayed/foreign tokens are
//     indistinguishable from invalid ones (uniform 410).

var (
	ErrBindCodeInvalid = errors.New("webpush: bind code is invalid or expired")
	ErrSessionInvalid  = errors.New("webpush: session is invalid or expired")
	ErrHandoffInvalid  = errors.New("webpush: handoff is invalid or expired")
)

// crockfordAlphabet excludes ambiguous characters (0/O, 1/I/L) so the code
// is safely typed from the app's screen into the PWA.
const crockfordAlphabet = "23456789ABCDEFGHJKMNPQRSTVWXYZ"

// randomBindCode returns a 9-character code (45 bits of entropy).
func randomBindCode() string {
	raw := make([]byte, 9)
	if _, err := rand.Read(raw); err != nil {
		panic(fmt.Sprintf("webpush: entropy unavailable: %v", err))
	}
	out := make([]byte, 9)
	for i, b := range raw {
		out[i] = crockfordAlphabet[int(b)%len(crockfordAlphabet)]
	}
	return string(out)
}

func randomToken() string {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		panic(fmt.Sprintf("webpush: entropy unavailable: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Manager owns bind/session persistence (via the store) and in-memory
// handoff tokens.
type Manager struct {
	Store store.WebPushTokenStore

	BindTTL    time.Duration
	SessionTTL time.Duration
	HandoffTTL time.Duration

	mu       sync.Mutex
	handoffs map[string]handoffEntry
	sweepAt  time.Time
}

type handoffEntry struct {
	DeviceID string
	CallID   string
	Expires  time.Time
}

// NewManager returns a token manager. TTLs <= 0 fall back to safe defaults.
func NewManager(st store.WebPushTokenStore) *Manager {
	return &Manager{
		Store:      st,
		BindTTL:    10 * time.Minute,
		SessionTTL: 30 * 24 * time.Hour,
		HandoffTTL: 120 * time.Second,
		handoffs:   map[string]handoffEntry{},
	}
}

// Session scopes. "push" sessions manage web-push subscriptions only;
// "client" sessions additionally authorize the standalone browser client
// (call/SMS/lines APIs, events, media), still bounded by the device's own
// line permissions.
const (
	ScopePush   = "push"
	ScopeClient = "client"
)

// ValidScope reports whether raw is a scope the owner may mint.
func ValidScope(raw string) bool { return raw == ScopePush || raw == ScopeClient }

// MintBindCode stores a fresh reusable-until-consumed bind code for the
// device and returns the raw code (the store keeps only its SHA-256). The
// scope is minted with the code and later inherited by the session.
func (m *Manager) MintBindCode(ctx context.Context, deviceID, scope string) (string, error) {
	if !ValidScope(scope) {
		return "", fmt.Errorf("webpush: unknown scope %q", scope)
	}
	code := randomBindCode()
	if err := m.Store.SaveWebPushBindToken(ctx, deviceID, hashToken(code), scope, m.BindTTL); err != nil {
		return "", err
	}
	return code, nil
}

// RedeemBindCode validates a bind code and, on success, consumes it and
// creates a web session for its device, inheriting the code's scope. Unknown,
// expired and already-consumed codes fail identically; an unconsumed code
// stays reusable so the install-then-bind flow can retry in the correct
// browser.
func (m *Manager) RedeemBindCode(ctx context.Context, code string) (sessionToken string, deviceID, scope string, err error) {
	deviceID, scope, err = m.Store.RedeemWebPushBindToken(ctx, hashToken(normalizeBindCode(code)))
	if err != nil {
		return "", "", "", ErrBindCodeInvalid
	}
	sessionToken = randomToken()
	if err := m.Store.SaveWebPushSession(ctx, deviceID, hashToken(sessionToken), scope, m.SessionTTL); err != nil {
		return "", "", "", err
	}
	if err := m.Store.MarkWebPushBindTokenUsed(ctx, hashToken(normalizeBindCode(code))); err != nil {
		// The session exists; a lost used-marker only widens the redemption
		// window to expiry, never breaks the flow.
		return sessionToken, deviceID, scope, nil
	}
	return sessionToken, deviceID, scope, nil
}

// normalizeBindCode uppercases and strips the common human separators so
// "abcd-efgh" typed from the app matches the minted code.
func normalizeBindCode(code string) string {
	out := make([]rune, 0, len(code))
	for _, r := range code {
		switch {
		case r >= 'a' && r <= 'z':
			out = append(out, r-'a'+'A')
		case r == '-' || r == ' ':
			continue
		default:
			out = append(out, r)
		}
	}
	return string(out)
}

// Authenticate resolves a bearer session token to its device, enforcing the
// sliding expiry and revocation on every call.
func (m *Manager) Authenticate(ctx context.Context, sessionToken string) (string, error) {
	deviceID, _, err := m.AuthenticateScoped(ctx, sessionToken)
	return deviceID, err
}

// AuthenticateScoped additionally returns the session scope so route
// wrappers can keep push-scoped sessions away from the client APIs.
func (m *Manager) AuthenticateScoped(ctx context.Context, sessionToken string) (string, string, error) {
	deviceID, scope, err := m.Store.TouchWebPushSession(ctx, hashToken(sessionToken), m.SessionTTL)
	if err != nil {
		return "", "", ErrSessionInvalid
	}
	return deviceID, scope, nil
}

// RevokeSessions expires every web session of the device (key deletion,
// unpair, owner revocation). Subscriptions are removed separately by the
// caller through the store so both always move together.
func (m *Manager) RevokeSessions(ctx context.Context, deviceID string) error {
	return m.Store.RevokeWebPushSessions(ctx, deviceID)
}

// RevokeSession expires ONE browser session (its "unbind this browser"
// action). The session token is presented one last time to authorize the
// revocation; afterwards the cookie/bearer is dead and a cold reload of
// that browser lands on the bind screen again.
func (m *Manager) RevokeSession(ctx context.Context, sessionToken string) error {
	return m.Store.RevokeWebPushSession(ctx, hashToken(sessionToken))
}

// MintHandoff registers a single-use handoff for (device, call).
func (m *Manager) MintHandoff(deviceID, callID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked(time.Now())
	token := randomToken()
	m.handoffs[token] = handoffEntry{
		DeviceID: deviceID, CallID: callID,
		Expires: time.Now().Add(m.HandoffTTL),
	}
	return token
}

// ConsumeHandoff validates and consumes a handoff token. A consumed, expired
// or unknown token all yield ErrHandoffInvalid so replays leak nothing.
func (m *Manager) ConsumeHandoff(token string) (handoffEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked(time.Now())
	entry, ok := m.handoffs[token]
	if !ok {
		return handoffEntry{}, ErrHandoffInvalid
	}
	delete(m.handoffs, token)
	if time.Now().After(entry.Expires) {
		return handoffEntry{}, ErrHandoffInvalid
	}
	return entry, nil
}

func (m *Manager) sweepLocked(now time.Time) {
	if now.Before(m.sweepAt) {
		return
	}
	m.sweepAt = now.Add(time.Minute)
	for token, entry := range m.handoffs {
		if now.After(entry.Expires) {
			delete(m.handoffs, token)
		}
	}
}
