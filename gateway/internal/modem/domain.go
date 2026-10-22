package modem

import (
	"context"
	"errors"
)

type CallID string
type SMSID string
type SMSCursor string

// ErrSMSSubmissionUnknown means the modem accepted the PDU write but the
// final result was lost (for example during a serial timeout). The carrier
// may already have received the message, so callers must not report a plain
// failed send or blindly retry it.
var ErrSMSSubmissionUnknown = errors.New("SMS submission result is unknown")

// ErrActiveCall indicates that a new dial was rejected because the modem
// still owns a call. The API uses it to perform a bounded stale-call cleanup
// before allowing the next dial attempt.
var ErrActiveCall = errors.New("active modem call exists")

// ErrDialUncertain reports that a previous dial's ATD was written but its
// final response was lost, so whether the modem holds a call leg is unknown.
// The adapter refuses new dials until a fresh serialized CLCC confirms there
// is no live voice leg; it never sends an automatic hangup, because a blind
// ATH/CHUP could tear down a new incoming call or an unrelated voice call.
var ErrDialUncertain = errors.New("modem dial outcome uncertain")

type ModemControl interface {
	Probe(context.Context) (Capabilities, error)
	Status(context.Context) (LineStatus, error)
	WaitReady(context.Context) error
	Dial(context.Context, string) (CallID, error)
	Answer(context.Context, CallID) error
	Hangup(context.Context, CallID) error
	SendDTMF(context.Context, CallID, rune) error
	ListSMS(context.Context, SMSCursor) ([]RawSMS, SMSCursor, error)
	SendSMS(context.Context, string, SMSPayload) (SMSID, error)
	DeleteSMS(context.Context, string) error
	Events() <-chan ModemEvent
	Close() error
}

type VoiceAudio interface {
	Probe(context.Context) (AudioCapabilities, error)
	Start(context.Context, CallID) error
	ReadPCM([]int16) (int, error)
	WritePCM([]int16) (int, error)
	Stop(context.Context) error
	Close() error
}

// ATDiagnostic is one bounded, strictly read-only AT query result. It exists
// so a failed dial can be attributed from the modem side (release cause via
// AT+CEER, IMS registration) without opening a second serial reader.
type ATDiagnostic struct {
	Command string   `json:"command"`
	Lines   []string `json:"lines,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// ATDiagnoser exposes the fixed read-only diagnostic allowlist through the
// modem backend's serialized command queue. Implementations must never send
// state-changing commands from this path.
type ATDiagnoser interface {
	Diagnostics(context.Context) ([]ATDiagnostic, error)
}

// OwnedVoiceAudio is implemented by the single-owner audio backends
// (QDC507/ALSA/raw PCM). StopIfOwner releases the capture/playback device ONLY
// when it is still held by callID: a stale socket/bridge teardown can then
// never cut the audio of a newer call that legitimately took over.
type OwnedVoiceAudio interface {
	VoiceAudio
	StopIfOwner(ctx context.Context, callID CallID) bool
	CurrentOwner() CallID
}

type Capabilities struct {
	Vendor              string                 `json:"vendor"`
	Model               string                 `json:"model"`
	USBVID              string                 `json:"usbVid"`
	USBPID              string                 `json:"usbPid"`
	Tier                string                 `json:"tier"`
	SMS                 bool                   `json:"sms"`
	Voice               bool                   `json:"voice"`
	DTMF                bool                   `json:"dtmf"`
	Audio               *AudioCapabilities     `json:"audio,omitempty"`
	Recording           *RecordingCapabilities `json:"recording,omitempty"`
	RequiresBootstrap   bool                   `json:"requiresBootstrap"`
	FirmwareFingerprint string                 `json:"firmwareFingerprint,omitempty"`
}

type RecordingCapabilities struct {
	Supported bool   `json:"supported"`
	Manual    bool   `json:"manual"`
	Auto      bool   `json:"auto"`
	Format    string `json:"format"`
}

type AudioCapabilities struct {
	Backend    string `json:"backend"`
	SampleRate int    `json:"sampleRate"`
	Channels   int    `json:"channels"`
}

type LineStatus struct {
	SIM            string  `json:"sim"`
	Operator       string  `json:"operator,omitempty"`
	Registration   string  `json:"registration"`
	Signal         Signal  `json:"signal"`
	CapabilityTier string  `json:"capabilityTier"`
	Voice          string  `json:"voice"`
	SMS            string  `json:"sms"`
	ActiveCallID   *CallID `json:"activeCallId"`
	// DialUncertain is true while the adapter is fail-closed quarantined:
	// a previous ATD was written but its final response was lost, so new
	// dials are refused (CB-CALL-013) until fresh CLCC reads confirm the
	// line has no live voice leg.
	DialUncertain bool `json:"dialUncertain,omitempty"`
}

const (
	CapabilityUnknown          = "unknown"
	CapabilitySMSOnly          = "sms_only"
	CapabilityVoiceControlOnly = "voice_control_only"
	CapabilityFullVoice        = "full_voice"
)

type Signal struct {
	RSSI int `json:"rssi"`
	Bars int `json:"bars"`
}

type SMSPayload struct {
	Body     string `json:"body"`
	Encoding string `json:"encoding,omitempty"`
	PDU      string `json:"pdu,omitempty"`
	PartNo   int    `json:"partNo,omitempty"`
	Total    int    `json:"totalParts,omitempty"`
}

type RawSMS struct {
	ModemStorage    string
	ModemIndex      int
	Sender          string
	ServiceTimeUnix int64
	RawPDU          []byte
}

type ModemEvent struct {
	Kind   string
	CallID CallID
	Peer   string
	Raw    string
}

// SIMIdentity is the read-only own-number / subscriber / serving-network
// identity read from the SIM and module through the worker's serialized AT
// queue. It is returned only on the loopback internal API (shared token) and
// MUST NOT be rendered to public clients or the console: callers persist it
// privately and expose masked/hashed forms.
//
// Number precedence: an explicit manual override always wins; otherwise the
// SIM-derived own number applies. NumberSource values:
//
//	"sim"         - AT+CNUM returned a usable own number (Number)
//	"empty"       - command OK but the SIM stores no own number
//	"unsupported" - the module rejected AT+CNUM
//	"error"       - the read could not be completed (timeout/truncation)
//
// Metadata never identifies the subscriber precisely: IMSI/ICCID stay raw
// here for private hashing only, operator/cell data describe the serving
// network and are never used to derive or guess the own number.
type SIMIdentity struct {
	Number          string `json:"number"`
	NumberSource    string `json:"numberSource"`
	TypeOfAddress   int    `json:"typeOfAddress,omitempty"`
	ICCID           string `json:"iccid"`
	IMSI            string `json:"imsi"`
	OperatorNumeric string `json:"operatorNumeric"`
	OperatorAlpha   string `json:"operatorAlpha"`
	Registration    string `json:"registration"`
	AccessTech      string `json:"accessTech"`
	CellID          string `json:"cellId"`
	LAC             string `json:"lac"`
	TAC             string `json:"tac"`
	PCI             int    `json:"pci"`
	EARFCN          int    `json:"earfcn"`
	// Commands reports, per read command, ok / unsupported / error / empty so
	// the daemon records precisely what the firmware supports.
	Commands map[string]string `json:"commands"`
}

// IdentityReader is implemented by AT adapters that can read the SIM's own
// number through their existing command queue. It never opens a second
// reader and performs only read commands (AT+CNUM / AT+CCID / AT+CIMI /
// AT+COPS? / registration / one serving-cell query).
type IdentityReader interface {
	OwnIdentity(ctx context.Context) (SIMIdentity, error)
}
