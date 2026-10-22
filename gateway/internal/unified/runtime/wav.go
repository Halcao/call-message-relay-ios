package runtime

import (
	"encoding/binary"
	"fmt"
	"io"
)

// wavWriter streams little-endian PCM to a file and patches the RIFF header
// on finalize. Voicemail messages are stored as 8 kHz mono 16-bit WAV on the
// the gateway host disk.
type wavWriter struct {
	writer    io.WriteCloser
	samples   int
	dataBytes int
}

func newWAVWriter(writer io.WriteCloser, sampleRate, channels int) *wavWriter {
	w := &wavWriter{writer: writer}
	header := make([]byte, 44)
	copy(header[0:4], "RIFF")
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(header[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(header[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(header[28:32], uint32(sampleRate*channels*2))
	binary.LittleEndian.PutUint16(header[32:34], uint16(channels*2))
	binary.LittleEndian.PutUint16(header[34:36], 16)
	copy(header[36:40], "data")
	_, _ = w.writer.Write(header)
	return w
}

func (w *wavWriter) WriteSamples(samples []int16) (int, error) {
	buffer := make([]byte, len(samples)*2)
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(buffer[i*2:i*2+2], uint16(sample))
	}
	written, err := w.writer.Write(buffer)
	w.dataBytes += written
	w.samples += written / 2
	return written, err
}

// Finalize patches sizes and closes the file. It returns the final data size.
func (w *wavWriter) Finalize() (int64, error) {
	if _, err := w.writer.Write(nil); err != nil && err != io.EOF {
		_ = w.writer.Close()
		return 0, err
	}
	// The header was written first; reopen is not possible on all writers,
	// so callers use a file that supports WriteAt through the wrapper below.
	if seeker, ok := w.writer.(interface {
		io.WriteSeeker
	}); ok {
		header := make([]byte, 44)
		if _, err := seeker.Seek(0, io.SeekStart); err != nil {
			return 0, err
		}
		copy(header[0:4], "RIFF")
		binary.LittleEndian.PutUint32(header[4:8], uint32(36+w.dataBytes))
		copy(header[8:12], "WAVE")
		copy(header[12:16], "fmt ")
		binary.LittleEndian.PutUint32(header[16:20], 16)
		binary.LittleEndian.PutUint16(header[20:22], 1)
		binary.LittleEndian.PutUint16(header[22:24], 1)
		binary.LittleEndian.PutUint32(header[24:28], 8000)
		binary.LittleEndian.PutUint32(header[28:32], 16000)
		binary.LittleEndian.PutUint16(header[32:34], 2)
		binary.LittleEndian.PutUint16(header[34:36], 16)
		copy(header[36:40], "data")
		binary.LittleEndian.PutUint32(header[40:44], uint32(w.dataBytes))
		if _, err := seeker.Write(header); err != nil {
			_ = w.writer.Close()
			return 0, err
		}
	} else {
		_ = w.writer.Close()
		return 0, fmt.Errorf("voicemail output does not support header finalize")
	}
	if err := w.writer.Close(); err != nil {
		return 0, err
	}
	return int64(w.dataBytes), nil
}
