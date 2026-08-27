package index

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/poitch/mirage/internal/store"
)

// progressKey is where a scan's live progress is recorded.
//
// It goes in the database rather than in memory because the process asking is
// almost never the process scanning: an operator runs `mirage status` in a
// second container exec while the server works. Anything held in memory would
// be invisible to them, which is the situation this exists to fix.
const progressKey = "scan_progress"

// progressInterval bounds how often progress is written and logged. A scan of a
// large tree touches hundreds of thousands of files, so this cannot be per file.
const progressInterval = 5 * time.Second

// staleAfter is how long without an update before a scan is reported as
// interrupted rather than running.
//
// Generous on purpose. Progress is throttled to progressInterval, but the
// throttle only fires when the scan reaches a point that reports, and a single
// directory holding tens of thousands of files - a photo library's derivatives,
// say - takes a while to work through on spinning disks. Too tight a threshold
// declares a healthy scan dead, which is worse than noticing a real stall a
// minute late.
const staleAfter = 90 * time.Second

// Progress is a point-in-time view of a running or finished scan.
type Progress struct {
	User      string    `json:"user"`
	Files     int64     `json:"files"`
	Dirs      int64     `json:"dirs"`
	Bytes     int64     `json:"bytes"`
	Current   string    `json:"current"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Done      bool      `json:"done"`
}

// Elapsed returns how long the scan has been running, or took.
func (p Progress) Elapsed() time.Duration {
	end := p.UpdatedAt
	if end.Before(p.StartedAt) {
		end = time.Now()
	}
	return end.Sub(p.StartedAt)
}

// Rate returns entries indexed per second.
func (p Progress) Rate() float64 {
	seconds := p.Elapsed().Seconds()
	if seconds <= 0 {
		return 0
	}
	return float64(p.Files+p.Dirs) / seconds
}

// Stale reports whether the record looks abandoned rather than live, which is
// what a scan interrupted by a restart leaves behind.
func (p Progress) Stale() bool {
	return !p.Done && time.Since(p.UpdatedAt) > staleAfter
}

// ScanProgress returns the most recent scan progress, if any has been recorded.
func ScanProgress(ctx context.Context, db *store.DB) (Progress, bool, error) {
	raw, err := db.Setting(ctx, progressKey)
	if err != nil || raw == "" {
		return Progress{}, false, err
	}
	var p Progress
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return Progress{}, false, nil
	}
	return p, true, nil
}

// progressReporter throttles progress writes during a scan.
type progressReporter struct {
	scanner *Scanner
	current Progress
	lastAt  time.Time
}

func (s *Scanner) newProgress(username string) *progressReporter {
	now := time.Now()
	return &progressReporter{
		scanner: s,
		current: Progress{User: username, StartedAt: now, UpdatedAt: now},
	}
}

// update records progress, writing at most once per interval.
func (r *progressReporter) update(ctx context.Context, stats *Stats, currentPath string) {
	if time.Since(r.lastAt) < progressInterval {
		return
	}
	r.flush(ctx, stats, currentPath, false)
}

// flush writes progress unconditionally.
func (r *progressReporter) flush(ctx context.Context, stats *Stats, currentPath string, done bool) {
	r.lastAt = time.Now()
	r.current.Files = stats.Files
	r.current.Dirs = stats.Dirs
	r.current.Bytes = stats.Bytes
	r.current.Current = currentPath
	r.current.UpdatedAt = r.lastAt
	r.current.Done = done

	encoded, err := json.Marshal(r.current)
	if err != nil {
		return
	}
	if err := r.scanner.db.SetSetting(ctx, progressKey, string(encoded)); err != nil {
		r.scanner.log.Debug("could not record scan progress", "error", err)
	}
	if done {
		return
	}
	// Also logged, because `docker compose logs -f` is what an operator
	// actually has open while wondering whether anything is happening.
	r.scanner.log.Info("scanning",
		"user", r.current.User, "files", stats.Files, "dirs", stats.Dirs,
		"bytes", stats.Bytes, "at", currentPath,
		"rate", fmt.Sprintf("%.0f/s", r.current.Rate()),
		"elapsed", r.current.Elapsed().Round(time.Second))
}
