package runtime

// Private SIM/own-number identity lifecycle. The worker reads the SIM over
// its serialized AT queue and returns the raw number/IMSI only on the
// loopback internal API; this file hashes/masks those values before they are
// persisted and exposes only friendly, safe fields to API/console views.
//
// Precedence: an operator manual override always wins. A SIM-derived number
// is bound to the ICCID hash: a changed card immediately invalidates the
// stale auto number (and is flagged pending confirmation) instead of letting
// the new SIM inherit the previous number/permissions.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"callrelay/gateway/internal/unified/store"
)

const (
	numberSourceNone        = "none"
	numberSourceSIM         = "sim"
	numberSourceManual      = "manual"
	numberSourceEmpty       = "empty"
	numberSourceUnsupported = "unsupported"
	numberSourceError       = "error"
	numberSourceSIMChanged  = "sim_changed"
)

var nonDigitsRE = regexp.MustCompile(`\D`)

// manualNumberRE is the strict stored form of an own number: 3-20 digits with
// at most one leading '+'. It mirrors the SIM/CNUM parser so a manually
// entered value can never be a supplementary-service/control string and
// every surface renders one normalized shape.
var manualNumberRE = regexp.MustCompile(`^\+?[0-9]{3,20}$`)

// normalizeManualNumber trims the typed value, removes the separators people
// actually type (spaces, dashes, parentheses, NBSP) and validates the result.
// An empty input is valid and means "clear the override"; '*'/'#' or any
// other control character is rejected rather than stripped into a number.
func normalizeManualNumber(input string) (string, bool) {
	number := strings.TrimSpace(input)
	if number == "" {
		return "", true
	}
	if strings.ContainsAny(number, "*#") {
		return "", false
	}
	normalized := strings.NewReplacer(
		" ", "", "\u00a0", "", "\t", "",
		"-", "", "(", "", ")", "",
	).Replace(number)
	if !manualNumberRE.MatchString(normalized) {
		return "", false
	}
	return normalized, true
}

func hash16(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])[:16]
}

func simHashFromICCID(iccid string) string {
	token := strings.ToUpper(regHexOnly.ReplaceAllString(iccid, ""))
	return hash16(token)
}

var regHexOnly = regexp.MustCompile(`[^0-9A-Fa-f]`)

func maskPhone(value string) string {
	digits := nonDigitsRE.ReplaceAllString(value, "")
	if len(digits) >= 7 {
		return digits[:3] + strings.Repeat("*", len(digits)-7) + digits[len(digits)-4:]
	}
	return ""
}

func maskICCID(value string) string {
	token := strings.ToUpper(regHexOnly.ReplaceAllString(value, ""))
	if len(token) >= 8 {
		return token[:4] + strings.Repeat("*", len(token)-8) + token[len(token)-4:]
	}
	return ""
}

// resolvedNumber applies manual-override/SIM precedence to a persisted row.
// It returns the FULL effective number (private), a masked form, and the
// authoritative source. The full number is only placed in authenticated,
// line-authorized responses; masked/source are safe for any surface.
func resolvedNumber(identity store.LineIdentity) (full, masked, source string) {
	if manual := strings.TrimSpace(identity.ManualNumber); manual != "" {
		return manual, maskPhone(manual), numberSourceManual
	}
	switch identity.NumberSource {
	case numberSourceSIM:
		return identity.PhoneNumber, maskPhone(identity.PhoneNumber), numberSourceSIM
	case numberSourceEmpty:
		return "", "", numberSourceEmpty
	case numberSourceUnsupported:
		return "", "", numberSourceUnsupported
	case numberSourceSIMChanged:
		return "", "", numberSourceSIMChanged
	case numberSourceError:
		return "", "", numberSourceError
	default:
		return "", "", numberSourceNone
	}
}

// refreshSIMIdentity queries the worker once and reconciles the private
// identity row. It never blocks line operation and swallows I/O errors (the
// next poll retries). Returns true when the resolved public fields changed.
func (m *Manager) refreshSIMIdentity(ctx context.Context, line *Line) bool {
	identity, err := line.Client.SIMIdentity(ctx)
	if err != nil {
		return false
	}
	row, err := m.Store.GetLineIdentity(ctx, line.Cfg.ID)
	if err != nil {
		slog.Warn("load line identity failed", "line", line.Cfg.ID, "error", err)
		return false
	}

	newSIMHash := simHashFromICCID(identity.ICCID)
	_, previousMasked, previousSource := resolvedNumber(row)
	manual := strings.TrimSpace(row.ManualNumber)

	row.LineID = line.Cfg.ID
	row.IMSIHash = hash16(identity.IMSI)
	row.ICCIDMasked = maskICCID(identity.ICCID)
	row.OperatorAlpha = identity.OperatorAlpha
	row.OperatorNumeric = identity.OperatorNumeric
	row.Registration = identity.Registration
	row.AccessTech = identity.AccessTech
	row.CellID = strings.ToUpper(identity.CellID)
	row.LAC = strings.ToUpper(identity.LAC)
	row.TAC = strings.ToUpper(identity.TAC)
	row.PCI = identity.PCI
	row.EARFCN = identity.EARFCN
	row.Commands = identity.Commands
	row.UpdatedAt = time.Now().UTC()

	simPending := false
	switch {
	case newSIMHash != "" && row.SIMHash != "" && row.SIMHash != newSIMHash:
		// The physical SIM changed. Invalidate the stale auto number; the
		// registry's confirm-sim gate keeps the worker stopped until an
		// operator confirms the module. A manual override is retained.
		slog.Warn("sim changed; stale auto number invalidated", "line", line.Cfg.ID)
		row.SIMHash = newSIMHash
		row.PhoneNumber = ""
		if manual == "" {
			row.NumberSource = numberSourceSIMChanged
		}
		simPending = true
	default:
		if newSIMHash != "" {
			row.SIMHash = newSIMHash
		}
		if manual == "" {
			// Only adopt a freshly read SIM number; never downgrade a
			// successful read on a transient error/unsupported answer.
			switch identity.NumberSource {
			case numberSourceSIM:
				row.PhoneNumber = identity.Number
				row.NumberSource = numberSourceSIM
			case numberSourceEmpty:
				if row.NumberSource != numberSourceSIM {
					row.NumberSource = numberSourceEmpty
					row.PhoneNumber = ""
				} else {
					// A transient empty answer must not erase a known number.
					row.NumberSource = numberSourceSIM
				}
			case numberSourceUnsupported:
				if row.NumberSource == "" || row.NumberSource == numberSourceNone {
					row.NumberSource = numberSourceUnsupported
				}
			}
		}
	}
	if row.NumberSource == "" {
		row.NumberSource = numberSourceNone
	}

	if err := m.Store.SaveLineIdentity(ctx, row); err != nil {
		slog.Warn("persist line identity failed", "line", line.Cfg.ID, "error", err)
		return false
	}
	_, masked, source := resolvedNumber(row)
	line.mu.Lock()
	line.identity = row
	line.identityAt = row.UpdatedAt
	line.simPending = simPending
	line.mu.Unlock()
	return masked != previousMasked || source != previousSource
}

// SetManualNumber records an operator-supplied own number that always takes
// precedence over the SIM-derived value. An empty value clears the override
// so the SIM number applies again. The number is normalized and strictly
// validated; the SIM-read number is kept separately and becomes effective
// again on clear.
func (m *Manager) SetManualNumber(ctx context.Context, lineID, number string) error {
	row, err := m.Store.GetLineIdentity(ctx, lineID)
	if err != nil {
		return err
	}
	number, ok := normalizeManualNumber(number)
	if !ok {
		return fail(400, "CB-V2-400", "号码格式不正确")
	}
	row.LineID = lineID
	row.ManualNumber = number
	if number != "" {
		row.NumberSource = numberSourceManual
	} else if row.PhoneNumber != "" {
		row.NumberSource = numberSourceSIM
	} else {
		// No stored SIM number to fall back to. The bounded worker poll
		// reconciles the true empty/unsupported state on the next cycle.
		row.NumberSource = numberSourceNone
	}
	row.UpdatedAt = time.Now().UTC()
	if err := m.Store.SaveLineIdentity(ctx, row); err != nil {
		return err
	}
	if line := m.Line(lineID); line != nil {
		line.mu.Lock()
		line.identity = row
		line.identityAt = row.UpdatedAt
		line.mu.Unlock()
	}
	m.publish(lineID, "", "line.updated", map[string]any{"id": lineID})
	return nil
}

// SetDeviceManualNumber is the device-facing, explicitly authorized variant:
// the caller must hold a live grant to the line AND the enrollment key must
// carry the off-by-default manage-number capability. Revoked devices/keys and
// disabled lines are rejected through requireLine. Returns the refreshed
// line view for the calling device.
func (m *Manager) SetDeviceManualNumber(ctx context.Context, deviceID, lineID, number string) (LineView, error) {
	access, _, _, err := m.requireLine(ctx, deviceID, lineID, nil)
	if err != nil {
		return LineView{}, err
	}
	if !access.Key.ManageNumber {
		return LineView{}, ErrForbidden
	}
	if err := m.SetManualNumber(ctx, lineID, number); err != nil {
		return LineView{}, err
	}
	views := m.LineViews(ctx, access, lineID)
	if len(views) != 1 {
		return LineView{}, fail(500, CodeInternal, "线路视图不可用")
	}
	return views[0], nil
}

// lineIdentitySnapshot returns the current cached private row for a line.
func (m *Manager) lineIdentitySnapshot(line *Line) store.LineIdentity {
	line.mu.RLock()
	identity := line.identity
	line.mu.RUnlock()
	if identity.LineID != "" {
		return identity
	}
	row, err := m.Store.GetLineIdentity(context.Background(), line.Cfg.ID)
	if err != nil {
		return store.LineIdentity{LineID: line.Cfg.ID, NumberSource: numberSourceNone}
	}
	return row
}

// storedLineIdentity builds the public identity view + full number directly
// from the private store row, for lines without a live runtime object.
func (m *Manager) storedLineIdentity(lineID string) (LineIdentity, string) {
	row, err := m.Store.GetLineIdentity(context.Background(), lineID)
	if err != nil {
		return LineIdentity{LineID: lineID, NumberSource: numberSourceNone}, ""
	}
	full, masked, source := resolvedNumber(row)
	return LineIdentity{
		LineID:        lineID,
		PhoneMasked:   masked,
		NumberSource:  source,
		SIMMasked:     row.ICCIDMasked,
		OperatorAlpha: row.OperatorAlpha,
		Registration:  friendlyRegistration(row.Registration),
		AccessTech:    row.AccessTech,
		SIMChanged:    source == numberSourceSIMChanged,
		UpdatedAt:     row.UpdatedAt.UnixMilli(),
	}, full
}

// RefreshLineIdentity performs an immediate bounded read of the line's own
// number/SIM metadata through the worker queue and returns the new masked view.
func (m *Manager) RefreshLineIdentity(ctx context.Context, lineID string) (LineIdentity, error) {
	line := m.Line(lineID)
	if line == nil || !line.Cfg.Enabled {
		return LineIdentity{}, fail(410, CodeLineUnavailable, "线路未启用")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	m.refreshSIMIdentity(probeCtx, line)
	view := m.LineIdentity(line)
	return view, nil
}

// pollIdentity runs the bounded read at startup, on SIM-status changes and
// periodically (well below the 15s status cadence) using the worker's own
// serialized AT queue.
func (m *Manager) pollIdentity(ctx context.Context, line *Line) {
	attempt := 0
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		changed := m.refreshSIMIdentity(probeCtx, line)
		cancel()
		if changed {
			m.refreshLine(ctx, line)
		}
		attempt++
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
