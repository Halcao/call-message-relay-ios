package recording

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
)

// WAVEncoder is the default finalizer. The gateway mixer hands it two
// canonical 8 kHz mono s16le tracks (uplink = what the user's audio sends to
// the line, downlink = what the line sends to the user). It mixes them in
// constant memory into a standard 44-byte-header PCM WAV file — no external
// binary, no codec dependency, and the output is lossless and directly
// playable/exportable from the phone.
//
// The mix keeps both sides audible: sum then halve, clamped. A track that is
// shorter than the other is treated as silence, so a one-sided segment is
// preserved rather than dropped.
type WAVEncoder struct{}

const (
	wavHeaderSize   = 44
	mixChunkSamples = 8192 // bounded read/write window (~1 s)
)

func (WAVEncoder) Encode(ctx context.Context, uplink, downlink, output string) ([]float32, error) {
	up, err := os.Open(uplink)
	if err != nil {
		return nil, fmt.Errorf("open uplink track: %w", err)
	}
	defer up.Close()
	down, err := os.Open(downlink)
	if err != nil {
		return nil, fmt.Errorf("open downlink track: %w", err)
	}
	defer down.Close()

	upInfo, err := up.Stat()
	if err != nil {
		return nil, err
	}
	downInfo, err := down.Stat()
	if err != nil {
		return nil, err
	}
	upSamples := upInfo.Size() / BytesPerSample
	downSamples := downInfo.Size() / BytesPerSample
	total := upSamples
	if downSamples > total {
		total = downSamples
	}
	if total <= 0 {
		return nil, fmt.Errorf("recording tracks are empty")
	}

	file, err := os.OpenFile(output, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return nil, err
	}
	cleanup := func(err error) ([]float32, error) {
		_ = file.Close()
		_ = os.Remove(output)
		return nil, err
	}
	if err := writeWAVHeader(file, total); err != nil {
		return cleanup(err)
	}

	peaks := make([]float32, WaveformSamples)
	upBuffer := make([]byte, mixChunkSamples*BytesPerSample)
	downBuffer := make([]byte, mixChunkSamples*BytesPerSample)
	outBuffer := make([]byte, mixChunkSamples*BytesPerSample)
	var index int64
	for index < total {
		if err := ctx.Err(); err != nil {
			return cleanup(err)
		}
		want := total - index
		if want > mixChunkSamples {
			want = mixChunkSamples
		}
		upRead, err := readPCMChunk(up, upBuffer, index, upSamples, want)
		if err != nil {
			return cleanup(err)
		}
		downRead, err := readPCMChunk(down, downBuffer, index, downSamples, want)
		if err != nil {
			return cleanup(err)
		}
		for frame := int64(0); frame < want; frame++ {
			var left, right int32
			if frame < upRead {
				left = int32(int16(binary.LittleEndian.Uint16(upBuffer[frame*2 : frame*2+2])))
			}
			if frame < downRead {
				right = int32(int16(binary.LittleEndian.Uint16(downBuffer[frame*2 : frame*2+2])))
			}
			mixed := (left + right) / 2
			if mixed > math.MaxInt16 {
				mixed = math.MaxInt16
			} else if mixed < math.MinInt16 {
				mixed = math.MinInt16
			}
			binary.LittleEndian.PutUint16(outBuffer[frame*2:frame*2+2], uint16(int16(mixed)))
			sampleIndex := index + frame
			bucket := int(sampleIndex * WaveformSamples / total)
			if bucket >= WaveformSamples {
				bucket = WaveformSamples - 1
			}
			peak := float32(math.Abs(float64(int16(mixed))) / 32768)
			if peak > peaks[bucket] {
				peaks[bucket] = peak
			}
		}
		if _, err := file.Write(outBuffer[:want*BytesPerSample]); err != nil {
			return cleanup(err)
		}
		index += want
	}
	if err := file.Sync(); err != nil {
		return cleanup(err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(output)
		return nil, err
	}
	return peaks, nil
}

// readPCMChunk reads up to want samples starting at sample offset. Samples
// beyond the track length read as silence (zero) without touching the buffer.
func readPCMChunk(file *os.File, buffer []byte, offset, samples, want int64) (int64, error) {
	available := samples - offset
	if available <= 0 {
		return 0, nil
	}
	if available > want {
		available = want
	}
	if _, err := file.ReadAt(buffer[:available*BytesPerSample], offset*BytesPerSample); err != nil && err != io.EOF {
		return 0, err
	}
	return available, nil
}

func writeWAVHeader(file *os.File, samples int64) error {
	dataSize := uint32(samples * BytesPerSample)
	byteRate := uint32(SampleRate * Channels * BytesPerSample)
	blockAlign := uint16(Channels * BytesPerSample)
	header := make([]byte, 0, wavHeaderSize)
	header = append(header, "RIFF"...)
	header = binary.LittleEndian.AppendUint32(header, 36+dataSize)
	header = append(header, "WAVE"...)
	header = append(header, "fmt "...)
	header = binary.LittleEndian.AppendUint32(header, 16)             // PCM fmt chunk size
	header = binary.LittleEndian.AppendUint16(header, 1)              // PCM
	header = binary.LittleEndian.AppendUint16(header, Channels)       // mono
	header = binary.LittleEndian.AppendUint32(header, SampleRate)     // 8 kHz
	header = binary.LittleEndian.AppendUint32(header, byteRate)       // bytes/s
	header = binary.LittleEndian.AppendUint16(header, blockAlign)     // block align
	header = binary.LittleEndian.AppendUint16(header, BitsPerSample)  // 16-bit
	header = append(header, "data"...)
	header = binary.LittleEndian.AppendUint32(header, dataSize)
	_, err := file.Write(header)
	return err
}

// WaveformFromWAV re-reads a finalized WAV so a recovered recording still gets
// its waveform when finalization was interrupted after the mixdown.
func WaveformFromWAV(path string) ([]float32, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	total := (info.Size() - wavHeaderSize) / BytesPerSample
	if total <= 0 {
		return nil, fmt.Errorf("recording WAV is empty")
	}
	peaks := make([]float32, WaveformSamples)
	buffer := make([]byte, mixChunkSamples*BytesPerSample)
	var index int64
	for index < total {
		want := total - index
		if want > mixChunkSamples {
			want = mixChunkSamples
		}
		if _, err := file.ReadAt(buffer[:want*BytesPerSample], wavHeaderSize+index*BytesPerSample); err != nil && err != io.EOF {
			return nil, err
		}
		for frame := int64(0); frame < want; frame++ {
			sample := int16(binary.LittleEndian.Uint16(buffer[frame*2 : frame*2+2]))
			bucket := int((index + frame) * WaveformSamples / total)
			if bucket >= WaveformSamples {
				bucket = WaveformSamples - 1
			}
			peak := float32(math.Abs(float64(sample)) / 32768)
			if peak > peaks[bucket] {
				peaks[bucket] = peak
			}
		}
		index += want
	}
	return peaks, nil
}

type waveformDocument struct {
	SampleCount int       `json:"sampleCount"`
	Peaks       []float32 `json:"peaks"`
}

func writeWaveformDocument(path string, peaks []float32) error {
	data, err := json.Marshal(waveformDocument{SampleCount: WaveformSamples, Peaks: peaks})
	if err != nil {
		return err
	}
	temporary := path + ".part"
	if err := os.WriteFile(temporary, data, 0o640); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
