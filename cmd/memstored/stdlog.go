package main

import (
	"context"
	"io"
	"log"
	"log/slog"
	"os"
	"strings"
)

// stdlogSink adapts the standard log package's io.Writer sink to slog, so a
// line written with log.Printf arrives as a leveled record instead of a bare
// string. Without it those lines reach Loki as detected_level=unknown and the
// error alert never sees them.
//
// It is a bridge, not a destination: packages should log through a slog logger
// of their own. It exists because memstored drives httpapi, pgstore and the
// root package, all of which still use the standard log package, and leaving
// their faults invisible until every one is converted is worse than
// classifying them here.
type stdlogSink struct{ logger *slog.Logger }

// Write emits one record per call at the level classifyStdlog assigns. It
// reports the full length written regardless: a short count makes the log
// package treat the line as an error, and there is nothing useful it could do
// about a logging failure anyway.
func (s stdlogSink) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	s.logger.Log(context.Background(), classifyStdlog(msg), msg)
	return len(p), nil
}

// errorWords mark a line that reports something going wrong. They are matched
// case-insensitively anywhere in the message because the standard log calls
// being classified have no structure to read -- they are formatted sentences,
// and the verb is the only signal available.
var errorWords = []string{"failed", "failure", "error", "cannot", "unable", "refused", "panic"}

// warnWords mark a line that is worth attention but is not a fault.
var warnWords = []string{"warning", "deprecated"}

// classifyStdlog guesses the level of a standard-log line from its text.
//
// The guess is the cost of the bridge: the Printf interface carries no level,
// so either every line is one level or the text decides. A line that announces
// itself as a warning is taken at its word even when it also mentions a
// failure; otherwise an error word wins, and anything else is info. Converting
// a package to slog replaces the guess with the level its author chose.
func classifyStdlog(msg string) slog.Level {
	lower := strings.ToLower(msg)
	for _, w := range warnWords {
		if strings.HasPrefix(lower, w) {
			return slog.LevelWarn
		}
	}
	for _, w := range errorWords {
		if strings.Contains(lower, w) {
			return slog.LevelError
		}
	}
	for _, w := range warnWords {
		if strings.Contains(lower, w) {
			return slog.LevelWarn
		}
	}
	return slog.LevelInfo
}

// bridgeStdlog points the standard log package at logger and returns a
// function restoring the previous sink and flags. Flags go to zero because
// slog stamps its own time: leaving them on puts a second, differently
// formatted timestamp inside the msg field.
func bridgeStdlog(logger *slog.Logger) (restore func()) {
	prevFlags := log.Flags()
	prevOutput := log.Writer()
	log.SetFlags(0)
	log.SetOutput(stdlogSink{logger: logger})
	return func() {
		log.SetFlags(prevFlags)
		log.SetOutput(prevOutput)
	}
}

// stderrOr returns w, or os.Stderr when w is nil, for the daemon's log
// destination. run() is handed a writer by its caller; main passes os.Stderr
// and tests pass io.Discard.
func stderrOr(w io.Writer) io.Writer {
	if w == nil {
		return os.Stderr
	}
	return w
}
