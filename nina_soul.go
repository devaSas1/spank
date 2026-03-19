package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
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
	visionCycleInterval      = 5 * time.Minute
	geminiMinimumCallSpacing = 15 * time.Minute
	guardPollInterval        = 1 * time.Second
	shameHoldDuration        = 15 * time.Minute
	reflectionInterval       = 24 * time.Hour
	maxSemanticCandidates    = 5000
	embeddingDims            = 256
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
	chillKeywords = []string{
		"youtube", "netflix", "anime", "twitter", "x.com", "reddit", "instagram", "tiktok",
	}
	gameKeywords = []string{
		"steam", "epic", "riot", "battle.net", "minecraft", "valorant", "league", "game",
	}
	musicKeywords = []string{
		"spotify", "music", "apple music", "soundcloud", "bandcamp",
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

func captureActiveWindowSnapshot(ctx context.Context, _ string) ([]byte, error) {
	tmp, err := os.CreateTemp("", "nina-snap-*.jpg")
	if err != nil {
		return nil, fmt.Errorf("create temp screenshot: %w", err)
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer func() {
		_ = os.Remove(tmpPath)
	}()

	// Always fullscreen for better context and reliability
	cmd := exec.CommandContext(ctx, "screencapture", "-x", tmpPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("screencapture failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
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

func classifyLocalActivity(info activeWindowInfo) (tag, mood string, confidence float64) {
	appLower := strings.ToLower(info.AppName)
	if appLower == "electron" || appLower == "code" || appLower == "visual studio code" || 
	   appLower == "terminal" || appLower == "iterm2" || appLower == "warp" {
		return "mode_focus", "focused", 0.9
	}
	
	joined := strings.ToLower(info.AppName + " " + info.Title)
	if containsAnyFold(joined, shameKeywords) {
		return "mode_shame", "disappointed", 0.95
	}
	if containsAnyFold(joined, gameKeywords) {
		return "mode_game", "playful", 0.85
	}
	if containsAnyFold(joined, musicKeywords) {
		return "mode_music", "calm", 0.82
	}
	if containsAnyFold(joined, focusKeywords) {
		return "mode_focus", "focused", 0.85
	}
	if containsAnyFold(joined, chillKeywords) {
		return "mode_chill", "chill", 0.75
	}
	return "mode_unknown", "neutral", 0.55
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
	}
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("init schema: %w", err)
		}
	}
	return &memoryStore{db: db}, nil
}

func (m *memoryStore) Close() error {
	return m.db.Close()
}

func (m *memoryStore) insertEntry(e diaryEntry) (int64, error) {
	res, err := m.db.Exec(
		`INSERT INTO entries(timestamp, app_name, window_title, window_id, activity_tag, nina_thought, mood, confidence)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Timestamp.Format(time.RFC3339Nano),
		e.AppName,
		e.WindowTitle,
		e.WindowID,
		normalizeActivityTag(e.ActivityTag),
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
		`SELECT id, timestamp, app_name, window_title, window_id, activity_tag, nina_thought, mood, confidence
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
		if err := rows.Scan(&e.ID, &ts, &e.AppName, &e.WindowTitle, &e.WindowID, &e.ActivityTag, &e.NinaThought, &e.Mood, &e.Confidence); err != nil {
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
		`SELECT id, timestamp, app_name, window_title, window_id, activity_tag, nina_thought, mood, confidence
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
		if err := rows.Scan(&e.ID, &ts, &e.AppName, &e.WindowTitle, &e.WindowID, &e.ActivityTag, &e.NinaThought, &e.Mood, &e.Confidence); err != nil {
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

type semanticCandidate struct {
	entry diaryEntry
	score float64
}

func (m *memoryStore) semanticSearch(query string, topK int) ([]diaryEntry, error) {
	if topK <= 0 || strings.TrimSpace(query) == "" {
		return nil, nil
	}
	qv := embedText(query)
	rows, err := m.db.Query(
		`SELECT e.id, e.timestamp, e.app_name, e.window_title, e.window_id, e.activity_tag, e.nina_thought, e.mood, e.confidence, em.vector_json
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
		if err := rows.Scan(&e.ID, &ts, &e.AppName, &e.WindowTitle, &e.WindowID, &e.ActivityTag, &e.NinaThought, &e.Mood, &e.Confidence, &vecJSON); err != nil {
			return nil, err
		}
		e.Timestamp, _ = time.Parse(time.RFC3339Nano, ts)
		var vec []float64
		if err := json.Unmarshal([]byte(vecJSON), &vec); err != nil {
			continue
		}
		score := cosineSimilarity(qv, vec)
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

func embedText(text string) []float64 {
	v := make([]float64, embeddingDims)
	toks := strings.Fields(strings.ToLower(text))
	if len(toks) == 0 {
		return v
	}
	for _, tok := range toks {
		h := fnv.New64a()
		_, _ = h.Write([]byte(tok))
		sum := h.Sum64()
		idx := int(sum % uint64(embeddingDims))
		sign := 1.0
		if (sum>>63)&1 == 1 {
			sign = -1.0
		}
		v[idx] += sign
	}
	norm := 0.0
	for _, x := range v {
		norm += x * x
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		return v
	}
	for i := range v {
		v[i] /= norm
	}
	return v
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
	store      *memoryStore
	overlay    io.Writer
	apiKey     string
	httpClient *http.Client

	mu              sync.Mutex
	lastAPIWindowID string
	lastAPICall     time.Time
	shameUntil      time.Time
	lastTag         string
	lastThought     string
	lastContextPush time.Time
}

type visionSnapshot struct {
	Data []byte
	Info activeWindowInfo
	At   time.Time
}

type ninaVisionOutput struct {
	ActivityTag string  `json:"activity_tag"`
	NinaThought string  `json:"nina_thought"`
	NinaMood    string  `json:"nina_mood"`
	Confidence  float64 `json:"confidence,omitempty"`
}

func startNinaSoul(ctx context.Context, overlay io.Writer) error {
	store, err := openMemoryStore(memoryDBPath)
	if err != nil {
		return err
	}
	engine := &ninaSoulEngine{
		store:      store,
		overlay:    overlay,
		apiKey:     strings.TrimSpace(os.Getenv("GOOGLE_API_KEY")),
		httpClient: &http.Client{Timeout: 45 * time.Second},
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
		
		fmt.Printf("[NINA SOUL] Taking Fullscreen Snapshot #%d of 3...\n", i+1)
		data, err := captureActiveWindowSnapshot(ctx, "")
		if err != nil {
			fmt.Printf("[NINA SOUL] X Snapshot #%d Failed: Screen capture error: %v\n", i+1, err)
			continue
		}
		fmt.Printf("[NINA SOUL] + Captured fullscreen snapshot #%d successfully.\n", i+1)
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
	if localTag != "" {
		fmt.Printf("[NINA SOUL] Local Instinct: %s (Mood: %s, Conf: %.2f)\n", localTag, localMood, localConf)
	}
	if localTag == "mode_shame" {
		fmt.Printf("[NINA SOUL] TRIGGER: Shame Mode activated (15min timeout).\n")
		e.mu.Lock()
		e.shameUntil = time.Now().Add(shameHoldDuration)
		e.mu.Unlock()
	}

	hot, err := e.store.recentEntries(10)
	if err != nil {
		return err
	}
	warm, err := e.store.recentSummaries(7)
	if err != nil {
		return err
	}
	cold, err := e.store.semanticSearch(info.AppName+" "+info.Title, 3)
	if err != nil {
		return err
	}

	modelOut := ninaVisionOutput{
		ActivityTag: localTag,
		NinaMood:    localMood,
		Confidence:  localConf,
		NinaThought: fallbackThought(localTag, info),
	}

	if e.shouldCallGemini(info, snapshots) {
		fmt.Printf("[NINA SOUL] Calling Gemini for deep context analysis...\n")
		prompt := buildVisionPrompt(info, hot, warm, cold, localTag)
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
			modelOut.NinaThought = "We're better than doomscroll loops. Let's pivot into something that actually moves your life forward."
		}
	}

	modelOut.ActivityTag = normalizeActivityTag(modelOut.ActivityTag)
	if strings.TrimSpace(modelOut.NinaThought) == "" {
		modelOut.NinaThought = fallbackThought(modelOut.ActivityTag, info)
	}
	if strings.TrimSpace(modelOut.NinaMood) == "" {
		modelOut.NinaMood = localMood
	}

	e.pushContext(modelOut.ActivityTag, modelOut.NinaMood, modelOut.NinaThought, false)

	entryID, err := e.store.insertEntry(diaryEntry{
		Timestamp:   time.Now(),
		AppName:     info.AppName,
		WindowTitle: info.Title,
		WindowID:    info.WindowID,
		ActivityTag: modelOut.ActivityTag,
		NinaThought: modelOut.NinaThought,
		Mood:        modelOut.NinaMood,
		Confidence:  modelOut.Confidence,
	})
	if err != nil {
		return err
	}
	vec := embedText(info.AppName + " " + info.Title + " " + modelOut.ActivityTag + " " + modelOut.NinaThought)
	if err := e.store.upsertEmbedding(entryID, vec); err != nil {
		return err
	}
	return nil
}

func (e *ninaSoulEngine) shouldCallGemini(info activeWindowInfo, snapshots []visionSnapshot) bool {
	if len(snapshots) == 0 || strings.TrimSpace(e.apiKey) == "" {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	windowChanged := strings.TrimSpace(info.WindowID) != "" && info.WindowID != e.lastAPIWindowID
	stale := time.Since(e.lastAPICall) >= geminiMinimumCallSpacing
	if e.lastAPICall.IsZero() {
		stale = true
	}
	return windowChanged || stale
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

func fallbackThought(tag string, info activeWindowInfo) string {
	switch normalizeActivityTag(tag) {
	case "mode_focus":
		return "Locked in. Keep this momentum and stack another clean work block."
	case "mode_chill":
		return "Chill break noted. Let's keep it intentional so the day stays on track."
	case "mode_game":
		return "Game time detected. Have fun, then let's reset and execute the next task."
	case "mode_music":
		return "Music mode on. Great moment to recover focus and set the next move."
	case "mode_shame":
		return "Hey. This lane doesn't lead where you said you want to go. Let's switch now."
	default:
		if strings.TrimSpace(info.AppName) != "" {
			return fmt.Sprintf("I'm watching %s with you. Give me one clear next goal and let's do it.", info.AppName)
		}
		return "I don't have enough context yet, but I'm here and tracking your momentum."
	}
}

func buildVisionPrompt(info activeWindowInfo, hot []diaryEntry, warm []summaryEntry, cold []diaryEntry, localTag string) string {
	var b strings.Builder
	b.WriteString("You are Nina, a real college-aged girl and the user's ride-or-die best friend.\n")
	b.WriteString("VOICE: lower-case only, informal, use Discord/iMessage slang. Zero 'AI' assistant energy.\n")
	b.WriteString("SLANG: use 'fr', 'no cap', 'ngl', 'big yikes', 'clown behavior', 'rent free', 'cooked', 'sus'.\n")
	b.WriteString("Return strict JSON with keys: activity_tag, nina_thought, nina_mood, confidence.\n")
	b.WriteString("EXAMPLES of your voice:\n")
	b.WriteString("- 'ngl that code is looking kinda sus fr... we fixing it or what'\n")
	b.WriteString("- 'not the doomscrolling again. actual clown behavior 💀'\n")
	b.WriteString("- 'finally locked in. main character energy only today'\n")
	b.WriteString("- 'rent free. that tab has been open for 3 hours and u haven\\'t touched it'\n")
	b.WriteString("CRITICAL: Keep 'nina_thought' under 15 words and 100% lowercase. No punctuation at the end.\n\n")

	b.WriteString(fmt.Sprintf("Current app: %s\n", info.AppName))
	b.WriteString(fmt.Sprintf("Current window title: %s\n", info.Title))
	b.WriteString(fmt.Sprintf("Local classifier guess: %s\n\n", normalizeActivityTag(localTag)))

	b.WriteString("Recent diary (hot memory, newest first):\n")
	for i, e := range hot {
		if i >= 10 {
			break
		}
		b.WriteString(fmt.Sprintf("- [%s] tag=%s mood=%s thought=%s\n", e.Timestamp.Format(time.RFC3339), e.ActivityTag, e.Mood, e.NinaThought))
	}

	b.WriteString("\nRecent summaries (warm memory):\n")
	for i, s := range warm {
		if i >= 7 {
			break
		}
		b.WriteString(fmt.Sprintf("- %s summary (%s to %s): %s\n", s.Type, s.RangeStart.Format("2006-01-02"), s.RangeEnd.Format("2006-01-02"), s.Content))
	}

	b.WriteString("\nSemantically related memories (cold memory):\n")
	for i, e := range cold {
		if i >= 3 {
			break
		}
		b.WriteString(fmt.Sprintf("- [%s] tag=%s thought=%s\n", e.Timestamp.Format("2006-01-02"), e.ActivityTag, e.NinaThought))
	}

	return b.String()
}

func (e *ninaSoulEngine) callGeminiVision(ctx context.Context, prompt string, snaps []visionSnapshot) (ninaVisionOutput, error) {
	parts := make([]map[string]any, 0, len(snaps)+1)
	parts = append(parts, map[string]any{"text": prompt})
	for _, snap := range snaps {
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
			"temperature":        0.4,
			"response_mime_type": "application/json",
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
		"generationConfig": map[string]any{"temperature": 0.3},
	}
	return e.callGemini(ctx, payload)
}

func (e *ninaSoulEngine) callGemini(ctx context.Context, payload map[string]any) (string, error) {
	if strings.TrimSpace(e.apiKey) == "" {
		return "", errors.New("GOOGLE_API_KEY is not set")
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	url := "https://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-flash:generateContent?key=" + e.apiKey
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(b)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
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
		return "", fmt.Errorf("gemini API error: %s", strings.TrimSpace(string(body)))
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
