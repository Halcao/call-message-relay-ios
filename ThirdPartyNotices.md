Third-Party Notices
===================

CallRelay original client and gateway code is licensed under the PolyForm
Noncommercial License 1.0.0 (see LICENSE) — that license covers ONLY the
original CallRelay contributions. The following third-party components are
used and retain their own licenses; nothing below is re-licensed.

## iOS client (Swift app)

1. Google WebRTC (binary framework), distributed via the stasel/WebRTC Swift
   package, release 151.0.0 (chromium milestone M151).

   - Source:   https://github.com/stasel/WebRTC
   - Artifact: WebRTC-M151.xcframework.zip
   - Verified SHA-256 / Swift Package Manager checksum:
       6f3f5693383ce65763190c46ca9f2c4325c34b83681acb9db30f01488e15f1e0
   - License:  BSD 3-Clause "New" or "Revised" License (WebRTC).
       https://webrtc.googlesource.com/src/+/refs/heads/main/LICENSE
   - WebRTC incorporates a number of third-party software components; see the
     WebRTC LICENSE and its NOTICE files for the full attribution list.

   Note on the pinned artifact: the Swift package *tag* 151.0.0 references a
   release-asset URL that currently returns HTTP 404 on GitHub. The byte
   identical M151 binary was republished under release tag 151.0.1 with the
   same name and the SAME digest above. This project therefore vendors a thin
   local package (Vendor/WebRTC/Package.swift) wrapping the M151 xcframework.
   Scripts/bootstrap.sh downloads the asset, verifies the pinned SHA-256 above
   and extracts the framework. The binary itself is not committed.

2. Opus reference codec in the iOS app — `Vendor/opus/opus.xcframework`
   (COMMITTED binary, device arm64 + simulator arm64/x86_64).

   - Library: libopus 1.5.2 (Xiph.Org reference codec) for the client-side
     network-hop audio path.
   - Source: https://downloads.xiph.org/releases/opus/opus-1.5.2.tar.gz
   - SHA-256 (pinned in tools/build-libopus.sh):
       65c1d2f78b9f2fb20082c38cbe47c951ad5839345876e46941612ee87f9a7ce1
   - License: BSD 3-Clause; full text in Vendor/opus/COPYING (and
     gateway/third_party/opus/COPYING for the gateway-side vendored copy).
   - The xcframework also embeds `callrelay_opus_shim` (a small fixed-
     signature C wrapper around the variadic opus_encoder_ctl/
     opus_decoder_ctl so Swift can call them; source shared with the gateway
     at gateway/internal/opus/shim/, original CallRelay code under the
     repository PolyForm license). Reproduction recipe: the `ios` mode of
     tools/build-libopus.sh (full Xcode required; not covered by gateway CI).

3. Apple system frameworks (SwiftUI, CallKit, PushKit, CryptoKit, AVFoundation,
   Network/URLSession and the iOS SDK) are used under the terms of the Apple
   developer programs and SDK agreements. They are operating-system components,
   not redistributed here.

## Gateway (Go, `gateway/`)

4. CellBridge (upstream lineage of the per-line worker and the modem/SMS/
   voice code paths).

   - Upstream project: https://github.com/mccding/CellBridge
   - Provenance commit pinned: 2ee9d6b05d8a4391f782e6ce8a6d23ef57015fda
   - License: MIT, "Copyright (c) 2026 CellBridge contributors"; full text
     preserved verbatim in gateway/LICENSE-CellBridge.
   - The unified daemon and all original CallRelay additions remain under the
     repository PolyForm license; the MIT grant covers the CellBridge-derived
     portions as upstream licensed them.
   - Module-side vendor firmware (qdc507_aprv3.ko, qdc507_voice.ko,
     mavo-pcm-bridge.armv7, alsaucm) is CLOSED-SOURCE vendor material, NOT
     distributed in this repository.

5. Opus 1.5.2 static archives for the gateway host targets
   (gateway/third_party/opus/lib-darwin-arm64,
   gateway/third_party/opus/lib-linux-arm64-musl; BSD 3-Clause,
   gateway/third_party/opus/COPYING; same pinned tarball + checksum as item 2;
   reproducible via tools/build-libopus.sh with neutral /build/callrelay path
   prefix maps. On linux/amd64 the gateway links the SYSTEM libopus from
   libopus-dev instead and redistributes no archive for that platform.)

6. SpeexDSP 1.2.1 preprocessor static archives for the gateway host targets
   (gateway/third_party/speexdsp; BSD 3-Clause,
   gateway/third_party/speexdsp/COPYING; source
   https://downloads.xiph.org/releases/speex/speexdsp-1.2.1.tar.gz,
   SHA-256 8c777343e4a6399569c72abc38a95b24db56882c83dbdb6c6424a5f4aeb54d3d
   pinned in tools/build-speexdsp.sh; system libspeexdsp is linked on
   linux/amd64). Used only for the per-call directional voice denoise; no
   SpeexDSP code ships in the iOS app.

7. Go module dependencies (gateway/go.mod): Pion WebRTC/ICE/interceptor
   suites, gorilla/websocket, grandcat/zeroconf, quic-go,
   SherClockHolmes/webpush-go, skip2/go-qrcode, google/uuid,
   golang.org/x/crypto, golang.org/x/sys, modernc.org/sqlite,
   gopkg.in/yaml.v3 and their indirect modules retain their own licenses
   (MIT, BSD-2/3, Apache-2.0, ISC as listed in each module).

Per-component attribution files live next to the code:
gateway/THIRD_PARTY_NOTICES.md (gateway detail), gateway/LICENSE-CellBridge
(full MIT text), the COPYING files under gateway/third_party/ and
Vendor/opus/COPYING. The in-app license copy under
CallRelay/Resources/Licenses/ is synced from this file.

8. Historical Chinese spam-number data (disabled by default), from
   blessing-gao/rubbish-phone at commit
   926dc0fcd2a36d783289b6499fdf0202f8383a41, Apache-2.0. Full attribution
   and license: CallRelay/Resources/Licenses/SpamList-CN-Historical-NOTICE.txt.
