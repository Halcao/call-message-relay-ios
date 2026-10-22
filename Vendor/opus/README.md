# Opus dependency for the iOS client

The client links libopus **1.5.2**, distributed under the BSD 3-Clause license
in [COPYING](COPYING). The application bundle includes the same license as
`Opus-LICENSE.txt`.

Upstream source: <https://downloads.xiph.org/releases/opus/opus-1.5.2.tar.gz>

Pinned upstream source SHA-256:
`65c1d2f78b9f2fb20082c38cbe47c951ad5839345876e46941612ee87f9a7ce1`.

`opus.xcframework` contains device arm64 and simulator arm64/x86_64 static
libraries with matching public headers. These libraries are intentionally
versioned so a clean client checkout can link without a separate native codec
build. WebRTC is fetched separately by `Scripts/bootstrap.sh`.

The small BSD-licensed [CTL wrappers](Source/callrelay_opus_shim.c) expose
fixed-signature functions for APIs that Swift cannot import directly from C
variadic declarations. Their [header](Source/callrelay_opus_shim.h) matches the
header included in each XCFramework slice. The gateway uses the same wrappers
in `gateway/internal/opus/shim`.

The gateway codec build scripts under `tools/` document their supported
platforms. They are not an iOS XCFramework rebuild script. This publication
verifies the existing iOS archives and their source/license correspondence;
it does not claim a bit-for-bit rebuild of those archives.
