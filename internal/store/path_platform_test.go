package store

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPlatformPathHelpersAndSQLiteURI(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "测试 用户", "My Project Space")
	uri := sqliteFileURI(filepath.Join(dir, "recalldock.db"), "mode=ro&_pragma=query_only(1)")
	if !strings.HasPrefix(uri, "file:///") || !strings.Contains(uri, "mode=ro") {
		t.Fatalf("unexpected sqlite file URI: %s", uri)
	}
	if strings.Contains(uri, `\`) {
		t.Fatalf("sqlite file URI must not contain backslashes: %s", uri)
	}

	if runtime.GOOS == "windows" {
		t.Setenv("LOCALAPPDATA", `C:\Users\测试 用户\AppData\Local`)
		got := DefaultDataDirForHome(`C:\Users\测试 用户`)
		want := `C:\Users\测试 用户\AppData\Local\RecallDock`
		if !samePath(got, want) {
			t.Fatalf("DefaultDataDirForHome() = %q, want %q", got, want)
		}
		if err := rejectUNCPath(`\\server\share\proj`); err == nil {
			t.Fatal("expected UNC path rejection on Windows")
		}
		if !samePath(`c:\Users\Test\Repo`, `C:\Users\test\repo`) {
			t.Fatal("expected case-insensitive samePath on Windows")
		}
		if norm := normalizePath(`c:\users\demo`); !strings.HasPrefix(norm, `C:\`) {
			t.Fatalf("expected uppercased drive letter, got %q", norm)
		}
		parsed, ok := parseFileSourceRefPath(`file:///C:/Users/My Space/doc.txt`)
		if !ok || !samePath(parsed, `C:\Users\My Space\doc.txt`) {
			t.Fatalf("parseFileSourceRefPath failed on Windows URI: %q %v", parsed, ok)
		}
	} else {
		got := DefaultDataDirForHome("/home/tester")
		if got != "/home/tester/.recalldock" {
			t.Fatalf("DefaultDataDirForHome() = %q, want /home/tester/.recalldock", got)
		}
		parsed, ok := parseFileSourceRefPath("file:///tmp/My Space/测试文件.txt")
		if !ok || parsed != "/tmp/My Space/测试文件.txt" {
			t.Fatalf("parseFileSourceRefPath failed on Unix URI: %q %v", parsed, ok)
		}
	}
}
