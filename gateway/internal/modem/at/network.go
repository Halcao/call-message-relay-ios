package at

// Bounded, strictly read-only parsing of subscriber/serving-network
// metadata: AT+CIMI (IMSI), AT+COPS? (operator), AT+CREG/CGREG/CEREG?
// (registration) and a single AT+QENG="servingcell" query (LAC/TAC/cell id).
// Nothing here selects a network, changes radio state, or derives/guesses an
// MSISDN: operator/cell data only describe the serving network. Raw IMSI and
// ICCID are private; callers hash them before persistence/display.

import (
	"strconv"
	"strings"
)

// NetworkMeta holds parsed serving-network identifiers.
type NetworkMeta struct {
	IMSI            string
	OperatorNumeric string
	OperatorAlpha   string
	Registration    string
	AccessTech      string
	CellID          string
	LAC             string
	TAC             string
	PCI             int
	EARFCN          int
}

func firstPrefixed(lines []string, prefixes ...string) (string, bool) {
	for _, line := range lines {
		upper := strings.ToUpper(strings.TrimSpace(line))
		for _, prefix := range prefixes {
			if strings.HasPrefix(upper, prefix) {
				return strings.TrimSpace(line), true
			}
		}
	}
	return "", false
}

// ParseIMSI extracts the subscriber identity from an AT+CIMI response
// (bare digits on their own line, 14-15 digits).
func ParseIMSI(lines []string) string {
	for _, line := range lines {
		token := strings.TrimSpace(line)
		if strings.EqualFold(token, "OK") || strings.EqualFold(token, "ERROR") ||
			strings.HasPrefix(strings.ToUpper(token), "AT") || strings.HasPrefix(strings.ToUpper(token), "+CME ERROR") {
			continue
		}
		if len(token) >= 14 && len(token) <= 15 && isAllDigits(token) {
			return token
		}
	}
	return ""
}

func isAllDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ParseCOPS parses '+COPS: <mode>,<format>,<oper>[,<AcT>]' where oper is an
// alpha string for format 0/1 and a numeric MCC+MNC id for format 2. The
// trailing AcT byte (e.g. the '7' in +COPS: 0,0,"CHN-UNICOM",7) is not an
// operator code and must not be mistaken for one.
func ParseCOPS(lines []string) (numeric, alpha string) {
	line, ok := firstPrefixed(lines, "+COPS:")
	if !ok {
		return "", ""
	}
	fields := splitCNUMFields(strings.TrimSpace(line[strings.Index(line, ":")+1:]))
	if len(fields) < 3 {
		return "", ""
	}
	format, _ := strconv.Atoi(strings.TrimSpace(fields[1]))
	oper := strings.TrimSpace(fields[2])
	if format == 2 {
		numeric = oper // already digits
	} else if isAllDigits(oper) {
		numeric = oper // format 1: quoted numeric id
	} else {
		alpha = oper
	}
	return numeric, alpha
}

// registrationText maps a CREG/CGREG/CEREG <stat> byte to a stable word.
func registrationText(stat string) string {
	switch strings.TrimSpace(stat) {
	case "1":
		return "registered_home"
	case "5":
		return "registered_roaming"
	case "2":
		return "searching"
	case "3":
		return "denied"
	case "0":
		return "not_registered"
	default:
		return "unknown"
	}
}

// ParseRegistration parses a +CREG/+CGREG/+CEREG unsolicited/read response.
// LTE CEREG carries <tac>,<ci>; 2G/3G CREG carries <lac>,<ci>.
func ParseRegistration(lines []string, lte bool) (registration, lac, tac, cell string) {
	prefix := "+CREG:"
	if lte {
		prefix = "+CEREG:"
	}
	line, ok := firstPrefixed(lines, prefix)
	if !ok {
		if lte {
			line, ok = firstPrefixed(lines, "+CGREG:")
		}
		if !ok {
			return "unknown", "", "", ""
		}
	}
	fields := splitCNUMFields(strings.TrimSpace(line[strings.Index(line, ":")+1:]))
	if len(fields) < 2 {
		return "unknown", "", "", ""
	}
	registration = registrationText(fields[1])
	if lte && len(fields) >= 4 {
		tac = strings.TrimSpace(fields[2])
		cell = strings.TrimSpace(fields[3])
	} else if !lte && len(fields) >= 4 {
		lac = strings.TrimSpace(fields[2])
		cell = strings.TrimSpace(fields[3])
	}
	return registration, lac, tac, cell
}

// ParseServingCell parses one AT+QENG="servingcell" information element.
// Field layouts vary by RAT; only stable identifier positions are extracted
// and everything is defensive (unsupported layouts yield empty fields).
// Observed shapes (synthetic):
//
//	+QENG: "servingcell","NOCONN","LTE","FDD",460,01,296649,123,1650,3,1,1,1234,...
//	+QENG: "servingcell","NOCONN","GSM",460,01,1234,5678,...
func ParseServingCell(lines []string) NetworkMeta {
	var meta NetworkMeta
	line, ok := firstPrefixed(lines, "+QENG:")
	if !ok {
		return meta
	}
	raw := splitCNUMFields(strings.TrimSpace(line[strings.Index(line, ":")+1:]))
	clean := make([]string, 0, len(raw))
	for _, field := range raw {
		clean = append(clean, strings.ToUpper(strings.TrimSpace(strings.Trim(field, `"`))))
	}
	if len(clean) == 0 {
		return meta
	}
	// Locate the RAT token: fields[0] is the literal "servingcell" response
	// header on Quectel firmware, so the RAT is at index 2; tolerate a layout
	// without the header where fields[0] is the connection state.
	rat := ""
	mccAt, mncAt, lacAt, cellAt, pciAt, earfcnAt, tacAt := -1, -1, -1, -1, -1, -1, -1
	if clean[0] == "SERVINGCELL" && len(clean) > 2 {
		rat = clean[2]
		switch rat {
		case "LTE":
			mccAt, mncAt, cellAt, pciAt, earfcnAt, tacAt = 4, 5, 6, 7, 8, 12
		case "GSM", "WCDMA":
			mccAt, mncAt, lacAt, cellAt = 3, 4, 5, 6
		}
	} else if len(clean) > 1 {
		rat = clean[1]
		switch rat {
		case "LTE":
			mccAt, mncAt, cellAt, pciAt, earfcnAt, tacAt = 3, 4, 5, 6, 7, 11
		case "GSM", "WCDMA":
			mccAt, mncAt, lacAt, cellAt = 2, 3, 4, 5
		}
	}
	meta.AccessTech = map[string]string{
		"GSM": "gsm", "WCDMA": "wcdma", "LTE": "lte", "NR5G": "nr5g",
	}[rat]
	get := func(index int) string {
		if index >= 0 && index < len(clean) {
			return clean[index]
		}
		return ""
	}
	if mccAt >= 0 {
		meta.OperatorNumeric = get(mccAt) + get(mncAt)
	}
	if value := get(cellAt); value != "" {
		meta.CellID = value
	}
	if value := get(lacAt); value != "" {
		meta.LAC = value
	}
	if value := get(tacAt); value != "" {
		meta.TAC = value
	}
	if value := get(pciAt); isAllDigits(value) {
		meta.PCI, _ = strconv.Atoi(value)
	}
	if value := get(earfcnAt); isAllDigits(value) {
		meta.EARFCN, _ = strconv.Atoi(value)
	}
	return meta
}
