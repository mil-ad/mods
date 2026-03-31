package main

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charmbracelet/mods/internal/audio"
)

const (
	pttHoldThreshold  = 300 * time.Millisecond
	pttReleaseTimeout = 500 * time.Millisecond
)

// pttReleaseCheckMsg is sent by a tick to check if the ] key was released.
type pttReleaseCheckMsg struct{}

// pttRecordingStartedMsg signals that audio capture started successfully.
type pttRecordingStartedMsg struct {
	recorder *audio.Recorder
}

// pttRecordingDoneMsg signals that recording stopped and the WAV file was saved.
type pttRecordingDoneMsg struct {
	filePath string
	duration time.Duration
	err      error
}

// handlePTTKey handles a "]" keypress for push-to-talk hold detection.
// Returns the updated model and command. The second bool is true if the key
// was consumed by PTT logic (caller should not pass it to the textarea).
func (m *Mods) handlePTTKey() (tea.Model, tea.Cmd, bool) {
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
	return m, nil
}

// handlePTTRecordingDone processes a completed recording.
func (m *Mods) handlePTTRecordingDone(msg pttRecordingDoneMsg) (tea.Model, tea.Cmd) {
	m.pttHolding = false
	m.pttRecording = false
	m.pttStopping = false
	m.pttRecorder = nil
	m.pttPressCount = 0
	m.pttCooldown = time.Now()

	if msg.err != nil {
		e := modsError{err: msg.err, reason: "push-to-talk recording failed"}
		m.Error = &e
		m.state = errorState
		return m, nil
	}

	// Insert the file path into the textarea so the user can see it.
	m.textarea.InsertString("[recording: " + msg.filePath + "]")
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
	m.pttRecorder = nil
	m.pttRecording = false
	m.pttStopping = true
	return func() tea.Msg {
		if rec == nil {
			return pttRecordingDoneMsg{err: nil}
		}
		path, dur, err := rec.StopAndSave()
		return pttRecordingDoneMsg{filePath: path, duration: dur, err: err}
	}
}
