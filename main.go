// spank detects slaps/hits on the laptop and plays audio responses.
// Default input is Apple Silicon accelerometer via IOKit HID (needs sudo).
// Optional --mic input mode detects impact transients from microphone audio.
package main

import (
	"bufio"
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"embed"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/fang"
	"github.com/gopxl/beep/v2"
	"github.com/gopxl/beep/v2/effects"
	"github.com/gopxl/beep/v2/mp3"
	"github.com/gopxl/beep/v2/speaker"
	"github.com/spf13/cobra"
	"github.com/taigrr/apple-silicon-accelerometer/detector"
	"github.com/taigrr/apple-silicon-accelerometer/sensor"
	"github.com/taigrr/apple-silicon-accelerometer/shm"
)

var version = "dev"

//go:embed audio/pain/*.mp3
var painAudio embed.FS

//go:embed audio/sexy/*.mp3
var sexyAudio embed.FS

//go:embed audio/halo/*.mp3
var haloAudio embed.FS

var (
	sexyMode        bool
	haloMode        bool
	susMode         bool
	customPath      string
	customFiles     []string
	micMode         bool
	micDevice       string
	strictMode      bool
	fastMode        bool
	cutOnSlap       bool
	audioBackend    string
	outputVolume    float64
	feedbackGuardMs int
	minAmplitude    float64
	cooldownMs      int
	stdioMode       bool
	volumeScaling   bool
	paused          bool
	pausedMu        sync.RWMutex
	speedRatio      float64
)

// sensorReady is closed once shared memory is created and the sensor
// worker is about to enter the CFRunLoop.
var sensorReady = make(chan struct{})

// sensorErr receives any error from the sensor worker.
var sensorErr = make(chan error, 1)

type playMode int

const (
	modeRandom playMode = iota
	modeEscalation
)

const (
	// decayHalfLife is how many seconds of inactivity before intensity
	// halves. Controls how fast escalation fades.
	decayHalfLife = 30.0

	// defaultMinAmplitude is the default detection threshold.
	defaultMinAmplitude = 0.05

	// defaultCooldownMs is the default cooldown between audio responses.
	defaultCooldownMs = 750

	// defaultSpeedRatio is the default playback speed (1.0 = normal).
	defaultSpeedRatio = 1.0

	// defaultOutputVolume is the global output gain (1.0 = unchanged).
	defaultOutputVolume = 1.0

	// defaultFeedbackGuardMs is an additional mic-only post-playback
	// suppression window to reduce speaker feedback self-triggers.
	defaultFeedbackGuardMs = 220

	// defaultSensorPollInterval is how often we check for new accelerometer data.
	defaultSensorPollInterval = 10 * time.Millisecond

	// defaultMaxSampleBatch caps the number of accelerometer samples processed
	// per tick to avoid falling behind.
	defaultMaxSampleBatch = 200

	// sensorStartupDelay gives the sensor time to start producing data.
	sensorStartupDelay = 100 * time.Millisecond

	// defaultMicSampleRate is the ffmpeg capture sample rate in Hz.
	defaultMicSampleRate = 16000

	// defaultMicChunkSize is the number of PCM bytes per detection frame
	// (20ms at 16kHz mono s16le = 320 samples = 640 bytes).
	defaultMicChunkSize = 640

	// recentRepeatWindow prevents replaying the same clip too soon.
	recentRepeatWindow = 3

	// audioBufferDefault is the speaker buffer for normal mode.
	// Beep's speaker is most reliable around ~100ms on macOS.
	audioBufferDefault = 100 * time.Millisecond

	// audioBufferFast is the speaker buffer when --fast is enabled.
	// Keep this lower than default for responsiveness, but not ultra-low.
	audioBufferFast = 60 * time.Millisecond

	// minInterruptHold is the minimum time a clip gets before a new slap can
	// interrupt it. This avoids rapid-cut silence when files have short intros.
	minInterruptHold = 140 * time.Millisecond

	// rapidFireIntroSkip trims a small leading portion of clips in aggressive
	// rapid-fire settings so audible content starts sooner.
	rapidFireIntroSkip = 220 * time.Millisecond
)

type runtimeTuning struct {
	minAmplitude float64
	cooldown     time.Duration
	pollInterval time.Duration
	maxBatch     int
}

func defaultTuning() runtimeTuning {
	return runtimeTuning{
		minAmplitude: defaultMinAmplitude,
		cooldown:     time.Duration(defaultCooldownMs) * time.Millisecond,
		pollInterval: defaultSensorPollInterval,
		maxBatch:     defaultMaxSampleBatch,
	}
}

func applyFastOverlay(base runtimeTuning) runtimeTuning {
	base.pollInterval = 4 * time.Millisecond
	base.cooldown = 350 * time.Millisecond
	if base.minAmplitude > 0.18 {
		base.minAmplitude = 0.18
	}
	if base.maxBatch < 320 {
		base.maxBatch = 320
	}
	return base
}

type soundPack struct {
	name     string
	fs       embed.FS
	dir      string
	mode     playMode
	files    []string
	custom   bool
	isSus    bool
	susFiles map[int][]string
}

func (sp *soundPack) loadFiles() error {
	if sp.isSus {
		sp.susFiles = make(map[int][]string)
		levels := []string{"1_nani", "2_tsun", "3_kyaa", "4_yamete", "5_grabbed"}
		for i, lvl := range levels {
			path := sp.dir + "/audio/" + lvl
			entries, err := os.ReadDir(path)
			if err != nil {
				continue
			}
			for _, entry := range entries {
				if !entry.IsDir() {
					ext := strings.ToLower(filepath.Ext(entry.Name()))
					if ext == ".mp3" || ext == ".m4a" || ext == ".wav" || ext == ".mov" || ext == ".aac" {
						sp.susFiles[i+1] = append(sp.susFiles[i+1], path+"/"+entry.Name())
					}
				}
			}
		}
		return nil
	}
	if sp.custom {
		entries, err := os.ReadDir(sp.dir)
		if err != nil {
			return err
		}
		sp.files = make([]string, 0, len(entries))
		for _, entry := range entries {
			if !entry.IsDir() {
				sp.files = append(sp.files, sp.dir+"/"+entry.Name())
			}
		}
	} else {
		entries, err := sp.fs.ReadDir(sp.dir)
		if err != nil {
			return err
		}
		sp.files = make([]string, 0, len(entries))
		for _, entry := range entries {
			if !entry.IsDir() {
				sp.files = append(sp.files, sp.dir+"/"+entry.Name())
			}
		}
	}
	sort.Strings(sp.files)
	if sp.mode == modeEscalation {
		shuffleFiles(sp.files)
	}
	if len(sp.files) == 0 {
		return fmt.Errorf("no audio files found in %s", sp.dir)
	}
	return nil
}

func shuffleFiles(files []string) {
	var seed int64
	var seedBuf [8]byte
	if _, err := cryptorand.Read(seedBuf[:]); err == nil {
		seed = int64(binary.LittleEndian.Uint64(seedBuf[:]))
	} else {
		seed = time.Now().UnixNano()
	}
	r := rand.New(rand.NewSource(seed))
	r.Shuffle(len(files), func(i, j int) {
		files[i], files[j] = files[j], files[i]
	})
}

type slapTracker struct {
	mu        sync.Mutex
	score     float64
	lastTime  time.Time
	total     int
	recentIdx []int
	halfLife  float64 // seconds
	scale     float64 // controls the escalation curve shape
	pack      *soundPack
	susSlaps  []time.Time
	susLevel  int
	susBags   map[int][]int // index of files for each level
}

func newSlapTracker(pack *soundPack, cooldown time.Duration) *slapTracker {
	// scale maps the exponential curve so that sustained max-rate
	// slapping (one per cooldown) reaches the final file. At steady
	// state the score converges to ssMax; we set scale so that score
	// maps to the last index.
	cooldownSec := cooldown.Seconds()
	ssMax := 1.0 / (1.0 - math.Pow(0.5, cooldownSec/decayHalfLife))
	scale := (ssMax - 1) / math.Log(float64(len(pack.files)+1))
	return &slapTracker{
		halfLife:  decayHalfLife,
		recentIdx: make([]int, 0, recentRepeatWindow),
		scale:     scale,
		pack:      pack,
		susBags:   make(map[int][]int),
	}
}

func (st *slapTracker) record(now time.Time, amplitude float64) (int, float64) {
	st.mu.Lock()
	defer st.mu.Unlock()

	if st.pack.isSus {
		// Clean up old slaps in the 10-second window
		cutoff := now.Add(-10 * time.Second)
		var valid []time.Time
		for _, s := range st.susSlaps {
			if s.After(cutoff) {
				valid = append(valid, s)
			}
		}
		valid = append(valid, now)
		st.susSlaps = valid

		st.total++
		
		count := len(st.susSlaps)
		if count >= 10 || amplitude > 0.8 {
			st.susLevel = 4 // Yamete
		} else if count >= 5 {
			st.susLevel = 3 // Kyaa
		} else if count >= 2 {
			st.susLevel = 2 // Tsun
		} else {
			st.susLevel = 1 // Nani
		}
		return st.total, float64(st.susLevel)
	}

	if !st.lastTime.IsZero() {
		elapsed := now.Sub(st.lastTime).Seconds()
		st.score *= math.Pow(0.5, elapsed/st.halfLife)
	}
	st.score += 1.0
	st.lastTime = now
	st.total++
	return st.total, st.score
}

func (st *slapTracker) getFile(score float64) (string, int) {
	st.mu.Lock()
	defer st.mu.Unlock()

	if st.pack.isSus {
		level := st.susLevel
		if level == 0 {
			level = 1
		}
		files := st.pack.susFiles[level]
		if len(files) == 0 {
			for l := 1; l <= 4; l++ {
				if len(st.pack.susFiles[l]) > 0 {
					files = st.pack.susFiles[l]
					level = l
					break
				}
			}
		}
		if len(files) == 0 {
			return "", level
		}

		// Shuffle-bag logic
		bag := st.susBags[level]
		if len(bag) == 0 {
			// Refill bag
			bag = make([]int, len(files))
			for i := range bag {
				bag[i] = i
			}
			rand.Shuffle(len(bag), func(i, j int) {
				bag[i], bag[j] = bag[j], bag[i]
			})
		}
		idx := bag[0]
		st.susBags[level] = bag[1:]
		return files[idx], level
	}

	maxIdx := len(st.pack.files) - 1
	poolMax := maxIdx

	if st.pack.mode == modeEscalation {
		// Escalation: 1-exp(-x) curve maps score to file index.
		// At sustained max slap rate, score reaches ssMax which maps
		// to the final file. Randomize within unlocked range to keep
		// variety while preserving intensity progression.
		baseIdx := int(float64(len(st.pack.files)) * (1.0 - math.Exp(-(score-1)/st.scale)))
		if baseIdx > maxIdx {
			baseIdx = maxIdx
		}
		// Keep at least 5 clips in the pool (idx 0..4) when available.
		// With a 3-event no-repeat window, 4 clips forces a cycle pattern.
		minPoolIdx := recentRepeatWindow + 1
		if minPoolIdx > maxIdx {
			minPoolIdx = maxIdx
		}
		poolMax = baseIdx
		if minPoolIdx > poolMax {
			poolMax = minPoolIdx
		}
	}

	idx := st.pickIdxFromPool(poolMax)
	st.rememberIdx(idx)
	return st.pack.files[idx], 0
}

func (sp *soundPack) getGrabbedFile() (string, int) {
	if !sp.isSus {
		return "", 0
	}
	files := sp.susFiles[5]
	if len(files) == 0 {
		return "", 5
	}
	idx := rand.Intn(len(files))
	return files[idx], 5
}

func (st *slapTracker) pickIdxFromPool(poolMax int) int {
	if poolMax <= 0 {
		return 0
	}

	candidates := make([]int, 0, poolMax+1)
	for i := 0; i <= poolMax; i++ {
		if !st.inRecent(i) {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		// Pool is smaller than the no-repeat window.
		return rand.Intn(poolMax + 1)
	}
	return candidates[rand.Intn(len(candidates))]
}

func (st *slapTracker) inRecent(idx int) bool {
	for _, r := range st.recentIdx {
		if r == idx {
			return true
		}
	}
	return false
}

func (st *slapTracker) rememberIdx(idx int) {
	st.recentIdx = append(st.recentIdx, idx)
	if len(st.recentIdx) > recentRepeatWindow {
		st.recentIdx = st.recentIdx[len(st.recentIdx)-recentRepeatWindow:]
	}
}

func main() {
	cmd := &cobra.Command{
		Use:   "spank",
		Short: "Yells 'ow!' when you slap the laptop",
		Long: `spank reads the Apple Silicon accelerometer directly via IOKit HID
and plays audio responses when a slap or hit is detected.

By default it uses the accelerometer and requires sudo.
Use --mic to detect impacts from microphone transients (no sudo).

Use --sexy for a different experience. In sexy mode, the more you slap
within a minute, the more intense the sounds become.

Use --halo to play random audio clips from Halo soundtracks on each slap.`,
		Version: version,
		RunE: func(cmd *cobra.Command, args []string) error {
			tuning := defaultTuning()
			if fastMode {
				tuning = applyFastOverlay(tuning)
			}
			// Explicit flags override fast preset defaults
			if cmd.Flags().Changed("min-amplitude") {
				tuning.minAmplitude = minAmplitude
			}
			if cmd.Flags().Changed("cooldown") {
				tuning.cooldown = time.Duration(cooldownMs) * time.Millisecond
			}
			return run(cmd.Context(), tuning)
		},
		SilenceUsage: true,
	}

	cmd.Flags().BoolVarP(&sexyMode, "sexy", "s", false, "Enable sexy mode")
	cmd.Flags().BoolVarP(&haloMode, "halo", "H", false, "Enable halo mode")
	cmd.Flags().BoolVar(&susMode, "sus", false, "Enable desktop pet anime mode")
	cmd.Flags().StringVarP(&customPath, "custom", "c", "", "Path to custom MP3 audio directory")
	cmd.Flags().BoolVar(&micMode, "mic", false, "Use microphone transient detection instead of accelerometer (no sudo)")
	cmd.Flags().StringVar(&micDevice, "mic-device", "0", "avfoundation audio device index used by --mic (for example: 0, 1)")
	cmd.Flags().BoolVar(&strictMode, "strict", false, "Stricter impact classifier for --mic (helps reject voice/screaming triggers)")
	cmd.Flags().BoolVar(&fastMode, "fast", false, "Enable faster detection tuning (shorter cooldown, higher sensitivity)")
	cmd.Flags().BoolVar(&cutOnSlap, "cut-on-slap", false, "Stop current audio and restart immediately on each new slap (can be choppy)")
	cmd.Flags().StringVar(&audioBackend, "audio-backend", "beep", "Audio output backend: beep or afplay")
	cmd.Flags().StringSliceVar(&customFiles, "custom-files", nil, "Comma-separated list of custom MP3 files")
	cmd.Flags().Float64Var(&minAmplitude, "min-amplitude", defaultMinAmplitude, "Minimum amplitude threshold (0.0-1.0, lower = more sensitive)")
	cmd.Flags().IntVar(&cooldownMs, "cooldown", defaultCooldownMs, "Cooldown between responses in milliseconds")
	cmd.Flags().IntVar(&feedbackGuardMs, "feedback-guard", defaultFeedbackGuardMs, "Extra mic-only suppression window in ms to reduce speaker self-trigger")
	cmd.Flags().BoolVar(&stdioMode, "stdio", false, "Enable stdio mode: JSON output and stdin commands (for GUI integration)")
	cmd.Flags().BoolVar(&volumeScaling, "volume-scaling", false, "Scale playback volume by slap amplitude (harder hits = louder)")
	cmd.Flags().Float64Var(&outputVolume, "output-volume", defaultOutputVolume, "Master playback volume cap (0.0-1.0)")
	cmd.Flags().Float64Var(&speedRatio, "speed", defaultSpeedRatio, "Playback speed multiplier (0.5 = half speed, 2.0 = double speed)")

	if err := fang.Execute(context.Background(), cmd); err != nil {
		os.Exit(1)
	}
}

func run(ctx context.Context, tuning runtimeTuning) error {
	if !micMode && os.Geteuid() != 0 {
		return fmt.Errorf("spank requires root privileges for accelerometer access, run with: sudo spank")
	}

	modeCount := 0
	if sexyMode {
		modeCount++
	}
	if haloMode {
		modeCount++
	}
	if susMode {
		modeCount++
	}
	if customPath != "" || len(customFiles) > 0 {
		modeCount++
	}
	if modeCount > 1 {
		return fmt.Errorf("--sexy, --halo, --sus, and --custom/--custom-files are mutually exclusive; pick one")
	}

	if tuning.minAmplitude < 0 || tuning.minAmplitude > 1 {
		return fmt.Errorf("--min-amplitude must be between 0.0 and 1.0")
	}
	if tuning.cooldown <= 0 {
		return fmt.Errorf("--cooldown must be greater than 0")
	}
	if outputVolume < 0 || outputVolume > 1 {
		return fmt.Errorf("--output-volume must be between 0.0 and 1.0")
	}
	if feedbackGuardMs < 0 {
		return fmt.Errorf("--feedback-guard must be 0 or greater")
	}
	audioBackend = strings.ToLower(strings.TrimSpace(audioBackend))
	if audioBackend != "beep" && audioBackend != "afplay" {
		return fmt.Errorf("--audio-backend must be one of: beep, afplay")
	}
	if audioBackend == "afplay" {
		if _, err := exec.LookPath("afplay"); err != nil {
			return fmt.Errorf("audio backend 'afplay' requires afplay in PATH")
		}
		defer stopAfplayPlayback()
		defer cleanupAfplayTempFiles()
	}
	if micMode && strings.TrimSpace(micDevice) == "" {
		return fmt.Errorf("--mic-device cannot be empty")
	}
	if strictMode && !micMode {
		return fmt.Errorf("--strict only applies with --mic")
	}

	if susMode {
		if runtime.GOOS == "darwin" && audioBackend == "beep" {
			audioBackend = "afplay"
		}
		// Overlap sounds by default in sus mode
		cutOnSlap = false
		// Drop cooldown significantly to allow "rapid fire"
		if tuning.cooldown == time.Duration(defaultCooldownMs)*time.Millisecond {
			tuning.cooldown = 180 * time.Millisecond
			cooldownMs = 180
		}
	}

	var pack *soundPack
	switch {
	case len(customFiles) > 0:
		// Validate all files exist and are MP3s
		for _, f := range customFiles {
			if !strings.HasSuffix(strings.ToLower(f), ".mp3") {
				return fmt.Errorf("custom file must be MP3: %s", f)
			}
			if _, err := os.Stat(f); err != nil {
				return fmt.Errorf("custom file not found: %s", f)
			}
		}
		pack = &soundPack{name: "custom", mode: modeRandom, custom: true, files: customFiles}
	case customPath != "":
		pack = &soundPack{name: "custom", dir: customPath, mode: modeRandom, custom: true}
	case susMode:
		pack = &soundPack{name: "sus", dir: "sus_assets", mode: modeRandom, custom: true, isSus: true}
	case sexyMode:
		pack = &soundPack{name: "sexy", fs: sexyAudio, dir: "audio/sexy", mode: modeEscalation}
	case haloMode:
		pack = &soundPack{name: "halo", fs: haloAudio, dir: "audio/halo", mode: modeRandom}
	default:
		pack = &soundPack{name: "pain", fs: painAudio, dir: "audio/pain", mode: modeRandom}
	}

	// Only load files if not already set (customFiles case)
	if len(pack.files) == 0 {
		if err := pack.loadFiles(); err != nil {
			return fmt.Errorf("loading %s audio: %w", pack.name, err)
		}
	}

	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var overlayStdin io.WriteCloser
	if susMode {
		cmdOverlay := exec.Command("swift", "sus_overlay.swift", pack.dir+"/visual")
		stdin, err := cmdOverlay.StdinPipe()
		if err != nil {
			fmt.Printf("spank warning: could not pipe to overlay: %v\n", err)
		} else {
			overlayStdin = stdin
			if stdout, err := cmdOverlay.StdoutPipe(); err == nil {
				if err := cmdOverlay.Start(); err != nil {
					fmt.Printf("spank warning: could not start overlay: %v\n", err)
				} else {
					defer func() {
						stdin.Close()
						cmdOverlay.Wait()
					}()
					go func() {
						scanner := bufio.NewScanner(stdout)
						for scanner.Scan() {
							line := scanner.Text()
							if line == "grabbed" {
								file, _ := pack.getGrabbedFile()
								if file != "" {
									var speakerInit bool
									playAudio(pack, file, 1.0, &speakerInit)
								}
							}
						}
					}()
				}
			} else {
				// Fallback if pipe fails
				cmdOverlay.Stdout = os.Stdout
				_ = cmdOverlay.Start()
			}
		}
	}

	if micMode {
		return listenForMicSlaps(ctx, pack, tuning, overlayStdin)
	}

	// Create shared memory for accelerometer data.
	accelRing, err := shm.CreateRing(shm.NameAccel)
	if err != nil {
		return fmt.Errorf("creating accel shm: %w", err)
	}
	defer accelRing.Close()
	defer accelRing.Unlink()

	// Start the sensor worker in a background goroutine.
	// sensor.Run() needs runtime.LockOSThread for CFRunLoop, which it
	// handles internally. We launch detection on the current goroutine.
	go func() {
		close(sensorReady)
		if err := sensor.Run(sensor.Config{
			AccelRing: accelRing,
			Restarts:  0,
		}); err != nil {
			sensorErr <- err
		}
	}()

	// Wait for sensor to be ready.
	select {
	case <-sensorReady:
	case err := <-sensorErr:
		return fmt.Errorf("sensor worker failed: %w", err)
	case <-ctx.Done():
		return nil
	}

	// Give the sensor a moment to start producing data.
	time.Sleep(sensorStartupDelay)

	return listenForSlaps(ctx, pack, accelRing, tuning, overlayStdin)
}

func listenForMicSlaps(ctx context.Context, pack *soundPack, tuning runtimeTuning, overlayStdin io.Writer) error {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return fmt.Errorf("mic mode requires ffmpeg in PATH (try: brew install ffmpeg)")
	}

	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-nostdin",
		"-f", "avfoundation",
		"-i", ":" + micDevice,
		"-ac", "1",
		"-ar", fmt.Sprintf("%d", defaultMicSampleRate),
		"-f", "s16le",
		"-",
	}
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("mic mode: creating ffmpeg stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("mic mode: starting ffmpeg: %w", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	tracker := newSlapTracker(pack, tuning.cooldown)
	speakerInit := false
	var lastYell time.Time
	noiseFloor := 0.0
	strictState := micStrictState{}
	chunk := make([]byte, defaultMicChunkSize)

	if stdioMode {
		go readStdinCommands()
	}

	presetLabel := "default"
	if fastMode {
		presetLabel = "fast"
	}
	classifierLabel := "normal"
	if strictMode {
		classifierLabel = "strict"
	}
	fmt.Printf("spank: listening for slaps in %s mode with %s tuning (input=mic classifier=%s)... (ctrl+c to quit)\n", pack.name, presetLabel, classifierLabel)
	if stdioMode {
		fmt.Println(`{"status":"ready"}`)
	}

	minGap := time.Duration(cooldownMs) * time.Millisecond
	feedbackGap := time.Duration(feedbackGuardMs) * time.Millisecond
	if feedbackGap > minGap {
		minGap = feedbackGap
	}

	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nbye!")
			return nil
		default:
		}

		if _, err := io.ReadFull(stdout, chunk); err != nil {
			if ctx.Err() != nil {
				fmt.Println("\nbye!")
				return nil
			}
			msg := strings.TrimSpace(stderr.String())
			if msg != "" {
				return fmt.Errorf("mic mode input stream ended: %w (%s)", err, msg)
			}
			return fmt.Errorf("mic mode input stream ended: %w", err)
		}

		pausedMu.RLock()
		isPaused := paused
		pausedMu.RUnlock()
		if isPaused {
			continue
		}

		stats := micFrameStatsFromPCM(chunk, noiseFloor)
		noiseFloor = stats.NoiseFloor
		amplitude := stats.Amplitude

		if time.Since(lastYell) <= minGap {
			continue
		}
		if strictMode {
			if !strictState.accept(stats, minAmplitude) {
				continue
			}
		} else {
			if amplitude < minAmplitude {
				continue
			}
		}

		now := time.Now()
		lastYell = now
		num, score := tracker.record(now, amplitude)
		file, level := tracker.getFile(score)
		if overlayStdin != nil && level > 0 {
			fmt.Fprintf(overlayStdin, "%d\n", level)
		}
		if stdioMode {
			event := map[string]interface{}{
				"timestamp":  now.Format(time.RFC3339Nano),
				"slapNumber": num,
				"amplitude":  amplitude,
				"severity":   "mic",
				"file":       file,
			}
			if strictMode {
				event["classifier"] = "strict"
				event["crest"] = stats.Crest
				event["hfRatio"] = stats.HFRatio
			}
			if data, err := json.Marshal(event); err == nil {
				fmt.Println(string(data))
			}
		} else {
			if strictMode {
				fmt.Printf("slap #%d [mic/strict amp=%.5f crest=%.2f hf=%.2f] -> %s\n", num, amplitude, stats.Crest, stats.HFRatio, file)
			} else {
				fmt.Printf("slap #%d [mic amp=%.5f] -> %s\n", num, amplitude, file)
			}
			if !stdioMode {
				printSusReaction(score)
			}
		}
		if file != "" {
			go playAudio(pack, file, amplitude, &speakerInit)
		}
	}
}

type micFrameStats struct {
	Amplitude  float64
	NoiseFloor float64
	RMS        float64
	Peak       float64
	Crest      float64
	HFRatio    float64
}

type micStrictState struct {
	prevAmplitude float64
	ambientEMA    float64
}

func (s *micStrictState) accept(stats micFrameStats, threshold float64) bool {
	amp := stats.Amplitude
	if s.ambientEMA == 0 && amp > 0 {
		s.ambientEMA = amp
	} else if s.ambientEMA > 0 {
		s.ambientEMA = s.ambientEMA*0.95 + amp*0.05
	} else {
		s.ambientEMA = stats.NoiseFloor
	}

	prev := math.Max(s.prevAmplitude, 0.001)
	rise := amp / prev
	dominatesAmbient := amp > s.ambientEMA*1.8
	strong := amp >= threshold*1.25
	impulsive := stats.Crest >= 4.0
	bright := stats.HFRatio >= 0.55
	sudden := rise >= 2.3 || amp >= threshold*3.5

	hit := strong && impulsive && bright && dominatesAmbient && sudden
	s.prevAmplitude = amp
	return hit
}

func micFrameStatsFromPCM(chunk []byte, noiseFloor float64) micFrameStats {
	if len(chunk) < 2 {
		return micFrameStats{NoiseFloor: noiseFloor}
	}

	sampleCount := len(chunk) / 2
	var sumSquares float64
	var diffSquares float64
	var peak float64
	var prevSample float64
	havePrev := false
	for i := 0; i+1 < len(chunk); i += 2 {
		sample := int16(binary.LittleEndian.Uint16(chunk[i : i+2]))
		v := float64(sample) / 32768.0
		sumSquares += v * v
		absV := math.Abs(v)
		if absV > peak {
			peak = absV
		}
		if havePrev {
			d := v - prevSample
			diffSquares += d * d
		}
		prevSample = v
		havePrev = true
	}
	rms := math.Sqrt(sumSquares / float64(sampleCount))
	diffRMS := math.Sqrt(diffSquares / float64(max(sampleCount-1, 1)))
	hfRatio := diffRMS / (rms + 1e-9)

	// Track ambient mic noise with a slowly moving floor.
	updatedNoiseFloor := noiseFloor
	if updatedNoiseFloor == 0 {
		updatedNoiseFloor = rms
	} else if rms < updatedNoiseFloor {
		updatedNoiseFloor = updatedNoiseFloor*0.98 + rms*0.02
	} else {
		updatedNoiseFloor = updatedNoiseFloor*0.999 + rms*0.001
	}

	amplitude := rms - updatedNoiseFloor
	if amplitude < 0 {
		amplitude = 0
	}
	crest := peak / (rms + 1e-9)
	return micFrameStats{
		Amplitude:  amplitude,
		NoiseFloor: updatedNoiseFloor,
		RMS:        rms,
		Peak:       peak,
		Crest:      crest,
		HFRatio:    hfRatio,
	}
}

func micAmplitudeFromPCM(chunk []byte, noiseFloor float64) (amplitude float64, updatedNoiseFloor float64) {
	stats := micFrameStatsFromPCM(chunk, noiseFloor)
	return stats.Amplitude, stats.NoiseFloor
}

func listenForSlaps(ctx context.Context, pack *soundPack, accelRing *shm.RingBuffer, tuning runtimeTuning, overlayStdin io.Writer) error {
	tracker := newSlapTracker(pack, tuning.cooldown)
	speakerInit := false
	det := detector.New()
	var lastAccelTotal uint64
	var lastEventTime time.Time
	var lastYell time.Time

	// Start stdin command reader if in JSON mode
	if stdioMode {
		go readStdinCommands()
	}

	presetLabel := "default"
	if fastMode {
		presetLabel = "fast"
	}
	fmt.Printf("spank: listening for slaps in %s mode with %s tuning... (ctrl+c to quit)\n", pack.name, presetLabel)
	if stdioMode {
		fmt.Println(`{"status":"ready"}`)
	}

	ticker := time.NewTicker(tuning.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nbye!")
			return nil
		case err := <-sensorErr:
			return fmt.Errorf("sensor worker failed: %w", err)
		case <-ticker.C:
		}

		// Check if paused
		pausedMu.RLock()
		isPaused := paused
		pausedMu.RUnlock()
		if isPaused {
			continue
		}

		now := time.Now()
		tNow := float64(now.UnixNano()) / 1e9

		samples, newTotal := accelRing.ReadNew(lastAccelTotal, shm.AccelScale)
		lastAccelTotal = newTotal
		if len(samples) > tuning.maxBatch {
			samples = samples[len(samples)-tuning.maxBatch:]
		}

		nSamples := len(samples)
		for idx, sample := range samples {
			tSample := tNow - float64(nSamples-idx-1)/float64(det.FS)
			det.Process(sample.X, sample.Y, sample.Z, tSample)
		}

		if len(det.Events) == 0 {
			continue
		}

		ev := det.Events[len(det.Events)-1]
		if ev.Time.Equal(lastEventTime) {
			continue
		}
		lastEventTime = ev.Time

		if time.Since(lastYell) <= time.Duration(cooldownMs)*time.Millisecond {
			continue
		}
		if ev.Amplitude < minAmplitude {
			continue
		}

		lastYell = now
		num, score := tracker.record(now, ev.Amplitude)
		file, level := tracker.getFile(score)
		if overlayStdin != nil && level > 0 {
			fmt.Fprintf(overlayStdin, "%d\n", level)
		}
		if stdioMode {
			event := map[string]interface{}{
				"timestamp":  now.Format(time.RFC3339Nano),
				"slapNumber": num,
				"amplitude":  ev.Amplitude,
				"severity":   string(ev.Severity),
				"file":       file,
			}
			if data, err := json.Marshal(event); err == nil {
				fmt.Println(string(data))
			}
		} else {
			fmt.Printf("slap #%d [%s amp=%.5fg] -> %s\n", num, ev.Severity, ev.Amplitude, file)
			if !stdioMode {
				printSusReaction(score)
			}
		}
		if file != "" {
			go playAudio(pack, file, ev.Amplitude, &speakerInit)
		}
	}
}

var speakerMu sync.Mutex
var activePlaybackStop func()
var activePlaybackID uint64
var activePlaybackStartedAt time.Time
var afplayMu sync.Mutex
var activeAfplay *exec.Cmd
var afplayTempMu sync.Mutex
var afplayTempFiles = map[string]string{}

// amplitudeToVolume maps a detected amplitude to a beep/effects.Volume
// level. Amplitude typically ranges from ~0.05 (light tap) to ~1.0+
// (hard slap). The mapping uses a logarithmic curve so that light taps
// are noticeably quieter and hard hits play near full volume.
//
// Returns a value in the range [-3.0, 0.0] for use with effects.Volume
// (base 2): -3.0 is ~1/8 volume, 0.0 is full volume.
func amplitudeToVolume(amplitude float64) float64 {
	const (
		minAmp = 0.05 // softest detectable
		maxAmp = 0.80 // treat anything above this as max
		minVol = -3.0 // quietest playback (1/8 volume with base 2)
		maxVol = 0.0  // full volume
	)

	// Clamp
	if amplitude <= minAmp {
		return minVol
	}
	if amplitude >= maxAmp {
		return maxVol
	}

	// Normalize to [0, 1]
	t := (amplitude - minAmp) / (maxAmp - minAmp)

	// Log curve for more natural volume scaling
	// log(1 + t*99) / log(100) maps [0,1] -> [0,1] with a log curve
	t = math.Log(1+t*99) / math.Log(100)

	return minVol + t*(maxVol-minVol)
}

func speakerBufferDuration() time.Duration {
	if fastMode {
		return audioBufferFast
	}
	return audioBufferDefault
}

func stopAfplayPlayback() {
	afplayMu.Lock()
	defer afplayMu.Unlock()
	if activeAfplay != nil && activeAfplay.Process != nil {
		_ = activeAfplay.Process.Kill()
	}
	activeAfplay = nil
}

func cleanupAfplayTempFiles() {
	afplayTempMu.Lock()
	defer afplayTempMu.Unlock()
	for _, p := range afplayTempFiles {
		_ = os.Remove(p)
	}
	afplayTempFiles = map[string]string{}
}

func resolveAfplayPath(pack *soundPack, path string) (string, error) {
	if abs, err := filepath.Abs(path); err == nil {
		if _, err := os.Stat(abs); err == nil {
			return abs, nil
		}
	}
	if pack.custom {
		return "", fmt.Errorf("custom audio file not found: %s", path)
	}

	afplayTempMu.Lock()
	if cached, ok := afplayTempFiles[path]; ok {
		if _, err := os.Stat(cached); err == nil {
			afplayTempMu.Unlock()
			return cached, nil
		}
		delete(afplayTempFiles, path)
	}
	afplayTempMu.Unlock()

	data, err := pack.fs.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read embedded audio %s: %w", path, err)
	}
	tmp, err := os.CreateTemp("", "spank-audio-*.mp3")
	if err != nil {
		return "", fmt.Errorf("create temp audio: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("write temp audio: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("close temp audio: %w", err)
	}

	afplayTempMu.Lock()
	afplayTempFiles[path] = tmp.Name()
	afplayTempMu.Unlock()
	return tmp.Name(), nil
}

func playAudioWithAfplay(pack *soundPack, path string) {
	if outputVolume <= 0 {
		return
	}

	audioPath, err := resolveAfplayPath(pack, path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "spank: %v\n", err)
		return
	}

	cmd := exec.Command("afplay", "-v", fmt.Sprintf("%.3f", outputVolume), audioPath)

	afplayMu.Lock()
	if activeAfplay != nil && activeAfplay.Process != nil {
		_ = activeAfplay.Process.Kill()
	}
	activeAfplay = cmd
	afplayMu.Unlock()

	go func(local *exec.Cmd, clip string) {
		if err := local.Run(); err != nil && !strings.Contains(err.Error(), "signal: killed") {
			// ignore normal kill errors
		}
		afplayMu.Lock()
		if activeAfplay == local {
			activeAfplay = nil
		}
		afplayMu.Unlock()
	}(cmd, path)
}

func playAudio(pack *soundPack, path string, amplitude float64, speakerInit *bool) {
	if outputVolume <= 0 {
		return
	}

	if audioBackend == "afplay" {
		playAudioWithAfplay(pack, path)
		return
	}

	var streamer beep.StreamSeekCloser
	var format beep.Format
	var closePlayback func()
	var closeOnce sync.Once

	if pack.custom {
		file, err := os.Open(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "spank: open %s: %v\n", path, err)
			return
		}
		streamer, format, err = mp3.Decode(file)
		if err != nil {
			_ = file.Close()
			fmt.Fprintf(os.Stderr, "spank: decode %s: %v\n", path, err)
			return
		}
		closePlayback = func() {
			closeOnce.Do(func() {
				_ = streamer.Close()
				_ = file.Close()
			})
		}
	} else {
		data, err := pack.fs.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "spank: read %s: %v\n", path, err)
			return
		}
		streamer, format, err = mp3.Decode(io.NopCloser(bytes.NewReader(data)))
		if err != nil {
			fmt.Fprintf(os.Stderr, "spank: decode %s: %v\n", path, err)
			return
		}
		closePlayback = func() {
			closeOnce.Do(func() {
				_ = streamer.Close()
			})
		}
	}

	// In rapid-fire cut mode, skip a short intro so audible content starts sooner.
	if cutOnSlap && time.Duration(cooldownMs)*time.Millisecond <= 180 {
		skipSamples := format.SampleRate.N(rapidFireIntroSkip)
		if total := streamer.Len(); total > 0 && skipSamples < total {
			_ = streamer.Seek(skipSamples)
		}
	}

	speakerMu.Lock()
	if !*speakerInit {
		if err := speaker.Init(format.SampleRate, format.SampleRate.N(speakerBufferDuration())); err != nil {
			speakerMu.Unlock()
			closePlayback()
			fmt.Fprintf(os.Stderr, "spank: speaker init failed: %v\n", err)
			return
		}
		*speakerInit = true
	}

	// Optionally scale volume based on slap amplitude
	var source beep.Streamer = streamer
	if volumeScaling {
		source = &effects.Volume{
			Streamer: streamer,
			Base:     2,
			Volume:   amplitudeToVolume(amplitude),
			Silent:   false,
		}
	}

	// Apply speed change via resampling trick:
	// Claiming the audio is at rate*speed and resampling back to rate
	// makes the speaker consume samples faster/slower.
	if speedRatio != 1.0 && speedRatio > 0 {
		fakeRate := beep.SampleRate(int(float64(format.SampleRate) * speedRatio))
		source = beep.Resample(4, fakeRate, format.SampleRate, source)
	}

	// Apply a master output cap to avoid mic feedback loops from loud clips.
	if outputVolume > 0 && outputVolume < 1 {
		source = &effects.Volume{
			Streamer: source,
			Base:     2,
			Volume:   math.Log2(outputVolume),
			Silent:   false,
		}
	}

	var playbackID uint64
	if cutOnSlap {
		// Hard-cut current clip so rapid slaps immediately play the new clip.
		if activePlaybackStop != nil {
			if !activePlaybackStartedAt.IsZero() && time.Since(activePlaybackStartedAt) < minInterruptHold {
				speakerMu.Unlock()
				closePlayback()
				return
			}
			speaker.Clear()
			activePlaybackStop()
			activePlaybackStop = nil
			activePlaybackStartedAt = time.Time{}
		}

		activePlaybackID++
		playbackID = activePlaybackID
		activePlaybackStop = closePlayback
		activePlaybackStartedAt = time.Now()
	}

	speaker.Play(beep.Seq(source, beep.Callback(func() {
		closePlayback()
		if cutOnSlap {
			speakerMu.Lock()
			if activePlaybackID == playbackID {
				activePlaybackStop = nil
				activePlaybackStartedAt = time.Time{}
			}
			speakerMu.Unlock()
		}
	})))
	speakerMu.Unlock()
}

// stdinCommand represents a command received via stdin
type stdinCommand struct {
	Cmd       string  `json:"cmd"`
	Amplitude float64 `json:"amplitude,omitempty"`
	Cooldown  int     `json:"cooldown,omitempty"`
	Speed     float64 `json:"speed,omitempty"`
}

// readStdinCommands reads JSON commands from stdin for live control
func readStdinCommands() {
	processCommands(os.Stdin, os.Stdout)
}

// processCommands reads JSON commands from r and writes responses to w.
// This is the testable core of the stdin command handler.
func processCommands(r io.Reader, w io.Writer) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var cmd stdinCommand
		if err := json.Unmarshal([]byte(line), &cmd); err != nil {
			if stdioMode {
				fmt.Fprintf(w, `{"error":"invalid command: %s"}%s`, err.Error(), "\n")
			}
			continue
		}

		switch cmd.Cmd {
		case "pause":
			pausedMu.Lock()
			paused = true
			pausedMu.Unlock()
			if stdioMode {
				fmt.Fprintln(w, `{"status":"paused"}`)
			}
		case "resume":
			pausedMu.Lock()
			paused = false
			pausedMu.Unlock()
			if stdioMode {
				fmt.Fprintln(w, `{"status":"resumed"}`)
			}
		case "set":
			if cmd.Amplitude > 0 && cmd.Amplitude <= 1 {
				minAmplitude = cmd.Amplitude
			}
			if cmd.Cooldown > 0 {
				cooldownMs = cmd.Cooldown
			}
			if cmd.Speed > 0 {
				speedRatio = cmd.Speed
			}
			if stdioMode {
				fmt.Fprintf(w, `{"status":"settings_updated","amplitude":%.4f,"cooldown":%d,"speed":%.2f}%s`, minAmplitude, cooldownMs, speedRatio, "\n")
			}
		case "volume-scaling":
			volumeScaling = !volumeScaling
			if stdioMode {
				fmt.Fprintf(w, `{"status":"volume_scaling_toggled","volume_scaling":%t}%s`, volumeScaling, "\n")
			}
		case "status":
			pausedMu.RLock()
			isPaused := paused
			pausedMu.RUnlock()
			if stdioMode {
				fmt.Fprintf(w, `{"status":"ok","paused":%t,"amplitude":%.4f,"cooldown":%d,"volume_scaling":%t,"speed":%.2f}%s`, isPaused, minAmplitude, cooldownMs, volumeScaling, speedRatio, "\n")
			}
		default:
			if stdioMode {
				fmt.Fprintf(w, `{"error":"unknown command: %s"}%s`, cmd.Cmd, "\n")
			}
		}
	}
}

func printSusReaction(score float64) {
	// ANSI colored ASCII art based on escalation score!
	pink := "\033[38;5;218m"
	red := "\033[38;5;196m"
	cyan := "\033[38;5;117m"
	reset := "\033[0m"

	var sprite string
	if score < 2 {
		sprite = `
   \` + cyan + `/\_/\` + reset + `  
  ( o.o ) 
   > ^ <  
  "Nani?"
`
	} else if score < 8 {
		sprite = `
   \` + cyan + `/\_/\` + reset + `  
  ( ` + pink + `>.<` + reset + ` ) 
   > ^ <  
  "Hmph! Weak..."
`
	} else if score < 18 {
		sprite = `
   \` + cyan + `/\_/\` + reset + `  
  ( ` + red + `≧ ω ≦` + reset + ` ) 
   > ^ <  
  "Kyaa~! Again!"
`
	} else {
		sprite = `
   \` + cyan + `/\_/\` + reset + `  
  ( ` + red + `♥ ͜ʖ ♥` + reset + ` ) 
   > ^ <  
  "Y-Yamete kudasai... more..."
`
	}
	fmt.Println(sprite)
}
