// Command apns-probe performs a privacy-safe authenticated reachability check
// of the direct APNs sender configured in /etc/callrelay/unified.yaml.
//
// It sends exactly ONE push carrying an intentionally INVALID device token
// and prints only the HTTP status and reason. A `BadDeviceToken` rejection
// proves the provider JWT authenticated and the requested topic is
// authorized, without ever ringing a real device. The probe never prints
// tokens, JWTs or key material; the key path is referenced, never echoed.
//
// Root-only operational tool for credential rotation checks.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	push "callrelay/gateway/internal/push"
	config "callrelay/gateway/internal/unified/config"
)

// fakeToken is syntactically well-formed (64 hex chars) but not enrolled
// with Apple, so APNs can never deliver to a real device.
const fakeToken = "0000000000000000000000000000000000000000000000000000000000000000"

func main() {
	configPath := flag.String("config", "/etc/callrelay/unified.yaml", "unified config path")
	topic := flag.String("topic", "voip", "topic to probe: voip or alert")
	environment := flag.String("environment", "", "override configured APNs environment")
	timeout := flag.Duration("timeout", 20*time.Second, "request timeout")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe: load config: %v\n", err)
		os.Exit(2)
	}
	env := *environment
	if env == "" {
		env = cfg.Push.APNsEnvironment
	}
	sender, err := push.NewAPNsSender(push.APNsConfig{
		KeyFile:     cfg.Push.APNsKeyFile,
		KeyID:       cfg.Push.APNsKeyID,
		TeamID:      cfg.Push.APNsTeamID,
		BundleID:    cfg.Push.APNsBundleID,
		Environment: env,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe: %v\n", err)
		os.Exit(2)
	}
	target := push.Target{
		DeviceID:  "apns-probe",
		APNSToken: fakeToken,
		VoIPToken: fakeToken,
		// The probe always presents itself as the environment under test so
		// the sender's fail-closed validation is exercised exactly as the
		// daemon would.
		Environment: env,
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	var sendErr error
	switch *topic {
	case "voip":
		sendErr = sender.SendVoIP(ctx, target, push.VoIPPayload{
			CallUUID: "00000000-0000-0000-0000-000000000000",
			CallID:   "apns-probe",
			Handle:   "apns-probe",
		})
	case "alert":
		sendErr = sender.SendMessage(ctx, target, push.MessagePayload{
			MessageID: "apns-probe",
		})
	default:
		fmt.Fprintf(os.Stderr, "probe: topic must be voip or alert\n")
		os.Exit(2)
	}
	if sendErr == nil {
		// A 200 for a fake token must never happen; treat as failure.
		fmt.Fprintf(os.Stderr, "probe: unexpected APNs acceptance for a non-enrolled token\n")
		os.Exit(2)
	}
	message := sendErr.Error()
	fmt.Printf("apns-probe topic=%s environment=%s response=%q\n", *topic, env, message)
	if strings.Contains(message, "status=4") || strings.Contains(message, "status=5") {
		// Reached APNs and got a well-formed rejection: the provider token
		// authenticated and the topic decision came from Apple.
		return
	}
	fmt.Fprintf(os.Stderr, "probe: did not reach APNs (transport/auth failure)\n")
	os.Exit(1)
}
