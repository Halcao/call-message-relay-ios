package push

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// APNsConfig configures the optional direct Apple Push Notification service
// sender. It stays disabled unless every field is present and the key file is
// readable; no value is ever invented.
type APNsConfig struct {
	// KeyFile is a path to the APNs auth key (.p8, PKCS#8 EC private key).
	// The file is read at startup and never logged.
	KeyFile string
	// KeyID is the APNs key identifier shown in the Apple developer account.
	KeyID string
	// TeamID is the Apple team identifier.
	TeamID string
	// BundleID is the app bundle identifier; the VoIP topic is BundleID.voip.
	BundleID string
	// Environment selects the APNs host: "sandbox" or "production".
	Environment string
}

// APNsSender implements Sender directly against APNs over HTTP/2 with an
// ES256 provider token. It is intentionally constructed only when the whole
// configuration is present; otherwise callrelayd keeps push disabled.
type APNsSender struct {
	config  APNsConfig
	key     *ecdsa.PrivateKey
	baseURL string
	client  *http.Client
	now     func() time.Time

	mu       sync.Mutex
	token    string
	issuedAt time.Time
}

const (
	apnsProductionHost = "https://api.push.apple.com"
	apnsSandboxHost    = "https://api.sandbox.push.apple.com"
	apnsTokenLifetime  = 50 * time.Minute
	// VoIP pushes must not survive the ring: expiration 0 means deliver now or
	// drop. The gateway voicemail answers unanswered calls after ~30 s, so a
	// stale ringer is worse than a missed one.
	apnsVoIPTTL    = 0
	apnsMessageTTL = 24 * time.Hour
	// Service alerts describe a current condition. A long TTL would deliver a
	// stale "network unavailable" notice after recovery, so alerts expire
	// after six hours; the durable dedupe/incident state still guarantees one
	// alert per fault while the device is reachable.
	apnsAlertTTL = 6 * time.Hour
	// APNs rejects payloads larger than 4 KB.
	apnsMaxPayloadBytes = 4096
)

// NewAPNsSender validates the configuration and loads the signing key. All
// error strings are privacy-safe: they never contain the key material or a
// device token.
func NewAPNsSender(config APNsConfig) (*APNsSender, error) {
	config.KeyFile = strings.TrimSpace(config.KeyFile)
	config.KeyID = strings.TrimSpace(config.KeyID)
	config.TeamID = strings.TrimSpace(config.TeamID)
	config.BundleID = strings.TrimSpace(config.BundleID)
	config.Environment = strings.ToLower(strings.TrimSpace(config.Environment))

	switch {
	case config.KeyFile == "":
		return nil, fmt.Errorf("APNs key file is not configured")
	case config.KeyID == "":
		return nil, fmt.Errorf("APNs key id is not configured")
	case config.TeamID == "":
		return nil, fmt.Errorf("APNs team id is not configured")
	case config.BundleID == "":
		return nil, fmt.Errorf("APNs bundle id is not configured")
	}

	baseURL := apnsProductionHost
	switch config.Environment {
	case "", "production":
		baseURL = apnsProductionHost
	case "sandbox":
		baseURL = apnsSandboxHost
	default:
		return nil, fmt.Errorf("APNs environment must be sandbox or production")
	}

	key, err := loadAPNsKey(config.KeyFile)
	if err != nil {
		return nil, err
	}
	if key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("APNs key must be a P-256 EC key for ES256")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ForceAttemptHTTP2 = true
	return &APNsSender{
		config:  config,
		key:     key,
		baseURL: baseURL,
		client:  &http.Client{Timeout: 15 * time.Second, Transport: transport},
		now:     time.Now,
	}, nil
}

func loadAPNsKey(path string) (*ecdsa.PrivateKey, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read APNs key file: %w", err)
	}
	block, _ := pem.Decode(payload)
	if block == nil {
		return nil, fmt.Errorf("APNs key file is not PEM encoded")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		ecdsaKey, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("APNs key is not an EC private key")
		}
		return ecdsaKey, nil
	}
	if ecdsaKey, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return ecdsaKey, nil
	}
	return nil, fmt.Errorf("APNs key could not be parsed")
}

// providerToken returns a cached ES256 provider token, refreshed before it
// expires. iat is deliberately backdated so a slightly fast server clock does
// not reject a fresh token.
func (s *APNsSender) providerToken(now time.Time) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && now.Sub(s.issuedAt) < apnsTokenLifetime {
		return s.token, nil
	}
	issuedAt := now.Add(-30 * time.Second)
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"ES256","kid":"` + s.config.KeyID + `"}`))
	claims, err := json.Marshal(map[string]any{"iss": s.config.TeamID, "iat": issuedAt.Unix()})
	if err != nil {
		return "", fmt.Errorf("encode APNs claims: %w", err)
	}
	signingInput := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	token, err := signES256(s.key, signingInput)
	if err != nil {
		return "", err
	}
	s.token = token
	s.issuedAt = now
	return token, nil
}

// voipEnvelope is the standard PushKit envelope: custom keys at the top level
// plus a content-available aps dictionary. No alert/sound is used for VoIP.
type voipEnvelope struct {
	VoIPPayload
	APS struct {
		ContentAvailable int `json:"content-available"`
	} `json:"aps"`
}

// messageEnvelope is the standard alert envelope for a new inbound SMS.
// Since the owner-authorized 2026-10-21 preview contract, aps.alert carries
// the sender and a bounded body preview (both truncated by the runtime via
// push.TruncatePreview) so the notification is actionable without opening
// the app; when no preview is available it degrades to the previous generic
// localized line. The custom fields next to aps (messageId/threadKey) drive
// tap routing, and aps.thread-id groups notifications per conversation.
// Delivery is gated upstream to devices holding the line's ReceiveSMS grant
// and passing per-key harassment suppression; the preview is display-only
// and never logged.
type messageEnvelope struct {
	MessagePayload
	APS struct {
		Alert    map[string]string `json:"alert"`
		Sound    string            `json:"sound"`
		ThreadID string            `json:"thread-id,omitempty"`
		Category string            `json:"category,omitempty"`
	} `json:"aps"`
}

// SendVoIP delivers a VoIP push; the app must report it to CallKit.
func (s *APNsSender) SendVoIP(ctx context.Context, target Target, payload VoIPPayload) error {
	envelope := voipEnvelope{VoIPPayload: payload}
	envelope.APS.ContentAvailable = 1
	return s.send(ctx, target, target.VoIPToken, s.config.BundleID+".voip", "voip", "10", apnsVoIPTTL, envelope)
}

// SendMessage delivers an alert push for a new inbound SMS. The visible
// alert shows the bounded sender/preview captured at the fan-out point; an
// empty preview falls back to the generic localized line (older runtimes or
// non-text messages), so this stays a superset of the previous contract.
//
// Budget: the DISPLAY fields (peer/preview) are truncated into the envelope,
// so a long SMS can never inflate the alert past the 4 KiB APNs limit. The
// routing identity (messageId/threadKey) is embedded COMPLETE and untouched —
// a clipped key is not a valid tap target; pathologically oversized routing
// metadata is instead rejected by the final-JSON size check in send().
// aps.thread-id is a stable digest of (gateway id, FULL thread key): distinct
// gateways and keys sharing any prefix never collide, while all messages of
// one conversation group together.
func (s *APNsSender) SendMessage(ctx context.Context, target Target, payload MessagePayload) error {
	display := payload
	display.Peer = TruncatePreview(display.Peer, PreviewPeerRunes)
	display.Preview = TruncatePreview(display.Preview, PreviewBodyRunes)
	envelope := messageEnvelope{MessagePayload: display}
	title := "CallRelay"
	if display.Peer != "" {
		// The bounded sender is the title whenever it exists, with or
		// without a body preview (sender-only non-text messages still
		// name the sender).
		title = display.Peer
	}
	body := messageFallbackBody(target.Locale)
	if display.Preview != "" {
		body = display.Preview
	} else if display.Peer != "" {
		body = display.Peer
	}
	envelope.APS.Alert = map[string]string{"title": title, "body": body}
	envelope.APS.Sound = "default"
	envelope.APS.Category = "CALLRELAY_SMS"
	if payload.ThreadKey != "" {
		// Stable per-conversation grouping id from the FULL identity: a
		// truncated hash of gateway+key cannot collide across gateways or
		// keys sharing a long prefix beyond any display bound.
		sum := sha256.Sum256([]byte(payload.GatewayID + "\x00" + payload.ThreadKey))
		envelope.APS.ThreadID = "sms-" + hex.EncodeToString(sum[:])[:16]
	}
	return s.send(ctx, target, target.APNSToken, s.config.BundleID, "alert", "5", apnsMessageTTL, envelope)
}

func messageFallbackBody(locale string) string {
	if len(locale) >= 2 && strings.EqualFold(locale[:2], "en") {
		return "New message"
	}
	return "收到一条新短信"
}

// alertEnvelope is the standard-alert envelope for service alerts: a normal
// aps.alert with sound, a category the app can route on, and the semantic
// payload fields at the top level. It is never sent on the VoIP topic.
type alertEnvelope struct {
	AlertPayload
	APS struct {
		Alert    map[string]string `json:"alert"`
		Sound    string            `json:"sound"`
		Category string            `json:"category"`
		ThreadID string            `json:"thread-id,omitempty"`
	} `json:"aps"`
}

// SendAlert delivers a confirmed-arrears or backup-network alert as an
// ordinary notification (topic = app bundle id, type "alert").
func (s *APNsSender) SendAlert(ctx context.Context, target Target, payload AlertPayload) error {
	if payload.Alert == "" || payload.LineID == "" {
		return fmt.Errorf("alert payload is incomplete")
	}
	title := strings.TrimSpace(payload.Title)
	body := strings.TrimSpace(payload.Body)
	if title == "" {
		title = "CallRelay"
	}
	envelope := alertEnvelope{AlertPayload: payload}
	envelope.APS.Alert = map[string]string{"title": title, "body": body}
	envelope.APS.Sound = "default"
	envelope.APS.Category = "CALLRELAY_LINE_ALERT"
	envelope.APS.ThreadID = "line-alert-" + payload.LineID
	return s.send(ctx, target, target.APNSToken, s.config.BundleID, "alert", "5", apnsAlertTTL, envelope)
}

// apnsHost validates the per-target environment and returns the matching host.
// A target that disagrees with the explicitly configured environment is
// rejected instead of being silently sent to the wrong endpoint (e.g. a
// Feather re-signed development build hitting production).
func (s *APNsSender) apnsHost(environment string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(environment))
	switch normalized {
	case "sandbox":
		if s.config.Environment != "" && s.config.Environment != "sandbox" {
			return "", fmt.Errorf("push target environment mismatch: target=sandbox configured=%s", s.config.Environment)
		}
		return apnsSandboxHost, nil
	case "production":
		if s.config.Environment != "" && s.config.Environment != "production" {
			return "", fmt.Errorf("push target environment mismatch: target=production configured=%s", s.config.Environment)
		}
		return apnsProductionHost, nil
	default:
		return "", fmt.Errorf("push target environment must be sandbox or production")
	}
}

func (s *APNsSender) send(
	ctx context.Context, target Target, token, topic, pushType, priority string,
	ttl time.Duration, payload any,
) error {
	if s == nil {
		return fmt.Errorf("APNs sender is not configured")
	}
	if target.DeviceID == "" || target.Environment == "" {
		return fmt.Errorf("push target is invalid")
	}
	if token == "" {
		return fmt.Errorf("push target token is missing")
	}
	host, err := s.apnsHost(target.Environment)
	if err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode APNs payload: %w", err)
	}
	if len(body) > apnsMaxPayloadBytes {
		return fmt.Errorf("APNs payload is %d bytes, over the %d byte limit", len(body), apnsMaxPayloadBytes)
	}
	nowFn := s.now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn()
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, host+"/3/device/"+token, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create APNs request: %w", err)
	}
	providerToken, err := s.providerToken(now)
	if err != nil {
		return err
	}
	request.Header.Set("authorization", "bearer "+providerToken)
	request.Header.Set("apns-topic", topic)
	request.Header.Set("apns-push-type", pushType)
	request.Header.Set("apns-priority", priority)
	if ttl <= 0 {
		// Immediate-or-drop.
		request.Header.Set("apns-expiration", "0")
	} else {
		request.Header.Set("apns-expiration", fmt.Sprintf("%d", now.Add(ttl).Unix()))
	}
	request.Header.Set("content-type", "application/json")

	response, err := s.client.Do(request)
	if err != nil {
		// url.Error can embed the request URL, which contains the device
		// token. Strip it to keep logs privacy-safe.
		return fmt.Errorf("send APNs request: transport error")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusOK {
		return nil
	}
	reason := apnsReason(response.Body)
	if reason == "" {
		return fmt.Errorf("APNs rejected push (status=%d)", response.StatusCode)
	}
	return fmt.Errorf("APNs rejected push (status=%d reason=%s)", response.StatusCode, reason)
}

func apnsReason(body io.Reader) string {
	payload, err := io.ReadAll(io.LimitReader(body, 4096))
	if err != nil {
		return ""
	}
	var envelope struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(payload, &envelope) != nil {
		return ""
	}
	return envelope.Reason
}

func signES256(key *ecdsa.PrivateKey, signingInput string) (string, error) {
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign APNs token: %w", err)
	}
	size := (key.Curve.Params().BitSize + 7) / 8
	signature := make([]byte, size*2)
	r.FillBytes(signature[:size])
	s.FillBytes(signature[size:])
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}
