package audio

import (
	"encoding/binary"
	"io"
)

// WAV format constants.
const (
	wavHeaderSize   = 44
	wavFormatPCM    = 1
	wavBitsPerSample = 16
	wavChannels     = 1
	wavSampleRate   = 16000
)

// WriteWAV writes a complete WAV file (header + PCM data) to w.
// The PCM data must be 16-bit signed little-endian mono at 16 kHz.
func WriteWAV(w io.Writer, pcm []byte) error {
	dataSize := uint32(len(pcm))
	fileSize := dataSize + wavHeaderSize - 8

	byteRate := uint32(wavSampleRate * wavChannels * wavBitsPerSample / 8)
	blockAlign := uint16(wavChannels * wavBitsPerSample / 8)

	// RIFF header
	if _, err := w.Write([]byte("RIFF")); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, fileSize); err != nil {
		return err
	}
	if _, err := w.Write([]byte("WAVE")); err != nil {
		return err
	}

	// fmt sub-chunk
	if _, err := w.Write([]byte("fmt ")); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(16)); err != nil { // chunk size
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint16(wavFormatPCM)); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint16(wavChannels)); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint32(wavSampleRate)); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, byteRate); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, blockAlign); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, uint16(wavBitsPerSample)); err != nil {
		return err
	}

	// data sub-chunk
	if _, err := w.Write([]byte("data")); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, dataSize); err != nil {
		return err
	}
	_, err := w.Write(pcm)
	return err
}
