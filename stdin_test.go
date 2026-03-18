package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// resetGlobals resets global state before each test
func resetGlobals() {
	pausedMu.Lock()
	paused = false
	pausedMu.Unlock()
	minAmplitude = 0.05
	cooldownMs = 750
	stdioMode = true
	volumeScaling = false
}

func TestPauseCommand(t *testing.T) {
	resetGlobals()

	input := `{"cmd":"pause"}` + "\n"
	var output bytes.Buffer

	processCommands(strings.NewReader(input), &output)

	// Check state changed
	pausedMu.RLock()
	if !paused {
		t.Error("expected paused to be true after pause command")
	}
	pausedMu.RUnlock()

	// Check output
	var resp map[string]string
	if err := json.Unmarshal(output.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["status"] != "paused" {
		t.Errorf("expected status 'paused', got %q", resp["status"])
	}
}

func TestResumeCommand(t *testing.T) {
	resetGlobals()

	// First pause
	pausedMu.Lock()
	paused = true
	pausedMu.Unlock()

	input := `{"cmd":"resume"}` + "\n"
	var output bytes.Buffer

	processCommands(strings.NewReader(input), &output)

	// Check state changed
	pausedMu.RLock()
	if paused {
		t.Error("expected paused to be false after resume command")
	}
	pausedMu.RUnlock()

	// Check output
	var resp map[string]string
	if err := json.Unmarshal(output.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["status"] != "resumed" {
		t.Errorf("expected status 'resumed', got %q", resp["status"])
	}
}

func TestSetAmplitudeCommand(t *testing.T) {
	resetGlobals()

	input := `{"cmd":"set","amplitude":0.15}` + "\n"
	var output bytes.Buffer

	processCommands(strings.NewReader(input), &output)

	// Check state changed
	if minAmplitude != 0.15 {
		t.Errorf("expected minAmplitude 0.15, got %f", minAmplitude)
	}

	// Check output
	var resp struct {
		Status    string  `json:"status"`
		Amplitude float64 `json:"amplitude"`
		Cooldown  int     `json:"cooldown"`
	}
	if err := json.Unmarshal(output.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.Status != "settings_updated" {
		t.Errorf("expected status 'settings_updated', got %q", resp.Status)
	}
	if resp.Amplitude != 0.15 {
		t.Errorf("expected amplitude 0.15 in response, got %f", resp.Amplitude)
	}
}

func TestSetCooldownCommand(t *testing.T) {
	resetGlobals()

	input := `{"cmd":"set","cooldown":500}` + "\n"
	var output bytes.Buffer

	processCommands(strings.NewReader(input), &output)

	// Check state changed
	if cooldownMs != 500 {
		t.Errorf("expected cooldownMs 500, got %d", cooldownMs)
	}

	// Check output
	var resp struct {
		Status   string `json:"status"`
		Cooldown int    `json:"cooldown"`
	}
	if err := json.Unmarshal(output.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.Cooldown != 500 {
		t.Errorf("expected cooldown 500 in response, got %d", resp.Cooldown)
	}
}

func TestSetBothCommand(t *testing.T) {
	resetGlobals()

	input := `{"cmd":"set","amplitude":0.2,"cooldown":1000}` + "\n"
	var output bytes.Buffer

	processCommands(strings.NewReader(input), &output)

	if minAmplitude != 0.2 {
		t.Errorf("expected minAmplitude 0.2, got %f", minAmplitude)
	}
	if cooldownMs != 1000 {
		t.Errorf("expected cooldownMs 1000, got %d", cooldownMs)
	}
}

func TestSetAmplitudeOutOfRange(t *testing.T) {
	resetGlobals()
	originalAmplitude := minAmplitude

	// Test amplitude > 1 (should be ignored)
	input := `{"cmd":"set","amplitude":1.5}` + "\n"
	var output bytes.Buffer
	processCommands(strings.NewReader(input), &output)

	if minAmplitude != originalAmplitude {
		t.Errorf("amplitude should not change for value > 1, got %f", minAmplitude)
	}

	// Test amplitude <= 0 (should be ignored)
	resetGlobals()
	input = `{"cmd":"set","amplitude":0}` + "\n"
	output.Reset()
	processCommands(strings.NewReader(input), &output)

	if minAmplitude != originalAmplitude {
		t.Errorf("amplitude should not change for value <= 0, got %f", minAmplitude)
	}

	// Test negative amplitude
	resetGlobals()
	input = `{"cmd":"set","amplitude":-0.5}` + "\n"
	output.Reset()
	processCommands(strings.NewReader(input), &output)

	if minAmplitude != originalAmplitude {
		t.Errorf("amplitude should not change for negative value, got %f", minAmplitude)
	}
}

func TestVolumeScalingCommand(t *testing.T) {
	resetGlobals()

	// Toggle on
	input := `{"cmd":"volume-scaling"}` + "\n"
	var output bytes.Buffer
	processCommands(strings.NewReader(input), &output)

	if !volumeScaling {
		t.Error("expected volumeScaling to be true after toggle")
	}

	var resp struct {
		Status        string `json:"status"`
		VolumeScaling bool   `json:"volume_scaling"`
	}
	if err := json.Unmarshal(output.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.Status != "volume_scaling_toggled" {
		t.Errorf("expected status 'volume_scaling_toggled', got %q", resp.Status)
	}
	if !resp.VolumeScaling {
		t.Error("expected volume_scaling true in response")
	}

	// Toggle off
	output.Reset()
	processCommands(strings.NewReader(input), &output)

	if volumeScaling {
		t.Error("expected volumeScaling to be false after second toggle")
	}
}

func TestStatusCommand(t *testing.T) {
	resetGlobals()
	minAmplitude = 0.1
	cooldownMs = 600

	input := `{"cmd":"status"}` + "\n"
	var output bytes.Buffer

	processCommands(strings.NewReader(input), &output)

	var resp struct {
		Status        string  `json:"status"`
		Paused        bool    `json:"paused"`
		Amplitude     float64 `json:"amplitude"`
		Cooldown      int     `json:"cooldown"`
		VolumeScaling bool    `json:"volume_scaling"`
	}
	if err := json.Unmarshal(output.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("expected status 'ok', got %q", resp.Status)
	}
	if resp.Paused != false {
		t.Errorf("expected paused false, got %t", resp.Paused)
	}
	if resp.Amplitude != 0.1 {
		t.Errorf("expected amplitude 0.1, got %f", resp.Amplitude)
	}
	if resp.Cooldown != 600 {
		t.Errorf("expected cooldown 600, got %d", resp.Cooldown)
	}
	if resp.VolumeScaling != false {
		t.Errorf("expected volume_scaling false, got %t", resp.VolumeScaling)
	}
}

func TestStatusCommandWhenPaused(t *testing.T) {
	resetGlobals()
	pausedMu.Lock()
	paused = true
	pausedMu.Unlock()

	input := `{"cmd":"status"}` + "\n"
	var output bytes.Buffer

	processCommands(strings.NewReader(input), &output)

	var resp struct {
		Paused bool `json:"paused"`
	}
	if err := json.Unmarshal(output.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.Paused != true {
		t.Errorf("expected paused true, got %t", resp.Paused)
	}
}

func TestUnknownCommand(t *testing.T) {
	resetGlobals()

	input := `{"cmd":"invalid"}` + "\n"
	var output bytes.Buffer

	processCommands(strings.NewReader(input), &output)

	var resp map[string]string
	if err := json.Unmarshal(output.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if _, hasError := resp["error"]; !hasError {
		t.Error("expected error field in response for unknown command")
	}
	if !strings.Contains(resp["error"], "unknown command") {
		t.Errorf("expected 'unknown command' error, got %q", resp["error"])
	}
}

func TestInvalidJSON(t *testing.T) {
	resetGlobals()

	input := `{not valid json}` + "\n"
	var output bytes.Buffer

	processCommands(strings.NewReader(input), &output)

	var resp map[string]string
	if err := json.Unmarshal(output.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if _, hasError := resp["error"]; !hasError {
		t.Error("expected error field in response for invalid JSON")
	}
	if !strings.Contains(resp["error"], "invalid command") {
		t.Errorf("expected 'invalid command' error, got %q", resp["error"])
	}
}

func TestEmptyLines(t *testing.T) {
	resetGlobals()

	// Empty lines should be skipped, only the status command should produce output
	input := "\n\n" + `{"cmd":"status"}` + "\n\n"
	var output bytes.Buffer

	processCommands(strings.NewReader(input), &output)

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 1 {
		t.Errorf("expected 1 output line, got %d: %v", len(lines), lines)
	}
}

func TestMultipleCommands(t *testing.T) {
	resetGlobals()

	input := `{"cmd":"pause"}
{"cmd":"status"}
{"cmd":"resume"}
{"cmd":"status"}
`
	var output bytes.Buffer

	processCommands(strings.NewReader(input), &output)

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 output lines, got %d", len(lines))
	}

	// First: paused
	var resp1 map[string]string
	json.Unmarshal([]byte(lines[0]), &resp1)
	if resp1["status"] != "paused" {
		t.Errorf("line 1: expected 'paused', got %q", resp1["status"])
	}

	// Second: status shows paused=true
	var resp2 struct {
		Paused bool `json:"paused"`
	}
	json.Unmarshal([]byte(lines[1]), &resp2)
	if !resp2.Paused {
		t.Error("line 2: expected paused=true")
	}

	// Third: resumed
	var resp3 map[string]string
	json.Unmarshal([]byte(lines[2]), &resp3)
	if resp3["status"] != "resumed" {
		t.Errorf("line 3: expected 'resumed', got %q", resp3["status"])
	}

	// Fourth: status shows paused=false
	var resp4 struct {
		Paused bool `json:"paused"`
	}
	json.Unmarshal([]byte(lines[3]), &resp4)
	if resp4.Paused {
		t.Error("line 4: expected paused=false")
	}
}

func TestAmplitudeToVolume(t *testing.T) {
	tests := []struct {
		name      string
		amplitude float64
		wantMin   float64
		wantMax   float64
	}{
		{"below minimum returns min volume", 0.01, -3.0, -3.0},
		{"at minimum returns min volume", 0.05, -3.0, -3.0},
		{"above maximum returns max volume", 1.0, 0.0, 0.0},
		{"at maximum returns max volume", 0.80, 0.0, 0.0},
		{"mid amplitude returns mid-range", 0.40, -1.0, -0.4},
		{"low amplitude is quieter than high", 0.10, -3.0, -1.5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := amplitudeToVolume(tt.amplitude)
			if got < tt.wantMin || got > tt.wantMax {
				t.Errorf("amplitudeToVolume(%f) = %f, want in [%f, %f]",
					tt.amplitude, got, tt.wantMin, tt.wantMax)
			}
		})
	}

	// Monotonicity: higher amplitude should yield higher (or equal) volume
	prev := amplitudeToVolume(0.05)
	for amp := 0.10; amp <= 0.80; amp += 0.05 {
		cur := amplitudeToVolume(amp)
		if cur < prev-1e-9 {
			t.Errorf("non-monotonic: amplitudeToVolume(%f)=%f < amplitudeToVolume(prev)=%f",
				amp, cur, prev)
		}
		prev = cur
	}

	// Verify no NaN or Inf
	for _, amp := range []float64{0, 0.05, 0.1, 0.5, 0.8, 1.0, 10.0} {
		v := amplitudeToVolume(amp)
		if math.IsNaN(v) || math.IsInf(v, 0) {
			t.Errorf("amplitudeToVolume(%f) returned %f", amp, v)
		}
	}
}

func TestNoOutputWhenStdioModeDisabled(t *testing.T) {
	resetGlobals()
	stdioMode = false

	input := `{"cmd":"pause"}
{"cmd":"status"}
{"cmd":"set","amplitude":0.5}
`
	var output bytes.Buffer

	processCommands(strings.NewReader(input), &output)

	// No output should be produced when stdioMode is false
	if output.Len() != 0 {
		t.Errorf("expected no output when stdioMode=false, got %q", output.String())
	}

	// But state should still change
	pausedMu.RLock()
	if !paused {
		t.Error("expected paused to be true even with stdioMode=false")
	}
	pausedMu.RUnlock()

	if minAmplitude != 0.5 {
		t.Errorf("expected minAmplitude 0.5, got %f", minAmplitude)
	}
}

func pcmFromInt16(samples []int16) []byte {
	out := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(out[i*2:i*2+2], uint16(s))
	}
	return out
}

func sinePCM(freqHz float64, peak int16, sampleRate int, sampleCount int) []byte {
	samples := make([]int16, sampleCount)
	for i := 0; i < sampleCount; i++ {
		v := math.Sin(2 * math.Pi * freqHz * float64(i) / float64(sampleRate))
		samples[i] = int16(v * float64(peak))
	}
	return pcmFromInt16(samples)
}

func TestMicAmplitudeFromPCM(t *testing.T) {
	noiseSamples := make([]int16, 320)
	for i := range noiseSamples {
		noiseSamples[i] = 700
	}

	noiseChunk := pcmFromInt16(noiseSamples)
	amp, floor := micAmplitudeFromPCM(noiseChunk, 0)
	if amp > 0.001 {
		t.Fatalf("expected near-zero amplitude for baseline chunk, got %f", amp)
	}
	if floor <= 0 {
		t.Fatalf("expected positive noise floor, got %f", floor)
	}

	amp2, floor2 := micAmplitudeFromPCM(noiseChunk, floor)
	if amp2 > 0.005 {
		t.Fatalf("expected low amplitude for repeated baseline chunk, got %f", amp2)
	}
	if floor2 <= 0 {
		t.Fatalf("expected positive updated floor, got %f", floor2)
	}

	impactSamples := make([]int16, 320)
	for i := 150; i < 162; i++ {
		if i%2 == 0 {
			impactSamples[i] = 28000
		} else {
			impactSamples[i] = -28000
		}
	}
	impactChunk := pcmFromInt16(impactSamples)
	impactAmp, _ := micAmplitudeFromPCM(impactChunk, floor2)
	if impactAmp <= 0.05 {
		t.Fatalf("expected impact amplitude > 0.05, got %f", impactAmp)
	}
}

func TestMicStrictClassifierRejectsVoice(t *testing.T) {
	noiseSamples := make([]int16, 320)
	for i := range noiseSamples {
		noiseSamples[i] = 700
	}
	noiseStats := micFrameStatsFromPCM(pcmFromInt16(noiseSamples), 0)

	voiceChunk := sinePCM(440, 12000, defaultMicSampleRate, 320)
	voiceStats := micFrameStatsFromPCM(voiceChunk, noiseStats.NoiseFloor)
	if voiceStats.Crest >= 4.0 {
		t.Fatalf("expected voice crest < 4.0, got %f", voiceStats.Crest)
	}
	if voiceStats.HFRatio >= 0.55 {
		t.Fatalf("expected voice hf ratio < 0.55, got %f", voiceStats.HFRatio)
	}

	state := micStrictState{}
	if state.accept(voiceStats, 0.02) {
		t.Fatalf("strict classifier should reject voice-like chunk: %+v", voiceStats)
	}
}

func TestMicStrictClassifierAcceptsImpact(t *testing.T) {
	noiseSamples := make([]int16, 320)
	for i := range noiseSamples {
		noiseSamples[i] = 700
	}
	noiseStats := micFrameStatsFromPCM(pcmFromInt16(noiseSamples), 0)

	impactSamples := make([]int16, 320)
	for i := 148; i < 164; i++ {
		if i%2 == 0 {
			impactSamples[i] = 30000
		} else {
			impactSamples[i] = -30000
		}
	}
	impactStats := micFrameStatsFromPCM(pcmFromInt16(impactSamples), noiseStats.NoiseFloor)

	state := micStrictState{}
	_ = state.accept(noiseStats, 0.05)
	if !state.accept(impactStats, 0.05) {
		t.Fatalf("strict classifier should accept impact-like chunk: %+v", impactStats)
	}
}

func TestSexyModeAvoidsLastThreeRepeats(t *testing.T) {
	pack := &soundPack{
		mode:  modeEscalation,
		files: []string{"a.mp3", "b.mp3", "c.mp3", "d.mp3", "e.mp3"},
	}
	tracker := newSlapTracker(pack, 750*time.Millisecond)

	recent := make([]string, 0, 3)
	for i := 0; i < 80; i++ {
		// High score unlocks the full range so this exercises no-repeat logic.
		cur, _ := tracker.getFile(20.0)
		for _, r := range recent {
			if cur == r {
				t.Fatalf("got repeat within last 3 at iteration %d: %s", i, cur)
			}
		}
		recent = append(recent, cur)
		if len(recent) > 3 {
			recent = recent[1:]
		}
	}
}

func TestRandomModeAvoidsLastThreeRepeats(t *testing.T) {
	pack := &soundPack{
		mode:  modeRandom,
		files: []string{"a.mp3", "b.mp3", "c.mp3", "d.mp3", "e.mp3"},
	}
	tracker := newSlapTracker(pack, 750*time.Millisecond)

	recent := make([]string, 0, 3)
	for i := 0; i < 80; i++ {
		cur, _ := tracker.getFile(1.0)
		for _, r := range recent {
			if cur == r {
				t.Fatalf("random mode repeated within last 3 at iteration %d: %s", i, cur)
			}
		}
		recent = append(recent, cur)
		if len(recent) > 3 {
			recent = recent[1:]
		}
	}
}

func TestSexyModeAvoidsLastThreeRepeatsAtLowScore(t *testing.T) {
	pack := &soundPack{
		mode:  modeEscalation,
		files: []string{"a.mp3", "b.mp3", "c.mp3", "d.mp3", "e.mp3"},
	}
	tracker := newSlapTracker(pack, 750*time.Millisecond)

	recent := make([]string, 0, 3)
	for i := 0; i < 80; i++ {
		// Low score exercises early-tier pool sizing.
		cur, _ := tracker.getFile(1.0)
		for _, r := range recent {
			if cur == r {
				t.Fatalf("low-score sexy mode repeated within last 3 at iteration %d: %s", i, cur)
			}
		}
		recent = append(recent, cur)
		if len(recent) > 3 {
			recent = recent[1:]
		}
	}
}
