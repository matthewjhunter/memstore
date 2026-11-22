package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/matthewjhunter/memstore"
)

// parseRetention reads the transcript retention setting. Empty or zero means
// keep everything, which is the default. The value is a Go duration, and
// since nobody thinks about retention in hours, a whole number of days may be
// written with a d suffix ("90d").
func parseRetention(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" || v == "0" {
		return 0, nil
	}
	if days, ok := strings.CutSuffix(v, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("transcript retention %q: want a duration such as 90d or 2160h", v)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("transcript retention %q: want a duration such as 90d or 2160h", v)
	}
	return d, nil
}

// retentionLabel is the setting as the startup log line states it.
func retentionLabel(keep time.Duration) string {
	if keep == 0 {
		return "forever"
	}
	return keep.String()
}

// archiveIndex is what the sweep needs of the session store.
type archiveIndex interface {
	ExpiredArchives(ctx context.Context, cutoff time.Time) ([]memstore.ExpiredArchive, error)
	MarkArchiveRemoved(ctx context.Context, id int64) error
}

// transcriptFiles is what the sweep needs of the raw store.
type transcriptFiles interface {
	Remove(userID int64, sessionID string) error
}

// sweepTranscripts removes the raw transcript of every session that ended
// before cutoff and returns how many it removed. The file goes first and the
// index row is marked after: if the mark fails the next sweep finds the row
// again, removes nothing, and marks it. The other order could record a file
// as gone that is still on disk.
func sweepTranscripts(ctx context.Context, idx archiveIndex, files transcriptFiles, cutoff time.Time) (int, error) {
	expired, err := idx.ExpiredArchives(ctx, cutoff)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, a := range expired {
		if err := files.Remove(a.UserID, a.SessionID); err != nil {
			return removed, fmt.Errorf("removing transcript of session %s: %w", a.SessionID, err)
		}
		if err := idx.MarkArchiveRemoved(ctx, a.ID); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// runTranscriptRetention sweeps once now and then daily until ctx ends.
func runTranscriptRetention(ctx context.Context, logger *slog.Logger, idx archiveIndex, files transcriptFiles, keep time.Duration) {
	tick := time.NewTicker(24 * time.Hour)
	defer tick.Stop()
	for {
		n, err := sweepTranscripts(ctx, idx, files, time.Now().Add(-keep))
		switch {
		case err != nil:
			logger.Error("transcript retention sweep failed", "removed", n, "err", err)
		case n > 0:
			logger.Info("transcript retention sweep", "removed", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
