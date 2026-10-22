package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

type Target struct {
	DeviceID    string
	APNSToken   string
	VoIPToken   string
	Environment string
	Locale      string
}

type VoIPPayload struct {
	CallUUID     string `json:"callUUID"`
	CallID       string `json:"callId"`
	Handle       string `json:"handle"`
	GatewayID    string `json:"gatewayId"`
	LineID       string `json:"lineId,omitempty"`
	ConferenceID string `json:"conferenceId,omitempty"`
	IssuedAt     int64  `json:"issuedAt"`
}

// MessagePayload describes one newly-delivered inbound SMS alert push.
//
// Sender + bounded body preview (owner-authorized 2026-10-21): the visible
// alert carries a truncated sender and message preview so a notification is
// actionable without opening the app. The preview is a display projection
// only: it is ALWAYS truncated (PreviewLimit) before leaving the runtime, it
// is never logged, and the fan-out is gated by the device's ReceiveSMS grant
// and per-key harassment suppression, so it reaches exactly the devices that
// may read the message in the inbox. Custom fields stay at the aps level for
// tap routing (messageId/threadId identify the conversation).
type MessagePayload struct {
	MessageID string `json:"messageId"`
	GatewayID string `json:"gatewayId"`
	LineID    string `json:"lineId,omitempty"`
	ThreadKey string `json:"threadKey,omitempty"`
	Peer      string `json:"peer,omitempty"`
	Preview   string `json:"preview,omitempty"`
	IssuedAt  int64  `json:"issuedAt"`
}

// SMS preview display bounds (Unicode code points). Peers are short numbers;
// the preview stays a phone-notification-sized line so the final APNs JSON
// fits its alert budget even with long UTF-8 content.
const (
	PreviewPeerRunes = 32
	PreviewBodyRunes = 120
)

// TruncatePreview clips s to at most maxRunes code points, collapsing
// leading/trailing whitespace and appending "…" when clipped. It operates on
// runes, so CJK text and emoji never split into invalid UTF-8.
func TruncatePreview(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if maxRunes < 1 {
		return ""
	}
	runes := []rune(s)
	// Collapse internal whitespace runs to single spaces (preview hygiene,
	// and it keeps multi-line SMS readable in one notification line).
	var b []rune
	prevSpace := false
	for _, r := range runes {
		isSpace := r == ' ' || r == '\n' || r == '\r' || r == '\t'
		if isSpace {
			if !prevSpace && len(b) > 0 {
				b = append(b, ' ')
			}
			prevSpace = true
			continue
		}
		prevSpace = false
		b = append(b, r)
	}
	s = strings.TrimRight(string(b), " ")
	if s == "" {
		return ""
	}
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	// Reserve one code point for the ellipsis.
	clipped := []rune(s)[:maxRunes-1]
	// Never end on a dangling space before the ellipsis.
	return strings.TrimRight(string(clipped), " ") + "…"
}

// AlertPayload is a standard (non-VoIP) service alert: a confirmed carrier
// arrears notice or a designated-backup-network availability change. It never
// carries an SMS body or a full phone number — only the line name and the
// card tail the owner already knows.
type AlertPayload struct {
	Kind       string `json:"kind"`  // always "line_alert"
	Alert      string `json:"alert"` // arrears|network_unavailable|network_recovered|restored
	LineID     string `json:"lineId,omitempty"`
	LineName   string `json:"lineName,omitempty"`
	Title      string `json:"title,omitempty"`
	Body       string `json:"body,omitempty"`
	IncidentID string `json:"incidentId,omitempty"`
	GatewayID  string `json:"gatewayId,omitempty"`
	IssuedAt   int64  `json:"issuedAt"`
}

type Sender interface {
	SendVoIP(context.Context, Target, VoIPPayload) error
	SendMessage(context.Context, Target, MessagePayload) error
	SendAlert(context.Context, Target, AlertPayload) error
}

// Broker is the Gateway-side client for the optional CellBridge Push Broker.
// The broker is deliberately kept behind Sender so a deployment can replace
// it with a direct APNs implementation without touching call/SMS code.
type Broker struct {
	BaseURL       string
	Authorization string
	Client        *http.Client
}

func NewBroker(baseURL, authorization string) *Broker {
	return &Broker{BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"), Authorization: authorization, Client: &http.Client{Timeout: 10 * time.Second}}
}

func (b *Broker) SendVoIP(ctx context.Context, target Target, payload VoIPPayload) error {
	return b.post(ctx, "/v1/push/voip", target, payload)
}

func (b *Broker) SendMessage(ctx context.Context, target Target, payload MessagePayload) error {
	return b.post(ctx, "/v1/push/message", target, payload)
}

// SendAlert delivers a standard service alert through the broker. The broker
// contract mirrors the direct APNs sender: standard alert topic only, never
// the VoIP topic.
func (b *Broker) SendAlert(ctx context.Context, target Target, payload AlertPayload) error {
	return b.post(ctx, "/v1/push/alert", target, payload)
}

func (b *Broker) post(ctx context.Context, path string, target Target, payload any) error {
	if b == nil || b.BaseURL == "" {
		return fmt.Errorf("push broker is not configured")
	}
	if target.DeviceID == "" || target.Environment == "" {
		return fmt.Errorf("push target is invalid")
	}
	token := target.APNSToken
	if strings.HasSuffix(path, "/voip") {
		token = target.VoIPToken
	}
	if token == "" {
		return fmt.Errorf("push target token is missing")
	}
	body, err := json.Marshal(struct {
		DeviceID    string `json:"deviceId"`
		Token       string `json:"token"`
		Environment string `json:"environment"`
		Locale      string `json:"locale"`
		Payload     any    `json:"payload"`
	}{DeviceID: target.DeviceID, Token: token, Environment: target.Environment, Locale: target.Locale, Payload: payload})
	if err != nil {
		return fmt.Errorf("encode push request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, b.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create push request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if b.Authorization != "" {
		request.Header.Set("Authorization", b.Authorization)
	}
	client := b.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("send push request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("push broker returned HTTP %d", response.StatusCode)
	}
	return nil
}
