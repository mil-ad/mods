package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"

	"github.com/charmbracelet/mods/internal/audio"
)

const (
	pttHoldThreshold  = 300 * time.Millisecond
	pttReleaseTimeout = 500 * time.Millisecond
	pttRecIndicator   = "● REC"
	pttTransIndicator = "transcribing..."
)

// pttReleaseCheckMsg is sent by a tick to check if the ] key was released.
type pttReleaseCheckMsg struct{}

// pttBlinkMsg toggles the recording indicator dot.
type pttBlinkMsg struct{}

// pttRecordingStartedMsg signals that audio capture started successfully.
type pttRecordingStartedMsg struct {
	recorder *audio.Recorder
}

// pttRecordingDoneMsg signals that recording stopped and transcription completed.
type pttRecordingDoneMsg struct {
	text     string
	filePath string
	duration time.Duration
	err      error
}

// checkTranscriptionServer probes the transcription API with a short timeout.
// Returns true if the server is reachable.
func checkTranscriptionServer(baseURL string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// handlePTTKey handles a "]" keypress for push-to-talk hold detection.
// Returns the updated model and command. The second bool is true if the key
// was consumed by PTT logic (caller should not pass it to the textarea).
func (m *Mods) handlePTTKey() (tea.Model, tea.Cmd, bool) {
	if !m.pttEnabled {
		return m, nil, false
	}

	now := time.Now()

	// Suppress all ] keypresses while stopping or during cooldown.
	if m.pttStopping {
		return m, nil, true
	}
	if !m.pttCooldown.IsZero() && time.Since(m.pttCooldown) < pttReleaseTimeout {
		return m, nil, true
	}
	m.pttCooldown = time.Time{}

	if !m.pttHolding {
		// First press — begin hold detection.
		m.pttHolding = true
		m.pttFirstPress = now
		m.pttLastPress = now
		m.pttPressCount = 1
		return m, pttReleaseTick(), true
	}

	// Subsequent press (key repeat).
	m.pttLastPress = now
	m.pttPressCount++

	if !m.pttRecording && time.Since(m.pttFirstPress) >= pttHoldThreshold {
		// Threshold reached — start recording.
		return m, startPTTRecordingCmd(), true
	}

	return m, nil, true
}

// handlePTTReleaseCheck processes a release-check tick.
func (m *Mods) handlePTTReleaseCheck() (tea.Model, tea.Cmd) {
	if !m.pttHolding || m.pttStopping {
		return m, nil
	}

	if time.Since(m.pttLastPress) <= pttReleaseTimeout {
		// Still held — schedule another check.
		return m, pttReleaseTick()
	}

	// Key was released.
	if m.pttRecording {
		return m, m.stopPTTRecordingCmd()
	}

	// If we already passed the hold threshold (recording was started and has
	// since stopped/is stopping), just clean up — don't insert any ']'.
	if time.Since(m.pttFirstPress) >= pttHoldThreshold {
		m.pttHolding = false
		m.pttPressCount = 0
		return m, nil
	}

	// Released before threshold — insert literal ']' characters.
	m.pttHolding = false
	for range m.pttPressCount {
		m.textarea.InsertString("]")
	}
	m.pttPressCount = 0
	m.syncTextareaHeight()
	return m, nil
}

// handlePTTRecordingStarted processes a successful recording start.
func (m *Mods) handlePTTRecordingStarted(msg pttRecordingStartedMsg) (tea.Model, tea.Cmd) {
	m.pttRecording = true
	m.pttRecorder = msg.recorder
	m.pttBlinkOn = true
	m.textarea.InsertString(pttRecIndicator)
	m.syncTextareaHeight()
	return m, pttBlinkTick()
}

// handlePTTRecordingDone processes a completed recording/transcription.
func (m *Mods) handlePTTRecordingDone(msg pttRecordingDoneMsg) (tea.Model, tea.Cmd) {
	m.pttHolding = false
	m.pttRecording = false
	m.pttStopping = false
	m.pttRecorder = nil
	m.pttPressCount = 0
	m.pttCooldown = time.Now()

	m.pttReplaceIndicator(pttTransIndicator, "")

	if msg.err != nil {
		m.textarea.InsertString("[error: " + msg.err.Error() + "]")
		m.syncTextareaHeight()
		return m, nil
	}

	if msg.text != "" {
		m.textarea.InsertString(msg.text)
	}
	m.syncTextareaHeight()
	return m, nil
}

// pttCleanup stops any active recording without saving. Call on quit.
func (m *Mods) pttCleanup() {
	if m.pttRecorder != nil {
		m.pttRecorder.Stop()
		m.pttRecorder = nil
	}
	m.pttRecording = false
	m.pttHolding = false
}

// pttReplaceIndicator swaps old for repl in the textarea value.
func (m *Mods) pttReplaceIndicator(old, repl string) {
	v := m.textarea.Value()
	if idx := strings.LastIndex(v, old); idx >= 0 {
		m.textarea.SetValue(v[:idx] + repl + v[idx+len(old):])
		// Move cursor to end so new text inserts after the indicator.
		m.textarea.CursorEnd()
		m.syncTextareaHeight()
	}
}

func pttBlinkTick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg {
		return pttBlinkMsg{}
	})
}

func pttReleaseTick() tea.Cmd {
	return tea.Tick(pttReleaseTimeout, func(time.Time) tea.Msg {
		return pttReleaseCheckMsg{}
	})
}

func startPTTRecordingCmd() tea.Cmd {
	return func() tea.Msg {
		rec, err := audio.NewRecorder()
		if err != nil {
			return pttRecordingDoneMsg{err: err}
		}
		if err := rec.Start(); err != nil {
			rec.Stop()
			return pttRecordingDoneMsg{err: err}
		}
		return pttRecordingStartedMsg{recorder: rec}
	}
}

func (m *Mods) stopPTTRecordingCmd() tea.Cmd {
	rec := m.pttRecorder
	cfg := m.Config
	m.pttRecorder = nil
	m.pttRecording = false
	m.pttStopping = true
	m.pttReplaceIndicator(pttRecIndicator, pttTransIndicator)
	return func() tea.Msg {
		if rec == nil {
			return pttRecordingDoneMsg{err: nil}
		}
		path, dur, err := rec.StopAndSave()
		if err != nil {
			return pttRecordingDoneMsg{err: err}
		}

		// If no transcription API is configured, just return the file path.
		if cfg.TranscriptionAPIBaseURL == "" {
			return pttRecordingDoneMsg{
				text:     "[recording: " + path + "]",
				filePath: path,
				duration: dur,
			}
		}

		text, err := transcribeAudio(cfg, path)
		if err != nil {
			return pttRecordingDoneMsg{err: fmt.Errorf("transcription: %w", err)}
		}

		// Clean up the temp WAV file after successful transcription.
		_ = os.Remove(path)

		return pttRecordingDoneMsg{
			text:     text,
			filePath: path,
			duration: dur,
		}
	}
}

// transcribeAudio sends the WAV file to the configured transcription API and returns the text.
func transcribeAudio(cfg *Config, wavPath string) (string, error) {
	f, err := os.Open(wavPath)
	if err != nil {
		return "", fmt.Errorf("open recording: %w", err)
	}
	defer f.Close()

	opts := []option.RequestOption{
		option.WithBaseURL(cfg.TranscriptionAPIBaseURL),
	}

	apiKey := cfg.TranscriptionAPIKey
	if apiKey == "" {
		apiKey = "sk-no-key-required"
	}
	opts = append(opts, option.WithAPIKey(apiKey))

	client := openai.NewClient(opts...)

	model := cfg.TranscriptionModel
	if model == "" {
		model = "whisper-1"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.Audio.Transcriptions.New(ctx, openai.AudioTranscriptionNewParams{
		File:  f,
		Model: openai.AudioModel(model),
	})
	if err != nil {
		return "", err
	}

	return resp.Text, nil
}
