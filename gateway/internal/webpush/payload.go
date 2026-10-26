package webpush

import "encoding/json"

// Declarative Web Push (WebKit, iOS 18.4+): the message body itself is the
// notification description. WebKit displays it without running any service
// worker JavaScript and navigates the web app to `navigate` on activation —
// robust even after ITP evicts the service worker. Older browsers (iOS
// 16.4–18.3) hand the identical bytes to the service worker's push event,
// and sw.js parses this same shape, so one payload serves both paths.
//
// Reference: https://webkit.org/blog/16535/meet-declarative-web-push/

// Notification is the declarative notification dictionary (NotificationOptions
// subset). Tag collapses re-sends for the same call; Renotify re-alerts when a
// newer call replaces a pending one with the same tag.
type Notification struct {
	Title    string `json:"title"`
	Body     string `json:"body"`
	Lang     string `json:"lang,omitempty"`
	Dir      string `json:"dir,omitempty"`
	Tag      string `json:"tag,omitempty"`
	Navigate string `json:"navigate"`
	Renotify bool   `json:"renotify"`
	Silent   bool   `json:"silent"`
}

// Message is the declarative push envelope. WebKit keys on the literal
// "web_push": 8030 (an homage to RFC 8030).
type Message struct {
	WebPush      int          `json:"web_push"`
	Notification Notification `json:"notification"`
}

// CallPayload builds the encrypted-body bytes for one incoming-call ring.
// It carries only the caller handle and a short-lived, single-use handoff
// navigation URL — never call tokens, credentials or message content.
func CallPayload(locale, peer, navigateURL, tag string) ([]byte, error) {
	language := "zh-CN"
	direction := "auto"
	if len(locale) >= 2 && locale[:2] == "en" {
		language = "en-US"
	}
	body := peer + " 正在呼叫"
	title := "CallRelay 来电"
	if language == "en-US" {
		title = "CallRelay incoming call"
		body = "Calling from " + peer
	}
	return json.Marshal(Message{
		WebPush: 8030,
		Notification: Notification{
			Title:    title,
			Body:     body,
			Lang:     language,
			Dir:      direction,
			Tag:      tag,
			Navigate: navigateURL,
			Renotify: true,
			Silent:   false,
		},
	})
}

// SmsPayload builds the encrypted-body bytes for one inbound SMS alert. The
// peer and preview are already bounded by the runtime (push.TruncatePreview)
// before they reach this function, so the final alert stays within the
// notification budget. The tag is the message id: a replayed worker event
// collapses onto the same notification instead of stacking a duplicate.
func SmsPayload(locale, peer, preview, navigateURL, tag string) ([]byte, error) {
	language := "zh-CN"
	title := "CallRelay 短信"
	separator := "："
	if len(locale) >= 2 && locale[:2] == "en" {
		language = "en-US"
		title = "CallRelay SMS"
		separator = ": "
	}
	body := peer
	switch {
	case preview == "":
	case body == "":
		body = preview
	default:
		body = peer + separator + preview
	}
	return json.Marshal(Message{
		WebPush: 8030,
		Notification: Notification{
			Title:    title,
			Body:     body,
			Lang:     language,
			Dir:      "auto",
			Tag:      tag,
			Navigate: navigateURL,
			Renotify: true,
			Silent:   false,
		},
	})
}

// TestPayload builds the settings-screen test notification body. The tap
// target is the gateway's own setup page (same origin) so activating the
// test notification always lands somewhere meaningful.
func TestPayload(locale, navigateURL string) ([]byte, error) {
	language := "zh-CN"
	body := "网页推送配置成功（测试）"
	title := "CallRelay 测试通知"
	if len(locale) >= 2 && locale[:2] == "en" {
		language = "en-US"
		title = "CallRelay test notification"
		body = "Web push configured successfully (test)"
	}
	return json.Marshal(Message{
		WebPush: 8030,
		Notification: Notification{
			Title:    title,
			Body:     body,
			Lang:     language,
			Dir:      "auto",
			Tag:      "callrelay-test",
			Navigate: navigateURL,
			Silent:   false,
		},
	})
}
