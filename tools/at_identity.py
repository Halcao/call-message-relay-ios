#!/usr/bin/env python3
"""Robust read-only parsers for SIM identity AT responses (AT+CNUM / AT+CCID).

Only SYNTHETIC values appear in this module and its tests; the real
module/SIM values live solely in the operator's private probe transcripts
(mode 600, outside version control) and are never copied here.

Observed firmware shapes (same shape, synthetic stand-ins):

* ``+CNUM: ,"+13800138000",145`` — the alpha field is present but EMPTY and
  UNQUOTED, so a regex expecting ``"[^"]*"`` never matches.
* ``+CCID: 0123456789ABCDEF0123`` — prefixed response whose token contains
  hex letters A-F, so an all-digits regex never matches.

Everything here is a pure function over a captured AT transcript so it can be
unit-tested with synthetic values only. No serial I/O lives in this module.
"""
from __future__ import annotations

import csv
import io
import re
from dataclasses import dataclass

_CNUM_PREFIX = "+CNUM:"
_CCID_PREFIX = "+CCID:"

# A usable own number is at least 3 / at most 20 digits with an optional
# leading '+'. Anything containing '*' or '#' is a supplementary-service
# control string (*#31# etc.), never an own number.
_NUMBER_RE = re.compile(r"\+?\d{3,20}$")
_HEX_TOKEN_RE = re.compile(r"^[0-9A-Fa-f]{18,22}$")
_DIGITS_TOKEN_RE = re.compile(r"^\d{18,20}$")


@dataclass(frozen=True)
class CNUMEntry:
    # Canonical display number. National numbers are kept verbatim (no "+"
    # and no invented country code); international numbers keep a single "+".
    number: str
    type_of_address: int | None
    alpha: str
    international: bool

    def as_dict(self) -> dict:
        return {
            "number": self.number,
            "typeOfAddress": self.type_of_address,
            "alpha": self.alpha,
            "international": self.international,
        }


@dataclass(frozen=True)
class CNUMResult:
    ok: bool                 # a terminal OK was seen
    error: bool              # ERROR / +CME ERROR / +CMS ERROR seen
    incomplete: bool         # no terminal result (truncated / echo only)
    unsupported: bool        # command rejected as unknown (+CME ERROR 4 etc.)
    entries: tuple[CNUMEntry, ...]

    @property
    def found(self) -> bool:
        return bool(self.entries)

    @property
    def number(self) -> str:
        """First valid entry's canonical number, or '' unless OK + entry."""
        return self.entries[0].number if self.entries else ""


def _split_csv_fields(payload: str) -> list[str]:
    # The CNUM payload is a single CSV record (alpha may itself be quoted and
    # contain commas). csv.reader handles both quoted and empty unquoted
    # fields, e.g. ',"+13...",145' -> ['', '+13...', '145'].
    try:
        rows = list(csv.reader(io.StringIO(payload), skipinitialspace=True))
    except csv.Error:
        return [part.strip() for part in payload.split(",")]
    return rows[0] if rows else []


def _canonical_number(raw: str, toa: int | None) -> tuple[str, bool]:
    """Return (canonical, international) without inventing a country code.

    The '+' the SIM itself stored is the only authority for international
    dialling. When it is absent we keep the reasonable national digits as-is
    even if the type-of-address byte claims international: firmware often
    stores 145 for national numbers, and blindly prefixing '+' corrupts an
    11-digit national result.
    """
    raw = (raw or "").strip()
    if not _NUMBER_RE.match(raw):
        return "", False
    explicit_international = raw.startswith("+")
    digits = re.sub(r"\D", "", raw)
    toa_international = toa is not None and 128 <= (toa & 0xFF) <= 163
    if explicit_international:
        return "+" + digits, True
    # Trust the SIM's stored digits over an inconsistent TOA byte: keep the
    # national form verbatim (no '+', no country code).
    return digits, False


def parse_cnum(transcript: str) -> CNUMResult:
    """Parse an AT+CNUM transcript.

    Parsing is scoped to the AT+CNUM command/result segment: it starts at the
    command echo and terminates at the segment's first OK/ERROR/+CME ERROR,
    so an unrelated command's result later in a combined transcript can never
    invalidate CNUM. Entries are only returned when that terminal is OK;
    echo-only/truncated transcripts are incomplete, and an error never exposes
    partial entries.
    """
    tentative: list[CNUMEntry] = []
    terminal_ok = False
    bare_ok = False
    error = False
    unsupported = False
    active = False
    for raw_line in (transcript or "").splitlines():
        line = raw_line.strip()
        upper = line.upper()
        if upper == "AT+CNUM":
            active = True
            bare_ok = False
            tentative = []
            terminal_ok = error = unsupported = False
            continue
        # A transcript can begin directly with a +CNUM response line (no
        # echo); activate on the first such line so both shapes parse.
        if not active and upper.startswith(_CNUM_PREFIX):
            active = True
        if upper == "OK":
            if active:
                terminal_ok = True
                break
            # A bare OK before any CNUM segment (worker filters the echo) is
            # treated as an empty-SIM answer unless a CNUM segment follows.
            bare_ok = True
            continue
        if upper == "ERROR" and not active:
            error = True
            break
        if not active:
            continue
        if upper.startswith("+CME ERROR") or upper.startswith("+CMS ERROR") or upper == "ERROR":
            error = True
            parts = line.split(":", 1)
            tail = parts[1].strip().lower() if len(parts) == 2 else ""
            if tail in {"4", "unknown command", "operation not supported", "command not supported"}:
                unsupported = True
            break
        if not upper.startswith(_CNUM_PREFIX):
            continue
        payload = line.split(":", 1)[1]
        fields = _split_csv_fields(payload)
        if len(fields) < 2:
            continue
        alpha = fields[0].strip()
        number_raw = fields[1].strip()
        if not _NUMBER_RE.match(number_raw):
            # Supplementary-service control strings / garbage are ignored.
            continue
        toa = None
        if len(fields) >= 3 and fields[2].strip() != "":
            try:
                toa = int(fields[2].strip())
            except ValueError:
                toa = None
        number, international = _canonical_number(number_raw, toa)
        if number:
            tentative.append(CNUMEntry(number=number, type_of_address=toa, alpha=alpha,
                                       international=international))
    if error:
        # Never expose numbers from a failed command.
        return CNUMResult(ok=False, error=True, incomplete=False, unsupported=unsupported, entries=())
    if terminal_ok:
        return CNUMResult(ok=True, error=False, incomplete=False, unsupported=False,
                          entries=tuple(tentative))
    if bare_ok and not active:
        # Only "OK" was present (echo filtered, no +CNUM line): empty SIM.
        return CNUMResult(ok=True, error=False, incomplete=False, unsupported=False, entries=())
    # Echo-only or truncated transcript (or a response without a
    # terminator): not a successful read.
    return CNUMResult(ok=False, error=False, incomplete=True, unsupported=False, entries=())


def parse_ccid(transcript: str) -> str:
    """Parse an AT+CCID/AT+ICCID? transcript into a canonical upper-case ICCID.

    Only whole-token shapes are accepted: a ``+CCID:`` value (optionally
    quoted) or a bare response line that is entirely 18-22 hex chars (ICCID
    printed in hex, possibly containing A-F) or 18-20 digits. Arbitrary text
    is never character-stripped into an accidental identity. Returns '' when
    nothing valid is present.
    """
    candidates: list[str] = []
    for raw_line in (transcript or "").splitlines():
        line = raw_line.strip()
        upper = line.upper()
        if upper.startswith("AT+") or upper in {"OK", "ERROR"} or upper.startswith("+CME ERROR"):
            continue
        token = line
        if upper.startswith(_CCID_PREFIX):
            token = line.split(":", 1)[1].strip()
        token = token.strip('"').strip()
        if token == "":
            continue
        if _HEX_TOKEN_RE.match(token):
            candidates.append(token.upper())
        elif _DIGITS_TOKEN_RE.match(token):
            candidates.append(token)
        # Any other shape (sentence, short hex, mixed punctuation) is ignored
        # rather than stripped into a partial identity.
    return candidates[0] if candidates else ""
