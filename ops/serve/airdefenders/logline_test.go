// Log-injection regression — autoSubmitScore logs the player-supplied name
// with log.Printf. A name containing a raw newline plus a line that looks
// like a log entry (e.g. "\nINFO forged: admin promoted") must not be able
// to masquerade as a second, independent log line. %q escapes the newline
// so the whole value stays on one line.
package main

import (
	"bytes"
	"log"
	"path/filepath"
	"strings"
	"testing"
)

func TestAutoSubmitScore_NameWithNewlineCannotForgeLogLine(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "scores.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	bus := newEventBus()
	h := newHub(bus, db, nil, nil)
	r := h.room

	var buf bytes.Buffer
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})

	const forged = "ALICE\nINFO forged: admin promoted"
	r.autoSubmitScore(forged, 100, 1, "ALICE+1", "ALICE,BOB")

	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("name with embedded newline produced %d log lines, want 1 (injection not prevented); output:\n%s", len(lines), out)
	}
	for _, line := range lines {
		if strings.Contains(line, "INFO forged") && !strings.Contains(line, `\n`) {
			t.Fatalf("forged line appeared unescaped as its own log entry: %q", line)
		}
	}
	if !strings.Contains(out, `"ALICE\nINFO forged: admin promoted"`) {
		t.Fatalf("expected %%q-escaped name in log output, got: %s", out)
	}
}
