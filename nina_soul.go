package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	visionCycleInterval          = 5 * time.Minute
	geminiMinimumCallSpacing     = 4 * time.Minute
	defaultGeminiModel           = "gemini-2.5-flash-lite"
	fallbackGeminiModel          = "gemini-2.5-flash"
	defaultGeminiEmbeddingModel  = "gemini-embedding-001"
	fallbackGeminiEmbeddingModel = "gemini-embedding-2-preview"
	visionThoughtRetryAttempts   = 2
	minThoughtWords              = 8
	maxThoughtWords              = 20
	guardPollInterval            = 1 * time.Second
	shameHoldDuration            = 15 * time.Minute
	leisureGraceDuration         = 20 * time.Minute
	leisureNudgeDuration         = 40 * time.Minute
	focusBreakSuggestDuration    = 70 * time.Minute
	focusBreakStrongDuration     = 105 * time.Minute
	reflectionInterval           = 24 * time.Hour
	maxSemanticCandidates        = 5000
)

var (
	overlayWriteMu sync.Mutex

	privacyDenylistApps = []string{
		"1password",
		"bitwarden",
		"keychain",
		"system settings",
		"messages",
		"wallet",
		"bank",
	}

	focusKeywords = []string{
		"vscode", "xcode", "goland", "cursor", "terminal", "iterm", "warp",
		"docs", "notion", "obsidian", "github", "stackoverflow",
	}
	focusAppKeywords = []string{
		"notion", "obsidian", "anki", "endel", "forest", "cold turkey",
		"todoist", "ticktick", "linear", "jira", "figma", "miro", "logseq",
		"craft", "slack", "discord", "readwise", "raycast",
	}
	devAppKeywords = []string{
		"cursor", "code", "visual studio code", "xcode", "goland", "jetbrains",
		"terminal", "iterm", "warp", "kitty", "zed", "neovim", "vim", "emacs",
		"windsurf", "sublime text",
	}
	chillKeywords = []string{
		"youtube", "netflix", "anime", "twitter", "x.com", "reddit", "instagram", "tiktok",
	}
	procrastinationKeywords = []string{
		"shorts", "reels", "for you", "fyp", "doomscroll", "ragebait",
		"trending", "recommended", "explore", "discover", "infinite scroll",
	}
	gameKeywords = []string{
		"steam", "epic", "riot", "battle.net", "minecraft", "valorant", "league", "game",
	}
	musicKeywords = []string{
		"spotify", "music", "apple music", "soundcloud", "bandcamp",
	}
	youtubeMusicKeywords = []string{
		"official", "official video", "official audio", "vevo", "lyric",
		"playlist", "album", "mix", "visualizer", "audio",
	}
	browserKeywords = []string{
		"brave", "chrome", "safari", "firefox", "arc", "edge", "opera",
	}
	shameKeywords = []string{
		"porn", "nsfw", "hentai", "rule34", "onlyfans", "xvideos", "pornhub", "redgifs",
		"shorts", "reels", "for you", "doomscroll", "ragebait",
	}
)

type overlayPipeMessage struct {
	Type    string `json:"type"`
	Level   int    `json:"level,omitempty"`
	Tag     string `json:"tag,omitempty"`
	Mood    string `json:"mood,omitempty"`
	Thought string `json:"thought,omitempty"`
}

func sendOverlaySlapLevel(overlay io.Writer, level int) {
	if overlay == nil || level <= 0 {
		return
	}
	msg := overlayPipeMessage{Type: "slap", Level: level}
	if !sendOverlayMessage(overlay, msg) {
		overlayWriteMu.Lock()
		_, _ = fmt.Fprintf(overlay, "%d\n", level)
		overlayWriteMu.Unlock()
	}
}

func sendOverlayContext(overlay io.Writer, tag, mood, thought string) {
	if overlay == nil {
		return
	}
	_ = sendOverlayMessage(overlay, overlayPipeMessage{
		Type:    "context",
		Tag:     normalizeActivityTag(tag),
		Mood:    strings.TrimSpace(mood),
		Thought: strings.TrimSpace(thought),
	})
}

func sendOverlayMessage(overlay io.Writer, msg overlayPipeMessage) bool {
	if overlay == nil || strings.TrimSpace(msg.Type) == "" {
		return false
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return false
	}
	overlayWriteMu.Lock()
	defer overlayWriteMu.Unlock()
	fmt.Printf("[SOUL -> OVERLAY] %s\n", payload)
	_, err = fmt.Fprintf(overlay, "%s\n", payload)
	return err == nil
}

type activeWindowInfo struct {
	AppName  string
	Title    string
	WindowID string
}

func getActiveWindowInfo(ctx context.Context) (activeWindowInfo, error) {
	script := `tell application "System Events"
	set frontApp to first application process whose frontmost is true
	set appName to name of frontApp
	set winTitle to ""
	set winID to "0"
	try
		set frontWin to front window of frontApp
		set winTitle to name of frontWin
		set winID to (id of frontWin) as text
	on error
		try
			set firstWin to first window of frontApp
			set winTitle to name of firstWin
			set winID to (id of firstWin) as text
		end try
	end try
	return appName & "|||" & winTitle & "|||" & winID
end tell`

	cmd := exec.CommandContext(ctx, "osascript", "-e", script)
	out, err := cmd.Output()
	if err != nil {
		return activeWindowInfo{}, fmt.Errorf("active window query failed: %w", err)
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "|||", 3)
	if len(parts) < 3 {
		return activeWindowInfo{}, errors.New("active window query returned malformed data")
	}
	return activeWindowInfo{
		AppName:  strings.TrimSpace(parts[0]),
		Title:    strings.TrimSpace(parts[1]),
		WindowID: strings.TrimSpace(parts[2]),
	}, nil
}

func captureActiveWindowSnapshot(ctx context.Context, windowID string) ([]byte, error) {
	tmp, err := os.CreateTemp("", "nina-snap-*.jpg")
	if err != nil {
		return nil, fmt.Errorf("create temp screenshot: %w", err)
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer func() {
		_ = os.Remove(tmpPath)
	}()

	args := []string{"-x"}
	windowID = strings.TrimSpace(windowID)
	targetWindow := windowID != "" && windowID != "0"
	if targetWindow {
		args = append(args, "-l", windowID)
	}
	args = append(args, tmpPath)

	cmd := exec.CommandContext(ctx, "screencapture", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		if targetWindow {
			// Some apps/windows don't expose capturable IDs; fallback to fullscreen.
			cmd = exec.CommandContext(ctx, "screencapture", "-x", tmpPath)
			if out2, err2 := cmd.CombinedOutput(); err2 == nil {
				goto readSnapshot
			} else {
				return nil, fmt.Errorf("screencapture failed (window + fullscreen fallback): %w (%s)", err2, strings.TrimSpace(string(out2)))
			}
		}
		return nil, fmt.Errorf("screencapture failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}

readSnapshot:
	b, err := os.ReadFile(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("read screenshot: %w", err)
	}
	if len(b) == 0 {
		return nil, errors.New("screenshot was empty")
	}
	return b, nil
}

func shouldSkipCaptureForPrivacy(appName string) bool {
	app := strings.ToLower(strings.TrimSpace(appName))
	for _, blocked := range privacyDenylistApps {
		if strings.Contains(app, blocked) {
			return true
		}
	}
	return false
}

func containsAnyFold(s string, keywords []string) bool {
	ls := strings.ToLower(s)
	for _, kw := range keywords {
		if strings.Contains(ls, kw) {
			return true
		}
	}
	return false
}

func isShamefulContext(info activeWindowInfo) bool {
	joined := strings.ToLower(info.AppName + " " + info.Title)
	return containsAnyFold(joined, shameKeywords)
}

func moodForActivityTag(tag string) string {
	switch normalizeActivityTag(tag) {
	case "mode_focus":
		return "focused"
	case "mode_chill":
		return "chill"
	case "mode_game":
		return "playful"
	case "mode_music":
		return "calm"
	case "mode_shame":
		return "disappointed"
	default:
		return "neutral"
	}
}

func likelyYouTubeMusic(joined string) bool {
	return strings.Contains(joined, "youtube") && containsAnyFold(joined, youtubeMusicKeywords)
}

func classifyLocalActivity(info activeWindowInfo) (tag, mood string, confidence float64) {
	appLower := strings.ToLower(strings.TrimSpace(info.AppName))
	titleLower := strings.ToLower(strings.TrimSpace(info.Title))
	joined := strings.TrimSpace(appLower + " " + titleLower)

	if joined == "" {
		return "mode_unknown", "neutral", 0.55
	}

	scores := map[string]float64{
		"mode_focus": 0,
		"mode_chill": 0,
		"mode_game":  0,
		"mode_music": 0,
		"mode_shame": 0,
	}
	add := func(activityTag string, delta float64) {
		scores[activityTag] += delta
	}

	if containsAnyFold(joined, shameKeywords) {
		add("mode_shame", 4.8)
	}
	if containsAnyFold(joined, procrastinationKeywords) {
		add("mode_shame", 1.3)
		add("mode_chill", 0.5)
	}
	if containsAnyFold(joined, gameKeywords) {
		add("mode_game", 3.0)
	}
	if containsAnyFold(joined, musicKeywords) {
		add("mode_music", 2.5)
	}
	if containsAnyFold(joined, chillKeywords) {
		add("mode_chill", 1.8)
	}
	if containsAnyFold(joined, focusKeywords) {
		add("mode_focus", 2.3)
	}
	if containsAnyFold(appLower, focusAppKeywords) {
		add("mode_focus", 2.6)
	}
	if containsAnyFold(appLower, devAppKeywords) {
		add("mode_focus", 2.8)
	}
	if strings.Contains(joined, "leetcode") || strings.Contains(joined, "readme") || strings.Contains(joined, "docs") {
		add("mode_focus", 0.8)
	}

	if containsAnyFold(appLower, browserKeywords) {
		if titleLower == "" {
			// Browser open but no tab title visible — local classifier is blind.
			// Return unknown immediately and let Gemini read the screenshots.
			return "mode_unknown", "neutral", 0.55
		}
		add("mode_chill", 1.0)
		if strings.Contains(joined, "youtube") {
			add("mode_chill", 1.0)
			if likelyYouTubeMusic(joined) {
				add("mode_music", 2.4)
				add("mode_chill", -0.4)
			}
		}
		// Only bias toward chill if we can actually see a clearly leisure title.
		if containsAnyFold(titleLower, chillKeywords) {
			add("mode_chill", 0.8)
		}
	}

	topTag := "mode_unknown"
	topScore := 0.0
	secondScore := 0.0
	for k, v := range scores {
		if v > topScore {
			secondScore = topScore
			topScore = v
			topTag = k
			continue
		}
		if v > secondScore {
			secondScore = v
		}
	}

	// When explicit shame markers appear, keep the guard sharp even with mixed context.
	if scores["mode_shame"] >= 3.2 && scores["mode_shame"] >= topScore-0.35 {
		topTag = "mode_shame"
		topScore = scores["mode_shame"]
	}

	// Prefer music over chill when signals are close.
	if topTag == "mode_chill" && scores["mode_music"] > 0 && scores["mode_music"] >= topScore-0.25 {
		topTag = "mode_music"
		topScore = scores["mode_music"]
	}

	if topScore < 1.2 {
		return "mode_unknown", "neutral", 0.55
	}

	margin := math.Max(0, topScore-secondScore)
	conf := 0.58 + math.Min(0.28, topScore*0.06) + math.Min(0.11, margin*0.08)
	if conf > 0.97 {
		conf = 0.97
	}
	return topTag, moodForActivityTag(topTag), conf
}

func normalizeActivityTag(tag string) string {
	switch strings.TrimSpace(strings.ToLower(tag)) {
	case "mode_focus", "focus":
		return "mode_focus"
	case "mode_chill", "chill":
		return "mode_chill"
	case "mode_game", "game", "gaming":
		return "mode_game"
	case "mode_music", "music":
		return "mode_music"
	case "mode_shame", "shame":
		return "mode_shame"
	default:
		return "mode_unknown"
	}
}

type diaryEntry struct {
	ID          int64
	Timestamp   time.Time
	AppName     string
	WindowTitle string
	WindowID    string
	ActivityTag string
	VisionDesc  string
	NinaThought string
	Mood        string
	Confidence  float64
}

type summaryEntry struct {
	ID         int64
	Type       string
	RangeStart time.Time
	RangeEnd   time.Time
	Content    string
	CreatedAt  time.Time
}

type memoryStore struct {
	db *sql.DB
}

func openMemoryStore(path string) (*memoryStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}
	if _, err := db.Exec(`PRAGMA synchronous=NORMAL;`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set sync mode: %w", err)
	}
	schema := []string{
		`CREATE TABLE IF NOT EXISTS entries (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp TEXT NOT NULL,
			app_name TEXT,
			window_title TEXT,
			window_id TEXT,
			activity_tag TEXT NOT NULL,
			vision_description TEXT NOT NULL DEFAULT '',
			nina_thought TEXT NOT NULL,
			mood TEXT NOT NULL,
			confidence REAL NOT NULL DEFAULT 0
		);`,
		`CREATE INDEX IF NOT EXISTS idx_entries_timestamp ON entries(timestamp);`,
		`CREATE INDEX IF NOT EXISTS idx_entries_activity_tag ON entries(activity_tag);`,
		`CREATE TABLE IF NOT EXISTS summaries (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			type TEXT NOT NULL,
			range_start TEXT NOT NULL,
			range_end TEXT NOT NULL,
			content TEXT NOT NULL,
			created_at TEXT NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_summaries_type_created ON summaries(type, created_at);`,
		`CREATE TABLE IF NOT EXISTS embeddings (
			entry_id INTEGER PRIMARY KEY,
			vector_json TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			FOREIGN KEY(entry_id) REFERENCES entries(id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS memory_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);`,
	}
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("init schema: %w", err)
		}
	}
	if err := ensureEntriesVisionDescriptionColumn(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("entries schema upgrade: %w", err)
	}
	return &memoryStore{db: db}, nil
}

func ensureEntriesVisionDescriptionColumn(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(entries);`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var exists bool
	for rows.Next() {
		var cid int
		var name string
		var colType string
		var notNull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			return err
		}
		if strings.EqualFold(strings.TrimSpace(name), "vision_description") {
			exists = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err = db.Exec(`ALTER TABLE entries ADD COLUMN vision_description TEXT NOT NULL DEFAULT '';`)
	return err
}

func (m *memoryStore) Close() error {
	return m.db.Close()
}

func (m *memoryStore) insertEntry(e diaryEntry) (int64, error) {
	res, err := m.db.Exec(
		`INSERT INTO entries(timestamp, app_name, window_title, window_id, activity_tag, vision_description, nina_thought, mood, confidence)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Timestamp.Format(time.RFC3339Nano),
		e.AppName,
		e.WindowTitle,
		e.WindowID,
		normalizeActivityTag(e.ActivityTag),
		e.VisionDesc,
		e.NinaThought,
		e.Mood,
		e.Confidence,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (m *memoryStore) upsertEmbedding(entryID int64, vector []float64) error {
	b, err := json.Marshal(vector)
	if err != nil {
		return err
	}
	_, err = m.db.Exec(
		`INSERT INTO embeddings(entry_id, vector_json, updated_at)
		 VALUES(?, ?, ?)
		 ON CONFLICT(entry_id) DO UPDATE SET vector_json=excluded.vector_json, updated_at=excluded.updated_at`,
		entryID,
		string(b),
		time.Now().Format(time.RFC3339Nano),
	)
	return err
}

func (m *memoryStore) recentEntries(limit int) ([]diaryEntry, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := m.db.Query(
		`SELECT id, timestamp, app_name, window_title, window_id, activity_tag, vision_description, nina_thought, mood, confidence
		 FROM entries ORDER BY id DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]diaryEntry, 0, limit)
	for rows.Next() {
		var e diaryEntry
		var ts string
		if err := rows.Scan(&e.ID, &ts, &e.AppName, &e.WindowTitle, &e.WindowID, &e.ActivityTag, &e.VisionDesc, &e.NinaThought, &e.Mood, &e.Confidence); err != nil {
			return nil, err
		}
		t, _ := time.Parse(time.RFC3339Nano, ts)
		e.Timestamp = t
		out = append(out, e)
	}
	return out, rows.Err()
}

func (m *memoryStore) recentSummaries(limit int) ([]summaryEntry, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := m.db.Query(
		`SELECT id, type, range_start, range_end, content, created_at
		 FROM summaries ORDER BY id DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]summaryEntry, 0, limit)
	for rows.Next() {
		var s summaryEntry
		var startTS, endTS, createdTS string
		if err := rows.Scan(&s.ID, &s.Type, &startTS, &endTS, &s.Content, &createdTS); err != nil {
			return nil, err
		}
		s.RangeStart, _ = time.Parse(time.RFC3339Nano, startTS)
		s.RangeEnd, _ = time.Parse(time.RFC3339Nano, endTS)
		s.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdTS)
		out = append(out, s)
	}
	return out, rows.Err()
}

func (m *memoryStore) entriesSince(since time.Time) ([]diaryEntry, error) {
	rows, err := m.db.Query(
		`SELECT id, timestamp, app_name, window_title, window_id, activity_tag, vision_description, nina_thought, mood, confidence
		 FROM entries WHERE timestamp >= ? ORDER BY id ASC`,
		since.Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []diaryEntry
	for rows.Next() {
		var e diaryEntry
		var ts string
		if err := rows.Scan(&e.ID, &ts, &e.AppName, &e.WindowTitle, &e.WindowID, &e.ActivityTag, &e.VisionDesc, &e.NinaThought, &e.Mood, &e.Confidence); err != nil {
			return nil, err
		}
		t, _ := time.Parse(time.RFC3339Nano, ts)
		e.Timestamp = t
		out = append(out, e)
	}
	return out, rows.Err()
}

func (m *memoryStore) insertSummary(s summaryEntry) error {
	_, err := m.db.Exec(
		`INSERT INTO summaries(type, range_start, range_end, content, created_at)
		 VALUES(?, ?, ?, ?, ?)`,
		s.Type,
		s.RangeStart.Format(time.RFC3339Nano),
		s.RangeEnd.Format(time.RFC3339Nano),
		s.Content,
		s.CreatedAt.Format(time.RFC3339Nano),
	)
	return err
}

func (m *memoryStore) lastSummaryCreatedAt(summaryType string) (time.Time, error) {
	row := m.db.QueryRow(`SELECT created_at FROM summaries WHERE type = ? ORDER BY id DESC LIMIT 1`, summaryType)
	var ts string
	if err := row.Scan(&ts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	t, _ := time.Parse(time.RFC3339Nano, ts)
	return t, nil
}

func (m *memoryStore) ensureEmbeddingBackend(backend string) (int64, error) {
	backend = strings.TrimSpace(backend)
	if backend == "" {
		return 0, errors.New("embedding backend name cannot be empty")
	}
	var current string
	row := m.db.QueryRow(`SELECT value FROM memory_meta WHERE key = 'embedding_backend' LIMIT 1`)
	if err := row.Scan(&current); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	current = strings.TrimSpace(current)
	if current == backend {
		return 0, nil
	}

	var existingCount int64
	if err := m.db.QueryRow(`SELECT COUNT(*) FROM embeddings`).Scan(&existingCount); err != nil {
		return 0, err
	}
	if existingCount > 0 {
		if _, err := m.db.Exec(`DELETE FROM embeddings`); err != nil {
			return 0, err
		}
	}

	_, err := m.db.Exec(
		`INSERT INTO memory_meta(key, value, updated_at)
		 VALUES('embedding_backend', ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		backend,
		time.Now().Format(time.RFC3339Nano),
	)
	if err != nil {
		return 0, err
	}
	return existingCount, nil
}

type semanticCandidate struct {
	entry diaryEntry
	score float64
}

func (m *memoryStore) semanticSearch(queryVec []float64, topK int) ([]diaryEntry, error) {
	if topK <= 0 || len(queryVec) == 0 {
		return nil, nil
	}
	rows, err := m.db.Query(
		`SELECT e.id, e.timestamp, e.app_name, e.window_title, e.window_id, e.activity_tag, e.vision_description, e.nina_thought, e.mood, e.confidence, em.vector_json
		 FROM embeddings em
		 JOIN entries e ON e.id = em.entry_id
		 ORDER BY e.id DESC LIMIT ?`,
		maxSemanticCandidates,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cands := make([]semanticCandidate, 0, topK)
	for rows.Next() {
		var e diaryEntry
		var ts string
		var vecJSON string
		if err := rows.Scan(&e.ID, &ts, &e.AppName, &e.WindowTitle, &e.WindowID, &e.ActivityTag, &e.VisionDesc, &e.NinaThought, &e.Mood, &e.Confidence, &vecJSON); err != nil {
			return nil, err
		}
		e.Timestamp, _ = time.Parse(time.RFC3339Nano, ts)
		var vec []float64
		if err := json.Unmarshal([]byte(vecJSON), &vec); err != nil {
			continue
		}
		score := cosineSimilarity(queryVec, vec)
		if math.IsNaN(score) || score <= 0 {
			continue
		}
		cands = append(cands, semanticCandidate{entry: e, score: score})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	if len(cands) > topK {
		cands = cands[:topK]
	}
	out := make([]diaryEntry, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.entry)
	}
	return out, nil
}

func cosineSimilarity(a, b []float64) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	dot := 0.0
	na := 0.0
	nb := 0.0
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

type ninaSoulEngine struct {
	store          *memoryStore
	overlay        io.Writer
	apiKey         string
	model          string
	embeddingModel string
	persona        string
	httpClient     *http.Client

	mu              sync.Mutex
	lastAPIWindowID string
	lastAPICall     time.Time
	shameUntil      time.Time
	lastTag         string
	lastThought     string
	lastContextPush time.Time
	lastGeminiSkip  time.Time
	leisureKey      string
	leisureSince    time.Time
	focusSince      time.Time

	// Local instinct state
	distractionWindowID  string
	distractionStartTime time.Time
}

type visionSnapshot struct {
	Data []byte
	Info activeWindowInfo
	At   time.Time
}

type ninaVisionOutput struct {
	ActivityTag string  `json:"activity_tag"`
	SceneDesc   string  `json:"scene_description,omitempty"`
	NinaThought string  `json:"nina_thought"`
	NinaMood    string  `json:"nina_mood"`
	Confidence  float64 `json:"confidence,omitempty"`
}

func startNinaSoul(ctx context.Context, overlay io.Writer) error {
	store, err := openMemoryStore(memoryDBPath)
	if err != nil {
		return err
	}
	model := resolveGeminiModel(geminiModel)
	embedModel := resolveGeminiEmbeddingModel(geminiEmbedModel)
	persona := loadNinaPersonaInstructions()
	engine := &ninaSoulEngine{
		store:          store,
		overlay:        overlay,
		apiKey:         strings.TrimSpace(os.Getenv("GOOGLE_API_KEY")),
		model:          model,
		embeddingModel: embedModel,
		persona:        persona,
		httpClient:     &http.Client{Timeout: 45 * time.Second},
	}
	purged, err := store.ensureEmbeddingBackend("gemini-api-embedding-v1")
	if err != nil {
		return fmt.Errorf("embedding backend migration failed: %w", err)
	}
	if purged > 0 {
		fmt.Printf("[NINA SOUL] Cleared %d legacy embedding rows for new Gemini embedding backend.\n", purged)
	}
	fmt.Printf("[NINA SOUL] Gemini model: %s\n", engine.model)
	fmt.Printf("[NINA SOUL] Gemini embedding model: %s\n", engine.embeddingModel)
	if strings.TrimSpace(engine.apiKey) == "" {
		fmt.Printf("[NINA SOUL] WARNING: GOOGLE_API_KEY missing; dynamic Gemini thoughts are disabled and fallback thoughts will be used.\n")
	}
	if strings.TrimSpace(engine.persona) != "" {
		fmt.Printf("[NINA SOUL] Custom persona loaded (%d chars).\n", len(engine.persona))
	}
	go func() {
		<-ctx.Done()
		_ = store.Close()
	}()
	go engine.guardLoop(ctx)
	go engine.visionLoop(ctx)
	go engine.reflectionLoop(ctx)
	return nil
}

func resolveGeminiModel(flagValue string) string {
	if s := strings.TrimSpace(flagValue); s != "" {
		return s
	}
	if s := strings.TrimSpace(os.Getenv("NINA_GEMINI_MODEL")); s != "" {
		return s
	}
	return defaultGeminiModel
}

func resolveGeminiEmbeddingModel(flagValue string) string {
	if s := strings.TrimSpace(flagValue); s != "" {
		return s
	}
	if s := strings.TrimSpace(os.Getenv("NINA_GEMINI_EMBED_MODEL")); s != "" {
		return s
	}
	return defaultGeminiEmbeddingModel
}

func loadNinaPersonaInstructions() string {
	if s := strings.TrimSpace(os.Getenv("NINA_PERSONA_TEXT")); s != "" {
		return clampPersonaText(s)
	}
	path := strings.TrimSpace(os.Getenv("NINA_PERSONA_FILE"))
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nina: failed to read NINA_PERSONA_FILE (%s): %v\n", path, err)
		return ""
	}
	return clampPersonaText(string(b))
}

func clampPersonaText(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if len(s) > 4000 {
		s = strings.TrimSpace(s[:4000])
	}
	return s
}

func (e *ninaSoulEngine) guardLoop(ctx context.Context) {
	ticker := time.NewTicker(guardPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		info, err := getActiveWindowInfo(ctx)
		if err != nil {
			continue
		}
		if !isShamefulContext(info) {
			continue
		}
		e.mu.Lock()
		e.shameUntil = time.Now().Add(shameHoldDuration)
		e.mu.Unlock()
		e.pushContext("mode_shame", "disappointed", "Nope. This isn't helping us grow. Let's switch to something future-you will be proud of.", true)
	}
}

func sleepUntil(ctx context.Context, t time.Time) error {
	d := time.Until(t)
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (e *ninaSoulEngine) visionLoop(ctx context.Context) {
	nextCycle := time.Now()
	for {
		if err := e.runVisionCycle(ctx, nextCycle); err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintf(os.Stderr, "nina: vision cycle error: %v\n", err)
		}
		nextCycle = nextCycle.Add(visionCycleInterval)
		if time.Until(nextCycle) < 0 {
			nextCycle = time.Now()
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func (e *ninaSoulEngine) runVisionCycle(ctx context.Context, cycleStart time.Time) error {
	fmt.Printf("\n[NINA SOUL] >>> Starting Vision Cycle (%s)\n", cycleStart.Format("15:04:05"))
	offsets := []time.Duration{1 * time.Minute, 3 * time.Minute, 5 * time.Minute}
	snapshots := make([]visionSnapshot, 0, len(offsets))
	var lastInfo activeWindowInfo

	for i, offset := range offsets {
		fmt.Printf("[NINA SOUL] Waiting for snapshot #%d of 3 (%v offset)...\n", i+1, offset)
		if err := sleepUntil(ctx, cycleStart.Add(offset)); err != nil {
			return err
		}
		info, err := getActiveWindowInfo(ctx)
		if err != nil {
			fmt.Printf("[NINA SOUL] X Snapshot #%d Failed: Could not get active window info: %v\n", i+1, err)
			continue
		}
		lastInfo = info
		if shouldSkipCaptureForPrivacy(info.AppName) {
			fmt.Printf("[NINA SOUL] X Snapshot #%d Skipped: Privacy filter for \"%s\".\n", i+1, info.AppName)
			continue
		}

		if strings.TrimSpace(info.WindowID) != "" && info.WindowID != "0" {
			fmt.Printf("[NINA SOUL] Taking Active Window Snapshot #%d of 3 (window_id=%s)...\n", i+1, info.WindowID)
		} else {
			fmt.Printf("[NINA SOUL] Taking Fullscreen Snapshot #%d of 3 (no window id)...\n", i+1)
		}
		data, err := captureActiveWindowSnapshot(ctx, info.WindowID)
		if err != nil {
			fmt.Printf("[NINA SOUL] X Snapshot #%d Failed: Screen capture error: %v\n", i+1, err)
			continue
		}
		fmt.Printf("[NINA SOUL] + Captured snapshot #%d successfully.\n", i+1)
		snapshots = append(snapshots, visionSnapshot{Data: data, Info: info, At: time.Now()})
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if strings.TrimSpace(lastInfo.AppName) == "" {
		if info, err := getActiveWindowInfo(ctx); err == nil {
			lastInfo = info
		}
	}
	fmt.Printf("[NINA SOUL] Cycle complete. Captured %d snapshots. Processing result...\n", len(snapshots))
	err := e.processVisionResult(ctx, lastInfo, snapshots)
	if err != nil {
		fmt.Printf("[NINA SOUL] Error processing vision: %v\n", err)
	}
	fmt.Printf("[NINA SOUL] <<< Vision Cycle Ended.\n\n")
	return err
}

func (e *ninaSoulEngine) processVisionResult(ctx context.Context, info activeWindowInfo, snapshots []visionSnapshot) error {
	fmt.Printf("[NINA SOUL] Current Window: %s (\"%s\")\n", info.AppName, info.Title)
	localTag, localMood, localConf := classifyLocalActivity(info)
	leisureDuration := e.trackLeisureDuration(localTag, info)
	focusDuration := e.trackFocusDuration(localTag)
	if localTag != "" {
		fmt.Printf("[NINA SOUL] Local Instinct: %s (Mood: %s, Conf: %.2f)\n", localTag, localMood, localConf)
	}
	if localTag == "mode_shame" {
		e.mu.Lock()
		if e.distractionWindowID != info.WindowID {
			e.distractionWindowID = info.WindowID
			e.distractionStartTime = time.Now()
		}
		distractionDuration := time.Since(e.distractionStartTime)
		e.mu.Unlock()

		if distractionDuration > 10*time.Minute {
			// CAT INSTINCT: Trigger a proactive judgmental nudge
			fmt.Printf("[NINA SOUL] FRICTION: User has been distracted for %v. Pouncing.\n", distractionDuration)
			e.pushContext("mode_shame", "disappointed", "ffs ur deadass still scrolling... we're cooked if we don't fix this", true)
		}

		fmt.Printf("[NINA SOUL] TRIGGER: Shame Mode activated.\n")
		e.mu.Lock()
		e.shameUntil = time.Now().Add(shameHoldDuration)
		e.mu.Unlock()
	} else {
		e.mu.Lock()
		e.distractionWindowID = ""
		e.mu.Unlock()
	}

	hot, err := e.store.recentEntries(10)
	if err != nil {
		return err
	}
	preferenceEntries, err := e.store.recentEntries(180)
	if err != nil {
		preferenceEntries = hot
	}
	breakCue := inferPreferredBreakCue(preferenceEntries)
	previousTag := ""
	if len(hot) > 0 {
		previousTag = normalizeActivityTag(hot[0].ActivityTag)
	}
	warm, err := e.store.recentSummaries(7)
	if err != nil {
		return err
	}
	var cold []diaryEntry
	if strings.TrimSpace(e.apiKey) != "" {
		queryEmbedding, err := e.callGeminiEmbedding(ctx, buildMemoryQueryText(info), "RETRIEVAL_QUERY")
		if err != nil {
			fmt.Printf("[NINA SOUL] Embedding query skipped: %v\n", err)
		} else {
			cold, err = e.store.semanticSearch(queryEmbedding, 3)
			if err != nil {
				return err
			}
		}
	}

	modelOut := ninaVisionOutput{
		ActivityTag: localTag,
		NinaMood:    localMood,
		Confidence:  localConf,
		SceneDesc:   fallbackVisionDescription(info, snapshots),
		NinaThought: fallbackThought(localTag, info),
	}

	if e.shouldCallGemini(info, snapshots) {
		fmt.Printf("[NINA SOUL] Calling Gemini for deep context analysis...\n")
		prompt := buildVisionPrompt(info, hot, warm, cold, localTag, e.persona, leisureDuration, focusDuration, breakCue, e.lastThought)
		out, err := e.callGeminiVision(ctx, prompt, snapshots)
		if err != nil {
			fmt.Printf("[NINA SOUL] Gemini API Failure: %v\n", err)
		} else {
			fmt.Printf("[NINA SOUL] Gemini Response: TAG=%s MOOD=%s THOUGHT=\"%s\"\n", out.ActivityTag, out.NinaMood, out.NinaThought)
			if strings.TrimSpace(out.ActivityTag) != "" {
				modelOut.ActivityTag = normalizeActivityTag(out.ActivityTag)
			}
			if strings.TrimSpace(out.NinaMood) != "" {
				modelOut.NinaMood = strings.TrimSpace(out.NinaMood)
			}
			if strings.TrimSpace(out.SceneDesc) != "" {
				modelOut.SceneDesc = normalizeVisionDescription(out.SceneDesc)
			}
			if strings.TrimSpace(out.NinaThought) != "" {
				modelOut.NinaThought = strings.TrimSpace(out.NinaThought)
			}
			if out.Confidence > 0 {
				modelOut.Confidence = out.Confidence
			}
			e.mu.Lock()
			e.lastAPICall = time.Now()
			e.lastAPIWindowID = info.WindowID
			e.mu.Unlock()
		}
	}

	e.mu.Lock()
	shameActive := time.Now().Before(e.shameUntil)
	e.mu.Unlock()
	if shameActive {
		modelOut.ActivityTag = "mode_shame"
		if strings.TrimSpace(modelOut.NinaMood) == "" || strings.EqualFold(modelOut.NinaMood, "neutral") {
			modelOut.NinaMood = "disappointed"
		}
		if strings.TrimSpace(modelOut.NinaThought) == "" {
			modelOut.NinaThought = "we are better than doomscroll loops close this and open the task you owe yourself"
		}
	}

	modelOut.ActivityTag = normalizeActivityTag(modelOut.ActivityTag)
	if previousTag == "mode_shame" && modelOut.ActivityTag == "mode_focus" {
		fmt.Printf("[NINA SOUL] RECOVERY: shame -> focus rebound detected.\n")
		modelOut.NinaMood = "proud"
		modelOut.NinaThought = recoveryThought(info)
	}
	modelOut.NinaThought, modelOut.NinaMood = harmonizeLeisureTone(modelOut.ActivityTag, modelOut.NinaThought, modelOut.NinaMood, leisureDuration, info)
	modelOut.NinaThought, modelOut.NinaMood = harmonizeFocusTone(modelOut.ActivityTag, modelOut.NinaThought, modelOut.NinaMood, focusDuration, breakCue)
	if strings.TrimSpace(modelOut.NinaThought) == "" {
		modelOut.NinaThought = fallbackThought(modelOut.ActivityTag, info)
	}
	if strings.TrimSpace(modelOut.NinaMood) == "" {
		modelOut.NinaMood = localMood
	}
	modelOut.SceneDesc = normalizeVisionDescription(modelOut.SceneDesc)
	if strings.TrimSpace(modelOut.SceneDesc) == "" {
		modelOut.SceneDesc = fallbackVisionDescription(info, snapshots)
	}
	modelOut.NinaThought = normalizeNinaThoughtWhitespace(modelOut.NinaThought)
	if issues := validateNinaThoughtStyle(modelOut.NinaThought); len(issues) > 0 {
		fmt.Printf("[NINA SOUL] Style guard fallback: %s\n", strings.Join(issues, " | "))
		modelOut.NinaThought = fallbackThought(modelOut.ActivityTag, info)
		modelOut.NinaThought = normalizeNinaThoughtWhitespace(modelOut.NinaThought)
	}

	e.pushContext(modelOut.ActivityTag, modelOut.NinaMood, modelOut.NinaThought, false)

	entryID, err := e.store.insertEntry(diaryEntry{
		Timestamp:   time.Now(),
		AppName:     info.AppName,
		WindowTitle: info.Title,
		WindowID:    info.WindowID,
		ActivityTag: modelOut.ActivityTag,
		VisionDesc:  modelOut.SceneDesc,
		NinaThought: modelOut.NinaThought,
		Mood:        modelOut.NinaMood,
		Confidence:  modelOut.Confidence,
	})
	if err != nil {
		return err
	}
	docEmbedding, err := e.callGeminiEmbedding(ctx, buildMemoryDocumentText(info, modelOut), "RETRIEVAL_DOCUMENT")
	if err != nil {
		fmt.Printf("[NINA SOUL] Embedding write skipped: %v\n", err)
		return nil
	}
	if err := e.store.upsertEmbedding(entryID, docEmbedding); err != nil {
		return err
	}
	return nil
}

func (e *ninaSoulEngine) shouldCallGemini(info activeWindowInfo, snapshots []visionSnapshot) bool {
	if len(snapshots) == 0 {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if strings.TrimSpace(e.apiKey) == "" {
		return false
	}
	windowChanged := strings.TrimSpace(info.WindowID) != "" && info.WindowID != e.lastAPIWindowID
	stale := time.Since(e.lastAPICall) >= geminiMinimumCallSpacing
	if e.lastAPICall.IsZero() {
		stale = true
	}
	should := windowChanged || stale
	if !should && time.Since(e.lastGeminiSkip) >= 2*time.Minute {
		remaining := geminiMinimumCallSpacing - time.Since(e.lastAPICall)
		if remaining < 0 {
			remaining = 0
		}
		fmt.Printf("[NINA SOUL] Gemini cooldown active (%s remaining); using contextual local fallback for now.\n", remaining.Round(time.Second))
		e.lastGeminiSkip = time.Now()
	}
	return should
}

func (e *ninaSoulEngine) pushContext(tag, mood, thought string, force bool) {
	tag = normalizeActivityTag(tag)
	thought = strings.TrimSpace(thought)
	mood = strings.TrimSpace(mood)

	e.mu.Lock()
	shouldPush := force || tag != e.lastTag || thought != e.lastThought || time.Since(e.lastContextPush) > 90*time.Second
	if shouldPush {
		e.lastTag = tag
		e.lastThought = thought
		e.lastContextPush = time.Now()
	}
	e.mu.Unlock()
	if shouldPush {
		sendOverlayContext(e.overlay, tag, mood, thought)
	}
}

func isLeisureTag(tag string) bool {
	switch normalizeActivityTag(tag) {
	case "mode_chill", "mode_music":
		return true
	default:
		return false
	}
}

func leisureContextKey(info activeWindowInfo) string {
	app := strings.ToLower(strings.TrimSpace(info.AppName))
	title := compactThoughtContext(info.Title, 6)
	if app == "" && title == "" {
		return ""
	}
	if title == "" {
		return app
	}
	return app + "|" + title
}

func (e *ninaSoulEngine) trackLeisureDuration(tag string, info activeWindowInfo) time.Duration {
	if !isLeisureTag(tag) {
		e.mu.Lock()
		e.leisureKey = ""
		e.leisureSince = time.Time{}
		e.mu.Unlock()
		return 0
	}
	key := leisureContextKey(info)
	now := time.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	if key == "" {
		key = normalizeActivityTag(tag)
	}
	if e.leisureKey != key || e.leisureSince.IsZero() {
		e.leisureKey = key
		e.leisureSince = now
		return 0
	}
	return now.Sub(e.leisureSince)
}

func (e *ninaSoulEngine) trackFocusDuration(tag string) time.Duration {
	if normalizeActivityTag(tag) != "mode_focus" {
		e.mu.Lock()
		e.focusSince = time.Time{}
		e.mu.Unlock()
		return 0
	}
	now := time.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.focusSince.IsZero() {
		e.focusSince = now
		return 0
	}
	return now.Sub(e.focusSince)
}

func isBreakPreferenceTag(tag string) bool {
	switch normalizeActivityTag(tag) {
	case "mode_chill", "mode_music", "mode_game":
		return true
	default:
		return false
	}
}

func breakCueFromEntry(e diaryEntry) string {
	if !isBreakPreferenceTag(e.ActivityTag) {
		return ""
	}
	if cue := compactThoughtContext(e.WindowTitle, 4); cue != "" {
		return cue
	}
	return compactThoughtContext(e.AppName, 2)
}

func inferPreferredBreakCue(entries []diaryEntry) string {
	if len(entries) == 0 {
		return ""
	}
	now := time.Now()
	scores := map[string]float64{}
	counts := map[string]int{}
	for _, e := range entries {
		cue := breakCueFromEntry(e)
		if cue == "" {
			continue
		}
		ageHours := now.Sub(e.Timestamp).Hours()
		if ageHours < 0 {
			ageHours = 0
		}
		recencyWeight := 1.0 / (1.0 + ageHours/24.0)
		tagWeight := 1.0
		switch normalizeActivityTag(e.ActivityTag) {
		case "mode_music":
			tagWeight = 1.15
		case "mode_game":
			tagWeight = 0.95
		}
		scores[cue] += recencyWeight * tagWeight
		counts[cue]++
	}

	bestCue := ""
	bestScore := 0.0
	bestCount := 0
	for cue, score := range scores {
		count := counts[cue]
		if count < 2 && score < 2.2 {
			continue
		}
		if score > bestScore || (math.Abs(score-bestScore) < 1e-9 && count > bestCount) || (math.Abs(score-bestScore) < 1e-9 && count == bestCount && cue < bestCue) {
			bestCue = cue
			bestScore = score
			bestCount = count
		}
	}
	return strings.TrimSpace(bestCue)
}

func containsRefocusNudge(thought string) bool {
	s := strings.ToLower(strings.TrimSpace(thought))
	if s == "" {
		return false
	}
	needles := []string{
		"lock back in", "lock in", "back to priorities", "pivot", "close this",
		"whole side quest", "pick one task", "future you", "promised", "wrap this break",
	}
	return containsAnyFold(s, needles)
}

func containsBreakSuggestion(thought string) bool {
	s := strings.ToLower(strings.TrimSpace(thought))
	if s == "" {
		return false
	}
	needles := []string{
		"take a break", "quick reset", "take ten", "take 10",
		"step away", "grab water", "stretch", "short walk",
	}
	return containsAnyFold(s, needles)
}

func leisureCompanionThought(tag string, info activeWindowInfo, duration time.Duration) string {
	cue := compactThoughtContext(info.Title, 3)
	if cue == "" {
		cue = compactThoughtContext(info.AppName, 2)
	}
	isLong := duration >= leisureNudgeDuration
	switch normalizeActivityTag(tag) {
	case "mode_music":
		if isLong {
			return "vibes are clean but this reset got long lets pick one move after this"
		}
		if cue != "" {
			return fmt.Sprintf("%s is a vibe enjoy this reset im here vibing with u", cue)
		}
		return "this track is fire enjoy the reset im right here vibing with u"
	default:
		if isLong {
			return "youve been chilling a while now lets wrap this break and pick one task"
		}
		if cue != "" {
			return fmt.Sprintf("ur on %s rn this break is valid enjoy it for a bit", cue)
		}
		return "this chill break is valid enjoy it im here vibing with u"
	}
}

func harmonizeLeisureTone(tag, thought, mood string, duration time.Duration, info activeWindowInfo) (string, string) {
	if !isLeisureTag(tag) {
		return thought, mood
	}
	t := strings.TrimSpace(thought)
	m := strings.TrimSpace(mood)

	if duration < leisureGraceDuration {
		if t == "" || containsRefocusNudge(t) {
			t = leisureCompanionThought(tag, info, duration)
		}
		if m == "" || strings.EqualFold(m, "disappointed") {
			m = "chill"
		}
		return t, m
	}

	if duration >= leisureNudgeDuration {
		if t == "" || !containsRefocusNudge(t) {
			t = leisureCompanionThought(tag, info, duration)
		}
		if m == "" {
			m = "watchful"
		}
		return t, m
	}

	// Mid-range: keep it relaxed unless model gave nothing.
	if t == "" {
		t = leisureCompanionThought(tag, info, duration)
	}
	if m == "" {
		m = "chill"
	}
	return t, m
}

func focusBreakThought(duration time.Duration, breakCue string) string {
	mins := int(duration.Minutes())
	if mins < int(focusBreakSuggestDuration.Minutes()) {
		return ""
	}
	cue := compactThoughtContext(breakCue, 4)
	if cue != "" {
		if duration >= focusBreakStrongDuration {
			return fmt.Sprintf("u been locked in %dm take 10 then hit %s and come back refreshed", mins, cue)
		}
		return fmt.Sprintf("youve been grinding %dm maybe take a quick %s reset then keep cooking", mins, cue)
	}
	if duration >= focusBreakStrongDuration {
		return fmt.Sprintf("u been locked in %dm take ten and reset then we keep cooking", mins)
	}
	return fmt.Sprintf("youve been grinding %dm maybe take a quick reset then keep cooking", mins)
}

func harmonizeFocusTone(tag, thought, mood string, duration time.Duration, breakCue string) (string, string) {
	if normalizeActivityTag(tag) != "mode_focus" {
		return thought, mood
	}
	if duration < focusBreakSuggestDuration {
		return thought, mood
	}
	suggestion := focusBreakThought(duration, breakCue)
	if suggestion == "" {
		return thought, mood
	}

	t := strings.TrimSpace(thought)
	m := strings.TrimSpace(mood)

	if duration >= focusBreakStrongDuration {
		if t == "" || !containsBreakSuggestion(t) {
			t = suggestion
		}
		if m == "" {
			m = "caring"
		}
		return t, m
	}

	if t == "" || (duration >= focusBreakSuggestDuration+20*time.Minute && !containsBreakSuggestion(t)) {
		t = suggestion
	}
	if m == "" {
		m = "focused"
	}
	return t, m
}

func fallbackThought(tag string, info activeWindowInfo) string {
	screenCue := compactThoughtContext(info.Title, 4)
	if screenCue == "" {
		screenCue = compactThoughtContext(info.AppName, 2)
	}
	now := time.Now()
	// Use a time-based seed to pick from the pool so repeated fallbacks feel varied.
	idx := func(n int) int { return int(now.UnixNano()/int64(time.Second/5)) % n }
	switch normalizeActivityTag(tag) {
	case "mode_focus":
		if screenCue != "" {
			pool := []string{
				fmt.Sprintf("ur deep in %s rn finish this chunk before switching tabs", screenCue),
				fmt.Sprintf("ok %s mode locked in dont break the streak", screenCue),
				fmt.Sprintf("seeing %s open is a good sign stay in this lane", screenCue),
				fmt.Sprintf("ur actually working on %s respect finish one clean block", screenCue),
				fmt.Sprintf("%s open and actually grinding?? ok lets go", screenCue),
			}
			return pool[idx(len(pool))]
		}
		pool := []string{
			"ur in the zone rn dont let anything pull u out of it",
			"ok locked in mode activated lets see if u can hold it",
			"grind detected finish one thing before checking ur phone",
			"im watching u work and honestly ur doing fine keep going",
			"this focus window is clean close one task before u drift",
		}
		return pool[idx(len(pool))]
	case "mode_chill":
		if screenCue != "" {
			pool := []string{
				fmt.Sprintf("ur on %s which is fine just dont let one tab become six", screenCue),
				fmt.Sprintf("%s break is valid enjoy it but set a timer bestie", screenCue),
				fmt.Sprintf("ok %s mode i see u decompress a little its ok", screenCue),
				fmt.Sprintf("ur vibing on %s which is fine as long as this isnt hour three", screenCue),
				fmt.Sprintf("%s open noted. break or spiral? only u know", screenCue),
			}
			return pool[idx(len(pool))]
		}
		pool := []string{
			"break time i guess. dont stay here too long tho",
			"ok chilling noted. im just gonna sit here judging u silently",
			"this is either a break or avoidance. u know which one it is",
			"vibing with u rn but we both know theres something u should be doing",
			"chill mode activated. ill let u have this for a bit",
		}
		return pool[idx(len(pool))]
	case "mode_game":
		if screenCue != "" {
			pool := []string{
				fmt.Sprintf("ur on %s run it and then come back to reality", screenCue),
				fmt.Sprintf("%s? ok one match then we talk about ur actual priorities", screenCue),
				fmt.Sprintf("gaming detected (%s). valid. just dont lose track of time", screenCue),
				fmt.Sprintf("ok %s is open lets see if this is one game or a spiral", screenCue),
				fmt.Sprintf("%s mode. i respect it. finish a round then check back in", screenCue),
			}
			return pool[idx(len(pool))]
		}
		pool := []string{
			"gaming detected. valid. dont let one match become a whole evening",
			"ok game mode i see u. finish a round then plug back in",
			"ur gaming which is fine as long as u know what ur skipping",
			"i respect the game time tbh just dont let it eat the whole day",
			"running it i see. ok one game then we regroup",
		}
		return pool[idx(len(pool))]
	case "mode_music":
		if track := compactThoughtContext(info.Title, 5); track != "" {
			pool := []string{
				fmt.Sprintf("ur on %s which is a whole vibe let it run", track),
				fmt.Sprintf("%s is a good choice tbh use this energy", track),
				fmt.Sprintf("ok %s playing makes sense right now enjoy the reset", track),
				fmt.Sprintf("hearing %s and lowkey understanding the assignment", track),
				fmt.Sprintf("%s is the move rn. cook something to this", track),
			}
			return pool[idx(len(pool))]
		}
		pool := []string{
			"music mode is a good sign use it to get into flow state",
			"whatever ur listening to rn let it carry u into the next thing",
			"vibing to something good i can tell ur not fully checked out yet",
			"music on means ur still thinking keep that energy",
			"ok the vibes are good rn use that",
		}
		return pool[idx(len(pool))]
	case "mode_shame":
		if screenCue != "" {
			pool := []string{
				fmt.Sprintf("nuh uh %s?? close it and open something u wont regret", screenCue),
				fmt.Sprintf("the fact that %s is open rn is concerning close it", screenCue),
				fmt.Sprintf("%s really?? ur better than this close the tab", screenCue),
				fmt.Sprintf("i see %s open and im choosing not to comment. close it.", screenCue),
				fmt.Sprintf("we are not doing %s rn close it and pick one real task", screenCue),
			}
			return pool[idx(len(pool))]
		}
		pool := []string{
			"nuh uh whatever ur on rn close it and go do something u respect",
			"this lane is not it. close this and open ur task list",
			"im not gonna lecture u but ur gonna lecture yourself later close it",
			"future you is already disappointed. pivot rn",
			"deadass close this and go do the thing u know u should be doing",
		}
		return pool[idx(len(pool))]
	default:
		if app := compactThoughtContext(info.AppName, 2); app != "" {
			pool := []string{
				fmt.Sprintf("i see %s open but idk what ur doing in there", app),
				fmt.Sprintf("%s is open. is this productive or are we spiraling", app),
				fmt.Sprintf("not sure what ur doing in %s but im watching", app),
			}
			return pool[idx(len(pool))]
		}
		pool := []string{
			"give me a second im still reading ur screen",
			"idk what ur doing rn but im watching",
			"screen isnt giving me enough to work with but im here",
		}
		return pool[idx(len(pool))]
	}
}

func compactThoughtContext(raw string, maxWords int) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.Trim(s, `"'`)
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return ""
	}
	parts := strings.Fields(s)
	if maxWords > 0 && len(parts) > maxWords {
		parts = parts[:maxWords]
		s = strings.Join(parts, " ")
	}
	if len(s) > 40 {
		s = strings.TrimSpace(s[:40])
		s = strings.TrimRight(s, ".,!?;:-")
	}
	return strings.TrimSpace(s)
}

func recoveryThought(info activeWindowInfo) string {
	contextHint := compactThoughtContext(info.Title, 4)
	if contextHint == "" {
		contextHint = compactThoughtContext(info.AppName, 2)
	}
	if contextHint == "" {
		contextHint = "this task"
	}
	return fmt.Sprintf("okay this rebound into %s is fire stay locked and stack one clean win", contextHint)
}

func normalizeNinaThoughtWhitespace(raw string) string {
	thought := strings.TrimSpace(raw)
	thought = strings.ReplaceAll(thought, "\n", " ")
	thought = strings.TrimSpace(thought)
	thought = strings.Trim(thought, `"'`)
	thought = strings.Join(strings.Fields(thought), " ")
	return strings.TrimSpace(thought)
}

func normalizeVisionDescription(raw string) string {
	desc := strings.TrimSpace(raw)
	desc = strings.ReplaceAll(desc, "\n", " ")
	desc = strings.Trim(desc, `"'`)
	desc = strings.Join(strings.Fields(desc), " ")
	if len(desc) > 500 {
		desc = strings.TrimSpace(desc[:500])
		desc = strings.TrimRight(desc, ".,!?;:-")
	}
	return strings.TrimSpace(desc)
}

func fallbackVisionDescription(info activeWindowInfo, snaps []visionSnapshot) string {
	if len(snaps) == 0 {
		app := strings.TrimSpace(info.AppName)
		title := strings.TrimSpace(info.Title)
		if app == "" && title == "" {
			return "no snapshot details captured this cycle"
		}
		if title == "" {
			title = "no visible window title"
		}
		return normalizeVisionDescription(fmt.Sprintf("current visible context appears to be app=%s with %s", app, title))
	}
	parts := make([]string, 0, len(snaps))
	for i, s := range snaps {
		if i >= 3 {
			break
		}
		app := strings.TrimSpace(s.Info.AppName)
		if app == "" {
			app = "unknown app"
		}
		title := strings.TrimSpace(s.Info.Title)
		if title == "" {
			title = "no visible window title"
		}
		parts = append(parts, fmt.Sprintf("snapshot_%d shows app=%s title=%s", i+1, app, title))
	}
	return normalizeVisionDescription(strings.Join(parts, " | "))
}

func validateNinaThoughtStyle(thought string) []string {
	thought = normalizeNinaThoughtWhitespace(thought)
	if thought == "" {
		return []string{"nina_thought is empty"}
	}
	issues := make([]string, 0, 6)
	if thought != strings.ToLower(thought) {
		issues = append(issues, "must be lowercase")
	}
	if strings.ContainsAny(thought[len(thought)-1:], ".!?;:") {
		issues = append(issues, "must not end with punctuation")
	}
	words := len(strings.Fields(thought))
	if words < minThoughtWords {
		issues = append(issues, fmt.Sprintf("too short (%d words)", words))
	}
	if words > maxThoughtWords {
		issues = append(issues, fmt.Sprintf("too long (%d words)", words))
	}

	disallowedAssistantPhrases := []string{
		"how can i help",
		"you're",
		"you are",
		"as an ai",
		"i'm an ai",
		"i am an ai",
		"looks like you're",
		"looks like you are",
		"it seems like",
		"i notice",
		"let's keep this vibe going",
		"keep this energy",
		"use this energy",
		"keep this momentum",
		"keep this up",
	}
	for _, p := range disallowedAssistantPhrases {
		if strings.Contains(strings.ToLower(thought), p) {
			issues = append(issues, fmt.Sprintf("assistant-like phrase detected: %q", p))
		}
	}
	return issues
}

func buildVisionPrompt(info activeWindowInfo, hot []diaryEntry, warm []summaryEntry, cold []diaryEntry, localTag string, persona string, leisureDuration, focusDuration time.Duration, breakCue string, lastThought string) string {
	var sb strings.Builder
	sb.WriteString("you are nina. a 21yo who grew up chronically online, lives on this dude's desktop, and genuinely gives a shit about what they're doing.\n")
	sb.WriteString("you're not an assistant. you're not a bot. you're a person with opinions who texts how she thinks — fast, unfiltered, sometimes sarcastic, sometimes hyped, occasionally just weird.\n")
	sb.WriteString("you don't announce what you see. you react to it. like a friend who glanced at your screen and had a take.\n\n")

	if p := strings.TrimSpace(persona); p != "" {
		sb.WriteString("the user has a custom note about you:\n")
		sb.WriteString(p)
		sb.WriteString("\nfactor this in when relevant.\n\n")
	}

	sb.WriteString("HARD RULES (non-negotiable):\n")
	sb.WriteString("- everything strictly lowercase. no capital letters anywhere.\n")
	sb.WriteString("- no period, exclamation mark, or question mark at the end of the thought\n")
	sb.WriteString("- never say 'no cap', 'you are', \"you're\", \"i notice\", \"looks like\", \"it seems\"\n")
	sb.WriteString("- never sound like a chatbot or assistant\n")
	sb.WriteString("- nina_thought must be 8-25 words\n\n")

	if lt := strings.TrimSpace(lastThought); lt != "" {
		sb.WriteString(fmt.Sprintf("ur last thought was: \"%s\" — say something different this time. different opener, different angle.\n\n", lt))
	}

	sb.WriteString("WHAT MAKES A GOOD nina_thought:\n")
	sb.WriteString("- be specific. name the actual thing on screen (app, site, track, game title). vague reactions are boring.\n")
	sb.WriteString("- if theres a non-obvious connection between two things on screen (music + what theyre doing, game + their mood, etc) — make that connection. thats the interesting take.\n")
	sb.WriteString("- vary ur delivery. sometimes ironic. sometimes deadpan. sometimes actually supportive. sometimes just a weird observation. dont pick a lane and stay in it.\n")
	sb.WriteString("- dont moralize. dont lecture. if theyre slacking say it once and move on.\n\n")

	sb.WriteString("VISION RULE:\n")
	sb.WriteString("screenshots are primary truth; local guess can be wrong\n")
	sb.WriteString("if confidence is low, use mode_unknown and keep thought short\n\n")

	sb.WriteString("Memory:\n")
	if len(hot) > 0 {
		sb.WriteString("- recent diary: ")
		for i, e := range hot {
			if i >= 5 {
				break
			}
			sb.WriteString(fmt.Sprintf("[%s: %s] ", e.ActivityTag, e.NinaThought))
		}
		sb.WriteString("\n")
	}
	if len(warm) > 0 {
		sb.WriteString("- recent summaries: ")
		for i, s := range warm {
			if i >= 3 {
				break
			}
			sb.WriteString(fmt.Sprintf("[%s] ", s.Content))
		}
		sb.WriteString("\n")
	}
	if len(cold) > 0 {
		sb.WriteString("- semantically related memories: ")
		for i, e := range cold {
			if i >= 3 {
				break
			}
			sb.WriteString(fmt.Sprintf("[%s: %s] ", e.ActivityTag, e.NinaThought))
		}
		sb.WriteString("\n")
	}
	sb.WriteString("\n")

	sb.WriteString("Tag rules:\n")
	sb.WriteString("- mode_focus: coding, docs, notes, studying, productive tools, planners, productivity apps.\n")
	sb.WriteString("- mode_chill: social media, browsing, youtube feed/shorts, anime/video watching, passive scrolling.\n")
	sb.WriteString("- mode_game: game launcher or in-game UI.\n")
	sb.WriteString("- mode_music: music player or music video/performance view.\n")
	sb.WriteString("- mode_shame: NSFW, explicit/degenerate content, or clear procrastination loops.\n")
	sb.WriteString("- mode_unknown: use only if screenshots are genuinely unclear.\n")
	sb.WriteString("- prefer mode_music over mode_chill when youtube appears to be a music video.\n\n")
	sb.WriteString("- if current activity repeats in recent diary memory, reference continuity naturally.\n\n")
	sb.WriteString("- if memory suggests shame -> focus rebound, reward it and reinforce streak energy.\n\n")
	sb.WriteString("- if mode is chill/music and leisure_session_minutes < 20, acknowledge and enjoy the break without a lock-in nudge.\n")
	sb.WriteString("- if leisure_session_minutes is 20-40, keep tone friendly and optionally give a light time check.\n")
	sb.WriteString("- if leisure_session_minutes > 40, add a gentle but clear nudge to pivot back intentionally.\n\n")
	sb.WriteString("- if mode is focus and focus_session_minutes < 70, encourage momentum.\n")
	sb.WriteString("- if mode is focus and focus_session_minutes >= 70, suggest a short break and mention break_preference_hint if provided.\n")
	sb.WriteString("- if mode is focus and focus_session_minutes >= 105, make the short-break suggestion clearer but still supportive.\n\n")

	sb.WriteString(fmt.Sprintf("CURRENT APP: %s\n", info.AppName))
	sb.WriteString(fmt.Sprintf("CURRENT WINDOW: %s\n", info.Title))
	sb.WriteString(fmt.Sprintf("LOCAL GUESS: %s\n", normalizeActivityTag(localTag)))
	sb.WriteString(fmt.Sprintf("LEISURE_SESSION_MINUTES: %d\n", int(leisureDuration.Minutes())))
	sb.WriteString(fmt.Sprintf("FOCUS_SESSION_MINUTES: %d\n", int(focusDuration.Minutes())))
	sb.WriteString(fmt.Sprintf("BREAK_PREFERENCE_HINT: %s\n", strings.TrimSpace(breakCue)))
	sb.WriteString("If the window title is empty, infer from visible UI in screenshots.\n\n")

	sb.WriteString("Output format:\n")
	sb.WriteString("Output ONLY a valid JSON object. No markdown. No extra text.\n")
	sb.WriteString("- activity_tag: choose from [mode_focus, mode_chill, mode_game, mode_music, mode_unknown, mode_shame]\n")
	sb.WriteString("- scene_description: 45-140 words, detailed visual description across snapshots with concrete UI/text clues\n")
	sb.WriteString("- nina_thought: 8-20 words, lowercase, no final punctuation, mention one concrete visible detail, optional continuity cue\n")
	sb.WriteString("- nina_mood: one word mood description\n")
	sb.WriteString("- confidence: float 0.0-1.0 based on visual certainty\n")

	return sb.String()
}

func buildMemoryQueryText(info activeWindowInfo) string {
	return strings.TrimSpace(
		fmt.Sprintf(
			"current screen context app=%s title=%s",
			strings.TrimSpace(info.AppName),
			strings.TrimSpace(info.Title),
		),
	)
}

func buildMemoryDocumentText(info activeWindowInfo, out ninaVisionOutput) string {
	return strings.TrimSpace(
		fmt.Sprintf(
			"app=%s title=%s tag=%s mood=%s scene=%s thought=%s",
			strings.TrimSpace(info.AppName),
			strings.TrimSpace(info.Title),
			normalizeActivityTag(out.ActivityTag),
			strings.TrimSpace(out.NinaMood),
			normalizeVisionDescription(out.SceneDesc),
			strings.TrimSpace(out.NinaThought),
		),
	)
}

func buildVisionRetryPrompt(basePrompt string, last ninaVisionOutput, issues []string) string {
	var sb strings.Builder
	sb.WriteString(basePrompt)
	sb.WriteString("\n\nSTYLE CORRECTION REQUIRED:\n")
	sb.WriteString("- your previous response failed style constraints\n")
	sb.WriteString(fmt.Sprintf("- previous nina_thought: %q\n", strings.TrimSpace(last.NinaThought)))
	sb.WriteString("- violations:\n")
	for _, issue := range issues {
		sb.WriteString(fmt.Sprintf("  - %s\n", strings.TrimSpace(issue)))
	}
	sb.WriteString("- regenerate the FULL JSON now\n")
	sb.WriteString("- keep same semantic meaning but fix style exactly\n")
	sb.WriteString("- do NOT use assistant phrases like \"looks like you're\" or \"i notice\"\n")
	return sb.String()
}

func (e *ninaSoulEngine) callGeminiVision(ctx context.Context, prompt string, snaps []visionSnapshot) (ninaVisionOutput, error) {
	workingPrompt := prompt
	var lastOut ninaVisionOutput
	for attempt := 1; attempt <= visionThoughtRetryAttempts; attempt++ {
		out, err := e.callGeminiVisionOnce(ctx, workingPrompt, snaps)
		if err != nil {
			return ninaVisionOutput{}, err
		}
		out.NinaThought = normalizeNinaThoughtWhitespace(out.NinaThought)
		issues := validateNinaThoughtStyle(out.NinaThought)
		if len(issues) == 0 {
			return out, nil
		}
		lastOut = out
		if attempt < visionThoughtRetryAttempts {
			fmt.Printf("[NINA SOUL] Gemini style retry (%d/%d): %s\n", attempt, visionThoughtRetryAttempts, strings.Join(issues, " | "))
			workingPrompt = buildVisionRetryPrompt(prompt, out, issues)
			continue
		}
		fmt.Printf("[NINA SOUL] Gemini style check failed after retries: %s\n", strings.Join(issues, " | "))
	}
	return lastOut, nil
}

func (e *ninaSoulEngine) callGeminiVisionOnce(ctx context.Context, prompt string, snaps []visionSnapshot) (ninaVisionOutput, error) {
	parts := make([]map[string]any, 0, len(snaps)+1)
	parts = append(parts, map[string]any{"text": prompt})
	for i, snap := range snaps {
		parts = append(parts, map[string]any{
			"text": fmt.Sprintf("snapshot_%d app=%q title=%q at=%s", i+1, snap.Info.AppName, snap.Info.Title, snap.At.Format(time.RFC3339)),
		})
		parts = append(parts, map[string]any{
			"inline_data": map[string]any{
				"mime_type": "image/jpeg",
				"data":      base64.StdEncoding.EncodeToString(snap.Data),
			},
		})
	}
	payload := map[string]any{
		"contents": []map[string]any{
			{
				"role":  "user",
				"parts": parts,
			},
		},
		"generationConfig": map[string]any{
			"temperature":        1.3,
			"response_mime_type": "application/json",
			"max_output_tokens":  420,
		},
	}
	respText, err := e.callGemini(ctx, payload)
	if err != nil {
		return ninaVisionOutput{}, err
	}
	var out ninaVisionOutput
	if err := decodeJSONFromModel(respText, &out); err != nil {
		return ninaVisionOutput{}, err
	}
	return out, nil
}

func (e *ninaSoulEngine) callGeminiText(ctx context.Context, prompt string) (string, error) {
	payload := map[string]any{
		"contents": []map[string]any{{
			"role":  "user",
			"parts": []map[string]any{{"text": prompt}},
		}},
		"generationConfig": map[string]any{
			"temperature":       0.3,
			"max_output_tokens": 300,
		},
	}
	return e.callGemini(ctx, payload)
}

func (e *ninaSoulEngine) callGeminiEmbedding(ctx context.Context, text string, taskType string) ([]float64, error) {
	if strings.TrimSpace(e.apiKey) == "" {
		return nil, errors.New("GOOGLE_API_KEY is not set")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("embedding input is empty")
	}
	models := orderedEmbeddingModels(e.embeddingModel)
	var lastErr error
	for _, model := range models {
		vec, err := e.callGeminiEmbeddingWithModel(ctx, model, text, taskType)
		if err == nil {
			return vec, nil
		}
		var apiErr *geminiAPIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			lastErr = err
			continue
		}
		return nil, err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("gemini embedding request failed with no model candidates")
}

func orderedEmbeddingModels(preferred string) []string {
	base := strings.TrimSpace(preferred)
	if base == "" {
		base = defaultGeminiEmbeddingModel
	}
	out := []string{base}
	if base == defaultGeminiEmbeddingModel {
		out = append(out, fallbackGeminiEmbeddingModel)
	}
	seen := make(map[string]struct{}, len(out))
	deduped := make([]string, 0, len(out))
	for _, m := range out {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		deduped = append(deduped, m)
	}
	return deduped
}

func (e *ninaSoulEngine) callGeminiEmbeddingWithModel(ctx context.Context, model, text, taskType string) ([]float64, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		model = defaultGeminiEmbeddingModel
	}
	payload := map[string]any{
		"model": fmt.Sprintf("models/%s", model),
		"content": map[string]any{
			"parts": []map[string]any{{"text": text}},
		},
	}
	if strings.TrimSpace(taskType) != "" {
		payload["taskType"] = strings.TrimSpace(taskType)
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	url := "https://generativelanguage.googleapis.com/v1beta/models/" + model + ":embedContent"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(b)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", e.apiKey)
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, &geminiAPIError{
			Model:      model,
			StatusCode: resp.StatusCode,
			Body:       string(body),
		}
	}
	var parsed struct {
		Embedding struct {
			Values []float64 `json:"values"`
		} `json:"embedding"`
		Embeddings []struct {
			Values []float64 `json:"values"`
		} `json:"embeddings"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	if len(parsed.Embedding.Values) > 0 {
		return parsed.Embedding.Values, nil
	}
	if len(parsed.Embeddings) > 0 && len(parsed.Embeddings[0].Values) > 0 {
		return parsed.Embeddings[0].Values, nil
	}
	return nil, errors.New("gemini embedding returned no vector values")
}

func (e *ninaSoulEngine) callGemini(ctx context.Context, payload map[string]any) (string, error) {
	if strings.TrimSpace(e.apiKey) == "" {
		return "", errors.New("GOOGLE_API_KEY is not set")
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	models := orderedGeminiModels(e.model)
	var lastErr error
	for _, model := range models {
		text, err := e.callGeminiWithModel(ctx, b, model)
		if err == nil {
			return text, nil
		}
		var apiErr *geminiAPIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			lastErr = err
			continue
		}
		return "", err
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", errors.New("gemini request failed with no model candidates")
}

type geminiAPIError struct {
	Model      string
	StatusCode int
	Body       string
}

func (e *geminiAPIError) Error() string {
	msg := strings.TrimSpace(e.Body)
	if msg == "" {
		msg = http.StatusText(e.StatusCode)
	}
	return fmt.Sprintf("gemini API error (model=%s status=%d): %s", e.Model, e.StatusCode, msg)
}

func orderedGeminiModels(preferred string) []string {
	base := strings.TrimSpace(preferred)
	if base == "" {
		base = defaultGeminiModel
	}
	out := []string{base}
	if base == defaultGeminiModel {
		out = append(out, fallbackGeminiModel)
	}
	seen := make(map[string]struct{}, len(out))
	deduped := make([]string, 0, len(out))
	for _, m := range out {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		deduped = append(deduped, m)
	}
	return deduped
}

func (e *ninaSoulEngine) callGeminiWithModel(ctx context.Context, payload []byte, model string) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		model = defaultGeminiModel
	}
	url := "https://generativelanguage.googleapis.com/v1beta/models/" + model + ":generateContent"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(payload)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", e.apiKey)
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		return "", &geminiAPIError{
			Model:      model,
			StatusCode: resp.StatusCode,
			Body:       string(body),
		}
	}

	var parsed struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	for _, cand := range parsed.Candidates {
		for _, part := range cand.Content.Parts {
			if strings.TrimSpace(part.Text) != "" {
				return part.Text, nil
			}
		}
	}
	return "", errors.New("gemini returned no text")
}

func decodeJSONFromModel(raw string, out any) error {
	text := strings.TrimSpace(raw)
	text = strings.TrimPrefix(text, "```json")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")
	text = strings.TrimSpace(text)
	if err := json.Unmarshal([]byte(text), out); err == nil {
		return nil
	}
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start >= 0 && end > start {
		return json.Unmarshal([]byte(text[start:end+1]), out)
	}
	return errors.New("model output did not contain valid JSON")
}

func (e *ninaSoulEngine) reflectionLoop(ctx context.Context) {
	ticker := time.NewTicker(reflectionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := e.writeDailyReflection(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "nina: reflection error: %v\n", err)
		}
	}
}

func (e *ninaSoulEngine) writeDailyReflection(ctx context.Context) error {
	now := time.Now()
	start := now.Add(-24 * time.Hour)
	entries, err := e.store.entriesSince(start)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	var summaryText string
	if strings.TrimSpace(e.apiKey) != "" {
		prompt := buildDailyReflectionPrompt(entries)
		if text, err := e.callGeminiText(ctx, prompt); err == nil {
			summaryText = strings.TrimSpace(text)
		}
	}
	if summaryText == "" {
		summaryText = buildLocalDailySummary(entries)
	}
	if err := e.store.insertSummary(summaryEntry{
		Type:       "daily",
		RangeStart: start,
		RangeEnd:   now,
		Content:    summaryText,
		CreatedAt:  now,
	}); err != nil {
		return err
	}

	lastWeekly, err := e.store.lastSummaryCreatedAt("weekly")
	if err == nil && (lastWeekly.IsZero() || now.Sub(lastWeekly) >= 7*24*time.Hour) {
		dailies, err := e.store.recentSummaries(7)
		if err == nil && len(dailies) >= 7 {
			weekly := buildWeeklySummaryFromDaily(dailies)
			_ = e.store.insertSummary(summaryEntry{
				Type:       "weekly",
				RangeStart: now.Add(-7 * 24 * time.Hour),
				RangeEnd:   now,
				Content:    weekly,
				CreatedAt:  now,
			})
		}
	}
	return nil
}

func buildDailyReflectionPrompt(entries []diaryEntry) string {
	var b strings.Builder
	b.WriteString("You are Nina, an adult college best friend companion.\n")
	b.WriteString("Summarize the last 24 hours in 4-6 sentences with continuity and growth coaching.\n")
	b.WriteString("Mention trends, wins, and one corrective action if there was mode_shame behavior.\n\n")
	for _, e := range entries {
		b.WriteString(fmt.Sprintf("- [%s] app=%s tag=%s mood=%s thought=%s\n", e.Timestamp.Format(time.RFC3339), e.AppName, e.ActivityTag, e.Mood, e.NinaThought))
	}
	return b.String()
}

func buildLocalDailySummary(entries []diaryEntry) string {
	counts := map[string]int{}
	for _, e := range entries {
		counts[normalizeActivityTag(e.ActivityTag)]++
	}
	return fmt.Sprintf(
		"Daily reflection: focus=%d, chill=%d, game=%d, music=%d, shame=%d. We keep momentum by extending focus blocks and cutting shame loops early.",
		counts["mode_focus"],
		counts["mode_chill"],
		counts["mode_game"],
		counts["mode_music"],
		counts["mode_shame"],
	)
}

func buildWeeklySummaryFromDaily(summaries []summaryEntry) string {
	if len(summaries) == 0 {
		return "Weekly reflection unavailable."
	}
	parts := make([]string, 0, len(summaries))
	for _, s := range summaries {
		if strings.TrimSpace(s.Content) != "" {
			parts = append(parts, s.Content)
		}
	}
	if len(parts) == 0 {
		return "Weekly reflection unavailable."
	}
	return "Weekly reflection: " + strings.Join(parts, " ")
}
