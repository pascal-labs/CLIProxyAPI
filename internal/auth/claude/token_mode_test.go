package claude

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Audit 1 C-3-3: login files must be 0600 even when the process umask is loose (the hub unit had UMask=0002),
// both when the file is new and when an existing 0664 file is rewritten on token refresh.
func TestSaveTokenToFileIsOwnerOnly(t *testing.T) {
	old := syscall.Umask(0o002)
	defer syscall.Umask(old)
	dir := t.TempDir()
	p := filepath.Join(dir, "claude-test.json")
	ts := &ClaudeTokenStorage{}
	if err := ts.SaveTokenToFile(p); err != nil {
		t.Fatalf("save new: %v", err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("new file mode %o, want 600", st.Mode().Perm())
	}
	if err := os.Chmod(p, 0o664); err != nil {
		t.Fatal(err)
	}
	if err := ts.SaveTokenToFile(p); err != nil {
		t.Fatalf("save existing: %v", err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("rewritten file mode %o, want 600", st.Mode().Perm())
	}
}
