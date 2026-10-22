package at

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"callrelay/gateway/internal/id"
	"callrelay/gateway/internal/modem"
	"callrelay/gateway/internal/modem/qdc507"
	"github.com/google/uuid"
)

// Adapter is the conservative V1 AT command adapter. It deliberately keeps
// vendor-specific audio commands out of the control path; voice audio is
// provided by the selected VoiceAudio backend while this type owns calls,
// SMS, and line state.
type Adapter struct {
	client *Client
	mu     sync.Mutex
	active modem.CallID
	// dialing is true while an ATD exchange owns the line. It serializes
	// dials so a late response from an aborted dial can never be misread as
	// the state of a newer one.
	dialing bool
	// uncertain is the fail-closed quarantine set when an ATD was written but
	// its final response was lost. While set, Dial refuses to write a new
	// ATD; unclear it only after fresh serialized CLCC exchanges confirm no
	// live voice leg. uncertainCallID records an observed unreleased outgoing
	// leg for evidence (never used to send a hangup).
	uncertain       bool
	uncertainCallID string
	// peer is the caller number observed for the current inbound call. It is
	// reset when that call ends so a stale +CLIP can never attach to the next
	// call. pendingPeer holds a +CLIP that arrived before its RING; it expires
	// quickly and is discarded while a just-ended call is still in cooldown.
	peer          string
	pendingPeer   string
	pendingPeerAt time.Time
	lastEndedAt   time.Time
	capMu         sync.RWMutex
	caps          modem.Capabilities
	events        chan modem.ModemEvent
	close         sync.Once
}

const (
	// clipPendingTTL bounds how long a +CLIP that precedes RING is kept.
	clipPendingTTL = 5 * time.Second
	// clipAfterEndGrace discards a late +CLIP for a just-terminated call so
	// its number cannot leak into whatever rings next.
	clipAfterEndGrace = 2 * time.Second
)

func NewAdapter(client *Client) *Adapter {
	adapter := &Adapter{client: client, events: make(chan modem.ModemEvent, 32)}
	if client != nil {
		client.SetLineHandler(adapter.HandleURC)
	}
	return adapter
}

func (a *Adapter) Probe(ctx context.Context) (modem.Capabilities, error) {
	if a.client == nil {
		return modem.Capabilities{}, fmt.Errorf("AT client is unavailable")
	}
	manufacturer, err := firstPayload(a.client.Exchange(ctx, "AT+CGMI"))
	if err != nil {
		return modem.Capabilities{}, err
	}
	model, _ := firstPayload(a.client.Exchange(ctx, "AT+CGMM"))
	revision, _ := firstPayload(a.client.Exchange(ctx, "AT+CGMR"))
	capabilities := modem.Capabilities{
		Vendor:              manufacturer,
		Model:               model,
		Tier:                modem.CapabilitySMSOnly,
		SMS:                 true,
		FirmwareFingerprint: strings.TrimSpace(manufacturer + " " + model + " " + revision),
	}
	if voiceCapabilities, capabilityErr := qdc507.ProbeCapabilities(ctx, a.client, manufacturer, model); capabilityErr == nil {
		capabilities.Voice = voiceCapabilities.Voice
		capabilities.DTMF = voiceCapabilities.DTMF
		capabilities.Audio = voiceCapabilities.Audio
		capabilities.RequiresBootstrap = voiceCapabilities.RequiresBootstrap
		capabilities.Tier = voiceCapabilities.Tier
	}
	a.capMu.Lock()
	a.caps = capabilities
	a.capMu.Unlock()
	// Caller ID (mobile-terminated +CLIP) is off by default on several
	// firmwares; enable it now and again after any reboot before dialing.
	a.ensureCallerID(ctx)
	return capabilities, nil
}

// ensureCallerID turns on calling-line-identification reporting so the modem
// emits +CLIP for an inbound call. It is strictly best effort: firmware that
// rejects the write simply leaves the peer unknown instead of failing the
// line, and the command runs on the adapter's serialized AT queue like every
// other read/write.
func (a *Adapter) ensureCallerID(ctx context.Context) {
	if a.client == nil {
		return
	}
	cmdCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	if _, err := a.client.Exchange(cmdCtx, "AT+CLIP=1"); err != nil {
		slog.Warn("enable caller ID reporting failed", "error", err)
	}
}

func (a *Adapter) Status(ctx context.Context) (modem.LineStatus, error) {
	if a.client == nil {
		return modem.LineStatus{}, fmt.Errorf("AT client is unavailable")
	}
	simLines, err := a.client.Exchange(ctx, "AT+CPIN?")
	if err != nil {
		return modem.LineStatus{}, err
	}
	registrationLines, err := a.client.Exchange(ctx, "AT+CEREG?")
	if err != nil {
		return modem.LineStatus{}, err
	}
	signalLines, err := a.client.Exchange(ctx, "AT+CSQ")
	if err != nil {
		return modem.LineStatus{}, err
	}
	a.capMu.RLock()
	capabilities := a.caps
	a.capMu.RUnlock()
	if capabilities.Tier == "" {
		capabilities.Tier = modem.CapabilitySMSOnly
	}
	voiceState := "unavailable"
	if capabilities.Voice {
		voiceState = "control_only"
	}
	status := modem.LineStatus{
		SIM:            normalizeSIM(payload(simLines)),
		Registration:   normalizeRegistration(payload(registrationLines)),
		Signal:         modem.Signal{RSSI: parseRSSI(payload(signalLines))},
		CapabilityTier: capabilities.Tier,
		Voice:          voiceState,
		SMS:            "ready",
	}
	status.Signal.Bars = signalBars(status.Signal.RSSI)
	a.mu.Lock()
	if a.active != "" {
		active := a.active
		status.ActiveCallID = &active
	}
	// Surface the fail-closed quarantine so operators can see WHY dials are
	// refused (mapped to CB-CALL-013) instead of only seeing refusals.
	status.DialUncertain = a.uncertain
	a.mu.Unlock()
	return status, nil
}

// diagnosticCommands is the fixed, strictly read-only allowlist used by the
// trusted loopback AT diagnostics endpoint. The list lives in code (never in
// a request), so the endpoint can never inject arbitrary AT commands.
var diagnosticCommands = []string{
	"AT+CEER",
	"AT+CIREG?",
	"AT+CEREG?",
	"AT+CGREG?",
	"AT+COPS?",
	"AT+QNWINFO",
	`AT+QCFG="ims"`,
	`AT+QCFG="volte_disable"`,
	"AT+CLCC",
	"AT+CSQ",
}

// Diagnostics runs the fixed read-only allowlist through the same serialized
// AT client used by calls and SMS. Every query is individually bounded and
// failures are reported per command, so one slow or rejected query can
// neither pin the queue nor hide the others. No state-changing command is
// ever sent from this path; a competing serial reader is never opened.
func (a *Adapter) Diagnostics(ctx context.Context) ([]modem.ATDiagnostic, error) {
	if a.client == nil {
		return nil, fmt.Errorf("AT client is unavailable")
	}
	results := make([]modem.ATDiagnostic, 0, len(diagnosticCommands))
	for _, command := range diagnosticCommands {
		item := modem.ATDiagnostic{Command: command}
		queryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		lines, err := a.client.Exchange(queryCtx, command)
		cancel()
		if err != nil {
			item.Error = err.Error()
		} else {
			item.Lines = lines
		}
		results = append(results, item)
		if ctx.Err() != nil {
			break
		}
	}
	return results, nil
}

// WaitReady polls the AT channel until the modem answers "AT" with
// OK (or the context ends). After a NAS reboot the module's internal
// system re-boots on its own schedule; waiting here turns a dial's
// failure mode from "immediate 500" into "ring while the module
// comes up".
func (a *Adapter) WaitReady(ctx context.Context) error {
	if a.client == nil {
		return fmt.Errorf("AT client is unavailable")
	}
	// Each probe gets a short independent timeout so a hung port does
	// not consume the whole ready budget.
	deadline := 120 * time.Second
	waitCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	attempt := 0
	for {
		if err := waitCtx.Err(); err != nil {
			return fmt.Errorf("QDC507 AT channel not ready after %s: %w", deadline, err)
		}
		probeCtx, probeCancel := context.WithTimeout(waitCtx, 2*time.Second)
		_, err := a.client.Exchange(probeCtx, "AT")
		probeCancel()
		if err == nil {
			slog.Info("modem AT channel ready", "after_attempts", attempt+1)
			// A module reboot can reset +CLIP to off; re-assert it before
			// the dial that follows while the queue is otherwise idle.
			a.ensureCallerID(waitCtx)
			return nil
		}
		attempt++
		select {
		case <-waitCtx.Done():
			return waitCtx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (a *Adapter) Dial(ctx context.Context, peer string) (modem.CallID, error) {
	a.mu.Lock()
	busy := a.active != "" || a.dialing
	if !busy {
		// Claim the dial slot before writing ATD so a second dial cannot
		// start while this attempt's final response is still unknown.
		a.dialing = true
	}
	a.mu.Unlock()
	if busy {
		return "", modem.ErrActiveCall
	}
	defer func() {
		a.mu.Lock()
		a.dialing = false
		a.mu.Unlock()
	}()
	// A previous dial whose ATD response was lost leaves the modem side
	// effect unknown. Fail closed: refuse the new dial until a fresh
	// serialized CLCC confirms there is no live voice leg. No automatic
	// hangup is ever sent from this path: a blind ATH/CHUP could tear down
	// a new incoming call or a voice call this adapter was never told about.
	if a.uncertainState() && !a.tryResolveUncertain(ctx) {
		a.mu.Lock()
		claimed := a.active
		a.mu.Unlock()
		if claimed != "" {
			// An inbound call took the line while the quarantine was being
			// resolved. Nothing was written, so this is a plain busy line,
			// not dial uncertainty.
			return "", modem.ErrActiveCall
		}
		return "", modem.ErrDialUncertain
	}
	// Pre-submit re-check (fast path): if an inbound RING already claimed the
	// line, refuse before touching the serial queue. This alone is NOT
	// sufficient — a RING can be dispatched while the exchange below waits
	// for the queue — so the write itself runs under the adapter lock via
	// ExchangeGuarded.
	a.mu.Lock()
	if a.active != "" {
		a.mu.Unlock()
		return "", modem.ErrActiveCall
	}
	if err := ctx.Err(); err != nil {
		a.mu.Unlock()
		return "", err
	}
	a.mu.Unlock()
	// After a NAS reboot the module's internal system boots on its own
	// schedule (USB re-enumeration + Android userspace, which can take
	// minutes). Sending ATD into a not-yet-ready modem just burns the
	// dial deadline and produces a 500 to the SIP client. The dial path
	// calls WaitReady upfront (see SIPCallSession.Dial), so by the time
	// we reach here the AT channel is known to answer; keep ATD's own
	// deadline tight so a mid-dial hang still fails fast.
	_, err := a.client.ExchangeGuarded(ctx, "ATD"+strings.TrimSpace(peer)+";",
		func() error {
			// beforeWrite: the port lock is held and the write happens
			// with a.mu held, so this active/ctx re-check is atomic with
			// the ATD submission. An inbound RING dispatched while the
			// exchange waited on the queue is seen here and ATD is not
			// written. On success the lock stays held until writeDone.
			a.mu.Lock()
			if a.active != "" {
				a.mu.Unlock()
				return modem.ErrActiveCall
			}
			if err := ctx.Err(); err != nil {
				a.mu.Unlock()
				return err
			}
			return nil
		},
		func() { a.mu.Unlock() },
	)
	if err != nil {
		// ErrResponseLost means the ATD was written but its final response
		// never arrived, so whether the modem holds a call leg is unknown.
		// Quarantine instead of guessing. A pre-write cancel is a plain
		// context error (nothing was written) and an explicit rejection is
		// ErrCommandFailed (an ERROR/+CME/+CMS final response; NO CARRIER and
		// BUSY are unsolicited URCs and never terminate this exchange) - no
		// quarantine is needed for either.
		if errors.Is(err, ErrResponseLost) {
			a.markUncertain()
		}
		return "", err
	}
	callID, err := id.New("call_", 16)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	if claimed := a.active; claimed != "" {
		// An inbound RING was claimed while the ATD was in flight. The
		// inbound call wins the single active slot: it must never be
		// overwritten, and this dial's outcome is unknown (the modem may
		// hold the outgoing leg), so re-quarantine and fail closed.
		a.mu.Unlock()
		slog.Error("dial completed while an inbound call owns the line; preserving inbound and quarantining outgoing outcome", "active", claimed)
		a.markUncertain()
		return "", modem.ErrDialUncertain
	}
	a.active = modem.CallID(callID)
	a.peer = ""
	a.pendingPeer = ""
	a.pendingPeerAt = time.Time{}
	a.uncertain = false
	a.uncertainCallID = ""
	a.mu.Unlock()
	return modem.CallID(callID), nil
}

func (a *Adapter) uncertainState() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.uncertain
}

// markUncertain quarantines the line after an ATD whose final response was
// lost. It records nothing about a call it never saw and sends no command.
func (a *Adapter) markUncertain() {
	a.mu.Lock()
	was := a.uncertain
	a.uncertain = true
	a.uncertainCallID = ""
	a.mu.Unlock()
	if !was {
		slog.Warn("modem dial outcome unknown; line quarantined until a fresh CLCC confirms no voice leg")
	}
}

// tryResolveUncertain attempts to clear the fail-closed quarantine with
// fresh serialized CLCC exchanges, and only ever reads:
//   - The first post-cancel exchange is untrusted: it may consume the stale
//     ATD final response, so it never counts as confirmation.
//   - A live voice leg (3GPP mode 0) keeps the line quarantined. Its call id
//     is recorded for evidence, but nothing is ever hung up.
//   - An incoming voice leg is never touched and never counted as clean.
//   - A malformed or unclassifiable CLCC response fails closed: it counts
//     like a failed read and never as "no calls".
//   - Only two consecutive trusted clean exchanges clear the quarantine.
//   - Data-session rows (mode 1, e.g. the persistent row this fleet reports)
//     and disconnected legs (state 6) never block confirmation.
//
// It is bounded, runs while the caller holds the dial slot, and returns true
// only when the quarantine was actually cleared.
func (a *Adapter) tryResolveUncertain(parent context.Context) bool {
	if a.client == nil {
		return false
	}
	deadline := time.Now().Add(2 * time.Second)
	exchanges := 0
	cleanStreak := 0
	for time.Now().Before(deadline) {
		a.mu.Lock()
		claimed := a.active != ""
		a.mu.Unlock()
		if claimed {
			// A call (for example a new inbound RING) now owns the line;
			// leave the quarantine in place and never disturb that call.
			return false
		}
		queryCtx, cancel := context.WithTimeout(parent, time.Second)
		lines, err := a.client.Exchange(queryCtx, "AT+CLCC")
		cancel()
		exchanges++
		voices, trusted := voiceCallsCLCC(lines)
		switch {
		case err != nil:
			// A failed or rejected read proves nothing; stay quarantined.
			cleanStreak = 0
		case !trusted:
			// Malformed or unclassifiable CLCC must fail closed, never
			// read back as an empty (clean) response.
			cleanStreak = 0
		case len(voices) == 0:
			if exchanges >= 2 { // first exchange may carry the stale final response
				cleanStreak++
			}
		default:
			cleanStreak = 0
			for _, voice := range voices {
				if voice.dir == "0" {
					a.mu.Lock()
					if a.uncertainCallID == "" {
						a.uncertainCallID = voice.id
						slog.Warn("unreleased outgoing modem leg kept quarantined", "call_id", voice.id, "state", voice.state)
					}
					a.mu.Unlock()
				}
			}
		}
		if cleanStreak >= 2 {
			a.mu.Lock()
			if a.active == "" {
				a.uncertain = false
				a.uncertainCallID = ""
				a.mu.Unlock()
				slog.Info("modem dial uncertainty cleared after fresh CLCC confirmed no voice leg")
				return true
			}
			a.mu.Unlock()
			return false
		}
		select {
		case <-parent.Done():
			return false
		case <-time.After(200 * time.Millisecond):
		}
	}
	slog.Warn("modem dial uncertainty not resolved within budget; refusing new dials")
	return false
}

type clccVoiceCall struct {
	id    string
	dir   string
	state string
}

// voiceCallsCLCC returns the live voice legs of a CLCC response and whether
// the response was fully parseable. Per 3GPP 27.007 mode 0 is voice;
// data-session rows (mode 1), fax rows (mode 2) and disconnected legs
// (state 6) are not calls for this purpose. A malformed row or an
// unclassifiable state/mode/dir makes the WHOLE response untrusted: callers
// must fail closed (treat it like a failed read), never as "no calls" —
// an empty-but-garbled CLCC must not clear the quarantine.
func voiceCallsCLCC(lines []string) ([]clccVoiceCall, bool) {
	var calls []clccVoiceCall
	for _, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "+CLCC:") {
			continue
		}
		fields := strings.Split(strings.TrimPrefix(strings.TrimSpace(line), "+CLCC:"), ",")
		if len(fields) < 4 {
			return nil, false
		}
		id := strings.TrimSpace(fields[0])
		dir := strings.TrimSpace(fields[1])
		state := strings.TrimSpace(fields[2])
		mode := strings.TrimSpace(fields[3])
		stateN, err := strconv.Atoi(state)
		if err != nil || id == "" {
			return nil, false
		}
		modeN, err := strconv.Atoi(mode)
		if err != nil {
			return nil, false
		}
		if stateN == 6 {
			continue
		}
		switch modeN {
		case 1, 2:
			continue
		case 0:
			if dir != "0" && dir != "1" {
				return nil, false
			}
			calls = append(calls, clccVoiceCall{id: id, dir: dir, state: state})
		default:
			// Unknown mode (3GPP reserves 9 = unknown): the leg cannot be
			// classified, so the read cannot confirm a clean line.
			return nil, false
		}
	}
	return calls, true
}

func (a *Adapter) Answer(ctx context.Context, callID modem.CallID) error {
	if err := a.validateActive(callID); err != nil {
		return err
	}
	_, err := a.client.Exchange(ctx, "ATA")
	return err
}

func (a *Adapter) Hangup(ctx context.Context, callID modem.CallID) error {
	if err := a.validateActive(callID); err != nil {
		return err
	}
	_, err := a.client.Exchange(ctx, "ATH")
	if err == nil {
		a.finishActive(callID, "local_hangup")
	}
	return err
}

func (a *Adapter) SendDTMF(ctx context.Context, callID modem.CallID, digit rune) error {
	if err := a.validateActive(callID); err != nil {
		return err
	}
	if !strings.ContainsRune("0123456789*#ABCD", digit) {
		return fmt.Errorf("invalid DTMF digit %q", digit)
	}
	_, err := a.client.Exchange(ctx, fmt.Sprintf("AT+VTS=\"%c\"", digit))
	return err
}

func (a *Adapter) SendSMS(ctx context.Context, destination string, payload modem.SMSPayload) (modem.SMSID, error) {
	destination = strings.TrimSpace(destination)
	if len(destination) <= 6 && payload.Body != "" {
		if _, err := a.client.Exchange(ctx, "AT+CMGF=1"); err != nil {
			return "", err
		}
		if _, err := a.client.Exchange(ctx, "AT+CSCS=\"GSM\""); err != nil {
			return "", err
		}
		if _, err := a.client.SendTextSMS(ctx, destination, payload.Body); err != nil {
			if errors.Is(err, ErrSubmissionResultUnknown) {
				return "", fmt.Errorf("%w: %v", modem.ErrSMSSubmissionUnknown, err)
			}
			return "", err
		}
		messageID, err := id.New("sms_", 16)
		return modem.SMSID(messageID), err
	}
	if payload.PDU == "" {
		return "", fmt.Errorf("SMS PDU is required")
	}
	if _, err := a.client.Exchange(ctx, "AT+CMGF=0"); err != nil {
		return "", err
	}
	pduBytes := len(strings.TrimSpace(payload.PDU)) / 2
	if pduBytes < 2 {
		return "", fmt.Errorf("invalid SMS PDU length")
	}
	// AT+CMGS takes TPDU octets, excluding the SMSC length octet.
	smscLength := 0
	if value, err := strconv.ParseInt(payload.PDU[:2], 16, 8); err == nil {
		smscLength = int(value)
	}
	tpduLength := pduBytes - 1 - smscLength
	if tpduLength < 1 {
		return "", fmt.Errorf("invalid SMS TPDU length")
	}
	if _, err := a.client.SendPDU(ctx, tpduLength, payload.PDU); err != nil {
		if errors.Is(err, ErrSubmissionResultUnknown) {
			return "", fmt.Errorf("%w: %v", modem.ErrSMSSubmissionUnknown, err)
		}
		return "", err
	}
	messageID, err := id.New("sms_", 16)
	return modem.SMSID(messageID), err
}

func (a *Adapter) ListSMS(ctx context.Context, cursor modem.SMSCursor) ([]modem.RawSMS, modem.SMSCursor, error) {
	// Explicitly select the modem's message storage ("MT" = module phone
	// memory). Without this, CMGL lists the *current preferred* storage
	// (often SM/SR on QDC507), which is empty because inbound SMS lands
	// in MT — so messages are never ingested, pushed, or deleted.
	// Observed 2026-09-06: +CPMS showed "MT",2,23 but CMGL=4 returned
	// nothing; user stopped receiving SMS entirely.
	if _, err := a.client.Exchange(ctx, "AT+CPMS=\"MT\""); err != nil {
		return nil, cursor, err
	}
	if _, err := a.client.Exchange(ctx, "AT+CMGF=0"); err != nil {
		return nil, cursor, err
	}
	lines, err := a.client.Exchange(ctx, "AT+CMGL=4")
	if err != nil {
		return nil, cursor, err
	}
	var result []modem.RawSMS
	var current *modem.RawSMS
	lastIndex := strings.TrimSpace(string(cursor))
	for _, line := range lines {
		if strings.HasPrefix(line, "+CMGL:") {
			fields := strings.Split(strings.TrimSpace(strings.TrimPrefix(line, "+CMGL:")), ",")
			if len(fields) == 0 {
				continue
			}
			index, parseErr := strconv.Atoi(strings.TrimSpace(fields[0]))
			if parseErr != nil {
				continue
			}
			current = &modem.RawSMS{ModemStorage: "SM", ModemIndex: index}
			lastIndex = strconv.Itoa(index)
			continue
		}
		if current != nil && !IsFinal(line) && !strings.HasPrefix(line, "+") {
			pdu, decodeErr := hex.DecodeString(strings.TrimSpace(line))
			if decodeErr == nil {
				current.RawPDU = pdu
				result = append(result, *current)
			}
			current = nil
		}
	}
	return result, modem.SMSCursor(lastIndex), nil
}

func (a *Adapter) DeleteSMS(ctx context.Context, storageIndex string) error {
	// CMGD without an explicit storage deletes from the *current
	// preferred* storage; pin it to MT (same reason as ListSMS) so
	// engine.go's post-ingest cleanup actually removes the message.
	if _, err := a.client.Exchange(ctx, "AT+CPMS=\"MT\""); err != nil {
		return err
	}
	if _, err := a.client.Exchange(ctx, "AT+CMGD="+strings.TrimSpace(storageIndex)); err != nil {
		return err
	}
	return nil
}

func (a *Adapter) Events() <-chan modem.ModemEvent { return a.events }

// OwnIdentity reads the SIM own number and subscriber/serving-network
// metadata through the adapter's existing serialized AT queue. It is
// strictly read-only: no SIM writes, phonebook selection, network selection,
// USSD, calls or SMS. The payload is returned only over the loopback internal
// API to the trusted unified daemon, which persists raw IMSI/ICCID privately
// and exposes only hashed/masked forms. Every command is individually bounded
// and recorded in Commands (ok/empty/unsupported/error) so one unsupported
// query never blanks the rest.
func (a *Adapter) OwnIdentity(ctx context.Context) (modem.SIMIdentity, error) {
	if a.client == nil {
		return modem.SIMIdentity{NumberSource: "error", Commands: map[string]string{}}, fmt.Errorf("AT client is unavailable")
	}
	identity := modem.SIMIdentity{Commands: map[string]string{}}
	meta := NetworkMeta{}

	// query runs one read command on the shared queue with its own deadline.
	query := func(command string, timeout time.Duration) ([]string, string) {
		cmdCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		lines, err := a.client.Exchange(cmdCtx, command)
		if err != nil {
			if strings.Contains(err.Error(), "ERROR") {
				return lines, "unsupported"
			}
			return lines, "error"
		}
		for _, line := range lines {
			if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(line)), "+CME ERROR") {
				return lines, "unsupported"
			}
		}
		return lines, "ok"
	}

	// Own number first (the primary signal), then ICCID/IMSI, then network.
	if lines, status := query("AT+CNUM", 6*time.Second); status == "ok" {
		result := ParseCNUM(strings.Join(lines, "\r\n"))
		switch {
		case result.Found() && result.OK:
			identity.Number = result.Number()
			identity.NumberSource = "sim"
			if len(result.Entries) > 0 {
				identity.TypeOfAddress = result.Entries[0].TypeOfAddress
			}
			identity.Commands["AT+CNUM"] = "ok"
		case result.OK:
			identity.NumberSource = "empty" // OK, no +CNUM entry: SIM stores no number
			identity.Commands["AT+CNUM"] = "empty"
		default:
			identity.NumberSource = "error"
			identity.Commands["AT+CNUM"] = "error"
		}
	} else {
		if status == "unsupported" {
			identity.NumberSource = "unsupported"
		} else {
			identity.NumberSource = "error"
		}
		identity.Commands["AT+CNUM"] = status
	}

	if lines, status := query("AT+CCID", 5*time.Second); status == "ok" {
		if iccid := ParseCCID(strings.Join(lines, "\r\n")); iccid != "" {
			identity.ICCID = iccid
			identity.Commands["AT+CCID"] = "ok"
		} else {
			identity.Commands["AT+CCID"] = "empty"
		}
	} else if status == "unsupported" {
		// AT+ICCID? read-command variant.
		if lines2, status2 := query("AT+ICCID?", 5*time.Second); status2 == "ok" {
			identity.ICCID = ParseCCID(strings.Join(lines2, "\r\n"))
			identity.Commands["AT+CCID"] = "unsupported"
			if identity.ICCID != "" {
				identity.Commands["AT+ICCID?"] = "ok"
			} else {
				identity.Commands["AT+ICCID?"] = "empty"
			}
		} else {
			identity.Commands["AT+CCID"] = "unsupported"
			identity.Commands["AT+ICCID?"] = status2
		}
	} else {
		identity.Commands["AT+CCID"] = status
	}

	if lines, status := query("AT+CIMI", 5*time.Second); status == "ok" {
		identity.IMSI = ParseIMSI(lines)
		if identity.IMSI != "" {
			identity.Commands["AT+CIMI"] = "ok"
		} else {
			identity.Commands["AT+CIMI"] = "empty"
		}
	} else {
		identity.Commands["AT+CIMI"] = status
	}

	if lines, status := query("AT+COPS?", 5*time.Second); status == "ok" {
		meta.OperatorNumeric, meta.OperatorAlpha = ParseCOPS(lines)
		identity.Commands["AT+COPS?"] = "ok"
	} else {
		identity.Commands["AT+COPS?"] = status
	}

	// Registration: prefer LTE EPS, fall back to packet/circuit switched.
	if lines, status := query("AT+CEREG?", 5*time.Second); status == "ok" {
		reg, lac, tac, cell := ParseRegistration(lines, true)
		meta.Registration, meta.LAC, meta.TAC, meta.CellID = reg, lac, tac, cell
		identity.Commands["AT+CEREG?"] = "ok"
	} else {
		identity.Commands["AT+CEREG?"] = status
		if lines, regStatus := query("AT+CREG?", 5*time.Second); regStatus == "ok" {
			reg, lac, _, cell := ParseRegistration(lines, false)
			meta.Registration, meta.LAC, meta.CellID = reg, lac, cell
			identity.Commands["AT+CREG?"] = "ok"
		} else {
			identity.Commands["AT+CREG?"] = regStatus
		}
	}

	// One serving-cell information element (read-only). Unsupported on
	// firmware without the Quectel QENG engine; that is recorded, not retried
	// across variants.
	if lines, status := query(`AT+QENG="servingcell"`, 5*time.Second); status == "ok" {
		cell := ParseServingCell(lines)
		if cell.CellID != "" {
			meta.CellID = cell.CellID
		}
		if cell.TAC != "" {
			meta.TAC = cell.TAC
		}
		if cell.LAC != "" {
			meta.LAC = cell.LAC
		}
		if cell.AccessTech != "" {
			meta.AccessTech = cell.AccessTech
		}
		if meta.OperatorNumeric == "" {
			meta.OperatorNumeric = cell.OperatorNumeric
		}
		meta.PCI, meta.EARFCN = cell.PCI, cell.EARFCN
		identity.Commands[`AT+QENG="servingcell"`] = "ok"
	} else {
		identity.Commands[`AT+QENG="servingcell"`] = status
	}

	identity.OperatorNumeric = meta.OperatorNumeric
	identity.OperatorAlpha = meta.OperatorAlpha
	identity.Registration = meta.Registration
	identity.AccessTech = meta.AccessTech
	identity.CellID = meta.CellID
	identity.LAC = meta.LAC
	identity.TAC = meta.TAC
	identity.PCI = meta.PCI
	identity.EARFCN = meta.EARFCN
	return identity, nil
}

// SetDryRun toggles SMS dry-run at runtime so the unified console can move
// the whole deployment between "store only" and live submission without a
// worker restart. Live sending stays gated by this explicit operator action.
func (a *Adapter) SetDryRun(dryRun bool) {
	if a.client != nil {
		a.client.SetDryRun(dryRun)
	}
}

// DryRun reports the current SMS submission mode.
func (a *Adapter) DryRun() bool {
	if a.client == nil {
		return true
	}
	return a.client.DryRunEnabled()
}

// WaitAnyActive is like WaitActive but accepts an inbound or outbound call
// whose CLCC state is active (state=0). The unified internal media path
// waits for this before opening UAC capture, which the firmware wedges
// into permanent silence if started while the cellular leg is still dialing.
// Only mode 0 (voice) counts: this fleet permanently reports an active
// mobile-terminated DATA row (+CLCC: 2,1,0,1,0), and treating it as an
// active call would make the wait return immediately even when idle.
func (a *Adapter) WaitAnyActive(ctx context.Context) (bool, error) {
	if a.client == nil {
		return false, fmt.Errorf("AT client is unavailable")
	}
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		default:
		}
		lines, err := a.client.Exchange(ctx, "AT+CLCC")
		if err == nil {
			for _, line := range lines {
				if !strings.HasPrefix(line, "+CLCC:") {
					continue
				}
				fields := strings.Split(strings.TrimPrefix(line, "+CLCC:"), ",")
				if len(fields) >= 4 && strings.TrimSpace(fields[2]) == "0" && strings.TrimSpace(fields[3]) == "0" {
					return true, nil
				}
			}
		}
		timer := time.NewTimer(300 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		}
	}
}

// WaitActive polls AT+CLCC until the outgoing call is answered (dir=0,
// state=0 active) or the context ends. Opening UAC capture before the
// cellular leg is active wedges the ALSA ASYNC stream into an XRUN that
// reads silence forever, so the SIP bridge must wait for this before
// starting audio. Only mode 0 (voice) counts: a mobile-originated DATA
// session would otherwise false-positive as an answered voice call.
func (a *Adapter) WaitActive(ctx context.Context) (bool, error) {
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		default:
		}
		lines, err := a.client.Exchange(ctx, "AT+CLCC")
		if err == nil {
			for _, line := range lines {
				// +CLCC: <id>,<dir>,<state>,<mode>,... — answered is dir=0, state=0, mode=0 (voice)
				if strings.HasPrefix(line, "+CLCC:") {
					fields := strings.Split(strings.TrimPrefix(line, "+CLCC:"), ",")
					if len(fields) >= 4 && strings.TrimSpace(fields[1]) == "0" && strings.TrimSpace(fields[2]) == "0" && strings.TrimSpace(fields[3]) == "0" {
						return true, nil
					}
				}
			}
		}
		timer := time.NewTimer(300 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		}
	}
}

func (a *Adapter) Run(ctx context.Context) error {
	if a.client == nil {
		return fmt.Errorf("AT client is unavailable")
	}
	return a.client.Run(ctx)
}

// HandleURC is called by a serial reader when a line arrives outside an AT
// command exchange. It is public so a platform-specific reader can preserve
// unsolicited RING/+CLIP/NO CARRIER events without coupling it to the parser.
func (a *Adapter) HandleURC(line string) {
	// A URC may arrive exactly as Close() shuts the events channel
	// down (observed as a restart-time panic: "send on closed
	// channel"). Swallow it instead of crashing the gateway.
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Warn("adapter URC dropped during shutdown", "cause", recovered)
		}
	}()
	line = strings.TrimSpace(line)
	if peer, isClip := ParseCLIP(line); isClip {
		a.handleCallerID(peer)
		return
	}
	now := time.Now()
	var event modem.ModemEvent
	a.mu.Lock()
	switch {
	case line == "RING":
		// An inbound RING always claims the line, including while a dial
		// is in flight: dropping it would lose a real inbound call. The
		// dial path cooperates instead — it re-checks a.active immediately
		// before submitting ATD (and refuses to write over an inbound
		// call), and its success path preserves an inbound claim made
		// mid-exchange rather than overwriting it (the outgoing outcome is
		// then quarantined as unknown). Both paths only read this state.
		callID := a.active
		if callID == "" {
			callID = modem.CallID(uuid.NewString())
			a.active = callID
			// A +CLIP that preceded this RING belongs to this call.
			if a.pendingPeer != "" && now.Sub(a.pendingPeerAt) <= clipPendingTTL {
				a.peer = a.pendingPeer
			}
			a.pendingPeer = ""
			a.pendingPeerAt = time.Time{}
		}
		event = modem.ModemEvent{Kind: "incoming", CallID: callID, Peer: a.peer}
	case strings.Contains(line, "NO CARRIER"), strings.Contains(line, "BUSY"), strings.Contains(line, "NO ANSWER"):
		callID := a.active
		event = modem.ModemEvent{Kind: "ended", CallID: callID, Raw: line}
		// The modem terminated the call (e.g. an unanswered inbound call).
		// Clear the active marker and the caller number here, or every later
		// dial fails with ErrActiveCall / the old number leaks to the next
		// caller.
		if callID != "" {
			a.active = ""
			a.peer = ""
			a.lastEndedAt = now
		}
		a.pendingPeer = ""
		a.pendingPeerAt = time.Time{}
	default:
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()
	select {
	case a.events <- event:
	default:
	}
}

// handleCallerID records a +CLIP caller number. If a call is already ringing
// (or active) the number updates that call in place and a peer event carries
// it to the call controller; if RING has not arrived yet the number is held
// briefly until the first RING. Empty/withheld numbers stay unknown, and a
// +CLIP that arrives just after a call ended is discarded so it cannot leak
// into the next call.
func (a *Adapter) handleCallerID(peer string) {
	peer = strings.TrimSpace(peer)
	now := time.Now()
	a.mu.Lock()
	if a.active == "" {
		if !a.lastEndedAt.IsZero() && now.Sub(a.lastEndedAt) < clipAfterEndGrace {
			a.mu.Unlock()
			return
		}
		if peer != "" {
			a.pendingPeer = peer
			a.pendingPeerAt = now
		}
		a.mu.Unlock()
		return
	}
	if peer == "" || peer == a.peer {
		a.mu.Unlock()
		return
	}
	a.peer = peer
	callID := a.active
	a.mu.Unlock()
	select {
	case a.events <- modem.ModemEvent{Kind: "peer", CallID: callID, Peer: peer}:
	default:
	}
}

func (a *Adapter) Close() error {
	var err error
	a.close.Do(func() {
		close(a.events)
		if a.client != nil {
			err = a.client.Close()
		}
	})
	return err
}

func (a *Adapter) validateActive(callID modem.CallID) error {
	a.mu.Lock()
	active := a.active
	a.mu.Unlock()
	if active == "" || (callID != "" && active != callID) {
		return fmt.Errorf("active call mismatch")
	}
	return nil
}

func (a *Adapter) finishActive(callID modem.CallID, reason string) {
	a.mu.Lock()
	if a.active == callID {
		a.active = ""
		a.peer = ""
		a.lastEndedAt = time.Now()
		a.pendingPeer = ""
		a.pendingPeerAt = time.Time{}
	}
	a.mu.Unlock()
	select {
	case a.events <- modem.ModemEvent{Kind: "ended", CallID: callID, Raw: reason}:
	default:
	}
}

func firstPayload(lines []string, err error) (string, error) {
	if err != nil {
		return "", err
	}
	return payload(lines), nil
}

func payload(lines []string) string {
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && line != "OK" && !IsFinal(line) && !strings.HasPrefix(line, "AT") {
			return line
		}
	}
	return ""
}

func normalizeSIM(value string) string {
	value = strings.ToLower(value)
	switch {
	case strings.Contains(value, "ready"):
		return "ready"
	case strings.Contains(value, "pin"), strings.Contains(value, "puk"):
		return "locked"
	case strings.Contains(value, "not ready"):
		return "absent"
	default:
		return "unknown"
	}
}

func normalizeRegistration(value string) string {
	value = strings.TrimSpace(value)
	if index := strings.LastIndex(value, ","); index >= 0 {
		value = strings.TrimSpace(value[index+1:])
	}
	switch value {
	case "1", "5":
		return "registered"
	case "2":
		return "searching"
	case "3":
		return "denied"
	default:
		return "unknown"
	}
}

func parseRSSI(value string) int {
	value = strings.TrimSpace(value)
	if index := strings.Index(value, ":"); index >= 0 {
		value = strings.TrimSpace(value[index+1:])
	}
	if index := strings.Index(value, ","); index >= 0 {
		value = value[:index]
	}
	rssi, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || rssi == 99 {
		return 0
	}
	return -113 + rssi*2
}

func signalBars(rssi int) int {
	switch {
	// CSQ 99 ("not known or not detectable") and invalid reports parse to the
	// 0 sentinel — a dBm value real cellular hardware never produces. It must
	// render as no bars, never as maximum signal.
	case rssi == 0:
		return 0
	case rssi >= -75:
		return 4
	case rssi >= -90:
		return 3
	case rssi >= -105:
		return 2
	case rssi > -113:
		return 1
	default:
		return 0
	}
}

var _ modem.ModemControl = (*Adapter)(nil)
