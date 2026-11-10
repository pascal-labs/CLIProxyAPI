package sessionmeta

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	claudeID = "3f2a6c1e-8b7d-4e21-9a55-0c1d2e3f4a5b"
	codexID  = "01a1c6f6-0664-7130-8a1b-2c3d4e5f6a7b"
)

func TestNativeID(t *testing.T) {
	for key, want := range map[string][2]string{
		"claude:" + claudeID:                   {ClientClaudeCode, claudeID},
		"claude:" + strings.ToUpper(claudeID):  {ClientClaudeCode, claudeID},
		"claude:" + claudeID + ":agent:a1b2c3": {ClientClaudeCode, claudeID},
		"codex:" + codexID:                     {ClientCodex, codexID},
		"codex:" + codexID + ":agent:explorer": {ClientCodex, codexID},
		"codex:../../etc/passwd":               {ClientCodex, ""},
		"header:hub-session":                   {ClientOther, ""},
		"derived:0123abcd":                     {ClientOther, ""},
		"execution:abc":                        {ClientOther, ""},
		"no-prefix":                            {ClientOther, ""},
	} {
		client, id := NativeID(key)
		if client != want[0] || id != want[1] {
			t.Fatalf("NativeID(%q) = %q, %q; want %q, %q", key, client, id, want[0], want[1])
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o700); errMkdir != nil {
		t.Fatal(errMkdir)
	}
	if errWrite := os.WriteFile(path, []byte(content), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
}

func TestResolveClaudeCodexAndT3(t *testing.T) {
	root := t.TempDir()
	claudeRoot := filepath.Join(root, "claude")
	codexRoot := filepath.Join(root, "codex")
	authDir := filepath.Join(root, "auths")

	// A big transcript: the first records are huge, so cwd and the newest
	// title are only found in the tail.
	big := strings.Repeat("x", 200<<10)
	writeFile(t, filepath.Join(claudeRoot, "projects", "-work-alpha", claudeID+".jsonl"),
		`{"type":"ai-title","aiTitle":"Early title","sessionId":"`+claudeID+`"}`+"\n"+
			`{"type":"user","message":{"content":"`+big+`"}}`+"\n"+
			`{"type":"user","cwd":"/work/alpha","sessionId":"`+claudeID+`"}`+"\n"+
			`{"type":"custom-title","customTitle":"Renamed by user","sessionId":"`+claudeID+`"}`+"\n")
	writeFile(t, filepath.Join(codexRoot, "sessions", "2026", "11", "10", "rollout-2026-11-10T17-04-52-"+codexID+".jsonl"),
		`{"timestamp":"2026-11-10T23:04:52Z","type":"session_meta","payload":{"id":"`+codexID+`","cwd":"/work/beta","base_instructions":"..."}}`+"\n"+
			`{"type":"event_msg","payload":{}}`+"\n")
	writeFile(t, filepath.Join(codexRoot, "session_index.jsonl"),
		`{"id":"`+codexID+`","thread_name":"Old name","updated_at":"2026-11-10T23:05:00Z"}`+"\n"+
			`{"id":"`+codexID+`","thread_name":"Fix the poller","updated_at":"2026-11-10T23:06:00Z"}`+"\n")
	writeFile(t, filepath.Join(authDir, "t3-threads.json"), `{"generated_at":"2026-11-10T23:10:00Z","threads":{
		"`+strings.ToUpper(codexID)+`":{"t3_thread_id":"t3-1","title":"Quota dashboard","project":"beta","parent_t3_thread_id":null,"parent_title":null,"subagent":false},
		"`+claudeID+`":{"t3_thread_id":"t3-2","title":"Explore","project":"alpha","parent_t3_thread_id":"t3-1","parent_title":"Quota dashboard","subagent":true}}}`)

	resolver := NewResolver(nil)
	resolver.Configure([]string{claudeRoot, codexRoot}, authDir)

	claude := resolver.Resolve("claude:"+claudeID+":agent:sub1", time.Time{})
	if claude.Client != ClientClaudeCode || claude.Cwd != "/work/alpha" || claude.Project != "alpha" || claude.Title != "Renamed by user" {
		t.Fatalf("claude = %+v", claude)
	}
	if claude.T3ThreadID != "t3-2" || claude.T3ParentTitle != "Quota dashboard" || claude.Subagent == nil || !*claude.Subagent {
		t.Fatalf("claude t3 = %+v", claude)
	}
	codex := resolver.Resolve("codex:"+codexID, time.Time{})
	if codex.Client != ClientCodex || codex.Cwd != "/work/beta" || codex.Project != "beta" || codex.Title != "Fix the poller" {
		t.Fatalf("codex = %+v", codex)
	}
	if codex.T3Title != "Quota dashboard" || codex.Subagent == nil || *codex.Subagent || codex.T3ParentTitle != "" {
		t.Fatalf("codex t3 = %+v", codex)
	}

	unknown := resolver.Resolve("claude:aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", time.Time{})
	if unknown.Cwd != "" || unknown.Title != "" || unknown.Subagent != nil || unknown.Client != ClientClaudeCode {
		t.Fatalf("unknown = %+v", unknown)
	}
	other := resolver.Resolve("header:x", time.Time{})
	if other.Client != ClientOther || other.NativeID != "" {
		t.Fatalf("other = %+v", other)
	}

	// The T3 export is re-read when it changes; an invalid file reads as empty.
	writeFile(t, filepath.Join(authDir, "t3-threads.json"), `{not json`)
	future := time.Now().Add(time.Minute)
	if errTimes := os.Chtimes(filepath.Join(authDir, "t3-threads.json"), future, future); errTimes != nil {
		t.Fatal(errTimes)
	}
	if again := resolver.Resolve("codex:"+codexID, time.Time{}); again.T3ThreadID != "" || again.Subagent != nil || again.Cwd != "/work/beta" {
		t.Fatalf("after invalid export = %+v", again)
	}
	if errRemove := os.Remove(filepath.Join(authDir, "t3-threads.json")); errRemove != nil {
		t.Fatal(errRemove)
	}
	if again := resolver.Resolve("codex:"+codexID, time.Time{}); again.T3ThreadID != "" {
		t.Fatalf("after missing export = %+v", again)
	}
}

func TestResolveDeadlineSkipsFileReads(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "projects", "p", claudeID+".jsonl"), `{"cwd":"/work/alpha"}`+"\n")
	resolver := NewResolver(nil)
	resolver.Configure([]string{root}, "")
	if info := resolver.Resolve("claude:"+claudeID, time.Now().Add(-time.Second)); info.Cwd != "" {
		t.Fatalf("read files past the deadline: %+v", info)
	}
	if info := resolver.Resolve("claude:"+claudeID, time.Time{}); info.Cwd != "/work/alpha" {
		t.Fatalf("resolve = %+v", info)
	}
}

func TestConfigureExpandsHome(t *testing.T) {
	home, errHome := os.UserHomeDir()
	if errHome != nil {
		t.Skip("no home directory")
	}
	resolver := NewResolver(nil)
	resolver.Configure([]string{"~/.claude", "~/.claude", " /tmp/x "}, "")
	roots := resolver.Roots()
	if len(roots) != 2 || (roots[0] != filepath.Join(home, ".claude") && roots[1] != filepath.Join(home, ".claude")) {
		t.Fatalf("roots = %v", roots)
	}
	if len(DefaultRoots()) == 0 {
		t.Fatal("no default roots")
	}
}
