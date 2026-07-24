package logger

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClearLogFilesTruncatesActiveAndRemovesRotations(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "app-2026-07-24.log")
	current := filepath.Join(dir, "app.log")
	old := filepath.Join(dir, "app-2026-07-23.log")
	compressed := filepath.Join(dir, "app-2026-07-22.log.gz")
	unrelated := filepath.Join(dir, "other.log")

	for path, content := range map[string]string{
		active:     "active-old\n",
		old:        "rotated-old\n",
		compressed: "compressed-old\n",
		unrelated:  "keep\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	if err := os.Link(active, current); err != nil {
		t.Fatalf("link active log: %v", err)
	}

	if err := clearLogFiles(current); err != nil {
		t.Fatalf("clearLogFiles: %v", err)
	}
	for _, path := range []string{current, active} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read active path %s: %v", path, err)
		}
		if len(data) != 0 {
			t.Fatalf("active path %s still contains %q", path, data)
		}
	}
	for _, path := range []string{old, compressed} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("rotated log %s was not removed: %v", path, err)
		}
	}
	if data, err := os.ReadFile(unrelated); err != nil || string(data) != "keep\n" {
		t.Fatalf("unrelated file changed: data=%q err=%v", data, err)
	}

	file, err := os.OpenFile(current, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open active log after clear: %v", err)
	}
	if _, err := file.WriteString("new\n"); err != nil {
		file.Close()
		t.Fatalf("write active log after clear: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close active log: %v", err)
	}
	if data, err := os.ReadFile(active); err != nil || string(data) != "new\n" {
		t.Fatalf("active writer target after clear: data=%q err=%v", data, err)
	}
}

func TestBroadcasterClearBuffered(t *testing.T) {
	broadcaster := NewBroadcaster(4)
	ch := broadcaster.Subscribe()
	defer broadcaster.Unsubscribe(ch)

	broadcaster.Broadcast(LogEntry{Message: "old-1"})
	broadcaster.Broadcast(LogEntry{Message: "old-2"})
	if len(ch) != 2 {
		t.Fatalf("buffered entries before clear = %d, want 2", len(ch))
	}
	broadcaster.ClearBuffered()
	if len(ch) != 0 {
		t.Fatalf("buffered entries after clear = %d, want 0", len(ch))
	}
	broadcaster.Broadcast(LogEntry{Message: "new"})
	select {
	case entry := <-ch:
		if entry.Message != "new" {
			t.Fatalf("entry after clear = %q, want new", entry.Message)
		}
	default:
		t.Fatal("new entry was not delivered after clear")
	}
}
