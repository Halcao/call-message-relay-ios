#!/usr/bin/env bash
# Reproducible static SpeexDSP builds for the CallRelay gateway CGO
# preprocessor (8 kHz / 20 ms / 160-sample per-frame denoise).
#
#   ./tools/build-speexdsp.sh            # vendored gateway host targets
#   ./tools/build-speexdsp.sh darwin-arm64
#   ./tools/build-speexdsp.sh linux-arm64-musl
#
# Outputs (committed so plain `go build`/`go test` work without extra env):
#   gateway/third_party/speexdsp/include/speex/speex_preprocess.h (+ peers)
#   gateway/third_party/speexdsp/lib-darwin-arm64/libspeexdsp.a
#   gateway/third_party/speexdsp/lib-linux-arm64-musl/libspeexdsp.a
#   gateway/third_party/speexdsp/COPYING   (BSD-3-Clause license text)
#   gateway/third_party/speexdsp/SPEEXDSP_VERSION
#
# Supported platforms (matching the cgo directives in internal/speexpp):
#   - darwin/arm64:      vendored static archive (Xcode CLT clang, native)
#   - linux/arm64:       vendored static musl archive (production target).
#     Cross-toolchain: brew install
#     messense/macos-cross-toolchains/aarch64-unknown-linux-musl, or set
#     MUSL_CC=/path/to/aarch64-unknown-linux-musl-cc.
#   - linux/amd64 (and other Linux arches): NO vendored archive is built;
#     the gateway links the SYSTEM libspeexdsp instead (apt install
#     libspeexdsp-dev). See docs/self-hosting.md in the repository root.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="1.2.1"
OUT="${ROOT}/gateway/third_party/speexdsp"
if [[ -f "${ROOT}/Scripts/build-env.sh" ]]; then
	source "${ROOT}/Scripts/build-env.sh"
elif [[ -f "${ROOT}/iPhoneApp/Scripts/build-env.sh" ]]; then
	source "${ROOT}/iPhoneApp/Scripts/build-env.sh"
fi
CACHE_ROOT="${CALLRELAY_CACHE_ROOT:-${TMPDIR:-/tmp}/callrelay-deps}"
TARBALL="${TARBALL:-${CACHE_ROOT}/downloads/speexdsp-${VERSION}.tar.gz}"
BUILD_ROOT="${BUILD_ROOT:-${CACHE_ROOT}/speexdsp}"
# Neutral logical prefix compiled into the vendored archives in place of the
# machine-local build directory (see prefix_map_flags).
PREFIX_MAP_ROOT="${PREFIX_MAP_ROOT:-/build/callrelay}"
JOBS="$( (sysctl -n hw.ncpu 2>/dev/null || nproc 2>/dev/null || echo 4) )"

mkdir -p "$(dirname "${TARBALL}")" "${BUILD_ROOT}"
if [[ ! -s "${TARBALL}" ]]; then
	curl -fsSL -o "${TARBALL}" "https://downloads.xiph.org/releases/speex/speexdsp-${VERSION}.tar.gz"
fi

if command -v shasum >/dev/null 2>&1; then sha256="$(shasum -a 256 "${TARBALL}" | awk '{print $1}')"; else sha256="$(sha256sum "${TARBALL}" | awk '{print $1}')"; fi
# Recorded checksum of the official Xiph SpeexDSP 1.2.1 release tarball.
EXPECT="8c777343e4a6399569c72abc38a95b24db56882c83dbdb6c6424a5f4aeb54d3d"
if [[ "${sha256}" != "${EXPECT}" ]]; then
	echo "speexdsp-${VERSION} tarball sha256 ${sha256} != expected ${EXPECT}" >&2
	exit 1
fi

filesize() {
	if stat -f%z "$1" >/dev/null 2>&1; then stat -f%z "$1"; else stat -c%s "$1"; fi
}

# Emit the compiler's supported path-relocation flags so the vendored
# archives never embed the operator's machine-local build paths.
prefix_map_flags() {
	local cc="$1" src="$2" out=()
	if echo 'int main(){}' | "$cc" -ffile-prefix-map="$src"=/ -x c - -o /dev/null 2>/dev/null; then
		out+=("-ffile-prefix-map=${src}=${PREFIX_MAP_ROOT}/speexdsp")
	else
		if echo 'int main(){}' | "$cc" -fmacro-prefix-map="$src"=/ -x c - -o /dev/null 2>/dev/null; then
			out+=("-fmacro-prefix-map=${src}=${PREFIX_MAP_ROOT}/speexdsp" "-fdebug-prefix-map=${src}=${PREFIX_MAP_ROOT}/speexdsp")
		else
			echo "warning: ${cc} supports no path-relocation flags; archives will embed local paths" >&2
		fi
	fi
	( IFS=' '; echo "${out[*]}" )
}

build_one() {
	local name="$1" host="$2" cc="$3" cflags="$4" ldflags="$5"
	local src="${BUILD_ROOT}/build-speexdsp-${name}"
	local prefix="${src}/prefix"
	local mapflags
	mapflags="$(prefix_map_flags "$cc" "$src")"
	cflags="${cflags} ${mapflags}"
	rm -rf "${src}"
	mkdir -p "${src}"
	tar -xzf "${TARBALL}" -C "${src}"
	local dir="${src}/speexdsp-${VERSION}"

	(
		cd "${dir}"
		CC="${cc}" CFLAGS="${cflags}" LDFLAGS="${ldflags}" ./configure \
			--host="${host}" \
			--prefix="${prefix}" \
			--enable-static --disable-shared \
			--disable-doc --disable-maintainer-mode
		make -j"${JOBS}"
		make install
	)

	mkdir -p "${OUT}/include" "${OUT}/lib-${name}"
	cp -R "${prefix}/include/speex" "${OUT}/include/"
	cp "${prefix}/lib/libspeexdsp.a" "${OUT}/lib-${name}/libspeexdsp.a"
	cp "${dir}/COPYING" "${OUT}/COPYING"
	echo "${VERSION}" > "${OUT}/SPEEXDSP_VERSION"
	echo "built ${name}: ${OUT}/lib-${name}/libspeexdsp.a ($(filesize "${OUT}/lib-${name}/libspeexdsp.a") bytes)"
}

case "${1:-all}" in
	darwin-arm64)
		build_one darwin-arm64 aarch64-apple-darwin clang "-arch arm64 -O2 -fPIC" "-arch arm64" ;;
	linux-arm64-musl)
		MUSL_CC="${MUSL_CC:-$(command -v aarch64-unknown-linux-musl-cc || echo /opt/homebrew/bin/aarch64-unknown-linux-musl-cc)}"
		[[ -x "${MUSL_CC}" ]] || { echo "musl cross-compiler ${MUSL_CC} not found (install the toolchain or set MUSL_CC)" >&2; exit 1; }
		build_one linux-arm64-musl aarch64-linux-musl "${MUSL_CC}" "-O2 -fPIC" "" ;;
	all)
		"$0" darwin-arm64
		"$0" linux-arm64-musl ;;
	*) echo "usage: $0 [darwin-arm64|linux-arm64-musl|all]" >&2; exit 2 ;;
esac
