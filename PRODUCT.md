# CallRelay

<!-- impeccable:product-schema 1 -->

## Platform

iOS / iPadOS 17+ and an installable web client (PWA), backed by a self-hosted cellular gateway.

## Stack

The native client uses Swift / SwiftUI, Apple LiveCommunicationKit (iOS 17.4+),
CallKit fallback, PushKit, CryptoKit and Keychain. Media uses WebRTC, optional
QUIC direct transport, or authenticated WebSocket relay. The Go gateway manages
per-module workers, a unified API, browser client, storage and media mixing.
Build the iOS project with XcodeGen; see [BUILD.md](BUILD.md).

## Users and Purpose

People with their own cellular modules, SIMs and always-on host can use their
lines from an iPhone, iPad or browser. The gateway handles calls, SMS and history;
the author does not operate a relay or push service. Hardware voice support
must be provisioned separately, as described in [self-hosting](docs/self-hosting.md).

## Current Scope

- One gateway connection exposes the lines permitted by the pairing key.
  Each device has independent credentials and can be revoked separately.
- Calling supports line selection, hold/resume, conferences, voicemail and
  manual gateway recording, subject to gateway capabilities and storage.
- SMS supports conversations, compose, local history, read state and batch
  actions. Pending actions survive restart and retry without blocking the UI.
- System contacts provide name/number suggestions. Optional gateway contact
  sync and harassment rules remain scoped to the authorized identity.
- Optional iCloud history/rule sync and Keychain pairing recovery require
  matching signing entitlements. Device private keys and access tokens remain
  local; a revoked device cannot silently register again.
- Native background ringing requires the operator's matching Apple signing
  and APNs setup. The PWA uses Web Push and a notification tap; it does not
  impersonate a native system call. Browser/OS push infrastructure is still used.
- The native Feather edition and the PWA handoff edition are separate build
  variants. Source availability, installable releases and real-device acceptance
  are tracked separately; see [release documentation](docs/release.md).

## Product Principles

- Use familiar iOS interactions, concise labels and accessible system styling.
- Keep unpaired, offline, unsupported, recovering and ready states distinct.
- Report gateway acceptance separately from carrier delivery and media readiness.
- Keep credentials, contacts, history and permissions isolated by gateway and key.
- Use only synthetic contacts and records in public screenshots and tests.
- Preserve private keys, deployment configuration, modem backups and recordings
  outside the public repository.

## Licensing and Verification

Original contributions use PolyForm Noncommercial 1.0.0; CellBridge-derived
source retains its MIT license, and dependencies retain their own terms.
[ThirdPartyNotices.md](ThirdPartyNotices.md) lists attribution.
Automated tests and service health do not replace signed-device, carrier and
hardware acceptance. Current features and limits are summarized in [README.md](README.md).
