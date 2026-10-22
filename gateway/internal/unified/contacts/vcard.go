package contacts

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Import limits are enforced before parsing so a huge or hostile file cannot
// consume unbounded memory or a single principal's quota.
const (
	MaxImportBytes   = 512 * 1024
	MaxCards         = 1000
	MaxFieldsPerCard = 100
	MaxValueBytes    = 2000
)

// ErrTooLarge is returned when an import exceeds the byte/card limits.
var ErrTooLarge = errors.New("vCard import exceeds limits")

// Field is one labeled value from a vCard property (also the wire form for
// mobile sync and web edits).
type Field struct {
	Label string `json:"label,omitempty"`
	Value string `json:"value"`
}

// ParsedContact is one vCard converted to the fields the gateway stores.
type ParsedContact struct {
	CardIndex    int
	Version      string
	DisplayName  string
	GivenName    string
	FamilyName   string
	Organization string
	Nickname     string
	Note         string
	// SearchKey is optional precomputed pinyin/T9 text supplied by the mobile
	// sync (vCard parsing leaves it empty: the server does not transliterate).
	SearchKey string
	// ClientRef is the mobile device's stable contact identifier, used to
	// suppress resurrection of an explicitly deleted contact.
	ClientRef string
	Phones    []Field
	Emails    []Field
	URLs      []Field
}

// ParseVCards parses a UTF-8 vCard 2.1/3.0/4.0 stream. It supports folded
// lines, backslash escapes, QUOTED-PRINTABLE values and multiple TEL/EMAIL
// entries. Warnings describe skipped properties or cards without failing the
// whole import; malformed cards are counted, never imported silently.
//
// Charset note: property values are decoded as UTF-8. A declared legacy
// charset (for example CHARSET=GB2312) that is not valid UTF-8 is replaced
// with U+FFFD and reported, rather than guessing a lossy conversion.
func ParseVCards(data []byte) ([]ParsedContact, []string, error) {
	if len(data) > MaxImportBytes {
		return nil, nil, fmt.Errorf("%w: %d bytes", ErrTooLarge, len(data))
	}
	logical := unfold(string(data))
	if len(logical) == 0 {
		return nil, []string{"empty vCard file"}, nil
	}

	var contacts []ParsedContact
	var warnings []string
	var current *ParsedContact
	inCard := false
	cardFields := 0
	startedCards := 0
	var version string

	flush := func() {
		if current == nil {
			return
		}
		if !current.hasIdentity() {
			warnings = append(warnings, fmt.Sprintf("card %d skipped: no name or number", current.CardIndex+1))
			current = nil
			return
		}
		contacts = append(contacts, *current)
		current = nil
	}

	for _, line := range logical {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, params, value, ok := splitProperty(line)
		if !ok {
			warnings = append(warnings, "unparsable line skipped")
			continue
		}
		switch strings.ToUpper(name) {
		case "BEGIN":
			if strings.EqualFold(value, "VCARD") {
				if startedCards >= MaxCards {
					return nil, nil, fmt.Errorf("%w: more than %d cards", ErrTooLarge, MaxCards)
				}
				inCard = true
				cardFields = 0
				current = &ParsedContact{CardIndex: startedCards}
				startedCards++
				version = ""
			}
			continue
		case "END":
			if strings.EqualFold(value, "VCARD") && inCard {
				if current != nil {
					current.Version = version
				}
				flush()
				inCard = false
			}
			continue
		}
		if !inCard || current == nil {
			continue
		}
		cardFields++
		if cardFields > MaxFieldsPerCard {
			warnings = append(warnings, fmt.Sprintf("card %d exceeds %d fields; extra fields ignored", current.CardIndex+1, MaxFieldsPerCard))
			continue
		}
		if len(value) > MaxValueBytes {
			value = value[:MaxValueBytes]
		}
		encoding := paramValue(params, "ENCODING")
		charset := paramValue(params, "CHARSET")
		if strings.Contains(strings.ToUpper(encoding), "QUOTED-PRINTABLE") || strings.Contains(strings.ToUpper(encoding), "QP") {
			decoded, qpErr := decodeQuotedPrintable(value)
			if qpErr != nil {
				warnings = append(warnings, fmt.Sprintf("card %d: invalid quoted-printable value skipped", current.CardIndex+1))
				continue
			}
			value = decoded
		} else if strings.EqualFold(encoding, "B") || strings.EqualFold(encoding, "BASE64") {
			// Binary/photo payloads are deliberately not imported.
			continue
		}
		if charset != "" && !isUTF8Like(charset) {
			if !utf8.ValidString(value) {
				warnings = append(warnings, fmt.Sprintf("card %d: %s value is not UTF-8; replacement characters used", current.CardIndex+1, strings.ToUpper(charset)))
			}
		}
		value = strings.ToValidUTF8(value, "\uFFFD")

		switch strings.ToUpper(name) {
		case "VERSION":
			version = strings.TrimSpace(value)
			if version != "2.1" && version != "3.0" && version != "4.0" {
				warnings = append(warnings, fmt.Sprintf("card %d: unsupported vCard version %q", current.CardIndex+1, version))
			}
		case "FN":
			if current.DisplayName == "" {
				current.DisplayName = unescapeText(value)
			}
		case "N":
			parts := splitEscaped(value, ';')
			if len(parts) > 0 && current.FamilyName == "" {
				current.FamilyName = unescapeText(parts[0])
			}
			if len(parts) > 1 && current.GivenName == "" {
				current.GivenName = unescapeText(parts[1])
			}
		case "ORG":
			if current.Organization == "" {
				unit := splitEscaped(value, ';')
				current.Organization = unescapeText(strings.TrimSpace(unit[0]))
			}
		case "NICKNAME":
			if current.Nickname == "" {
				current.Nickname = unescapeText(strings.TrimSpace(value))
			}
		case "NOTE":
			if current.Note == "" {
				current.Note = unescapeText(strings.TrimSpace(value))
			}
		case "TEL":
			if number := normalizeTEL(value); number != "" {
				current.Phones = append(current.Phones, Field{Label: telLabel(params), Value: number})
			}
		case "EMAIL":
			if email := strings.TrimSpace(unescapeText(value)); email != "" {
				current.Emails = append(current.Emails, Field{Label: telLabel(params), Value: email})
			}
		case "URL":
			if url := strings.TrimSpace(unescapeText(value)); url != "" {
				current.URLs = append(current.URLs, Field{Label: telLabel(params), Value: url})
			}
		}
	}
	if inCard && current != nil {
		flush()
	}
	if len(contacts) == 0 && len(warnings) == 0 {
		warnings = append(warnings, "no vCard entries found")
	}
	return contacts, warnings, nil
}

func (c *ParsedContact) hasIdentity() bool {
	return c.DisplayName != "" || c.GivenName != "" || c.FamilyName != "" ||
		c.Organization != "" || c.Nickname != "" || len(c.Phones) > 0 || len(c.Emails) > 0
}

// unfold joins physical lines: a line starting with space/tab continues the
// previous one (RFC 2425 folding), and a QUOTED-PRINTABLE soft break ending
// in '=' continues into the next line. Blank lines separate cards.
func unfold(data string) []string {
	data = strings.ReplaceAll(data, "\r\n", "\n")
	data = strings.ReplaceAll(data, "\r", "\n")
	physical := strings.Split(data, "\n")
	var logical []string
	var current strings.Builder
	for _, line := range physical {
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			if current.Len() > 0 {
				current.WriteString(strings.TrimLeft(line, " \t"))
				continue
			}
			line = strings.TrimLeft(line, " \t")
		}
		text := current.String()
		if strings.HasSuffix(text, "=") {
			// QP soft break: the '=' is dropped and the next physical line
			// continues the same logical property.
			current.Reset()
			current.WriteString(strings.TrimSuffix(text, "="))
			current.WriteString(strings.TrimLeft(line, " \t"))
			continue
		}
		if current.Len() > 0 {
			logical = append(logical, current.String())
		}
		current.Reset()
		current.WriteString(line)
	}
	if current.Len() > 0 {
		logical = append(logical, current.String())
	}
	return logical
}

// splitProperty splits "NAME;PARAM=...:value" at the first unquoted colon.
func splitProperty(line string) (name string, params []string, value string, ok bool) {
	inQuotes := false
	colon := -1
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			inQuotes = !inQuotes
		case ':':
			if !inQuotes {
				colon = i
				i = len(line)
			}
		}
	}
	if colon < 0 {
		return "", nil, "", false
	}
	head := line[:colon]
	value = line[colon+1:]
	segments := splitUnquoted(head, ';')
	if len(segments) == 0 || strings.TrimSpace(segments[0]) == "" {
		return "", nil, "", false
	}
	return segments[0], segments[1:], value, true
}

func splitUnquoted(input string, sep byte) []string {
	var parts []string
	var current strings.Builder
	inQuotes := false
	for i := 0; i < len(input); i++ {
		c := input[i]
		switch {
		case c == '"':
			inQuotes = !inQuotes
			current.WriteByte(c)
		case c == sep && !inQuotes:
			parts = append(parts, current.String())
			current.Reset()
		default:
			current.WriteByte(c)
		}
	}
	parts = append(parts, current.String())
	return parts
}

func paramValue(params []string, key string) string {
	for _, param := range params {
		pieces := strings.SplitN(param, "=", 2)
		if len(pieces) != 2 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(pieces[0]), key) {
			return strings.Trim(strings.TrimSpace(pieces[1]), "\"")
		}
	}
	return ""
}

func telLabel(params []string) string {
	var labels []string
	for _, param := range params {
		pieces := strings.SplitN(param, "=", 2)
		if len(pieces) != 2 {
			// vCard 2.1 bare type: TEL;CELL;VOICE:...
			bare := strings.ToUpper(strings.TrimSpace(param))
			if isTelType(bare) {
				labels = append(labels, bare)
			}
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(pieces[0]), "TYPE") {
			continue
		}
		for _, value := range strings.Split(strings.Trim(strings.TrimSpace(pieces[1]), "\""), ",") {
			value = strings.ToUpper(strings.TrimSpace(value))
			if isTelType(value) {
				labels = append(labels, value)
			}
		}
	}
	if len(labels) == 0 {
		return ""
	}
	return labels[0]
}

func isTelType(value string) bool {
	switch value {
	case "CELL", "MOBILE", "HOME", "WORK", "FAX", "PAGER", "VOICE", "TEXT", "MAIN", "OTHER", "IPHONE":
		return true
	}
	return false
}

// normalizeTEL accepts plain numbers and URI values ("tel:+86...").
func normalizeTEL(value string) string {
	value = strings.TrimSpace(unescapeText(value))
	if len(value) >= 4 && strings.EqualFold(value[:4], "tel:") {
		value = value[4:]
	}
	if Digits(value) == "" {
		return ""
	}
	return strings.TrimSpace(value)
}

// unescapeText resolves vCard backslash escapes.
func unescapeText(value string) string {
	var builder strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' || i+1 >= len(value) {
			builder.WriteByte(value[i])
			continue
		}
		i++
		switch value[i] {
		case 'n', 'N':
			builder.WriteByte('\n')
		case ',', ';', '\\', ':':
			builder.WriteByte(value[i])
		default:
			builder.WriteByte(value[i])
		}
	}
	return builder.String()
}

// splitEscaped splits on an unescaped separator.
func splitEscaped(value string, sep byte) []string {
	var parts []string
	var current strings.Builder
	for i := 0; i < len(value); i++ {
		switch {
		case value[i] == '\\' && i+1 < len(value):
			current.WriteByte(value[i])
			i++
			current.WriteByte(value[i])
		case value[i] == sep:
			parts = append(parts, current.String())
			current.Reset()
		default:
			current.WriteByte(value[i])
		}
	}
	parts = append(parts, current.String())
	return parts
}

func decodeQuotedPrintable(value string) (string, error) {
	var out []byte
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c == '_' {
			out = append(out, ' ')
			continue
		}
		if c != '=' {
			out = append(out, c)
			continue
		}
		if i+2 < len(value) {
			if b, err := strconv.ParseUint(value[i+1:i+3], 16, 8); err == nil {
				out = append(out, byte(b))
				i += 2
				continue
			}
		}
		if i+1 == len(value) {
			// trailing soft break
			continue
		}
		return "", fmt.Errorf("invalid quoted-printable escape")
	}
	return string(out), nil
}

func isUTF8Like(charset string) bool {
	switch strings.ToUpper(strings.ReplaceAll(charset, "-", "")) {
	case "UTF8", "UTF", "USASCII", "ASCII", "ISO88591", "LATIN1":
		return true
	}
	return false
}
