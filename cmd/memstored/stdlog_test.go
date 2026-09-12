package main

import (
	"bytes"
	"log"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/infodancer/logging"
)

func TestClassifyStdlog(t *testing.T) {
	tests := []struct {
		msg  string
		want slog.Level
	}{
		{"embed queue: SetFactVectors id=12: connection refused", slog.LevelError},
		{"session store init failed: no such table", slog.LevelError},
		{"embed queue: MarkEmbedFailed id=3: boom", slog.LevelError},
		{"oauth user provisioning: cannot resolve subject", slog.LevelError},
		{"WARNING: reranker NormalizeScores is off", slog.LevelWarn},
		{"WARNING: memstored listening WITHOUT TLS", slog.LevelWarn},
		// A warning that mentions a failure is still a warning: the line's own
		// marker is a stronger signal than a keyword anywhere in its text.
		{"WARNING: the last attempt failed, retrying", slog.LevelWarn},
		{"MEMSTORE_PG is deprecated, use MEMSTORE_PG_SECRET", slog.LevelWarn},
		{"embed queue: embedded 12/32 facts", slog.LevelInfo},
		{"task selector: rerank", slog.LevelInfo},
		{"", slog.LevelInfo},
	}
	for _, tt := range tests {
		t.Run(tt.msg, func(t *testing.T) {
			if got := classifyStdlog(tt.msg); got != tt.want {
				t.Errorf("classifyStdlog(%q) = %v, want %v", tt.msg, got, tt.want)
			}
		})
	}
}

func TestStdlogSinkEmitsLeveledRecords(t *testing.T) {
	var buf bytes.Buffer
	sink := stdlogSink{logger: logging.NewLoggerTo(&buf, "info")}

	n, err := sink.Write([]byte("embed queue: NeedingEmbedding: connection refused\n"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if want := len("embed queue: NeedingEmbedding: connection refused\n"); n != want {
		t.Errorf("Write returned %d, want %d (a short count makes the log package report an error)", n, want)
	}

	out := buf.String()
	if !strings.Contains(out, "level=error") {
		t.Errorf("expected level=error, got: %s", out)
	}
	if !strings.Contains(out, `msg="embed queue: NeedingEmbedding: connection refused"`) {
		t.Errorf("expected the message verbatim without its newline, got: %s", out)
	}
	if strings.Count(strings.TrimRight(out, "\n"), "\n") != 0 {
		t.Errorf("expected a single record, got: %s", out)
	}
}

// The point of the bridge: lines that packages still write through the standard
// log package come out of the daemon leveled, not as detected_level=unknown.
func TestBridgeStdlogRoutesPackageLogOutput(t *testing.T) {
	var buf bytes.Buffer
	restore := bridgeStdlog(logging.NewLoggerTo(&buf, "info"))
	defer restore()

	log.Printf("embed queue: quarantining id=%d (permanent embed failure): %v", 7, "boom")

	out := buf.String()
	if !strings.Contains(out, "level=error") {
		t.Errorf("expected level=error, got: %s", out)
	}
	if !strings.Contains(out, "id=7") {
		t.Errorf("expected the formatted message, got: %s", out)
	}
	// The stdlib prefix would otherwise duplicate slog's own timestamp.
	if strings.Contains(out, "msg=\"20") {
		t.Errorf("expected the stdlib date prefix to be off, got: %s", out)
	}
}

func TestBridgeStdlogRestores(t *testing.T) {
	var before bytes.Buffer
	log.SetOutput(&before)
	log.SetFlags(log.LstdFlags)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})

	var bridged bytes.Buffer
	restore := bridgeStdlog(logging.NewLoggerTo(&bridged, "info"))
	restore()

	log.Printf("direct")
	if bridged.Len() != 0 {
		t.Errorf("bridge still active after restore: %s", bridged.String())
	}
	if !strings.Contains(before.String(), "direct") {
		t.Errorf("expected the restored sink to receive the line, got: %s", before.String())
	}
	if log.Flags() != log.LstdFlags {
		t.Errorf("expected the restored flags, got %d", log.Flags())
	}
}
