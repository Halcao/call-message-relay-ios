// Package worker is the unified service's client for one forked CellBridge
// worker. Control, history and events use the stable v1 HTTP API with a
// dedicated paired device; media uses the worker's loopback-only internal
// PCMU socket so ALSA/UAC ownership stays with the worker.
package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"callrelay/gateway/internal/unified/identity"
	"callrelay/gateway/internal/unified/store"

	"github.com/gorilla/websocket"
)

type Client struct {
	LineID          string
	BaseURL         string
	InternalBaseURL string
	TokenFile       string
	IdentityFile    string
	Store           *store.Store
	GatewayIdentity *identity.Identity
	HTTP            *http.Client

	mu           sync.Mutex
	accessToken  string
	refreshToken string
	accessExpiry time.Time
}

func New(lineID, baseURL, internalURL, tokenFile, identityFile string, st *store.Store, gatewayIdentity *identity.Identity) *Client {
	client := &Client{
		LineID:          lineID,
		BaseURL:         strings.TrimRight(baseURL, "/"),
		InternalBaseURL: strings.TrimRight(strings.TrimSpace(internalURL), "/"),
		TokenFile:       tokenFile,
		IdentityFile:    identityFile,
		Store:           st,
		GatewayIdentity: gatewayIdentity,
		HTTP:            &http.Client{Timeout: 30 * time.Second},
	}
	if client.InternalBaseURL == "" {
		// Older lines config without internal_url: the internal listener is
		// a separate loopback port; fall back to the worker base so the path
		// degrades rather than failing outright.
		client.InternalBaseURL = client.BaseURL
	}
	return client
}

type APIError struct {
	Status  int
	Code    string
	Message string
	Body    []byte
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("worker API %d: %s (%s)", e.Status, e.Message, e.Code)
	}
	return fmt.Sprintf("worker API %d: %s", e.Status, e.Message)
}

func (c *Client) InternalToken() (string, error) {
	payload, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return "", fmt.Errorf("read internal token: %w", err)
	}
	token := strings.TrimSpace(string(payload))
	if token == "" {
		return "", errors.New("internal token file is empty")
	}
	return token, nil
}

// ---------- token management ----------

func (c *Client) loadStoredTokens() {
	access, accessExpiry, err := c.Store.LoadWorkerToken(context.Background(), c.LineID, "access")
	if err == nil {
		c.accessToken = access
		c.accessExpiry = accessExpiry
	}
	refresh, _, refreshErr := c.Store.LoadWorkerToken(context.Background(), c.LineID, "refresh")
	if refreshErr == nil {
		c.refreshToken = refresh
	}
}

func (c *Client) ensureAccess(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.accessToken == "" && c.refreshToken == "" {
		c.loadStoredTokens()
	}
	if !force && c.accessToken != "" && time.Now().UTC().Before(c.accessExpiry.Add(-30*time.Second)) {
		return c.accessToken, nil
	}
	if c.refreshToken != "" {
		if pair, err := c.refresh(ctx); err == nil {
			return pair.AccessToken, nil
		}
		slog.Warn("worker token refresh failed; re-pairing", "line", c.LineID)
	}
	pair, err := c.pair(ctx)
	if err != nil {
		return "", err
	}
	return pair.AccessToken, nil
}

type tokenPair struct {
	DeviceID     string `json:"deviceId"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
}

func (c *Client) refresh(ctx context.Context) (tokenPair, error) {
	payload, _ := json.Marshal(map[string]string{"refreshToken": c.refreshToken})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/v1/auth/refresh", bytes.NewReader(payload))
	if err != nil {
		return tokenPair{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.HTTP.Do(request)
	if err != nil {
		return tokenPair{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return tokenPair{}, errors.New("refresh rejected")
	}
	var pair tokenPair
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&pair); err != nil {
		return tokenPair{}, err
	}
	c.storePair(ctx, pair)
	return pair, nil
}

func (c *Client) pair(ctx context.Context) (tokenPair, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/v1/pairing/start", strings.NewReader("{}"))
	if err != nil {
		return tokenPair{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.HTTP.Do(request)
	if err != nil {
		return tokenPair{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return tokenPair{}, fmt.Errorf("worker pairing start failed: %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var started struct {
		GatewayID     string `json:"gatewayId"`
		PairingID     string `json:"pairingId"`
		OneTimeSecret string `json:"oneTimeSecret"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&started); err != nil {
		return tokenPair{}, err
	}
	const deviceName = "callrelayd"
	message := started.PairingID + "\n" + started.OneTimeSecret + "\n" + started.GatewayID + "\n" + deviceName
	proof := c.GatewayIdentity.Sign([]byte(message))
	completeBody, _ := json.Marshal(map[string]string{
		"pairingId":       started.PairingID,
		"deviceName":      deviceName,
		"devicePublicKey": base64.StdEncoding.EncodeToString(c.GatewayIdentity.Public),
		"proof":           base64.StdEncoding.EncodeToString(proof),
	})
	completeRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/v1/pairing/complete", bytes.NewReader(completeBody))
	if err != nil {
		return tokenPair{}, err
	}
	completeRequest.Header.Set("Content-Type", "application/json")
	completeResponse, err := c.HTTP.Do(completeRequest)
	if err != nil {
		return tokenPair{}, err
	}
	defer completeResponse.Body.Close()
	if completeResponse.StatusCode != http.StatusOK && completeResponse.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(completeResponse.Body, 4096))
		return tokenPair{}, fmt.Errorf("worker pairing complete failed: %s: %s", completeResponse.Status, strings.TrimSpace(string(body)))
	}
	var pair tokenPair
	if err := json.NewDecoder(io.LimitReader(completeResponse.Body, 1<<20)).Decode(&pair); err != nil {
		return tokenPair{}, err
	}
	c.storePair(ctx, pair)
	slog.Info("worker paired", "line", c.LineID, "device_id", pair.DeviceID)
	return pair, nil
}

func (c *Client) storePair(ctx context.Context, pair tokenPair) {
	now := time.Now().UTC()
	c.accessToken = pair.AccessToken
	c.refreshToken = pair.RefreshToken
	c.accessExpiry = now.Add(14 * time.Minute)
	_ = c.Store.SaveWorkerToken(ctx, c.LineID, "access", pair.AccessToken, now.Add(14*time.Minute))
	_ = c.Store.SaveWorkerToken(ctx, c.LineID, "refresh", pair.RefreshToken, now.Add(29*24*time.Hour))
}

// ---------- authorized HTTP ----------

func (c *Client) do(ctx context.Context, method, path, idempotencyKey string, body []byte, out any) error {
	payload, _, err := c.doRaw(ctx, method, path, idempotencyKey, body)
	if err != nil {
		return err
	}
	if out != nil && len(payload) > 0 {
		if err := json.Unmarshal(payload, out); err != nil {
			return err
		}
	}
	return nil
}

// doRaw performs an authorized request and returns the raw body plus
// response headers (used for X-CellBridge-Has-More).
func (c *Client) doRaw(ctx context.Context, method, path, idempotencyKey string, body []byte) ([]byte, http.Header, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		token, err := c.ensureAccess(ctx, attempt > 0)
		if err != nil {
			return nil, nil, err
		}
		request, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewReader(body))
		if err != nil {
			return nil, nil, err
		}
		request.Header.Set("Authorization", "Bearer "+token)
		if len(body) > 0 {
			request.Header.Set("Content-Type", "application/json")
		}
		if idempotencyKey != "" {
			request.Header.Set("Idempotency-Key", idempotencyKey)
		}
		response, err := c.HTTP.Do(request)
		if err != nil {
			return nil, nil, err
		}
		payload, readErr := io.ReadAll(io.LimitReader(response.Body, 4<<20))
		response.Body.Close()
		if readErr != nil {
			return nil, nil, readErr
		}
		if response.StatusCode == http.StatusUnauthorized && attempt == 0 {
			lastErr = &APIError{Status: response.StatusCode, Message: "unauthorized"}
			continue
		}
		if response.StatusCode >= 400 {
			apiErr := &APIError{Status: response.StatusCode, Body: payload}
			var envelope struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if json.Unmarshal(payload, &envelope) == nil {
				apiErr.Code = envelope.Code
				apiErr.Message = envelope.Message
			}
			if apiErr.Message == "" {
				apiErr.Message = strings.TrimSpace(string(payload))
			}
			return nil, response.Header, apiErr
		}
		return payload, response.Header, nil
	}
	if lastErr == nil {
		lastErr = errors.New("worker request failed")
	}
	return nil, nil, lastErr
}

// ---------- types ----------

type LineStatus struct {
	SIM            string `json:"sim"`
	Operator       string `json:"operator"`
	Registration   string `json:"registration"`
	Voice          string `json:"voice"`
	SMS            string `json:"sms"`
	CapabilityTier string `json:"capabilityTier"`
	Signal         struct {
		RSSI int `json:"rssi"`
		Bars int `json:"bars"`
	} `json:"signal"`
	ActiveCallID *string `json:"activeCallId"`
}

type Identity struct {
	GatewayID   string `json:"gatewayId"`
	GatewayName string `json:"gatewayName"`
	APIVersion  string `json:"apiVersion"`
	PublicKey   string `json:"publicKey"`
	Fingerprint string `json:"fingerprint"`
}

type CallRecord struct {
	ID          string `json:"id"`
	GatewayID   string `json:"gatewayID"`
	LineID      string `json:"lineID"`
	Direction   string `json:"direction"`
	Peer        string `json:"peer"`
	State       string `json:"state"`
	StartedAt   int64  `json:"startedAt"`
	ConnectedAt *int64 `json:"connectedAt"`
	EndedAt     *int64 `json:"endedAt"`
	EndReason   string `json:"endReason"`
}

type Message struct {
	ID        string `json:"id"`
	GatewayID string `json:"gatewayID"`
	LineID    string `json:"lineID"`
	ThreadKey string `json:"threadKey"`
	Direction string `json:"direction"`
	Peer      string `json:"peer"`
	Body      string `json:"body"`
	Encoding  string `json:"encoding"`
	Status    string `json:"status"`
	CreatedAt int64  `json:"createdAt"`
}

type Thread struct {
	Key         string  `json:"key"`
	Peer        string  `json:"peer"`
	UnreadCount int     `json:"unreadCount"`
	LastMessage Message `json:"lastMessage"`
}

type Event struct {
	ID        string          `json:"id"`
	Seq       int64           `json:"seq"`
	Type      string          `json:"type"`
	CreatedAt int64           `json:"createdAt"`
	Data      json.RawMessage `json:"data"`
}

// ---------- public API ----------

func (c *Client) Health(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1/health", nil)
	if err != nil {
		return err
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return &APIError{Status: response.StatusCode, Message: "health check failed"}
	}
	return nil
}

func (c *Client) Identity(ctx context.Context) (Identity, error) {
	var result Identity
	err := c.do(ctx, http.MethodGet, "/api/v1/identity", "", nil, &result)
	return result, err
}

func (c *Client) Line(ctx context.Context) (LineStatus, error) {
	var result LineStatus
	err := c.do(ctx, http.MethodGet, "/api/v1/line", "", nil, &result)
	return result, err
}

func (c *Client) Dial(ctx context.Context, to, clientCallID, idempotencyKey string) (CallRecord, error) {
	body, _ := json.Marshal(map[string]string{"to": to, "clientCallId": clientCallID})
	var record CallRecord
	err := c.do(ctx, http.MethodPost, "/api/v1/calls", idempotencyKey, body, &record)
	return record, err
}

func (c *Client) GetCall(ctx context.Context, callID string) (CallRecord, error) {
	var record CallRecord
	err := c.do(ctx, http.MethodGet, "/api/v1/calls/"+url.PathEscape(callID), "", nil, &record)
	return record, err
}

func (c *Client) ListCalls(ctx context.Context, limit int) ([]CallRecord, error) {
	var records []CallRecord
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/v1/calls?limit=%d", limit), "", nil, &records)
	return records, err
}

func (c *Client) CallAction(ctx context.Context, callID, action, idempotencyKey string, body []byte) error {
	return c.do(ctx, http.MethodPost, "/api/v1/calls/"+url.PathEscape(callID)+"/"+action, idempotencyKey, body, nil)
}

func (c *Client) Answer(ctx context.Context, callID, idempotencyKey string) error {
	return c.CallAction(ctx, callID, "answer", idempotencyKey, nil)
}

func (c *Client) Reject(ctx context.Context, callID, idempotencyKey string) error {
	return c.CallAction(ctx, callID, "reject", idempotencyKey, nil)
}

func (c *Client) Hangup(ctx context.Context, callID, idempotencyKey string) error {
	return c.CallAction(ctx, callID, "hangup", idempotencyKey, nil)
}

func (c *Client) DTMF(ctx context.Context, callID, digit, idempotencyKey string) error {
	body, _ := json.Marshal(map[string]string{"digit": digit})
	return c.CallAction(ctx, callID, "dtmf", idempotencyKey, body)
}

func (c *Client) ListMessages(ctx context.Context, after int64, limit int) ([]Message, error) {
	var messages []Message
	path := fmt.Sprintf("/api/v1/messages?after=%d&limit=%d", after, limit)
	err := c.do(ctx, http.MethodGet, path, "", nil, &messages)
	return messages, err
}

func (c *Client) ThreadMessages(ctx context.Context, threadKey string, before int64, beforeID string, limit int) ([]Message, bool, error) {
	query := url.Values{}
	query.Set("threadKey", threadKey)
	if before > 0 {
		query.Set("before", fmt.Sprintf("%d", before))
	}
	if beforeID != "" {
		query.Set("beforeId", beforeID)
	}
	query.Set("limit", fmt.Sprintf("%d", limit))
	payload, headers, err := c.doRaw(ctx, http.MethodGet, "/api/v1/messages?"+query.Encode(), "", nil)
	if err != nil {
		return nil, false, err
	}
	var messages []Message
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &messages); err != nil {
			return nil, false, err
		}
	}
	hasMore := strings.EqualFold(strings.TrimSpace(headers.Get("X-CellBridge-Has-More")), "true")
	return messages, hasMore, nil
}

func (c *Client) Threads(ctx context.Context) ([]Thread, error) {
	var threads []Thread
	err := c.do(ctx, http.MethodGet, "/api/v1/threads", "", nil, &threads)
	return threads, err
}

func (c *Client) SendMessage(ctx context.Context, to, body, idempotencyKey string) (Message, error) {
	payload, _ := json.Marshal(map[string]string{"to": to, "body": body})
	response, _, err := c.doRaw(ctx, http.MethodPost, "/api/v1/messages", idempotencyKey, payload)
	if err != nil {
		// The worker persists a failed message and returns 502 with the
		// message record in the body; surface it as a real failed message
		// rather than a transport error, matching the v1 contract.
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusBadGateway && len(apiErr.Body) > 0 {
			var failed Message
			if json.Unmarshal(apiErr.Body, &failed) == nil && failed.ID != "" {
				return failed, nil
			}
		}
		return Message{}, err
	}
	var message Message
	if len(response) > 0 {
		if err := json.Unmarshal(response, &message); err != nil {
			return Message{}, err
		}
	}
	return message, nil
}

func (c *Client) ReadMessage(ctx context.Context, messageID, idempotencyKey string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/messages/"+url.PathEscape(messageID)+"/read", idempotencyKey, nil, nil)
}

// ---------- internal API ----------

func (c *Client) internalURL() string {
	if c.InternalBaseURL != "" {
		return c.InternalBaseURL
	}
	return c.BaseURL
}

func (c *Client) SMSDryRun(ctx context.Context) (bool, bool, error) {
	token, err := c.InternalToken()
	if err != nil {
		return true, false, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.internalURL()+"/internal/v1/settings/sms-dry-run", nil)
	if err != nil {
		return true, false, err
	}
	request.Header.Set("X-Internal-Token", token)
	response, err := c.HTTP.Do(request)
	if err != nil {
		return true, false, err
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusOK {
		return true, false, &APIError{Status: response.StatusCode, Message: strings.TrimSpace(string(payload))}
	}
	var result struct {
		Enabled   bool `json:"enabled"`
		Supported bool `json:"supported"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		return true, false, err
	}
	return result.Enabled, result.Supported, nil
}

func (c *Client) SetSMSDryRun(ctx context.Context, enabled bool) error {
	token, err := c.InternalToken()
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]bool{"enabled": enabled})
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, c.internalURL()+"/internal/v1/settings/sms-dry-run", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("X-Internal-Token", token)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.HTTP.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return &APIError{Status: response.StatusCode, Message: strings.TrimSpace(string(body))}
	}
	return nil
}

// SIMIdentity is the worker's read-only SIM own-number / subscriber /
// serving-network payload. It is loopback-internal only: raw number/IMSI
// must never be forwarded to public clients or the console.
type SIMIdentity struct {
	Number          string            `json:"number"`
	NumberSource    string            `json:"numberSource"`
	TypeOfAddress   int               `json:"typeOfAddress"`
	ICCID           string            `json:"iccid"`
	IMSI            string            `json:"imsi"`
	OperatorNumeric string            `json:"operatorNumeric"`
	OperatorAlpha   string            `json:"operatorAlpha"`
	Registration    string            `json:"registration"`
	AccessTech      string            `json:"accessTech"`
	CellID          string            `json:"cellId"`
	LAC             string            `json:"lac"`
	TAC             string            `json:"tac"`
	PCI             int               `json:"pci"`
	EARFCN          int               `json:"earfcn"`
	Commands        map[string]string `json:"commands"`
}

func (c *Client) SIMIdentity(ctx context.Context) (SIMIdentity, error) {
	token, err := c.InternalToken()
	if err != nil {
		return SIMIdentity{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.internalURL()+"/internal/v1/identity/sim", nil)
	if err != nil {
		return SIMIdentity{}, err
	}
	request.Header.Set("X-Internal-Token", token)
	response, err := c.HTTP.Do(request)
	if err != nil {
		return SIMIdentity{}, err
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusOK {
		return SIMIdentity{}, &APIError{Status: response.StatusCode, Message: strings.TrimSpace(string(payload))}
	}
	var identity SIMIdentity
	if err := json.Unmarshal(payload, &identity); err != nil {
		return SIMIdentity{}, err
	}
	return identity, nil
}

func (c *Client) Recover(ctx context.Context) error {
	token, err := c.InternalToken()
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.internalURL()+"/internal/v1/recover", nil)
	if err != nil {
		return err
	}
	request.Header.Set("X-Internal-Token", token)
	response, err := c.HTTP.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return &APIError{Status: response.StatusCode, Message: strings.TrimSpace(string(body))}
	}
	return nil
}

// ---------- event stream ----------

// StreamEvents reconnects indefinitely and emits worker events on the
// returned channel until the context is done.
func (c *Client) StreamEvents(ctx context.Context) <-chan Event {
	events := make(chan Event, 128)
	go func() {
		defer close(events)
		backoff := time.Second
		for ctx.Err() == nil {
			if err := c.streamOnce(ctx, events); err != nil && ctx.Err() == nil {
				slog.Warn("worker event stream interrupted", "line", c.LineID, "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
	}()
	return events
}

func (c *Client) streamOnce(ctx context.Context, events chan<- Event) error {
	token, err := c.ensureAccess(ctx, false)
	if err != nil {
		return err
	}
	wsURL, err := toWebSocketURL(c.BaseURL)
	if err != nil {
		return err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	conn, response, err := dialer.DialContext(ctx, wsURL+"/api/v1/events", header)
	if err != nil {
		if response != nil && response.StatusCode == http.StatusUnauthorized {
			_, _ = c.ensureAccess(ctx, true)
		}
		return err
	}
	defer conn.Close()
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var event Event
		if json.Unmarshal(payload, &event) != nil || event.Type == "" {
			continue
		}
		select {
		case events <- event:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func toWebSocketURL(base string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	default:
		return "", fmt.Errorf("unsupported worker scheme %q", parsed.Scheme)
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

// ---------- media ----------

type Media struct {
	conn    *websocket.Conn
	Frames  chan []byte
	ready   chan struct{}
	readyMu sync.Mutex
	err     error

	writeMu sync.Mutex
	closed  bool
}

// AttachMedia opens the worker's internal PCMU socket for an active call.
// Ready is signalled once the worker has verified the cellular leg and
// opened its UAC pair.
func (c *Client) AttachMedia(ctx context.Context, callID string) (*Media, error) {
	token, err := c.InternalToken()
	if err != nil {
		return nil, err
	}
	wsURL, err := toWebSocketURL(c.internalURL())
	if err != nil {
		return nil, err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	header := http.Header{}
	header.Set("X-Internal-Token", token)
	conn, response, err := dialer.DialContext(ctx, wsURL+"/internal/v1/media/"+url.PathEscape(callID), header)
	if err != nil {
		if response != nil {
			body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
			return nil, fmt.Errorf("attach media: %s: %s", response.Status, strings.TrimSpace(string(body)))
		}
		return nil, err
	}
	media := &Media{conn: conn, Frames: make(chan []byte, mediaFrameBuffer), ready: make(chan struct{})}
	go media.readLoop()
	return media, nil
}

// mediaFrameBuffer bounds the worker->daemon audio backlog. Eight frames is
// 160 ms: enough to absorb loopback/scheduler jitter, small enough that a
// stalled consumer can never turn into seconds of stale speech. The mixer
// applies the same latency bound per leg; this is the hard upstream stop.
const mediaFrameBuffer = 8

// offerMediaFrame enqueues one frame, dropping the single oldest frame when
// the bounded buffer is full so latency cannot grow without limit.
func offerMediaFrame(frames chan []byte, payload []byte) {
	frame := append([]byte(nil), payload...)
	select {
	case frames <- frame:
	default:
		// Keep latency bounded: drop the oldest frame.
		select {
		case <-frames:
		default:
		}
		select {
		case frames <- frame:
		default:
		}
	}
}

func (m *Media) readLoop() {
	defer close(m.Frames)
	for {
		messageType, payload, err := m.conn.ReadMessage()
		if err != nil {
			m.finish(err)
			return
		}
		switch messageType {
		case websocket.BinaryMessage:
			if len(payload) != 160 {
				continue
			}
			offerMediaFrame(m.Frames, payload)
		case websocket.TextMessage:
			var control struct {
				Type string `json:"type"`
				Code string `json:"code"`
			}
			if json.Unmarshal(payload, &control) != nil {
				continue
			}
			switch control.Type {
			case "ready":
				m.readyMu.Lock()
				if m.err == nil {
					select {
					case <-m.ready:
					default:
						close(m.ready)
					}
				}
				m.readyMu.Unlock()
			case "error":
				m.finish(fmt.Errorf("worker media error: %s", control.Code))
				return
			}
		}
	}
}

func (m *Media) finish(err error) {
	m.readyMu.Lock()
	if m.err == nil {
		m.err = err
		select {
		case <-m.ready:
		default:
			close(m.ready)
		}
	}
	m.readyMu.Unlock()
	m.writeMu.Lock()
	m.closed = true
	m.writeMu.Unlock()
}

// WaitReady blocks until the worker reports the cellular leg active and the
// UAC pair open, or the context expires.
func (m *Media) WaitReady(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.ready:
		m.readyMu.Lock()
		defer m.readyMu.Unlock()
		return m.err
	}
}

// mediaWriteDeadline bounds one synchronous frame write toward the worker.
// The mixer calls Send from its 20 ms loop; without a deadline a stalled
// loopback socket would freeze the shared mixer (and every call on it) for
// as long as the kernel buffers allow.
const mediaWriteDeadline = 1 * time.Second

func (m *Media) Send(frame []byte) error {
	if len(frame) != 160 {
		return errors.New("PCMU frame must be 160 bytes")
	}
	m.writeMu.Lock()
	if m.closed {
		m.writeMu.Unlock()
		return errors.New("media connection is closed")
	}
	_ = m.conn.SetWriteDeadline(time.Now().Add(mediaWriteDeadline))
	err := m.conn.WriteMessage(websocket.BinaryMessage, frame)
	if err != nil {
		m.closed = true
	}
	m.writeMu.Unlock()
	if err != nil {
		// A timed-out websocket is corrupt by contract; close so the read
		// loop converges and the normal host-lost/teardown path runs.
		_ = m.conn.Close()
		return err
	}
	return nil
}

func (m *Media) Close() {
	m.writeMu.Lock()
	closed := m.closed
	m.closed = true
	m.writeMu.Unlock()
	if !closed {
		_ = m.conn.Close()
	}
}
