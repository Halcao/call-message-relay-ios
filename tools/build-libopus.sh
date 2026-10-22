#!/usr/bin/env bash
# Reproducible static libopus builds for the CallRelay gateway CGO codec.
#
#   ./tools/build-libopus.sh                # vendored gateway host targets
#   ./tools/build-libopus.sh darwin-arm64
#   ./tools/build-libopus.sh linux-arm64-musl
#
# Outputs (committed to the repo so plain `go build`/`go test` work without
# extra environment, for exactly the two gateway host targets):
#   gateway/third_party/opus/include/opus/*.h
#   gateway/third_party/opus/lib-darwin-arm64/libopus.a
#   gateway/third_party/opus/lib-linux-arm64-musl/libopus.a
#   gateway/third_party/opus/COPYING    (BSD-3-Clause license text)
#   gateway/third_party/opus/OPUS_VERSION
#
# Supported platforms (matching the cgo directives in internal/opus):
#   - darwin/arm64:      vendored static archive (Xcode CLT clang, native)
#   - linux/arm64:       vendored static musl archive (production target).
#     Cross-toolchain: brew install
#     messense/macos-cross-toolchains/aarch64-unknown-linux-musl, or set
#     MUSL_CC=/path/to/aarch64-unknown-linux-musl-cc.
#   - linux/amd64: NO vendored archive is built;
#     the gateway links the SYSTEM libopus instead (apt install
#     libopus-dev). See docs/self-hosting.md.
#
# `ios` mode is the recipe for the iOS app's opus.xcframework (device +
# simulator slices with callrelay_opus_shim). It requires the FULL Xcode
# (iphoneos SDKs), is NOT covered by gateway CI verification, and only needs
# to run when regenerating the vendored xcframework.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="1.5.2"
OUT="${ROOT}/gateway/third_party/opus"
# Rebuildable caches and downloads live outside the source tree when
# Scripts/build-env.sh is available; /tmp is the portable fallback.
if [[ -f "${ROOT}/Scripts/build-env.sh" ]]; then
	source "${ROOT}/Scripts/build-env.sh"
elif [[ -f "${ROOT}/iPhoneApp/Scripts/build-env.sh" ]]; then
	source "${ROOT}/iPhoneApp/Scripts/build-env.sh"
fi
CACHE_ROOT="${CALLRELAY_CACHE_ROOT:-${TMPDIR:-/tmp}/callrelay-deps}"
TARBALL="${TARBALL:-${CACHE_ROOT}/downloads/opus-${VERSION}.tar.gz}"
BUILD_ROOT="${BUILD_ROOT:-${CACHE_ROOT}/libopus}"
# Neutral logical prefix compiled into the vendored archives in place of the
# machine-local build directory (see prefix_map_flags).
PREFIX_MAP_ROOT="${PREFIX_MAP_ROOT:-/build/callrelay}"
JOBS="$( (sysctl -n hw.ncpu 2>/dev/null || nproc 2>/dev/null || echo 4) )"

mkdir -p "$(dirname "${TARBALL}")" "${BUILD_ROOT}"
if [[ ! -s "${TARBALL}" ]]; then
	curl -fsSL -o "${TARBALL}" "https://downloads.xiph.org/releases/opus/opus-${VERSION}.tar.gz"
fi

if command -v shasum >/dev/null 2>&1; then sha256="$(shasum -a 256 "${TARBALL}" | awk '{print $1}')"; else sha256="$(sha256sum "${TARBALL}" | awk '{print $1}')"; fi
# Recorded checksum of the official Xiph release tarball (downloads.xiph.org).
EXPECT="65c1d2f78b9f2fb20082c38cbe47c951ad5839345876e46941612ee87f9a7ce1"
if [[ "${sha256}" != "${EXPECT}" ]]; then
	echo "opus-${VERSION} tarball sha256 ${sha256} != expected ${EXPECT}" >&2
	exit 1
fi

filesize() {
	if stat -f%z "$1" >/dev/null 2>&1; then stat -f%z "$1"; else stat -c%s "$1"; fi
}

# Emit the compiler's supported path-relocation flags so the vendored
# archives never embed the operator's machine-local build paths (assert
# strings keep the neutral logical prefix instead).
prefix_map_flags() {
	local cc="$1" src="$2" out=()
	if echo 'int main(){}' | "$cc" -ffile-prefix-map="$src"=/ -x c - -o /dev/null 2>/dev/null; then
		out+=("-ffile-prefix-map=${src}=${PREFIX_MAP_ROOT}/opus")
	else
		if echo 'int main(){}' | "$cc" -fmacro-prefix-map="$src"=/ -x c - -o /dev/null 2>/dev/null; then
			out+=("-fmacro-prefix-map=${src}=${PREFIX_MAP_ROOT}/opus" "-fdebug-prefix-map=${src}=${PREFIX_MAP_ROOT}/opus")
		else
			echo "warning: ${cc} supports no path-relocation flags; archives will embed local paths" >&2
		fi
	fi
	( IFS=' '; echo "${out[*]}" )
}

build_one() {
	local name="$1" host="$2" cc="$3"
	local src="${BUILD_ROOT}/build-opus-${name}"
	local prefix="${src}/prefix"
	local mapflags
	mapflags="$(prefix_map_flags "$cc" "$src")"
	rm -rf "${src}"
	mkdir -p "${src}"
	tar -xzf "${TARBALL}" -C "${src}"
	local dir="${src}/opus-${VERSION}"

	local extra=()
	# OSCE (Deep-PLC/DRED neural models) is off by default in 1.5.x; disable
	# explicitly when the flag exists so future releases cannot pull model
	# blobs into the telephony build.
	if (cd "${dir}" && ./configure --help 2>/dev/null | grep -q -- '--disable-osce'); then
		extra+=(--disable-osce)
	fi

	(
		cd "${dir}"
		CC="${cc}" CFLAGS="-O2 ${mapflags}" ./configure \
			--host="${host}" \
			--prefix="${prefix}" \
			--enable-static --disable-shared \
			--disable-extra-programs --disable-doc \
			--disable-maintainer-mode \
			${extra[@]+"${extra[@]}"}
		make -j"${JOBS}"
		make install
	)

	mkdir -p "${OUT}/include" "${OUT}/lib-${name}"
	cp -R "${prefix}/include/opus" "${OUT}/include/"
	cp "${prefix}/lib/libopus.a" "${OUT}/lib-${name}/libopus.a"
	cp "${dir}/COPYING" "${OUT}/COPYING"
	echo "${VERSION}" > "${OUT}/OPUS_VERSION"
	echo "built ${name}: ${OUT}/lib-${name}/libopus.a ($(filesize "${OUT}/lib-${name}/libopus.a") bytes)"
}

build_ios() {
	# XCFramework for the iOS app (device arm64 + simulator arm64/x86_64).
	# Requires the full Xcode (not CommandLineTools) for the iphoneos SDKs.
	# NOTE: experimental from this repository — not covered by gateway CI.
	export DEVELOPER_DIR="${DEVELOPER_DIR:-$(xcode-select -p)}"
	XCODEBUILD="$(xcrun --find xcodebuild)"
	SHIM_DIR="${ROOT}/gateway/internal/opus/shim"
	DEV_SRC="${BUILD_ROOT}/build-opus-iphoneos-arm64"
	SIM_SRC="${BUILD_ROOT}/build-opus-iphonesimulator"
	for src in "${DEV_SRC}" "${SIM_SRC}"; do rm -rf "${src}"; mkdir -p "${src}"; done
	tar -xzf "${TARBALL}" -C "${DEV_SRC}"
	tar -xzf "${TARBALL}" -C "${SIM_SRC}"
	# Fixed-signature CTL shim: opus_encoder_ctl is variadic in C and
	# therefore not importable from Swift; the shim exposes plain
	# functions compiled into both slices.
	for src in "${DEV_SRC}" "${SIM_SRC}"; do
		cp "${SHIM_DIR}/callrelay_opus_shim.h" "${src}/opus-${VERSION}/include/"
		cp "${SHIM_DIR}/callrelay_opus_shim.c" "${src}/opus-${VERSION}/src/"
	done
	DEV_SDK="$(xcrun --sdk iphoneos --show-sdk-path)"
	SIM_SDK="$(xcrun --sdk iphonesimulator --show-sdk-path)"
	(
		cd "${DEV_SRC}/opus-${VERSION}"
		CC=clang CFLAGS="-arch arm64 -isysroot ${DEV_SDK} -miphoneos-version-min=15.0 -O2 $(prefix_map_flags clang "${DEV_SRC}")" \
			./configure --host=aarch64-apple-darwin --prefix="${DEV_SRC}/prefix" \
			--enable-static --disable-shared --disable-extra-programs --disable-doc
		make -j"${JOBS}" && make install
		# Compile the shim into the installed static library.
		clang -arch arm64 -isysroot "${DEV_SDK}" -miphoneos-version-min=15.0 -O2 -I include \
			-c src/callrelay_opus_shim.c -o "${DEV_SRC}/shim.o"
		ar rcs "${DEV_SRC}/prefix/lib/libopus.a" "${DEV_SRC}/shim.o"
	)
	(
		cd "${SIM_SRC}/opus-${VERSION}"
		CC=clang CFLAGS="-arch arm64 -arch x86_64 -isysroot ${SIM_SDK} -miphonesimulator-version-min=15.0 -O2 $(prefix_map_flags clang "${SIM_SRC}")" \
			./configure --host=aarch64-apple-darwin --prefix="${SIM_SRC}/prefix" \
			--enable-static --disable-shared --disable-extra-programs --disable-doc
		make -j"${JOBS}" && make install
		# The simulator lib is a fat archive (arm64+x86_64) and ar cannot
		# edit fat archives: split per arch, inject the shim, recombine.
		for arch in arm64 x86_64; do
			clang -arch "${arch}" -isysroot "${SIM_SDK}" -miphonesimulator-version-min=15.0 -O2 -I include \
				-c src/callrelay_opus_shim.c -o "${SIM_SRC}/shim-${arch}.o"
			lipo "${SIM_SRC}/prefix/lib/libopus.a" -thin "${arch}" -output "${SIM_SRC}/libopus-${arch}.a"
			ar rcs "${SIM_SRC}/libopus-${arch}.a" "${SIM_SRC}/shim-${arch}.o"
		done
		lipo -create "${SIM_SRC}/libopus-arm64.a" "${SIM_SRC}/libopus-x86_64.a" \
			-output "${SIM_SRC}/prefix/lib/libopus.a"
	)
	for src in "${DEV_SRC}" "${SIM_SRC}"; do
		mkdir -p "${src}/prefix/include-opus"
		cp -R "${src}/prefix/include/opus" "${src}/prefix/include-opus/"
		cp "${SHIM_DIR}/callrelay_opus_shim.h" "${src}/prefix/include-opus/opus/"
		cat > "${src}/prefix/include-opus/opus/module.modulemap" <<'EOF'
module Opus {
    header "opus.h"
    header "opus_multistream.h"
    header "callrelay_opus_shim.h"
    export *
}
EOF
	done
	XC="${OPUS_XCFRAMEWORK_DIR:-${ROOT}/Vendor/opus/opus.xcframework}"
	mkdir -p "$(dirname "${XC}")"
	rm -rf "${XC}"
	"${XCODEBUILD}" -create-xcframework \
		-library "${DEV_SRC}/prefix/lib/libopus.a" -headers "${DEV_SRC}/prefix/include-opus/opus" \
		-library "${SIM_SRC}/prefix/lib/libopus.a" -headers "${SIM_SRC}/prefix/include-opus/opus" \
		-output "${XC}" >/dev/null
	cp "${DEV_SRC}/opus-${VERSION}/COPYING" "$(dirname "${XC}")/COPYING"
	echo "built ${XC}"
}

case "${1:-all}" in
	darwin-arm64)
		build_one darwin-arm64 aarch64-apple-darwin "${CC:-clang}" ;;
	linux-arm64-musl)
		MUSL_CC="${MUSL_CC:-$(command -v aarch64-unknown-linux-musl-cc || echo /opt/homebrew/bin/aarch64-unknown-linux-musl-cc)}"
		[[ -x "${MUSL_CC}" ]] || { echo "musl cross-compiler ${MUSL_CC} not found (install the toolchain or set MUSL_CC)" >&2; exit 1; }
		build_one linux-arm64-musl aarch64-linux-musl "${MUSL_CC}" ;;
	ios)
		build_ios ;;
	all)
		"$0" darwin-arm64
		"$0" linux-arm64-musl ;;
	*) echo "usage: $0 [darwin-arm64|linux-arm64-musl|ios|all]" >&2; exit 2 ;;
esac
