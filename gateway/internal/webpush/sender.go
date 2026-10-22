package webpush

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	lib "github.com/SherClockHolmes/webpush-go"
)

// Subscription is the stored browser push subscription. Auth and P256dh are
// secrets: they are write-only from every API view and are read solely by the
// outbound notifier.
type Subscription struct {
	ID       string
	Endpoint string
	Auth     string
	P256dh   string
}

// Sender dispatches Web Push messages with this gateway's VAPID identity.
// The transport is the hardened public-only client; sending re-validates the
// endpoint so a subscription stored under an older policy can never be used
// to reach an internal address.
type Sender struct {
	Subject    string
	PublicKey  string
	PrivateKey string
	TTL        time.Duration
	Timeout    time.Duration
	// Policy bounds acceptable endpoint destinations (default: deny
	// special-use ranges; allowlist mode for vendor-restricted operators).
	Policy Policy

	// Post performs one encrypted POST. Nil uses lib.SendNotificationWithContext
	// with the hardened client; tests inject a recorder here. The injected
	// function receives the endpoint only AFTER validation.
	Post func(ctx context.Context, endpoint string, message []byte) (statusCode int, err error)
}

func (s *Sender) ttl() int {
	if s.TTL <= 0 {
		return 30
	}
	return int(s.TTL.Seconds())
}

// Send encrypts and posts one payload. Errors are sanitized: they never
// contain the endpoint, the keys or the payload.
func (s *Sender) Send(ctx context.Context, sub Subscription, payload []byte) error {
	if s == nil || s.PrivateKey == "" || s.PublicKey == "" {
		return errors.New("webpush: sender is not configured")
	}
	if err := s.Policy.ResolveEndpointHost(ctx, sub.Endpoint); err != nil {
		return err
	}
	if s.Post != nil {
		code, err := s.Post(ctx, sub.Endpoint, payload)
		if err != nil {
			return errors.New("webpush: delivery failed")
		}
		if code < 200 || code >= 300 {
			return &StatusError{Code: code}
		}
		return nil
	}
	resp, err := lib.SendNotificationWithContext(ctx, payload, &lib.Subscription{
		Endpoint: sub.Endpoint,
		Keys:     lib.Keys{Auth: sub.Auth, P256dh: sub.P256dh},
	}, &lib.Options{
		HTTPClient:      hardenedClient(s.Policy, s.Timeout),
		Subscriber:      s.Subject,
		TTL:             s.ttl(),
		Topic:           "callrelay-call",
		VAPIDPublicKey:  s.PublicKey,
		VAPIDPrivateKey: s.PrivateKey,
	})
	if err != nil {
		return errors.New("webpush: delivery failed")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 404/410 mean the subscription expired at the push service; the
		// caller treats those as revoke signals via SubscriptionGone.
		return &StatusError{Code: resp.StatusCode}
	}
	return nil
}

// StatusError is a sanitized delivery failure carrying only the HTTP status.
type StatusError struct{ Code int }

func (e *StatusError) Error() string {
	return fmt.Sprintf("webpush: endpoint rejected delivery (HTTP %d)", e.Code)
}

// SubscriptionGone reports whether a send failure means the push service
// dropped the subscription (410 Gone or 404): the row must be removed.
func SubscriptionGone(err error) bool {
	var statusErr *StatusError
	if errors.As(err, &statusErr) {
		return statusErr.Code == http.StatusGone || statusErr.Code == http.StatusNotFound
	}
	return false
}
