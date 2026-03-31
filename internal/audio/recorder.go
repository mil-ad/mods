package audio

import (
	"bytes"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/gen2brain/malgo"
)

// Recorder captures audio from the default input device.
type Recorder struct {
	ctx    *malgo.AllocatedContext
	device *malgo.Device

	mu      sync.Mutex
	buf     bytes.Buffer
	started time.Time
}

// NewRecorder creates a Recorder ready to capture 16-bit mono PCM at 16 kHz.
// Call Start to begin recording.
func NewRecorder() (*Recorder, error) {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, fmt.Errorf("audio: init context: %w", err)
	}

	r := &Recorder{ctx: ctx}

	deviceConfig := malgo.DefaultDeviceConfig(malgo.Capture)
	deviceConfig.Capture.Format = malgo.FormatS16
	deviceConfig.Capture.Channels = wavChannels
	deviceConfig.SampleRate = wavSampleRate

	callbacks := malgo.DeviceCallbacks{
		Data: r.onData,
	}

	device, err := malgo.InitDevice(ctx.Context, deviceConfig, callbacks)
	if err != nil {
		ctx.Free()
		return nil, fmt.Errorf("audio: init device: %w", err)
	}
	r.device = device

	return r, nil
}

// onData is the malgo capture callback. It runs on a separate goroutine.
func (r *Recorder) onData(_, inputSamples []byte, frameCount uint32) {
	r.mu.Lock()
	r.buf.Write(inputSamples)
	r.mu.Unlock()
}

// Start begins audio capture.
func (r *Recorder) Start() error {
	r.mu.Lock()
	r.buf.Reset()
	r.started = time.Now()
	r.mu.Unlock()
	return r.device.Start()
}

// Stop stops the capture device without saving.
func (r *Recorder) Stop() {
	_ = r.device.Stop()
	r.device.Uninit()
	r.ctx.Free()
}

// StopAndSave stops recording and writes the captured audio to a temporary WAV file.
// It returns the file path, recording duration, and any error.
func (r *Recorder) StopAndSave() (string, time.Duration, error) {
	_ = r.device.Stop()
	defer func() {
		r.device.Uninit()
		r.ctx.Free()
	}()

	r.mu.Lock()
	pcm := make([]byte, r.buf.Len())
	copy(pcm, r.buf.Bytes())
	r.mu.Unlock()

	duration := time.Duration(len(pcm)/(wavSampleRate*wavChannels*(wavBitsPerSample/8))) * time.Second

	f, err := os.CreateTemp("", "mods-recording-*.wav")
	if err != nil {
		return "", 0, fmt.Errorf("audio: create temp file: %w", err)
	}
	defer f.Close()

	if err := WriteWAV(f, pcm); err != nil {
		_ = os.Remove(f.Name())
		return "", 0, fmt.Errorf("audio: write wav: %w", err)
	}

	return f.Name(), duration, nil
}
