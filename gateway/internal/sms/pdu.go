package sms

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
)

type Encoding string

const (
	EncodingGSM7 Encoding = "gsm7"
	EncodingUCS2 Encoding = "ucs2"
	// Encoding8Bit marks a message whose TP-DCS declares 8-bit/binary user
	// data (or an unknown alphabet). The raw octets are preserved as hex in
	// Body instead of being force-decoded through the GSM 7-bit alphabet,
	// which would fabricate readable-looking mojibake.
	Encoding8Bit Encoding = "8bit"
)

type SubmitSegment struct {
	PDU      string   `json:"pdu"`
	Encoding Encoding `json:"encoding"`
	PartNo   int      `json:"partNo"`
	Total    int      `json:"totalParts"`
	Body     string   `json:"-"`
}

type DecodedMessage struct {
	Sender      string
	Body        string
	Encoding    Encoding
	ConcatRef   uint8
	PartNo      int
	TotalParts  int
	ServiceTime string
}

var ErrInvalidPDU = errors.New("invalid SMS PDU")

// GSM7 is the GSM 03.38 default alphabet. Extension-table characters are
// represented by ESC + extension code and therefore consume two septets.
var gsm7Alphabet = []rune("@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞ\u001bÆæßÉ !\"#¤%&'()*+,-./0123456789:;<=>?¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà")

var gsm7Extension = map[rune]byte{
	'^': 0x14, '{': 0x28, '}': 0x29, '\\': 0x2f,
	'[': 0x3c, '~': 0x3d, ']': 0x3e, '|': 0x40, '€': 0x65,
}

var gsm7Reverse = func() map[rune]byte {
	result := make(map[rune]byte, len(gsm7Alphabet))
	for index, value := range gsm7Alphabet {
		result[value] = byte(index)
	}
	return result
}()

func CanEncodeGSM7(body string) bool {
	for _, value := range body {
		if _, ok := gsm7Reverse[value]; ok {
			continue
		}
		if _, ok := gsm7Extension[value]; !ok {
			return false
		}
	}
	return true
}

func GSM7Length(body string) int {
	length := 0
	for _, value := range body {
		if _, ok := gsm7Extension[value]; ok {
			length += 2
		} else {
			length++
		}
	}
	return length
}

func EncodeSubmitSegments(destination, body string, concatRef uint8) ([]SubmitSegment, error) {
	if strings.TrimSpace(destination) == "" || body == "" {
		return nil, fmt.Errorf("%w: destination and body are required", ErrInvalidPDU)
	}
	encoding := EncodingUCS2
	if CanEncodeGSM7(body) {
		encoding = EncodingGSM7
	}
	parts := splitBody(body, encoding)
	total := len(parts)
	segments := make([]SubmitSegment, 0, total)
	for index, part := range parts {
		udh := []byte(nil)
		if total > 1 {
			udh = []byte{0x05, 0x00, 0x03, concatRef, byte(total), byte(index + 1)}
		}
		pdu, err := encodeSubmit(destination, part, encoding, udh)
		if err != nil {
			return nil, err
		}
		segments = append(segments, SubmitSegment{PDU: pdu, Encoding: encoding, PartNo: index + 1, Total: total, Body: part})
	}
	return segments, nil
}

func splitBody(body string, encoding Encoding) []string {
	max := 160
	if encoding == EncodingUCS2 {
		max = 70
	}
	if (encoding == EncodingGSM7 && GSM7Length(body) <= max) || (encoding == EncodingUCS2 && utf16Length(body) <= max) {
		return []string{body}
	}
	if encoding == EncodingGSM7 {
		max = 153
	} else {
		max = 67
	}
	var parts []string
	var current []rune
	length := 0
	for _, value := range body {
		cost := 1
		if encoding == EncodingGSM7 {
			if _, ok := gsm7Extension[value]; ok {
				cost = 2
			}
		} else if value > 0xffff {
			cost = 2
		}
		if length+cost > max && len(current) > 0 {
			parts = append(parts, string(current))
			current = nil
			length = 0
		}
		current = append(current, value)
		length += cost
	}
	if len(current) > 0 {
		parts = append(parts, string(current))
	}
	return parts
}

func utf16Length(body string) int { return len(utf16.Encode([]rune(body))) }

func encodeSubmit(destination, body string, encoding Encoding, udh []byte) (string, error) {
	address, err := encodeAddress(destination)
	if err != nil {
		return "", err
	}
	firstOctet := byte(0x01) // SMS-SUBMIT, no validity period.
	if len(udh) > 0 {
		firstOctet |= 0x40 // UDHI.
	}
	dcs := byte(0x00)
	var userData []byte
	var userDataLength int
	if encoding == EncodingGSM7 {
		dcs = 0x00
		septets := encodeGSM7(body)
		if len(udh) > 0 {
			userData = packUDHAndSeptets(udh, septets)
			userDataLength = udhSeptetCount(len(udh)) + len(septets)
		} else {
			userData = packSeptets(septets)
			userDataLength = len(septets)
		}
	} else {
		dcs = 0x08 // UCS-2.
		bodyBytes := utf16.Encode([]rune(body))
		encoded := make([]byte, 0, len(bodyBytes)*2)
		for _, value := range bodyBytes {
			encoded = append(encoded, byte(value>>8), byte(value))
		}
		if len(udh) > 0 {
			userData = append(append([]byte(nil), udh...), encoded...)
		} else {
			userData = encoded
		}
		userDataLength = len(userData)
	}
	pdu := []byte{0x00, firstOctet, 0x00}
	pdu = append(pdu, address...)
	pdu = append(pdu, 0x00, dcs, byte(userDataLength))
	pdu = append(pdu, userData...)
	return strings.ToUpper(hex.EncodeToString(pdu)), nil
}

func encodeAddress(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	ton := byte(0x81)
	if strings.HasPrefix(value, "+") {
		ton = 0x91
		value = value[1:]
	}
	if value == "" {
		return nil, fmt.Errorf("%w: empty destination", ErrInvalidPDU)
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return nil, fmt.Errorf("%w: invalid destination %q", ErrInvalidPDU, value)
		}
	}
	result := []byte{byte(len(value)), ton}
	for index := 0; index < len(value); index += 2 {
		low := value[index] - '0'
		high := byte(0x0f)
		if index+1 < len(value) {
			high = value[index+1] - '0'
		}
		result = append(result, low|(high<<4))
	}
	return result, nil
}

func encodeGSM7(body string) []byte {
	result := make([]byte, 0, GSM7Length(body))
	for _, value := range body {
		if code, ok := gsm7Reverse[value]; ok {
			result = append(result, code)
		} else {
			result = append(result, 0x1b, gsm7Extension[value])
		}
	}
	return result
}

func packSeptets(septets []byte) []byte {
	return packBits(nil, septets, 0)
}

func packUDHAndSeptets(udh, septets []byte) []byte {
	// The text septets start at the next septet boundary after the
	// octet-aligned UDH; the intervening bits are zero fill.
	return packBits(udh, septets, udhSeptetCount(len(udh))*7)
}

func packBits(prefix, septets []byte, offset int) []byte {
	result := append([]byte(nil), prefix...)
	for index, septet := range septets {
		bitPosition := offset + index*7
		bytePosition := bitPosition / 8
		bitOffset := bitPosition % 8
		for bit := 0; bit < 7; bit++ {
			if septet&(1<<bit) == 0 {
				continue
			}
			position := bytePosition + (bitOffset+bit)/8
			for len(result) <= position {
				result = append(result, 0)
			}
			result[position] |= 1 << ((bitOffset + bit) % 8)
		}
	}
	return result
}

func unpackSeptets(data []byte, count, offset int) []byte {
	result := make([]byte, 0, count)
	for index := 0; index < count; index++ {
		bitPosition := offset + index*7
		value := byte(0)
		for bit := 0; bit < 7; bit++ {
			position := bitPosition + bit
			if position/8 < len(data) && data[position/8]&(1<<uint(position%8)) != 0 {
				value |= 1 << uint(bit)
			}
		}
		result = append(result, value)
	}
	return result
}

// udhSeptetCount is the number of septets the UDH (plus its fill bits)
// occupies in a GSM 7-bit packed user-data field. The first text septet
// starts at the NEXT septet boundary after the octet-aligned UDH, so for the
// common 6-octet concatenation header it is 7 septets (49 bits), not 48.
// Reading the text at bit 48 instead of 49 shifts every septet by one bit
// and garbles every UDH-bearing GSM7 message (field report 2026-10-05).
func udhSeptetCount(udhBytes int) int {
	if udhBytes <= 0 {
		return 0
	}
	return (udhBytes*8 + 6) / 7
}

// dcsEncoding classifies the alphabet declared by TP-DCS (3GPP TS 23.038
// clause 4, verified against ETSI TS 123 038 V18.0.0):
//
//   - 00xx General Data Coding and 01xx Message Marked for Automatic
//     Deletion: bits 3-2 select the character set (00 GSM7, 01 8-bit,
//     10 UCS2, 11 reserved). Bit 5 indicates 23.042 compression and bit 4
//     class presence; bits 1-0 are only a class in that case.
//   - 1000..1011: reserved coding groups. The spec requires a receiving
//     entity to assume the GSM 7 bit default alphabet for reserved values,
//     so no mojibake is invented for them.
//   - 1100 discard / 1101 store Message Waiting Indication: text is GSM 7
//     bit default alphabet; bits 3..0 are indication state, not alphabet.
//   - 1110 Message Waiting Indication: text is uncompressed UCS2.
//   - 1111 data coding/message class: bit 2 selects GSM7 (0) or 8-bit (1).
//
// Compressed payloads (bit 5) cannot be decoded without the 23.042
// decompressor, so they are preserved as raw hex instead of being passed
// through the 7-bit alphabet.
func dcsEncoding(dcs byte) Encoding {
	switch dcs & 0xC0 {
	case 0x00, 0x40:
		if dcs&0x20 != 0 {
			return Encoding8Bit
		}
		switch dcs & 0x0C {
		case 0x04:
			return Encoding8Bit
		case 0x08:
			return EncodingUCS2
		default:
			return EncodingGSM7
		}
	case 0x80:
		return EncodingGSM7
	default:
		switch dcs & 0xF0 {
		case 0xC0, 0xD0:
			return EncodingGSM7
		case 0xE0:
			return EncodingUCS2
		default:
			if dcs&0x04 != 0 {
				return Encoding8Bit
			}
			return EncodingGSM7
		}
	}
}

func decodeGSM7(septets []byte) string {
	var result strings.Builder
	for index := 0; index < len(septets); index++ {
		value := septets[index]
		if value == 0x1b && index+1 < len(septets) {
			index++
			for character, code := range gsm7Extension {
				if code == septets[index] {
					result.WriteRune(character)
					break
				}
			}
			continue
		}
		if int(value) < len(gsm7Alphabet) {
			result.WriteRune(gsm7Alphabet[value])
		}
	}
	return result.String()
}

func decodeAddress(data []byte, offset *int) (string, error) {
	if *offset+2 > len(data) {
		return "", ErrInvalidPDU
	}
	digitCount := int(data[*offset])
	typeOfNumber := data[*offset+1]
	*offset += 2
	byteCount := (digitCount + 1) / 2
	if *offset+byteCount > len(data) {
		return "", ErrInvalidPDU
	}
	var result strings.Builder
	if typeOfNumber&0x70 == 0x10 {
		result.WriteByte('+')
	}
	for index := 0; index < byteCount; index++ {
		value := data[*offset+index]
		result.WriteByte('0' + value&0x0f)
		if len(result.String())-boolToInt(typeOfNumber&0x70 == 0x10) < digitCount && value>>4 != 0x0f {
			result.WriteByte('0' + value>>4)
		}
	}
	*offset += byteCount
	return result.String(), nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// DecodePDU decodes SMS-DELIVER and SMS-SUBMIT user data. It intentionally
// leaves timestamp formatting as the modem-provided semi-octet string; the
// database layer can convert it without losing the original value.
func DecodePDU(pdu string) (DecodedMessage, error) {
	data, err := hex.DecodeString(strings.TrimSpace(pdu))
	if err != nil {
		return DecodedMessage{}, fmt.Errorf("%w: hex: %v", ErrInvalidPDU, err)
	}
	if len(data) < 2 {
		return DecodedMessage{}, ErrInvalidPDU
	}
	offset := 0
	smscLength := int(data[offset])
	offset++
	if offset+smscLength > len(data) {
		return DecodedMessage{}, ErrInvalidPDU
	}
	offset += smscLength
	if offset >= len(data) {
		return DecodedMessage{}, ErrInvalidPDU
	}
	firstOctet := data[offset]
	offset++
	message := DecodedMessage{}
	if firstOctet&0x03 == 0x01 { // SMS-SUBMIT: TP-MR follows.
		if offset >= len(data) {
			return DecodedMessage{}, ErrInvalidPDU
		}
		offset++
	}
	sender, err := decodeAddress(data, &offset)
	if err != nil {
		return DecodedMessage{}, err
	}
	message.Sender = sender
	if offset+2 > len(data) {
		return DecodedMessage{}, ErrInvalidPDU
	}
	offset++ // PID
	dcs := data[offset]
	offset++
	if firstOctet&0x03 != 0x01 {
		if offset+7 > len(data) {
			return DecodedMessage{}, ErrInvalidPDU
		}
		message.ServiceTime = hex.EncodeToString(data[offset : offset+7])
		offset += 7
	}
	if offset >= len(data) {
		return DecodedMessage{}, ErrInvalidPDU
	}
	userDataLength := int(data[offset])
	offset++
	if offset > len(data) {
		return DecodedMessage{}, ErrInvalidPDU
	}
	userData := data[offset:]
	udhLength := 0
	if firstOctet&0x40 != 0 {
		if len(userData) == 0 {
			return DecodedMessage{}, ErrInvalidPDU
		}
		udhLength = int(userData[0]) + 1
		if udhLength > len(userData) {
			return DecodedMessage{}, ErrInvalidPDU
		}
		parseConcat(userData[1:udhLength], &message)
	}
	switch message.Encoding = dcsEncoding(dcs); message.Encoding {
	case EncodingUCS2:
		payload := userData[udhLength:]
		if len(payload)%2 != 0 {
			return DecodedMessage{}, ErrInvalidPDU
		}
		units := make([]uint16, len(payload)/2)
		for index := range units {
			units[index] = uint16(payload[index*2])<<8 | uint16(payload[index*2+1])
		}
		message.Body = string(utf16.Decode(units))
	case Encoding8Bit:
		// Binary/unknown alphabet: preserve the raw octets as hex rather
		// than inventing text from them.
		message.Body = hex.EncodeToString(userData[udhLength:])
	default:
		// Decode the full septet stream from the start of the user data,
		// then skip the septets occupied by the UDH and its fill bits. This
		// is equivalent to a septet-aligned start offset and is correct for
		// both UDH and non-UDH messages.
		septets := unpackSeptets(userData, userDataLength, 0)
		skip := 0
		if firstOctet&0x40 != 0 {
			skip = udhSeptetCount(udhLength)
		}
		if skip >= len(septets) {
			message.Body = ""
		} else {
			message.Body = decodeGSM7(septets[skip:])
		}
	}
	return message, nil
}

func parseConcat(udh []byte, message *DecodedMessage) {
	for index := 0; index+1 < len(udh); {
		length := int(udh[index+1])
		if index+2+length > len(udh) {
			return
		}
		if udh[index] == 0x00 && length == 3 {
			message.ConcatRef = udh[index+2]
			message.TotalParts = int(udh[index+3])
			message.PartNo = int(udh[index+4])
			return
		}
		index += 2 + length
	}
}
