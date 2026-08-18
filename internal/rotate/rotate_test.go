package rotate

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func readGz(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("%s is not valid gzip: %v", path, err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("decompress %s: %v", path, err)
	}
	return string(b)
}

func TestRotatesAndGzips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dup.log")
	w, err := Open(Config{Path: path, MaxBytes: 32, Keep: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	for i := range 5 {
		if _, err := fmt.Fprintf(w, "line %d aaaaaaaaaaaaaaaaaaaa\n", i); err != nil {
			t.Fatal(err)
		}
	}

	if got := read(t, path); !strings.Contains(got, "line 4") {
		t.Errorf("live file does not hold the newest line: %q", got)
	}
	if got := readGz(t, path+".1.gz"); !strings.Contains(got, "line 3") {
		t.Errorf(".1.gz should hold the previous line, got %q", got)
	}
	if _, err := os.Stat(path + ".2.gz"); err != nil {
		t.Errorf("expected a second archive: %v", err)
	}
}

// A record must never be split across two files, or a JSONL reader sees a
// truncated line at every rotation boundary.
func TestRecordsAreNeverSplit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	w, err := Open(Config{Path: path, MaxBytes: 40, Keep: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	for i := range 8 {
		if _, err := fmt.Fprintf(w, "{\"n\":%d,\"pad\":\"xxxxxxxxxxxxxxxxxx\"}\n", i); err != nil {
			t.Fatal(err)
		}
	}
	_ = w.Close()

	matches, _ := filepath.Glob(path + "*")
	for _, m := range matches {
		body := read(t, m)
		if strings.HasSuffix(m, ".gz") {
			body = readGz(t, m)
		}
		for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
			if line == "" {
				continue
			}
			if !strings.HasPrefix(line, "{") || !strings.HasSuffix(line, "}") {
				t.Errorf("%s holds a split record: %q", m, line)
			}
		}
	}
}

func TestKeepIsRespected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dup.log")
	w, err := Open(Config{Path: path, MaxBytes: 16, Keep: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	for range 12 {
		if _, err := w.Write([]byte("0123456789abcdef\n")); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := os.Stat(path + ".3.gz"); !os.IsNotExist(err) {
		t.Error("kept more archives than Keep allows")
	}
	for i := 1; i <= 2; i++ {
		if _, err := os.Stat(fmt.Sprintf("%s.%d.gz", path, i)); err != nil {
			t.Errorf("archive %d missing: %v", i, err)
		}
	}
}

func TestKeepZeroDiscards(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dup.log")
	w, err := Open(Config{Path: path, MaxBytes: 16, Keep: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	for range 4 {
		if _, err := w.Write([]byte("0123456789abcdef\n")); err != nil {
			t.Fatal(err)
		}
	}
	if matches, _ := filepath.Glob(path + ".*"); len(matches) != 0 {
		t.Errorf("Keep 0 left archives behind: %v", matches)
	}
}

// Reopening must append rather than truncate, or a restart loses the file.
func TestReopenAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dup.log")
	for range 2 {
		w, err := Open(Config{Path: path, MaxBytes: 1 << 20, Keep: 2})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("run\n")); err != nil {
			t.Fatal(err)
		}
		_ = w.Close()
	}
	if got := read(t, path); strings.Count(got, "run") != 2 {
		t.Errorf("reopen truncated the file: %q", got)
	}
}

func TestConcurrentWritesDoNotInterleave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dup.log")
	w, err := Open(Config{Path: path, MaxBytes: 1 << 20, Keep: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, _ = fmt.Fprintf(w, "line-%02d\n", n)
		}(i)
	}
	wg.Wait()

	for _, line := range strings.Split(strings.TrimRight(read(t, path), "\n"), "\n") {
		if len(line) != 7 {
			t.Errorf("interleaved write: %q", line)
		}
	}
}

func TestOpenRejectsRelativePath(t *testing.T) {
	if _, err := Open(Config{Path: "dup.log"}); err == nil {
		t.Error("a relative path was accepted")
	}
}

func TestWriteAfterCloseErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dup.log")
	w, err := Open(Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	if _, err := w.Write([]byte("x")); err == nil {
		t.Error("write after close succeeded")
	}
	if err := w.Close(); err != nil {
		t.Errorf("second Close should be a no-op, got %v", err)
	}
}
