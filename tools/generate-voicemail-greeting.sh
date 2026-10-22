#!/usr/bin/env bash
# Regenerate the voicemail greeting asset embedded in callrelayd.
#
# The asset is synthesized locally by macOS system TTS (no third-party
# recording or external service) as 8 kHz mono 16-bit PCM WAV, which is the
# exact format the mixer already uses. Run this on a Mac with the chosen
# voice available (say -v '?' | grep zh_CN).
set -euo pipefail

VOICE="${VOICE:-Tingting}"
TEXT="${TEXT:-您好，这里是语音留言服务。机主暂时无法接听您的电话，请在提示音后留言，留言完成后请挂机。谢谢。}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${ROOT}/gateway/internal/unified/runtime/assets/voicemail-greeting.wav"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

say -v "${VOICE}" -o "${TMP}/greeting.aiff" "${TEXT}"
mkdir -p "$(dirname "${OUT}")"
afconvert -f WAVE -d LEI16@8000 -c 1 "${TMP}/greeting.aiff" "${OUT}"

afinfo "${OUT}" | grep -E "Data format|estimated duration"
shasum -a 256 "${OUT}"
echo "wrote ${OUT}"
