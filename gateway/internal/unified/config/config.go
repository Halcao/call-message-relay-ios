// Package config defines the on-disk configuration for the unified CallRelay
// service (callrelayd). One process owns all physical lines, the console,
// the v2 API and the media mixer; each line points at a forked CellBridge
// worker on loopback that keeps exclusive ownership of its modem and UAC.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Line struct {
	ID           string `yaml:"id"`
	Name         string `yaml:"name"`
	WorkerURL    string `yaml:"worker_url"`
	InternalURL  string `yaml:"internal_url"`
	TokenFile    string `yaml:"internal_token_file"`
	IdentityFile string `yaml:"identity_file"`
	Enabled      bool   `yaml:"enabled"`
}

type TURN struct {
	Enabled       bool   `yaml:"enabled"`
	Host          string `yaml:"host"`
	Port          int    `yaml:"port"`
	BindIP        string `yaml:"bind_ip"`
	SecretEnv     string `yaml:"secret_env"`
	CredentialTTL string `yaml:"credential_ttl"`
}

// ICE configures peer-to-peer address discovery INDEPENDENTLY of TURN relay.
// Public STUN servers let both the gateway and the client discover their
// server-reflexive (public) addresses and attempt direct UDP connectivity —
// no relay purchase and no new self-hosted service. LAN host candidates and
// the HTTPS/WSS fallback are always retained regardless of this section.
type ICE struct {
	// STUNServers lists STUN endpoints as bare host:port (NO scheme and no
	// credentials), e.g. "stun.cloudflare.com:3478". Empty keeps host-only
	// ICE (the previous behaviour, in which server-reflexive candidates
	// cannot be discovered).
	STUNServers []string `yaml:"stun_servers"`
}

// Voicemail configures disposable voicemail audio storage. Audio blobs live
// on the SD card only; the index and every other durable artifact stay on
// the eMMC. The mount is verified before each recording and there is no
// silent fallback when the card is absent.
type Voicemail struct {
	AudioRoot      string `yaml:"audio_root"`
	MountPoint     string `yaml:"mount_point"`
	ExpectedDevice string `yaml:"expected_device"`
	ExpectedFSType string `yaml:"expected_fstype"`
}

// Recording configures manual call-recording storage. Like voicemail, audio
// blobs live on the verified SD card only and the mount is checked before any
// recording starts; a missing or full card reports failure truthfully and
// never touches the call. Recording is manual-only: there is no automatic
// mode and no in-band announcement, because the capture taps the gateway's
// canonical mixer audio and is never mixed back into the call path.
type Recording struct {
	AudioRoot      string `yaml:"audio_root"`
	MountPoint     string `yaml:"mount_point"`
	ExpectedDevice string `yaml:"expected_device"`
	ExpectedFSType string `yaml:"expected_fstype"`
	// RetentionDays > 0 deletes READY recordings after N days; 0 keeps them
	// until the owner deletes them.
	RetentionDays int `yaml:"retention_days"`
	// MinimumFreeBytes blocks starting a new recording below this free
	// space. Existing recordings are never touched by the threshold.
	MinimumFreeBytes int64 `yaml:"minimum_free_bytes"`
	// MaxBytes > 0 caps the total recording footprint; reaching it blocks
	// new recordings.
	MaxBytes int64 `yaml:"max_bytes"`
}

type Push struct {
	BrokerURL        string `yaml:"broker_url"`
	AuthorizationEnv string `yaml:"authorization_env"`
	// Optional direct APNs sender. Disabled unless KeyFile, KeyID, TeamID and
	// BundleID are all present; the key file is read at startup and never
	// logged or returned by any endpoint.
	APNsKeyFile     string `yaml:"apns_key_file"`
	APNsKeyID       string `yaml:"apns_key_id"`
	APNsTeamID      string `yaml:"apns_team_id"`
	APNsBundleID    string `yaml:"apns_bundle_id"`
	APNsEnvironment string `yaml:"apns_environment"`
}

// Bark configures the OPTIONAL self-hosted Bark notification path. It is
// disabled by default: native APNs/PushKit ringing never depends on it. A
// device must additionally opt in and store its own server URL + device key
// through the authenticated settings API.
//
// AllowPrivate is the operator's deliberate LAN switch: when false, Bark
// servers must resolve to public addresses only; when true, a device that
// also opts in may point at a private/loopback self-hosted server.
type Bark struct {
	Enabled      bool   `yaml:"enabled"`
	AllowPrivate bool   `yaml:"allow_private"`
	Timeout      string `yaml:"timeout"`
	// MaxCallAge bounds how old a ringing call may be before a Bark
	// notification is considered stale and suppressed. Zero uses 60s.
	MaxCallAge string `yaml:"max_call_age"`
}

type Settings struct {
	MaxConcurrentCalls        int    `yaml:"max_concurrent_calls"`
	DisconnectGraceSeconds    int    `yaml:"disconnect_grace_seconds"`
	AccessTokenTTL            string `yaml:"access_token_ttl"`
	RefreshTokenTTL           string `yaml:"refresh_token_ttl"`
	ConsoleSessionTTL         string `yaml:"console_session_ttl"`
	ConsoleLoginRatePerMinute int    `yaml:"console_login_rate_per_minute"`
	MaxHistoryMerge           int    `yaml:"max_history_merge"`
}

// WebPush configures the self-hosted PWA notification path. Every gateway
// owns its VAPID identity and sends Web Push itself; there is no central
// broker. Enabled by default because the path has no external dependency:
// the VAPID key is generated at runtime under DataDir with private
// permissions, and subscriptions only exist after a device deliberately
// binds its browser.
type WebPush struct {
	Enabled    bool   `yaml:"enabled"`
	Subject    string `yaml:"subject"`
	PrivateKeyFile string `yaml:"private_key_file"`
	TTL        string `yaml:"ttl"`
	BindTokenTTL   string `yaml:"bind_token_ttl"`
	SessionTTL     string `yaml:"session_ttl"`
	HandoffTTL     string `yaml:"handoff_ttl"`
	MaxSubscriptionsPerDevice int `yaml:"max_subscriptions_per_device"`
	// AllowedEndpointHosts switches endpoint validation from the default
	// deny-special-use policy to an explicit vendor-compatible allowlist
	// (host suffixes). Empty keeps the default open-but-safe behaviour.
	AllowedEndpointHosts []string `yaml:"allowed_endpoint_hosts"`
}

// Notify configures ordinary (non-VoIP) service alerts: confirmed carrier
// arrears notices from allowlisted senders and designated-backup network
// availability changes. Every field has a bounded default; the feature can be
// disabled explicitly with enabled: false.
type Notify struct {
	Enabled bool `yaml:"enabled"`
	// WanLineID is the designated backup line the network monitor speaks for.
	WanLineID string `yaml:"wan_line_id"`
	// WanStateFile is the read-only state file written by the WAN fallback
	// agent; missing means "no agent signal", never "unavailable".
	WanStateFile string `yaml:"wan_state_file"`
	// WanProbeCommand is the existing fallback agent binary; the monitor
	// reuses its isolated --probe-only validation instead of re-implementing
	// policy routing. Empty/missing disables active probing (state file only).
	WanProbeCommand string `yaml:"wan_probe_command"`
	WanProbeInterval        string `yaml:"wan_probe_interval"`
	WanFailureProbeInterval string `yaml:"wan_failure_probe_interval"`
	WanProbeTimeout         string `yaml:"wan_probe_timeout"`
	SustainedFailures int `yaml:"sustained_failures"`
	RecoverySuccesses int `yaml:"recovery_successes"`
	// CarrierSenders is the exact sender allowlist for arrears/restored
	// classification. Messages from any other sender are never classified.
	CarrierSenders []string `yaml:"carrier_senders"`
}

type Config struct {
	Gateway struct {
		ID                string `yaml:"id"`
		Name              string `yaml:"name"`
		Listen            string `yaml:"listen"`
		DataDir           string `yaml:"data_dir"`
		KeyFile           string `yaml:"key_file"`
		ConsoleBcryptFile string `yaml:"console_bcrypt_file"`
		PublicBaseURL     string `yaml:"public_base_url"`
		Version           string `yaml:"version"`
		LinesFile         string `yaml:"lines_file"`
		// ConsoleSecretFile holds the 32-byte AES-256 master key (base64,
		// root-only 0600) that seals operator-recoverable enrollment secrets.
		// Empty defaults to <data_dir>/console-secrets.key.
		ConsoleSecretFile string `yaml:"console_secret_file"`
	} `yaml:"gateway"`
	Lines     []Line    `yaml:"lines"`
	Settings  Settings  `yaml:"settings"`
	TURN      TURN      `yaml:"turn"`
	ICE       ICE       `yaml:"ice"`
	Push      Push      `yaml:"push"`
	Bark      Bark      `yaml:"bark"`
	WebPush   WebPush   `yaml:"webpush"`
	Notify    Notify    `yaml:"notify"`
	Voicemail Voicemail `yaml:"voicemail"`
	Recording Recording `yaml:"recording"`
	// QUIC is the build-61 DATAGRAM direct media listener (private UDP,
	// no public port mapping). Disabled by default; when enabled the
	// listener still advertises only while it actually starts.
	QUIC QUIC `yaml:"quic"`
}

// QUIC configures the direct QUIC media listener.
type QUIC struct {
	Enabled  bool     `yaml:"enabled"`
	Listen   string   `yaml:"listen"`
	LANPunch bool     `yaml:"lan_punch"`
	// STUNServers defaults to the ICE STUN list when empty.
	STUNServers []string `yaml:"stun_servers"`
}

func Default() Config {
	config := Config{}
	config.Gateway.ID = "callrelay-local"
	config.Gateway.Name = "CallRelay"
	config.Gateway.Listen = "127.0.0.1:8789"
	config.Gateway.DataDir = "/var/lib/callrelay"
	config.Gateway.Version = "dev"
	config.Settings = Settings{
		MaxConcurrentCalls:        2,
		DisconnectGraceSeconds:    45,
		AccessTokenTTL:            "15m",
		RefreshTokenTTL:           "720h",
		ConsoleSessionTTL:         "12h",
		ConsoleLoginRatePerMinute: 8,
		MaxHistoryMerge:           400,
	}
	config.TURN.CredentialTTL = "10m"
	config.Notify = Notify{
		Enabled:                 true,
		WanLineID:               "line2",
		WanStateFile:            "/run/cmr-wan-fallback/state",
		WanProbeCommand:         "/usr/local/sbin/cmr-wan-fallback",
		WanProbeInterval:        "15m",
		WanFailureProbeInterval: "5m",
		WanProbeTimeout:         "60s",
		SustainedFailures:       2,
		RecoverySuccesses:       2,
		CarrierSenders:          []string{"10086", "10010", "10000"},
	}
	config.Voicemail = Voicemail{
		AudioRoot:      "/srv/call-message-relay/voicemail",
		MountPoint:     "/srv/call-message-relay",
		ExpectedDevice: "/dev/mmcblk0",
		ExpectedFSType: "vfat",
	}
	config.Recording = Recording{
		AudioRoot:        "/srv/call-message-relay/recordings",
		MountPoint:       "/srv/call-message-relay",
		ExpectedDevice:   "/dev/mmcblk0",
		ExpectedFSType:   "vfat",
		MinimumFreeBytes: 512 * 1024 * 1024,
	}
	config.QUIC = QUIC{
		Enabled:  false,
		Listen:   ":8792",
		LANPunch: false,
	}
	return config
}

func Load(path string) (Config, error) {
	config := Default()
	payload, err := os.ReadFile(path)
	if err != nil {
		return config, err
	}
	if err := yaml.Unmarshal(payload, &config); err != nil {
		return config, err
	}
	if file := strings.TrimSpace(config.Gateway.LinesFile); file != "" {
		lines, err := LoadLinesFile(file)
		if err != nil {
			return config, err
		}
		config.Lines = mergeLines(config.Lines, lines)
	}
	return config, nil
}

// LinesDocument is the generated per-line block rendered by
// tools/line_registry.py render-lines.
type LinesDocument struct {
	Lines []Line `yaml:"lines"`
}

// LoadLinesFile reads a generated lines file. It is re-read while running so
// newly discovered modules become available without editing the main config.
func LoadLinesFile(path string) ([]Line, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var document LinesDocument
	if err := yaml.Unmarshal(payload, &document); err != nil {
		return nil, err
	}
	return document.Lines, nil
}

func mergeLines(base, extra []Line) []Line {
	merged := make([]Line, 0, len(base)+len(extra))
	seen := map[string]bool{}
	for _, line := range extra {
		if line.ID == "" {
			continue
		}
		seen[line.ID] = true
		merged = append(merged, line)
	}
	for _, line := range base {
		if !seen[line.ID] {
			merged = append(merged, line)
		}
	}
	return merged
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.Gateway.ID) == "" {
		return fmt.Errorf("gateway.id is required")
	}
	if strings.TrimSpace(c.Gateway.Listen) == "" {
		return fmt.Errorf("gateway.listen is required")
	}
	if !strings.HasPrefix(c.Gateway.Listen, "127.") && !strings.HasPrefix(c.Gateway.Listen, "localhost") {
		return fmt.Errorf("gateway.listen must stay on loopback; Caddy terminates public TLS")
	}
	if strings.TrimSpace(c.Gateway.DataDir) == "" {
		return fmt.Errorf("gateway.data_dir is required")
	}
	if c.Settings.MaxConcurrentCalls < 1 || c.Settings.MaxConcurrentCalls > 32 {
		return fmt.Errorf("settings.max_concurrent_calls must be between 1 and 32")
	}
	if c.Settings.DisconnectGraceSeconds < 5 || c.Settings.DisconnectGraceSeconds > 600 {
		return fmt.Errorf("settings.disconnect_grace_seconds must be between 5 and 600")
	}
	if c.QUIC.Enabled {
		if strings.TrimSpace(c.QUIC.Listen) == "" {
			return fmt.Errorf("quic.listen is required when quic is enabled")
		}
		if _, err := net.ResolveUDPAddr("udp", c.QUIC.Listen); err != nil {
			return fmt.Errorf("quic.listen must be a UDP address: %w", err)
		}
	}
	if _, err := c.AccessTokenDuration(); err != nil {
		return err
	}
	if _, err := c.RefreshTokenDuration(); err != nil {
		return err
	}
	if _, err := c.ConsoleSessionDuration(); err != nil {
		return err
	}
	if _, err := c.TURNCredentialDuration(); err != nil {
		return err
	}
	if _, err := c.BarkTimeout(); err != nil {
		return err
	}
	if _, err := c.BarkMaxCallAge(); err != nil {
		return err
	}
	if _, err := c.WebPushTTL(); err != nil {
		return err
	}
	if _, err := c.WebPushBindTokenTTL(); err != nil {
		return err
	}
	if _, err := c.WebPushSessionTTL(); err != nil {
		return err
	}
	if _, err := c.WebPushHandoffTTL(); err != nil {
		return err
	}
	if _, err := c.NotifyProbeInterval(); err != nil {
		return err
	}
	if _, err := c.NotifyFailureProbeInterval(); err != nil {
		return err
	}
	if _, err := c.NotifyProbeTimeout(); err != nil {
		return err
	}
	if c.Notify.SustainedFailures < 0 || c.Notify.SustainedFailures > 10 {
		return fmt.Errorf("notify.sustained_failures must be between 0 and 10")
	}
	if c.Notify.RecoverySuccesses < 0 || c.Notify.RecoverySuccesses > 10 {
		return fmt.Errorf("notify.recovery_successes must be between 0 and 10")
	}
	if c.TURN.Enabled {
		if strings.TrimSpace(c.TURN.Host) == "" || os.Getenv(c.TURN.SecretEnv) == "" {
			return fmt.Errorf("turn.enabled requires turn.host and the %s environment variable", c.TURN.SecretEnv)
		}
	}
	seen := map[string]bool{}
	for _, line := range c.Lines {
		if strings.TrimSpace(line.ID) == "" || strings.TrimSpace(line.WorkerURL) == "" {
			return fmt.Errorf("lines require id and worker_url")
		}
		if seen[line.ID] {
			return fmt.Errorf("duplicate line id %q", line.ID)
		}
		if strings.Contains(line.ID, ":") {
			return fmt.Errorf("line id %q must not contain ':'", line.ID)
		}
		seen[line.ID] = true
	}
	if strings.TrimSpace(c.Voicemail.AudioRoot) != "" {
		mountPoint := filepath.Clean(c.Voicemail.MountPoint)
		audioRoot := filepath.Clean(c.Voicemail.AudioRoot)
		if !strings.HasPrefix(audioRoot, mountPoint+string(filepath.Separator)) {
			return fmt.Errorf("voicemail.audio_root %q must live under voicemail.mount_point %q", audioRoot, mountPoint)
		}
	}
	if c.Recording.RetentionDays < 0 {
		return fmt.Errorf("recording.retention_days cannot be negative")
	}
	if c.Recording.MinimumFreeBytes < 0 {
		return fmt.Errorf("recording.minimum_free_bytes cannot be negative")
	}
	if c.Recording.MaxBytes < 0 {
		return fmt.Errorf("recording.max_bytes cannot be negative")
	}
	if strings.TrimSpace(c.Recording.AudioRoot) != "" {
		mountPoint := filepath.Clean(c.Recording.MountPoint)
		audioRoot := filepath.Clean(c.Recording.AudioRoot)
		if !strings.HasPrefix(audioRoot, mountPoint+string(filepath.Separator)) {
			return fmt.Errorf("recording.audio_root %q must live under recording.mount_point %q", audioRoot, mountPoint)
		}
	}
	return nil
}

func parseDuration(value string, fallback time.Duration) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("invalid duration %q", value)
	}
	return parsed, nil
}

func (c Config) AccessTokenDuration() (time.Duration, error) {
	return parseDuration(c.Settings.AccessTokenTTL, 15*time.Minute)
}

func (c Config) RefreshTokenDuration() (time.Duration, error) {
	return parseDuration(c.Settings.RefreshTokenTTL, 30*24*time.Hour)
}

func (c Config) ConsoleSessionDuration() (time.Duration, error) {
	return parseDuration(c.Settings.ConsoleSessionTTL, 12*time.Hour)
}

func (c Config) DisconnectGrace() time.Duration {
	return time.Duration(c.Settings.DisconnectGraceSeconds) * time.Second
}

// ConsoleSecretsPath resolves the master-key file for recoverable enrollment
// secrets, defaulting under the private data directory.
func (c Config) ConsoleSecretsPath() string {
	if path := strings.TrimSpace(c.Gateway.ConsoleSecretFile); path != "" {
		return path
	}
	return filepath.Join(c.Gateway.DataDir, "console-secrets.key")
}

func (c Config) TURNCredentialDuration() (time.Duration, error) {
	return parseDuration(c.TURN.CredentialTTL, 10*time.Minute)
}

// STUNEndpoints returns the validated STUN host:port entries in order with
// duplicates removed, plus the original strings that were malformed. A
// malformed entry is NEVER fatal: the caller logs it and continues, so a bad
// STUN configuration cannot block host/LAN candidates or the HTTPS/WSS
// fallback. Acceptance rules: trimmed, no scheme (stun: is added by the ICE
// layer), host non-empty, numeric port 1-65535.
func (c Config) STUNEndpoints() (valid []string, invalid []string) {
	seen := map[string]bool{}
	for _, entry := range c.ICE.STUNServers {
		raw := strings.TrimSpace(entry)
		if raw == "" || seen[raw] {
			continue
		}
		seen[raw] = true
		host, portText, err := net.SplitHostPort(raw)
		if err != nil || strings.TrimSpace(host) == "" {
			invalid = append(invalid, entry)
			continue
		}
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			invalid = append(invalid, entry)
			continue
		}
		valid = append(valid, raw)
	}
	return valid, invalid
}

// BarkTimeout bounds one Bark HTTP request (default 10s).
func (c Config) BarkTimeout() (time.Duration, error) {
	return parseDuration(c.Bark.Timeout, 10*time.Second)
}

// BarkMaxCallAge bounds how old a still-ringing call may be before an
// incoming Bark notification is suppressed as stale (default 60s).
func (c Config) BarkMaxCallAge() (time.Duration, error) {
	return parseDuration(c.Bark.MaxCallAge, 60*time.Second)
}

// WebPushPrivateKeyPath locates the runtime-generated VAPID private key.
func (c Config) WebPushPrivateKeyPath() string {
	if path := strings.TrimSpace(c.WebPush.PrivateKeyFile); path != "" {
		return path
	}
	return filepath.Join(c.Gateway.DataDir, "webpush", "vapid.pem")
}

// WebPushTTL bounds how long a push service holds an undelivered
// notification (default 30s — an incoming call is worthless when stale).
func (c Config) WebPushTTL() (time.Duration, error) {
	return parseDuration(c.WebPush.TTL, 30*time.Second)
}

// WebPushBindTokenTTL bounds the single-use app→browser bind token
// (default 10m).
func (c Config) WebPushBindTokenTTL() (time.Duration, error) {
	return parseDuration(c.WebPush.BindTokenTTL, 10*time.Minute)
}

// WebPushSessionTTL bounds a browser web session with sliding renewal
// (default 30 days).
func (c Config) WebPushSessionTTL() (time.Duration, error) {
	return parseDuration(c.WebPush.SessionTTL, 30*24*time.Hour)
}

// WebPushHandoffTTL bounds the single-use notification→call-screen token
// (default 120s).
func (c Config) WebPushHandoffTTL() (time.Duration, error) {
	return parseDuration(c.WebPush.HandoffTTL, 120*time.Second)
}

// NotifyProbeInterval is the normal backup-network observation cadence while
// the designated link looks healthy (default 15m; bounded low traffic).
func (c Config) NotifyProbeInterval() (time.Duration, error) {
	return parseDuration(c.Notify.WanProbeInterval, 15*time.Minute)
}

// NotifyFailureProbeInterval is the faster cadence after a failed probe
// (default 5m) so a sustained fault is confirmed quickly without hammering.
func (c Config) NotifyFailureProbeInterval() (time.Duration, error) {
	return parseDuration(c.Notify.WanFailureProbeInterval, 5*time.Minute)
}

// NotifyProbeTimeout bounds one isolated probe invocation (default 60s).
func (c Config) NotifyProbeTimeout() (time.Duration, error) {
	return parseDuration(c.Notify.WanProbeTimeout, 60*time.Second)
}

// WebPushMaxSubscriptions caps browsers bound to one device (default 4).
func (c Config) WebPushMaxSubscriptions() int {
	if c.WebPush.MaxSubscriptionsPerDevice > 0 {
		return c.WebPush.MaxSubscriptionsPerDevice
	}
	return 4
}

// WebPushSubject returns the VAPID JWT subscriber. It defaults to a
// gateway-local mailto so the contact claim stays operator-owned.
func (c Config) WebPushSubject() string {
	if subject := strings.TrimSpace(c.WebPush.Subject); subject != "" {
		return subject
	}
	return "mailto:admin@" + strings.TrimSpace(c.Gateway.ID)
}
