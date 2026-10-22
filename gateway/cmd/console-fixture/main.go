// Command console-fixture serves the real operator console against an
// isolated, synthetic store for browser review of key reveal/copy and
// migration flows. It binds 127.0.0.1 only, uses a fresh throwaway gateway
// identity and a disposable data directory, and never reads production
// configuration, credentials, keys or line data.
//
// Usage:
//
//	go run ./cmd/console-fixture -addr 127.0.0.1:8899 \
//	    -data /path/to/throwaway-dir [-reset]
//
// The fixture prints its synthetic login and key ids (never key secrets) at
// startup. Reveal the secrets through the console UI, exactly like production.
package main

import (
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"callrelay/gateway/internal/unified/auth"
	"callrelay/gateway/internal/unified/config"
	"callrelay/gateway/internal/unified/console"
	"callrelay/gateway/internal/unified/identity"
	"callrelay/gateway/internal/unified/runtime"
	"callrelay/gateway/internal/unified/store"

	"golang.org/x/crypto/bcrypt"
)

const (
	fixtureUser     = "fixture-admin"
	fixturePassword = "fixture-review-only" // synthetic, loopback-only, never production
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8899", "loopback listen address")
	dataDir := flag.String("data", "", "throwaway state directory (required)")
	reset := flag.Bool("reset", false, "wipe the fixture directory before starting")
	flag.Parse()

	if *dataDir == "" {
		slog.Error("a throwaway -data directory is required; refusing to touch production paths")
		os.Exit(2)
	}
	if *reset {
		if err := os.RemoveAll(*dataDir); err != nil {
			slog.Error("reset fixture directory", "error", err)
			os.Exit(1)
		}
	}
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		slog.Error("create fixture directory", "error", err)
		os.Exit(1)
	}

	cfg := config.Default()
	cfg.Gateway.ID = "callrelay-console-fixture"
	cfg.Gateway.Name = "CallRelay 控制台测试夹具"
	cfg.Gateway.DataDir = *dataDir
	cfg.Gateway.PublicBaseURL = "http://" + *addr
	cfg.Gateway.ConsoleBcryptFile = filepath.Join(*dataDir, "fixture-bcrypt")
	cfg.Gateway.KeyFile = filepath.Join(*dataDir, "fixture-gateway.key")
	cfg.Gateway.ConsoleSecretFile = filepath.Join(*dataDir, "fixture-console-secrets.key")

	if err := writeCredentialFile(cfg.Gateway.ConsoleBcryptFile); err != nil {
		slog.Error("write synthetic credential", "error", err)
		os.Exit(1)
	}

	ctx := context.Background()
	database, err := store.Open(filepath.Join(*dataDir, "fixture.sqlite"))
	if err != nil {
		slog.Error("open fixture store", "error", err)
		os.Exit(1)
	}
	defer database.Close()
	if err := database.Migrate(ctx); err != nil {
		slog.Error("migrate fixture store", "error", err)
		os.Exit(1)
	}
	secretBox, err := store.LoadOrCreateSecretBox(cfg.ConsoleSecretsPath())
	if err != nil {
		slog.Error("load fixture secret storage", "error", err)
		os.Exit(1)
	}
	database.Secrets = secretBox

	gatewayIdentity, err := identity.LoadOrCreate(cfg.Gateway.KeyFile, cfg.Gateway.ID, cfg.Gateway.Name)
	if err != nil {
		slog.Error("load fixture identity", "error", err)
		os.Exit(1)
	}
	authService := auth.New(database, gatewayIdentity, time.Hour, 24*time.Hour)

	if err := seedOnce(ctx, database, authService); err != nil {
		slog.Error("seed fixture", "error", err)
		os.Exit(1)
	}
	report(ctx, database)

	manager := runtime.NewManager(cfg, database, nil, authService, nil, nil)
	consoleServer := console.NewServer(cfg, database, manager, authService, gatewayIdentity)
	mux := http.NewServeMux()
	mux.Handle("/console", consoleServer.Handler())
	mux.Handle("/console/", consoleServer.Handler())
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/console/login", http.StatusFound)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"fixture-ok"}`))
	})

	fmt.Printf("fixture console: http://%s/console/login\n", *addr)
	fmt.Printf("fixture login:   %s / %s (synthetic, loopback only)\n", fixtureUser, fixturePassword)
	fmt.Println("fixture state:   " + *dataDir + " (delete freely; no production data)")
	server := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("fixture server stopped", "error", err)
		os.Exit(1)
	}
}

// writeCredentialFile writes a bcrypt hash for the fixed synthetic password so
// the review can use the real login form. The password is not a secret and
// works only on the loopback fixture.
func writeCredentialFile(path string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(fixturePassword), bcrypt.MinCost)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(fixtureUser+" "+string(hash)+"\n"), 0o600)
}

// seedOnce creates synthetic lines, a recoverable key, a legacy hash-only key
// and a revoked key the first time it runs; later starts reuse the same
// database so reveal-after-restart can be reviewed.
func seedOnce(ctx context.Context, database *store.Store, authService *auth.Service) error {
	keys, err := database.ListEnrollmentKeys(ctx)
	if err != nil {
		return err
	}
	if len(keys) > 0 {
		return nil
	}
	for _, line := range []store.Line{
		{ID: "line1", Name: "一号测试线路", WorkerURL: "http://127.0.0.1:19781", Enabled: true},
		{ID: "line2", Name: "备用测试线路", WorkerURL: "http://127.0.0.1:19782", Enabled: true},
	} {
		if err := database.EnsureLine(ctx, line); err != nil {
			return err
		}
	}

	recoverable, _, err := authService.CreateKey(ctx, "可恢复密钥（夹具）", true)
	if err != nil {
		return err
	}
	if err := database.SetKeyManageNumber(ctx, recoverable, true); err != nil {
		return err
	}
	if err := database.SetKeyLine(ctx, store.KeyLine{
		KeyID: recoverable, LineID: "line1",
		ReceiveSMS: true, ReceiveCalls: true, SendSMS: true, Dial: true,
	}); err != nil {
		return err
	}
	if err := database.CreateDevice(ctx, store.Device{
		ID: "dev_fixture_phone", KeyID: recoverable, Name: "夹具手机", PublicKey: []byte("fixture-public-key"),
	}); err != nil {
		return err
	}

	// Hash-only legacy key: inserted exactly like a pre-migration row.
	legacySecret := "fixture-legacy-irreversible"
	legacyHash := sha256.Sum256([]byte(legacySecret))
	if _, err := database.DB.ExecContext(ctx, `INSERT INTO enrollment_keys
		(id, name, secret_hash, all_lines, created_at) VALUES ('key_fixture_legacy', '旧版密钥（不可恢复）', ?, 1, ?)`,
		legacyHash[:], time.Now().UTC().UnixMilli()); err != nil {
		return err
	}

	revoked, _, err := authService.CreateKey(ctx, "已撤销密钥（夹具）", false)
	if err != nil {
		return err
	}
	if err := authService.RevokeKey(ctx, revoked); err != nil {
		return err
	}

	_, _ = database.DB.ExecContext(ctx, `INSERT OR IGNORE INTO settings (key, value) VALUES ('voicemail_enabled','0')`)
	return nil
}

// report prints only ids and status; secrets are never written to output.
func report(ctx context.Context, database *store.Store) {
	keys, err := database.ListEnrollmentKeys(ctx)
	if err != nil {
		return
	}
	fmt.Println("fixture keys (ids only; reveal/copy through the console UI):")
	for _, key := range keys {
		state := "active"
		if key.RevokedAt != nil {
			state = "revoked"
		}
		recoverable, _ := database.KeySecretStored(ctx, key.ID)
		recovery := "unrecoverable"
		if recoverable {
			recovery = "recoverable"
		}
		fmt.Printf("  %s\t%s\t%s\t%s\n", key.ID, key.Name, state, recovery)
	}
}
