/* SPDX-License-Identifier: BSD-3-Clause (see callrelay_opus_shim.h). */
#include "callrelay_opus_shim.h"

int callrelay_opus_enc_set_bitrate(OpusEncoder *st, opus_int32 value) {
	return opus_encoder_ctl(st, OPUS_SET_BITRATE(value));
}

int callrelay_opus_enc_set_loss_perc(OpusEncoder *st, opus_int32 value) {
	return opus_encoder_ctl(st, OPUS_SET_PACKET_LOSS_PERC(value));
}

int callrelay_opus_enc_set_fec(OpusEncoder *st, opus_int32 value) {
	return opus_encoder_ctl(st, OPUS_SET_INBAND_FEC(value));
}

int callrelay_opus_enc_set_dtx(OpusEncoder *st, opus_int32 value) {
	return opus_encoder_ctl(st, OPUS_SET_DTX(value));
}

int callrelay_opus_enc_set_complexity(OpusEncoder *st, opus_int32 value) {
	return opus_encoder_ctl(st, OPUS_SET_COMPLEXITY(value));
}
