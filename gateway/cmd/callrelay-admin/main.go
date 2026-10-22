// Command callrelay-admin performs operations that must not depend on the
// console UI: enrollment key lifecycle, device revocation and a real
// end-to-end enrollment self-test against a running callrelayd.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"callrelay/gateway/internal/unified/auth"
	"callrelay/gateway/internal/unified/config"
	"callrelay/gateway/internal/unified/identity"
	"callrelay/gateway/internal/unified/store"
)

func main() {
	configPath := flag.String("config", "/etc/callrelay/unified.yaml", "unified configuration path")
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	cfg := config.Default()
	if *configPath != "" {
		loaded, err := config.Load(*configPath)
		if err == nil {
			cfg = loaded
		}
	}
	switch args[0] {
	case "key":
		runKey(cfg, args[1:])
	case "device":
		runDevice(cfg, args[1:])
	case "selftest":
		runSelfTest(args[1:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: callrelay-admin [-config path] <command>
  key create -name NAME [-all-lines=false]
  key list
  key revoke -id key_x
  device list
  device revoke -id dev_x
  selftest -base https://host -key key_x.secret`)
}

func openServices(cfg config.Config) (*store.Store, *auth.Service) {
	keyFile := cfg.Gateway.KeyFile
	if keyFile == "" {
		keyFile = filepath.Join(cfg.Gateway.DataDir, "gateway-ed25519.key")
	}
	gatewayIdentity, err := identity.LoadOrCreate(keyFile, cfg.Gateway.ID, cfg.Gateway.Name)
	if err != nil {
		fatal(err)
	}
	database, err := store.Open(filepath.Join(cfg.Gateway.DataDir, "unified.sqlite"))
	if err != nil {
		fatal(err)
	}
	if err := database.Migrate(context.Background()); err != nil {
		fatal(err)
	}
	// Keep CLI-created keys recoverable through the same root-only master key
	// the console uses, so `key create` never silently produces hash-only keys.
	if secretBox, err := store.LoadOrCreateSecretBox(cfg.ConsoleSecretsPath()); err == nil {
		database.Secrets = secretBox
	} else {
		fatal(err)
	}
	accessTTL, _ := cfg.AccessTokenDuration()
	refreshTTL, _ := cfg.RefreshTokenDuration()
	return database, auth.New(database, gatewayIdentity, accessTTL, refreshTTL)
}

func runKey(cfg config.Config, args []string) {
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	database, authService := openServices(cfg)
	defer database.Close()
	switch args[0] {
	case "create":
		flags := flag.NewFlagSet("key create", flag.ExitOnError)
		name := flags.String("name", "cli-key", "key name")
		allLines := flags.Bool("all-lines", true, "grant every current and future line")
		_ = flags.Parse(args[1:])
		keyID, secret, err := authService.CreateKey(context.Background(), *name, *allLines)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("key_id=%s\nkey=%s.%s\n", keyID, keyID, secret)
	case "list":
		keys, err := database.ListEnrollmentKeys(context.Background())
		if err != nil {
			fatal(err)
		}
		for _, key := range keys {
			status := "active"
			if key.RevokedAt != nil {
				status = "revoked"
			}
			fmt.Printf("%s\t%s\t%s\t%s\n", key.ID, key.Name, status, key.CreatedAt.Format(time.RFC3339))
		}
	case "revoke":
		flags := flag.NewFlagSet("key revoke", flag.ExitOnError)
		id := flags.String("id", "", "key id")
		_ = flags.Parse(args[1:])
		if *id == "" {
			fatal(fmt.Errorf("-id is required"))
		}
		if err := authService.RevokeKey(context.Background(), *id); err != nil {
			fatal(err)
		}
		fmt.Println("revoked")
	default:
		usage()
		os.Exit(2)
	}
}

func runDevice(cfg config.Config, args []string) {
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	database, authService := openServices(cfg)
	defer database.Close()
	switch args[0] {
	case "list":
		devices, err := database.ListDevices(context.Background())
		if err != nil {
			fatal(err)
		}
		for _, device := range devices {
			status := "active"
			if device.RevokedAt != nil {
				status = "revoked"
			}
			fmt.Printf("%s\t%s\t%s\t%s\n", device.ID, device.Name, status, device.CreatedAt.Format(time.RFC3339))
		}
	case "revoke":
		flags := flag.NewFlagSet("device revoke", flag.ExitOnError)
		id := flags.String("id", "", "device id")
		_ = flags.Parse(args[1:])
		if *id == "" {
			fatal(fmt.Errorf("-id is required"))
		}
		if err := authService.RevokeDevice(context.Background(), *id); err != nil {
			fatal(err)
		}
		fmt.Println("revoked")
	default:
		usage()
		os.Exit(2)
	}
}

// runSelfTest enrolls a fresh ephemeral installation, verifies the public
// API through the real HTTPS entry point and prints the device id so the
// caller can revoke it immediately.
func runSelfTest(args []string) {
	flags := flag.NewFlagSet("selftest", flag.ExitOnError)
	base := flags.String("base", "https://gateway.example.com", "public base URL")
	key := flags.String("key", "", "enrollment key id.secret")
	_ = flags.Parse(args)
	if *key == "" {
		fatal(fmt.Errorf("-key is required"))
	}
	baseURL := strings.TrimRight(*base, "/")
	client := &http.Client{Timeout: 15 * time.Second}

	status, payload := doJSON(client, http.MethodGet, baseURL+"/api/v2/identity", "", "", nil)
	if status != http.StatusOK {
		fatal(fmt.Errorf("identity status %d: %s", status, payload))
	}
	var identityResponse struct {
		GatewayID   string `json:"gatewayId"`
		Fingerprint string `json:"fingerprint"`
	}
	_ = json.Unmarshal(payload, &identityResponse)

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fatal(err)
	}
	keyID, secret, ok := strings.Cut(*key, ".")
	if !ok {
		fatal(fmt.Errorf("enrollment key must be id.secret"))
	}
	name := "callrelay-selftest"
	message := keyID + "\n" + secret + "\n" + identityResponse.GatewayID + "\n" + name
	proof := ed25519.Sign(privateKey, []byte(message))
	enrollBody, _ := json.Marshal(map[string]string{
		"enrollmentKey": *key, "deviceName": name,
		"devicePublicKey": base64.StdEncoding.EncodeToString(publicKey),
		"proof":           base64.StdEncoding.EncodeToString(proof),
	})
	status, payload = doJSON(client, http.MethodPost, baseURL+"/api/v2/enroll", "", "", enrollBody)
	if status != http.StatusOK {
		fatal(fmt.Errorf("enroll status %d: %s", status, payload))
	}
	var pair struct {
		DeviceID    string `json:"deviceId"`
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(payload, &pair); err != nil {
		fatal(err)
	}
	status, payload = doJSON(client, http.MethodGet, baseURL+"/api/v2/lines", pair.AccessToken, "", nil)
	if status != http.StatusOK {
		fatal(fmt.Errorf("lines status %d: %s", status, payload))
	}
	var lines []map[string]any
	_ = json.Unmarshal(payload, &lines)
	lineIDs := make([]string, 0, len(lines))
	for _, line := range lines {
		if id, _ := line["id"].(string); id != "" {
			lineIDs = append(lineIDs, id)
		}
	}
	status, payload = doJSON(client, http.MethodGet, baseURL+"/api/v2/threads", pair.AccessToken, "", nil)
	if status != http.StatusOK {
		fatal(fmt.Errorf("threads status %d: %s", status, payload))
	}
	var threads []map[string]any
	_ = json.Unmarshal(payload, &threads)
	fmt.Printf("gateway_id=%s\nfingerprint=%s\ndevice_id=%s\nlines=%s\nthreads=%d\n",
		identityResponse.GatewayID, identityResponse.Fingerprint, pair.DeviceID, strings.Join(lineIDs, ","), len(threads))
}

func doJSON(client *http.Client, method, url, token, idempotencyKey string, body []byte) (int, []byte) {
	request, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		fatal(err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return response.StatusCode, payload
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
