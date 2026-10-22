// Command callrelayd is the unified CallRelay gateway: one process owns the
// line registry, v2 API, console, call/conference arbitration and the media
// mixer, delegating modem/UAC ownership to one forked worker per line.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"callrelay/gateway/internal/push"
	"callrelay/gateway/internal/unified/api"
	"callrelay/gateway/internal/unified/auth"
	"callrelay/gateway/internal/unified/config"
	"callrelay/gateway/internal/unified/console"
	"callrelay/gateway/internal/unified/contacts"
	"callrelay/gateway/internal/unified/events"
	"callrelay/gateway/internal/unified/identity"
	"callrelay/gateway/internal/unified/media"
	"callrelay/gateway/internal/unified/notify"
	"callrelay/gateway/internal/unified/quic"
	"callrelay/gateway/internal/unified/recording"
	"callrelay/gateway/internal/unified/runtime"
	"callrelay/gateway/internal/unified/store"
	"callrelay/gateway/internal/webpush"
	"callrelay/gateway/internal/webui"
)

func main() {
	configPath := flag.String("config", "/etc/callrelay/unified.yaml", "unified YAML configuration path")
	flag.Parse()

	cfg := config.Default()
	if *configPath != "" {
		loaded, err := config.Load(*configPath)
		if err != nil {
			slog.Error("load configuration", "path", *configPath, "error", err)
			os.Exit(1)
		}
		cfg = loaded
	}
	if cfg.Gateway.Version == "dev" {
		cfg.Gateway.Version = "0.2.0"
	}
	if err := cfg.Validate(); err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(cfg.Gateway.DataDir, 0o750); err != nil {
		slog.Error("create data directory", "error", err)
		os.Exit(1)
	}
	keyFile := cfg.Gateway.KeyFile
	if keyFile == "" {
		keyFile = filepath.Join(cfg.Gateway.DataDir, "gateway-ed25519.key")
	}
	gatewayIdentity, err := identity.LoadOrCreate(keyFile, cfg.Gateway.ID, cfg.Gateway.Name)
	if err != nil {
		slog.Error("load gateway identity", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	database, err := store.Open(filepath.Join(cfg.Gateway.DataDir, "unified.sqlite"))
	if err != nil {
		slog.Error("open unified database", "error", err)
		os.Exit(1)
	}
	defer database.Close()
	if err := database.Migrate(ctx); err != nil {
		slog.Error("migrate unified database", "error", err)
		os.Exit(1)
	}
	// The master key for recoverable enrollment secrets lives outside the
	// database in a root-only file. Failing hard is deliberate: silently
	// creating hash-only keys would make the console's reveal feature lie.
	secretBox, err := store.LoadOrCreateSecretBox(cfg.ConsoleSecretsPath())
	if err != nil {
		slog.Error("load console secret storage", "error", err)
		os.Exit(1)
	}
	database.Secrets = secretBox
	slog.Info("console secret storage ready", "path", cfg.ConsoleSecretsPath())
	for _, line := range cfg.Lines {
		if err := database.EnsureLine(ctx, store.Line{
			ID: line.ID, Name: line.Name, WorkerURL: line.WorkerURL,
			TokenFile: line.TokenFile, IdentityFile: line.IdentityFile, Enabled: line.Enabled,
		}); err != nil {
			slog.Error("register line", "line", line.ID, "error", err)
			os.Exit(1)
		}
	}
	seedSetting(ctx, database, "max_concurrent_calls", strconv.Itoa(cfg.Settings.MaxConcurrentCalls))
	seedSetting(ctx, database, "disconnect_grace_seconds", strconv.Itoa(cfg.Settings.DisconnectGraceSeconds))

	accessTTL, _ := cfg.AccessTokenDuration()
	refreshTTL, _ := cfg.RefreshTokenDuration()
	authService := auth.New(database, gatewayIdentity, accessTTL, refreshTTL)
	bus := events.NewBus(database)
	engine, err := media.NewEngine()
	if err != nil {
		slog.Error("initialize media engine", "error", err)
		os.Exit(1)
	}
	var pushSender push.Sender
	switch {
	case strings.TrimSpace(cfg.Push.BrokerURL) != "" &&
		!strings.Contains(cfg.Push.BrokerURL, "example.com"):
		brokerURL := strings.TrimSpace(cfg.Push.BrokerURL)
		pushSender = push.NewBroker(brokerURL, os.Getenv(cfg.Push.AuthorizationEnv))
		slog.Info("push broker configured", "endpoint", brokerURL)
	case strings.TrimSpace(cfg.Push.APNsKeyFile) != "":
		// Optional direct APNs sender. It only activates when every field is
		// present and the key file loads; otherwise push stays disabled with a
		// privacy-safe reason (never the key or a device token).
		direct, apnsErr := push.NewAPNsSender(push.APNsConfig{
			KeyFile:     cfg.Push.APNsKeyFile,
			KeyID:       cfg.Push.APNsKeyID,
			TeamID:      cfg.Push.APNsTeamID,
			BundleID:    cfg.Push.APNsBundleID,
			Environment: cfg.Push.APNsEnvironment,
		})
		if apnsErr != nil {
			slog.Warn("push unavailable", "error_code", "BLOCKED_EXTERNAL_APNS", "reason", apnsErr.Error())
		} else {
			pushSender = direct
			slog.Info("direct APNs push configured", "environment", cfg.Push.APNsEnvironment)
		}
	default:
		slog.Warn("push unavailable", "error_code", "BLOCKED_EXTERNAL_APNS", "reason", "no APNs broker or key configured; realtime events remain available")
	}
	manager := runtime.NewManager(cfg, database, bus, authService, engine, pushSender)
	// Manual call recording: SD-backed blobs, eMMC index, canonical mixer
	// taps. Wiring before Start so every media session gets its taps; a
	// storage failure only disables recording and never affects telephony.
	recordingManager, recordingErr := recording.NewManager(database, recording.Options{
		Root:             cfg.Recording.AudioRoot,
		MountPoint:       cfg.Recording.MountPoint,
		ExpectedDevice:   cfg.Recording.ExpectedDevice,
		ExpectedFSType:   cfg.Recording.ExpectedFSType,
		RetentionDays:    cfg.Recording.RetentionDays,
		MinimumFreeBytes: cfg.Recording.MinimumFreeBytes,
		MaxBytes:         cfg.Recording.MaxBytes,
	}, nil)
	if recordingErr != nil {
		slog.Error("initialize call recording", "error", recordingErr)
		os.Exit(1)
	}
	manager.SetRecordings(recordingManager)
	if recoverErr := recordingManager.Recover(ctx); recoverErr != nil {
		slog.Warn("call recording recovery incomplete", "error", recoverErr)
	}
	go recordingManager.RunRetention(ctx)
	slog.Info("manual call recording configured", "root", cfg.Recording.AudioRoot,
		"mount", cfg.Recording.MountPoint, "retention_days", cfg.Recording.RetentionDays)
	var barkClient *push.BarkClient
	if cfg.Bark.Enabled {
		barkTimeout, barkErr := cfg.BarkTimeout()
		if barkErr != nil {
			slog.Error("invalid bark.timeout", "error", barkErr)
			os.Exit(1)
		}
		barkClient = push.NewBarkClient(cfg.Bark.AllowPrivate, barkTimeout)
		manager.Bark = barkClient
		// No secret or device data: enabled state and the LAN policy only.
		slog.Info("optional bark notification bridge enabled", "allow_private", cfg.Bark.AllowPrivate)
	}
	if err := manager.Start(ctx); err != nil {
		slog.Error("start unified runtime", "error", err)
		os.Exit(1)
	}
	defer manager.Close()

	// Self-hosted PWA Web Push: per-gateway VAPID identity generated at
	// runtime with private permissions; the gateway sends Web Push itself.
	var webPushManager *webpush.Manager
	var webPushSender *webpush.Sender
	vapidPublicKey := ""
	if cfg.WebPush.Enabled {
		keys, keyErr := webpush.LoadOrCreateKeys(cfg.WebPushPrivateKeyPath())
		if keyErr != nil {
			slog.Error("webpush vapid key unavailable", "error", keyErr)
			os.Exit(1)
		}
		webPushManager = webpush.NewManager(database)
		webPushManager.BindTTL, _ = cfg.WebPushBindTokenTTL()
		webPushManager.SessionTTL, _ = cfg.WebPushSessionTTL()
		webPushManager.HandoffTTL, _ = cfg.WebPushHandoffTTL()
		pushTTL, _ := cfg.WebPushTTL()
		webPushSender = &webpush.Sender{
			Subject:    cfg.WebPushSubject(),
			PublicKey:  keys.PublicKey,
			PrivateKey: keys.PrivateKey,
			TTL:        pushTTL,
			Timeout:    10 * time.Second,
			Policy:     webpush.Policy{Allowlist: cfg.WebPush.AllowedEndpointHosts},
		}
		manager.WebPush = webPushManager
		manager.WebPushSender = webPushSender
		vapidPublicKey = keys.PublicKey
		slog.Info("self-hosted web push enabled", "key_file", cfg.WebPushPrivateKeyPath())
	}

	// Principal-scoped contacts and ordinary service alerts. The contacts
	// service is server-side scoped by the authenticated pairing key; the
	// notifier reuses the existing WAN agent for bounded probes and sends
	// standard alerts only.
	contactsService := contacts.NewService(database)
	notifyMonitor := notify.New(database, pushSender, notify.Config{
		Enabled:                 cfg.Notify.Enabled,
		GatewayID:               cfg.Gateway.ID,
		WanLineID:               cfg.Notify.WanLineID,
		WanStateFile:            cfg.Notify.WanStateFile,
		WanProbeCommand:         cfg.Notify.WanProbeCommand,
		WanProbeInterval:        mustDuration(cfg.NotifyProbeInterval()),
		WanFailureProbeInterval: mustDuration(cfg.NotifyFailureProbeInterval()),
		WanProbeTimeout:         mustDuration(cfg.NotifyProbeTimeout()),
		SustainedFailures:       cfg.Notify.SustainedFailures,
		RecoverySuccesses:       cfg.Notify.RecoverySuccesses,
		CarrierSenders:          cfg.Notify.CarrierSenders,
	})
	notifyMonitor.WebPush = webPushManager
	notifyMonitor.WebPushSender = webPushSender
	manager.Notify = notifyMonitor
	notifyMonitor.Start(ctx)

	// QUIC DATAGRAM direct media (build 61): private UDP listener sharing
	// its socket with STUN candidate discovery. Any setup failure degrades
	// to capability-absent — ice+ws service is never affected. The pin
	// certificate lives in the data dir (0600) and is signed by the
	// gateway identity; nothing secret is served or logged.
	var quicServer *quic.Server
	if cfg.QUIC.Enabled {
		stunServers := cfg.QUIC.STUNServers
		if len(stunServers) == 0 {
			stunServers = cfg.ICE.STUNServers
		}
		server, quicErr := quic.New(quic.Config{
			Listen:      cfg.QUIC.Listen,
			STUNServers: stunServers,
			LANPunch:    cfg.QUIC.LANPunch,
			DataDir:     filepath.Join(cfg.Gateway.DataDir, "quic"),
		}, gatewayIdentity, authService, manager.ServeQUICMedia)
		if quicErr != nil {
			slog.Error("quic setup failed", "error", quicErr)
			os.Exit(1)
		}
		if startErr := server.Start(); startErr != nil {
			slog.Error("quic start failed", "error", startErr)
			os.Exit(1)
		}
		quicServer = server
		defer server.Close()
		manager.SetQUICCapabilityProvider(func() (map[string]any, bool) {
			if !server.Live() {
				return nil, false
			}
			report := server.Info()
			return map[string]any{
				"alpn":        report.ALPN,
				"pin":         report.Pin,
				"candidates":  report.Candidates,
				"stun":        report.STUNServers,
			}, true
		})
		slog.Info("quic direct media enabled", "listen", cfg.QUIC.Listen, "lan_punch", cfg.QUIC.LANPunch)
	} else {
		slog.Info("quic direct media disabled by configuration")
	}

	apiServer := api.NewServer(cfg, authService, database, bus, manager, gatewayIdentity)
	apiServer.QUIC = quicServer
	apiServer.Bark = barkClient
	apiServer.WebPush = webPushManager
	apiServer.WebPushSender = webPushSender
	apiServer.VAPIDPublicKey = vapidPublicKey
	apiServer.WebPushPolicy = webpush.Policy{Allowlist: cfg.WebPush.AllowedEndpointHosts}
	apiServer.Contacts = contactsService
	apiServer.Notify = notifyMonitor
	consoleServer := console.NewServer(cfg, database, manager, authService, gatewayIdentity)
	mux := http.NewServeMux()
	// The JSON admin API is an additive, more specific prefix under the same
	// /api/v2 origin: it reuses the console session/CSRF store and the exact
	// console business logic, so the native management screens and the web
	// console can never disagree. Existing device routes are untouched.
	mux.Handle("/api/v2/admin/", consoleServer.AdminAPIHandler())
	mux.Handle("/api/v2/", apiServer.Handler())
	mux.Handle("/console", consoleServer.Handler())
	mux.Handle("/console/", consoleServer.Handler())
	mux.Handle("/pwa", webui.PWAHandler())
	mux.Handle("/pwa/", webui.PWAHandler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	server := &http.Server{
		Addr:              cfg.Gateway.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	slog.Info("callrelayd listening", "address", cfg.Gateway.Listen, "gateway_id", cfg.Gateway.ID,
		"lines", len(cfg.Lines), "console", strings.TrimSpace(cfg.Gateway.ConsoleBcryptFile) != "")
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("callrelayd stopped", "error", err)
		os.Exit(1)
	}
}

// mustDuration keeps the (duration, error) config helpers readable at the
// single call site; Validate already rejected malformed values at startup.
func mustDuration(value time.Duration, err error) time.Duration {
	if err != nil {
		return 0
	}
	return value
}

func seedSetting(ctx context.Context, database *store.Store, key, value string) {
	if _, err := database.Setting(ctx, key); err == nil {
		return
	}
	if err := database.SetSetting(ctx, key, value); err != nil {
		slog.Warn("seed setting failed", "key", key, "error", err)
	}
}
