package at

// Read-only parsing of SIM identity responses (AT+CNUM own number and
// AT+CCID). These functions are pure so they can be unit-tested with
// synthetic values; they issue no modem I/O themselves and contain no real
// identifiers.
//
// The QDC507 firmware does not follow the textbook 27.007 shape exactly:
//
//	+CNUM: ,"+13800138000",145   (alpha present, empty and UNQUOTED)
//	+CCID: 0123456789ABCDEF0123   (prefixed; token contains hex A-F nibbles)
//
// A regex expecting a quoted alpha token or an all-digits ICCID never
// matched, which is why the registry identity fields stayed empty despite a
// real number being present.

import (
	"encoding/csv"
	"regexp"
	"strconv"
	"strings"
)

// OwnNumber is one parsed AT+CNUM entry.
type OwnNumber struct {
	// Number is the canonical display number. National numbers are kept
	// verbatim (no '+', no invented country code); international numbers
	// keep the single '+' the SIM itself stored.
	Number        string
	TypeOfAddress int
	Alpha         string
	International bool
}

// OwnNumberResult distinguishes "SIM stores no number" (OK with no entries)
// from a failed/unsupported command and an incomplete (truncated/echo-only)
// transcript.
type OwnNumberResult struct {
	OK          bool
	Error       bool
	Incomplete  bool
	Unsupported bool
	Entries     []OwnNumber
}

// Found reports whether at least one usable own-number entry was present.
func (r OwnNumberResult) Found() bool { return len(r.Entries) > 0 }

// Number returns the first canonical entry or "" for an empty/failed read.
func (r OwnNumberResult) Number() string {
	if len(r.Entries) == 0 {
		return ""
	}
	return r.Entries[0].Number
}

var (
	cnumPrefix    = "+CNUM:"
	ccidPrefix    = "+CCID:"
	clipPrefix    = "+CLIP:"
	iccidReadToken = "+ICCID"
	digitsTokenRE = regexp.MustCompile(`\d`)
	// Own number: optional '+' and 3-20 digits only. Any '*' or '#' makes it
	// a supplementary-service control string, which is never an own number.
	ownNumberRE = regexp.MustCompile(`^\+?\d{3,20}$`)
	nonDigitRE  = regexp.MustCompile(`\D`)
	// ICCID token: whole token must be 18-22 hex chars (ICCID printed in hex,
	// possibly with A-F nibbles) or 18-20 digits. Arbitrary text is never
	// character-stripped into an identity.
	iccidHexRE    = regexp.MustCompile(`^[0-9A-Fa-f]{18,22}$`)
	iccidDigitsRE = regexp.MustCompile(`^\d{18,20}$`)
)

// splitCNUMFields parses the CSV payload of one +CNUM line, accepting both
// quoted and empty-unquoted alpha fields.
func splitCNUMFields(payload string) []string {
	reader := csv.NewReader(strings.NewReader(payload))
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true
	reader.LazyQuotes = true
	if records, err := reader.ReadAll(); err == nil && len(records) > 0 {
		return records[0]
	}
	return strings.Split(payload, ",")
}

// canonicalNumber normalizes a CNUM number without inventing a country code.
// The '+' the SIM stored is the only authority for international dialling;
// when it is absent the national digits are kept verbatim even if the
// type-of-address byte claims international (firmware commonly stores 145
// for national numbers).
func canonicalNumber(raw string, toa int, hasTOA bool) (number string, international bool) {
	raw = strings.TrimSpace(raw)
	if !ownNumberRE.MatchString(raw) {
		return "", false
	}
	explicitPlus := strings.HasPrefix(raw, "+")
	digits := nonDigitRE.ReplaceAllString(raw, "")
	toaInternational := false
	if hasTOA {
		v := toa & 0xFF
		// Type-of-number = international (145 = 128 + national NPI 17 etc.)
		toaInternational = v >= 128 && v <= 163
	}
	if explicitPlus {
		return "+" + digits, true
	}
	if toaInternational {
		// Inconsistent TOA: trust the stored digits, keep national form.
		return digits, false
	}
	return digits, toaInternational
}

// ParseCNUM parses a full AT+CNUM transcript. Scoping starts at the command
// echo and ends at that segment's first OK/ERROR/+CME ERROR, so an unrelated
// command's result later in a combined transcript cannot invalidate CNUM.
// Entries are returned only after a terminal OK; an ERROR or an incomplete
// (echo-only/truncated) transcript never exposes partial entries.
func ParseCNUM(transcript string) OwnNumberResult {
	var tentative []OwnNumber
	terminalOK := false
	bareOK := false
	errorResult := false
	unsupported := false
	active := false
	for _, rawLine := range strings.Split(transcript, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(rawLine, "\r"))
		upper := strings.ToUpper(line)
		if upper == "AT+CNUM" {
			active = true
			bareOK = false
			tentative = nil
			terminalOK, errorResult, unsupported = false, false, false
			continue
		}
		// A transcript can begin directly with a +CNUM response line (no
		// echo); activate on the first such line so both shapes parse.
		if !active && strings.HasPrefix(upper, cnumPrefix) {
			active = true
		}
		if upper == "OK" {
			if active {
				terminalOK = true
				break
			}
			// Only OK so far (worker filters the echo): empty SIM record.
			bareOK = true
			continue
		}
		if upper == "ERROR" {
			errorResult = true
			if !active {
				break
			}
			break
		}
		if !active {
			continue
		}
		if strings.HasPrefix(upper, "+CME ERROR") || strings.HasPrefix(upper, "+CMS ERROR") {
			errorResult = true
			tail := ""
			if parts := strings.SplitN(line, ":", 2); len(parts) == 2 {
				tail = strings.ToLower(strings.TrimSpace(parts[1]))
			}
			switch tail {
			case "4", "unknown command", "operation not supported", "command not supported":
				unsupported = true
			}
			break
		}
		if !strings.HasPrefix(upper, cnumPrefix) {
			continue
		}
		payload := strings.TrimSpace(line[len(cnumPrefix):])
		fields := splitCNUMFields(payload)
		if len(fields) < 2 {
			continue
		}
		alpha := strings.TrimSpace(fields[0])
		numberRaw := strings.TrimSpace(fields[1])
		// Reject supplementary-service control strings (*#31# and friends)
		// outright, regardless of digit count.
		if strings.ContainsAny(numberRaw, "*#") || !digitsTokenRE.MatchString(numberRaw) {
			continue
		}
		toa := 0
		hasTOA := false
		if len(fields) >= 3 && strings.TrimSpace(fields[2]) != "" {
			if parsed, err := strconv.Atoi(strings.TrimSpace(fields[2])); err == nil {
				toa, hasTOA = parsed, true
			}
		}
		number, international := canonicalNumber(numberRaw, toa, hasTOA)
		if number != "" {
			tentative = append(tentative, OwnNumber{
				Number: number, TypeOfAddress: toa, Alpha: alpha, International: international,
			})
		}
	}
	if errorResult {
		return OwnNumberResult{Error: true, Unsupported: unsupported}
	}
	if terminalOK {
		return OwnNumberResult{OK: true, Entries: tentative}
	}
	if bareOK && !active {
		// Worker filters the command echo: a lone "OK" means an empty SIM.
		return OwnNumberResult{OK: true}
	}
	return OwnNumberResult{Incomplete: true}
}

// ParseCLIP parses a calling-line-identification URC:
//
//	+CLIP: "<number>",<type>[,<subaddr>,<satype>[,<alpha>]]
//
// It returns the caller number and whether the line was a CLIP URC at all.
// Withheld, empty and malformed numbers return ("", true) so the caller
// treats them as unknown instead of inventing a value; the strict
// own-number shape (digits with at most one leading '+') is reused so a
// supplementary-service string can never masquerade as a caller.
func ParseCLIP(line string) (string, bool) {
	line = strings.TrimSpace(line)
	upper := strings.ToUpper(line)
	if !strings.HasPrefix(upper, clipPrefix) {
		return "", false
	}
	payload := strings.TrimSpace(line[len(clipPrefix):])
	fields := splitCNUMFields(payload)
	if len(fields) == 0 {
		return "", true
	}
	raw := strings.Trim(strings.TrimSpace(fields[0]), `"`)
	if !ownNumberRE.MatchString(raw) {
		return "", true
	}
	return raw, true
}

// ParseCCID parses an AT+CCID (or AT+ICCID?) transcript into a canonical
// upper-case hex ICCID. Whole-token shapes only: a '+CCID:' value
// (optionally quoted) or a bare response line that is entirely 18-22 hex
// chars / 18-20 digits. Noise text is ignored rather than stripped into an
// accidental identity. Returns "" when absent.
func ParseCCID(transcript string) string {
	for _, rawLine := range strings.Split(transcript, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(rawLine, "\r"))
		upper := strings.ToUpper(line)
		if strings.HasPrefix(upper, "AT+") || upper == "OK" || upper == "ERROR" || strings.HasPrefix(upper, "+CME ERROR") {
			continue
		}
		token := line
		if strings.HasPrefix(upper, ccidPrefix) {
			token = strings.TrimSpace(line[len(ccidPrefix):])
		} else if strings.Contains(upper, iccidReadToken) {
			if index := strings.Index(line, ":"); index >= 0 {
				token = strings.TrimSpace(line[index+1:])
			}
		}
		token = strings.Trim(strings.TrimSpace(token), `"`)
		if token == "" {
			continue
		}
		if iccidHexRE.MatchString(token) {
			return strings.ToUpper(token)
		}
		if iccidDigitsRE.MatchString(token) {
			return token
		}
	}
	return ""
}
