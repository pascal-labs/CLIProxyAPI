// Package sessionmeta resolves a hub session key (as derived by the session
// affinity selector, e.g. "claude:<uuid>" or "codex:<uuid>") to local
// metadata: the agent's working directory and project, a best-effort title,
// and the T3 Code thread exported to <auth-dir>/t3-threads.json. The hub runs
// on the same machine as the agents, so everything is read from local files.
// Lookups are cached and bounded (session files are only read at their head
// and tail), and every failure degrades to empty fields.
package sessionmeta

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/hubsidecar"
)

// Client kinds reported for a session key.
const (
	ClientClaudeCode = "claude-code"
	ClientCodex      = "codex"
	ClientOther      = "other"
)

const (
	headBytes        = 64 << 10
	tailBytes        = 128 << 10
	maxFirstLine     = 1 << 20
	maxIndexFileSize = 16 << 20
	indexRefresh     = time.Minute
	infoRecheck      = 2 * time.Minute
	missRecheck      = 5 * time.Minute
)

// Info is the resolved metadata of one session. Empty strings mean unknown.
type Info struct {
	Client        string
	NativeID      string
	Cwd           string
	Project       string
	Title         string
	T3ThreadID    string
	T3Title       string
	T3ParentTitle string
	// Subagent is nil when no T3 thread matched.
	Subagent *bool
}

var nativeIDPattern = regexp.MustCompile(`^[0-9a-f][0-9a-f-]{7,63}$`)

// NativeID splits a session key into its client kind and the provider-native
// session id used by the agent's own files and the T3 export. For
// "claude:<id>[:agent:<agent>]" and "codex:<id>[:agent:<agent>]" the native id
// is the segment after the prefix up to the next ':' (subagent and agent
// suffixes are dropped), lowercased; it must look like a UUID (hex digits and
// dashes) or it is returned empty. Other keys ("header:", "derived:", ...)
// are ClientOther with no native id.
func NativeID(sessionKey string) (client, id string) {
	sessionKey = strings.TrimSpace(sessionKey)
	prefix, rest, ok := strings.Cut(sessionKey, ":")
	if !ok {
		return ClientOther, ""
	}
	switch strings.ToLower(prefix) {
	case "claude":
		client = ClientClaudeCode
	case "codex":
		client = ClientCodex
	default:
		return ClientOther, ""
	}
	id, _, _ = strings.Cut(rest, ":")
	id = strings.ToLower(strings.TrimSpace(id))
	if !nativeIDPattern.MatchString(id) {
		return client, ""
	}
	return client, id
}

// DefaultRoots returns the default search roots: $CLAUDE_CONFIG_DIR,
// ~/.claude, ~/.claude-hub, $CODEX_HOME and ~/.codex.
func DefaultRoots() []string {
	var roots []string
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		roots = append(roots, dir)
	}
	home, errHome := os.UserHomeDir()
	if errHome == nil && home != "" {
		roots = append(roots, filepath.Join(home, ".claude"), filepath.Join(home, ".claude-hub"))
	}
	if dir := strings.TrimSpace(os.Getenv("CODEX_HOME")); dir != "" {
		roots = append(roots, dir)
	}
	if errHome == nil && home != "" {
		roots = append(roots, filepath.Join(home, ".codex"))
	}
	return dedupe(roots)
}

func expandRoots(roots []string) []string {
	home, _ := os.UserHomeDir()
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		if home != "" && (root == "~" || strings.HasPrefix(root, "~/")) {
			root = filepath.Join(home, strings.TrimPrefix(root, "~"))
		}
		out = append(out, filepath.Clean(root))
	}
	return dedupe(out)
}

func dedupe(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := values[:0]
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

type fileInfo struct {
	cwd   string
	title string
}

type cachedInfo struct {
	path      string
	modTime   time.Time
	size      int64
	checkedAt time.Time
	found     bool
	info      fileInfo
}

type t3Thread struct {
	T3ThreadID       string  `json:"t3_thread_id"`
	Title            string  `json:"title"`
	Project          string  `json:"project"`
	ParentT3ThreadID *string `json:"parent_t3_thread_id"`
	ParentTitle      *string `json:"parent_title"`
	Subagent         bool    `json:"subagent"`
}

type t3File struct {
	GeneratedAt string              `json:"generated_at"`
	Threads     map[string]t3Thread `json:"threads"`
}

type statKey struct {
	modTime time.Time
	size    int64
}

// Resolver caches lookups. It is safe for concurrent use.
type Resolver struct {
	now func() time.Time

	mu      sync.Mutex
	roots   []string
	authDir string

	claudeIndex   map[string]string
	codexIndex    map[string]string
	indexBuiltAt  time.Time
	infoCache     map[string]*cachedInfo
	codexTitles   map[string]string
	codexTitleKey map[string]statKey
	t3Threads     map[string]t3Thread
	t3Key         statKey
	t3Loaded      bool
}

var defaultResolver = NewResolver(nil)

// Default returns the process-wide resolver.
func Default() *Resolver { return defaultResolver }

// NewResolver constructs a resolver; now defaults to time.Now.
func NewResolver(now func() time.Time) *Resolver {
	if now == nil {
		now = time.Now
	}
	return &Resolver{now: now, infoCache: make(map[string]*cachedInfo), codexTitles: make(map[string]string), codexTitleKey: make(map[string]statKey)}
}

// Configure sets the search roots (empty means DefaultRoots) and the auth
// directory holding t3-threads.json. Changing the roots resets the caches.
func (r *Resolver) Configure(roots []string, authDir string) {
	if r == nil {
		return
	}
	if len(roots) == 0 {
		roots = DefaultRoots()
	} else {
		roots = expandRoots(roots)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.Join(roots, "\x00") != strings.Join(r.roots, "\x00") {
		r.roots = roots
		r.claudeIndex, r.codexIndex, r.indexBuiltAt = nil, nil, time.Time{}
		r.infoCache = make(map[string]*cachedInfo)
		r.codexTitles = make(map[string]string)
		r.codexTitleKey = make(map[string]statKey)
	}
	if authDir != r.authDir {
		r.authDir = authDir
		r.t3Loaded = false
	}
}

// Resolve returns the metadata for one session key. The deadline bounds file
// reads for uncached sessions: past it, only cached data and the T3 export
// are used.
func (r *Resolver) Resolve(sessionKey string, deadline time.Time) Info {
	client, id := NativeID(sessionKey)
	info := Info{Client: client, NativeID: id}
	if r == nil || id == "" {
		return info
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	allowIO := deadline.IsZero() || now.Before(deadline)
	if file, ok := r.fileInfoLocked(client, id, now, allowIO); ok {
		info.Cwd = file.cwd
		info.Title = file.title
	}
	if client == ClientCodex && info.Title == "" && allowIO {
		info.Title = r.codexTitleLocked(id)
	}
	if info.Cwd != "" {
		info.Project = filepath.Base(info.Cwd)
	}
	if thread, ok := r.t3ThreadLocked(id); ok {
		info.T3ThreadID = thread.T3ThreadID
		info.T3Title = thread.Title
		if thread.ParentTitle != nil {
			info.T3ParentTitle = *thread.ParentTitle
		}
		subagent := thread.Subagent
		info.Subagent = &subagent
		if info.Project == "" {
			info.Project = strings.TrimSpace(thread.Project)
		}
	}
	return info
}

func (r *Resolver) fileInfoLocked(client, id string, now time.Time, allowIO bool) (fileInfo, bool) {
	cacheKey := client + ":" + id
	cached := r.infoCache[cacheKey]
	if cached != nil {
		recheck := infoRecheck
		if !cached.found {
			recheck = missRecheck
		}
		if !allowIO || now.Sub(cached.checkedAt) < recheck {
			return cached.info, cached.found
		}
		if cached.found {
			if stat, errStat := os.Stat(cached.path); errStat == nil && stat.ModTime().Equal(cached.modTime) && stat.Size() == cached.size {
				cached.checkedAt = now
				return cached.info, true
			}
		}
	}
	if !allowIO {
		return fileInfo{}, false
	}
	path := r.lookupPathLocked(client, id, now)
	entry := &cachedInfo{path: path, checkedAt: now}
	r.infoCache[cacheKey] = entry
	if path == "" {
		return fileInfo{}, false
	}
	stat, errStat := os.Stat(path)
	if errStat != nil {
		return fileInfo{}, false
	}
	entry.modTime, entry.size = stat.ModTime(), stat.Size()
	if client == ClientClaudeCode {
		entry.info = readClaudeSession(path, stat.Size())
	} else {
		entry.info = readCodexRollout(path)
	}
	entry.found = true
	return entry.info, true
}

func (r *Resolver) lookupPathLocked(client, id string, now time.Time) string {
	index := r.claudeIndex
	if client == ClientCodex {
		index = r.codexIndex
	}
	if path, ok := index[id]; ok {
		return path
	}
	if index != nil && now.Sub(r.indexBuiltAt) < indexRefresh {
		return ""
	}
	r.buildIndexLocked(now)
	if client == ClientCodex {
		return r.codexIndex[id]
	}
	return r.claudeIndex[id]
}

// buildIndexLocked lists session file names (no file contents) under every root.
func (r *Resolver) buildIndexLocked(now time.Time) {
	claude := make(map[string]string)
	codex := make(map[string]string)
	for _, root := range r.roots {
		projects, _ := os.ReadDir(filepath.Join(root, "projects"))
		for _, project := range projects {
			if !project.IsDir() {
				continue
			}
			dir := filepath.Join(root, "projects", project.Name())
			entries, _ := os.ReadDir(dir)
			for _, entry := range entries {
				name := entry.Name()
				if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") {
					continue
				}
				id := strings.ToLower(strings.TrimSuffix(name, ".jsonl"))
				if _, exists := claude[id]; !exists && nativeIDPattern.MatchString(id) {
					claude[id] = filepath.Join(dir, name)
				}
			}
		}
		for _, sessionsDir := range []string{filepath.Join(root, "sessions"), filepath.Join(root, "archived_sessions")} {
			_ = filepath.WalkDir(sessionsDir, func(path string, entry fs.DirEntry, errWalk error) error {
				if errWalk != nil {
					if entry != nil && entry.IsDir() {
						return fs.SkipDir
					}
					return nil
				}
				name := entry.Name()
				if entry.IsDir() || !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
					return nil
				}
				if id := codexRolloutID(name); id != "" {
					if _, exists := codex[id]; !exists {
						codex[id] = path
					}
				}
				return nil
			})
		}
	}
	r.claudeIndex, r.codexIndex, r.indexBuiltAt = claude, codex, now
}

// codexRolloutID extracts the trailing UUID from rollout-<timestamp>-<uuid>.jsonl.
func codexRolloutID(name string) string {
	base := strings.TrimSuffix(name, ".jsonl")
	if len(base) < 36 {
		return ""
	}
	id := strings.ToLower(base[len(base)-36:])
	if !nativeIDPattern.MatchString(id) || strings.Count(id, "-") != 4 {
		return ""
	}
	return id
}

var cwdPattern = regexp.MustCompile(`"cwd"\s*:\s*("(?:[^"\\]|\\.)*")`)

// readClaudeSession reads only the head and tail of a Claude Code transcript.
// The cwd comes from the first record carrying one (else the last in the
// tail); the title is the newest custom title, else the newest AI title,
// else the newest summary.
func readClaudeSession(path string, size int64) fileInfo {
	file, errOpen := os.Open(path)
	if errOpen != nil {
		return fileInfo{}
	}
	defer func() { _ = file.Close() }()
	head := make([]byte, minInt64(size, headBytes))
	if _, errRead := io.ReadFull(file, head); errRead != nil {
		return fileInfo{}
	}
	var tail []byte
	tailStart := size - tailBytes
	if tailStart < int64(len(head)) {
		tailStart = int64(len(head))
	}
	if tailStart < size {
		tail = make([]byte, size-tailStart)
		if _, errRead := file.ReadAt(tail, tailStart); errRead != nil && errRead != io.EOF {
			tail = nil
		}
	}
	var info fileInfo
	if match := cwdPattern.FindSubmatch(head); match != nil {
		info.cwd = unquote(match[1])
	} else if matches := cwdPattern.FindAllSubmatch(tail, -1); len(matches) > 0 {
		info.cwd = unquote(matches[len(matches)-1][1])
	}

	var custom, ai, summary string
	scan := func(chunk []byte, dropFirst, dropLast bool) {
		lines := bytes.Split(chunk, []byte("\n"))
		if dropFirst && len(lines) > 0 {
			lines = lines[1:]
		}
		if dropLast && len(lines) > 0 {
			lines = lines[:len(lines)-1]
		}
		for _, line := range lines {
			if !bytes.Contains(line, []byte(`"custom-title"`)) && !bytes.Contains(line, []byte(`"ai-title"`)) && !bytes.Contains(line, []byte(`"summary"`)) {
				continue
			}
			var record struct {
				Type        string `json:"type"`
				CustomTitle string `json:"customTitle"`
				AITitle     string `json:"aiTitle"`
				Summary     string `json:"summary"`
			}
			if json.Unmarshal(line, &record) != nil {
				continue
			}
			switch record.Type {
			case "custom-title":
				if value := strings.TrimSpace(record.CustomTitle); value != "" {
					custom = value
				}
			case "ai-title":
				if value := strings.TrimSpace(record.AITitle); value != "" {
					ai = value
				}
			case "summary":
				if value := strings.TrimSpace(record.Summary); value != "" {
					summary = value
				}
			}
		}
	}
	scan(head, false, int64(len(head)) < size)
	if len(tail) > 0 {
		scan(tail, true, false)
	}
	for _, title := range []string{custom, ai, summary} {
		if title != "" {
			info.title = title
			break
		}
	}
	return info
}

// readCodexRollout reads the first record (session_meta) of a Codex rollout.
func readCodexRollout(path string) fileInfo {
	file, errOpen := os.Open(path)
	if errOpen != nil {
		return fileInfo{}
	}
	defer func() { _ = file.Close() }()
	reader := bufio.NewReaderSize(io.LimitReader(file, maxFirstLine), 64<<10)
	line, errRead := reader.ReadBytes('\n')
	if errRead != nil && errRead != io.EOF {
		return fileInfo{}
	}
	var record struct {
		Payload struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if json.Unmarshal(bytes.TrimSpace(line), &record) == nil && record.Payload.Cwd != "" {
		return fileInfo{cwd: record.Payload.Cwd}
	}
	if match := cwdPattern.FindSubmatch(line); match != nil {
		return fileInfo{cwd: unquote(match[1])}
	}
	return fileInfo{}
}

// codexTitleLocked reads thread names from <root>/session_index.jsonl
// (re-read only when the file changes; the newest line per id wins).
func (r *Resolver) codexTitleLocked(id string) string {
	for _, root := range r.roots {
		path := filepath.Join(root, "session_index.jsonl")
		stat, errStat := os.Stat(path)
		if errStat != nil {
			continue
		}
		key := statKey{modTime: stat.ModTime(), size: stat.Size()}
		if previous, ok := r.codexTitleKey[path]; !ok || previous != key {
			r.codexTitleKey[path] = key
			loadCodexIndex(path, stat.Size(), r.codexTitles)
		}
	}
	return r.codexTitles[id]
}

func loadCodexIndex(path string, size int64, titles map[string]string) {
	file, errOpen := os.Open(path)
	if errOpen != nil {
		return
	}
	defer func() { _ = file.Close() }()
	if size > maxIndexFileSize {
		if _, errSeek := file.Seek(size-maxIndexFileSize, io.SeekStart); errSeek != nil {
			return
		}
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var record struct {
			ID         string `json:"id"`
			ThreadName string `json:"thread_name"`
		}
		if json.Unmarshal(scanner.Bytes(), &record) != nil {
			continue
		}
		id := strings.ToLower(strings.TrimSpace(record.ID))
		if name := strings.TrimSpace(record.ThreadName); id != "" && name != "" {
			titles[id] = name
		}
	}
}

// t3ThreadLocked reads <auth-dir>/t3-threads.json, re-reading it only when its
// mtime or size changes. A missing or invalid file is treated as empty.
func (r *Resolver) t3ThreadLocked(id string) (t3Thread, bool) {
	if r.authDir == "" {
		return t3Thread{}, false
	}
	path := filepath.Join(r.authDir, hubsidecar.T3ThreadsFile)
	stat, errStat := os.Stat(path)
	if errStat != nil {
		r.t3Threads, r.t3Key, r.t3Loaded = nil, statKey{}, true
		return t3Thread{}, false
	}
	key := statKey{modTime: stat.ModTime(), size: stat.Size()}
	if !r.t3Loaded || key != r.t3Key {
		r.t3Key, r.t3Loaded = key, true
		r.t3Threads = nil
		var parsed t3File
		if data, errRead := os.ReadFile(path); errRead == nil && json.Unmarshal(data, &parsed) == nil {
			r.t3Threads = make(map[string]t3Thread, len(parsed.Threads))
			for nativeID, thread := range parsed.Threads {
				r.t3Threads[strings.ToLower(strings.TrimSpace(nativeID))] = thread
			}
		}
	}
	thread, ok := r.t3Threads[id]
	return thread, ok
}

func unquote(raw []byte) string {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// Roots returns the configured search roots (for diagnostics and tests).
func (r *Resolver) Roots() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	roots := append([]string(nil), r.roots...)
	sort.Strings(roots)
	return roots
}
