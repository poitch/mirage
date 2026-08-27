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

// Scan states. A scan records what happened to it rather than leaving it to be
// inferred from how long ago it last spoke.
//
// Inferring it from a timestamp cannot be made reliable: the gap between
// updates depends on what the scan is walking through, so any threshold is
// either short enough to libel a healthy scan working through a huge directory,
// or long enough to be useless. A process that dies cannot record its own
// death, but the next process to start can observe that a scan was still marked
// running when nothing was running, which is exact rather than probabilistic.
const (
	StateRunning     = "running"
	StateDone        = "done"
	StateFailed      = "failed"
	StateInterrupted = "interrupted"
)

// Progress is a point-in-time view of a scan.
type Progress struct {
	User    string `json:"user"`
	State   string `json:"state"`
	Files   int64  `json:"files"`
	Dirs    int64  `json:"dirs"`
	Bytes   int64  `json:"bytes"`
	Current string `json:"current"`
	Error   string `json:"error,omitempty"`
	// Stamp is the scan generation. Keeping it lets an interrupted scan be
	// resumed under the same generation, so directories it already finished
	// can be recognised and skipped rather than walked again.
	Stamp int64 `json:"stamp"`
	// DetectRenames records the decision made when the scan began, so a resume
	// does not switch it on merely because the partial index is now non-empty.
	DetectRenames bool      `json:"detect_renames"`
	StartedAt     time.Time `json:"started_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Running reports whether a scan is in progress.
func (p Progress) Running() bool { return p.State == StateRunning }

// Elapsed returns how long the scan has been running, or took.
func (p Progress) Elapsed() time.Duration {
	if p.Running() {
		return time.Since(p.StartedAt)
	}
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

// Resumable reports whether a previous scan can be picked up rather than
// started again.
//
// Any unfinished scan qualifies, not only one already reconciled as
// interrupted. A scan killed outright leaves its record saying "running",
// because a process that is killed writes nothing more - and whether some
// later startup has relabelled that yet is beside the point. If a new scan of
// this account is beginning, whatever came before it is over.
func (p Progress) Resumable(username string) bool {
	return p.User == username && p.Stamp > 0 &&
		(p.State == StateRunning || p.State == StateInterrupted)
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

func (s *Scanner) newProgress(ctx context.Context, username string, stamp int64, detectRenames bool) *progressReporter {
	now := time.Now()
	r := &progressReporter{
		scanner: s,
		current: Progress{
			User: username, State: StateRunning, Stamp: stamp, DetectRenames: detectRenames,
			StartedAt: now, UpdatedAt: now,
		},
	}
	// Written immediately, so that a process dying at any point from here on
	// leaves a record saying a scan was running.
	r.write(ctx)
	return r
}

// finish records how the scan ended.
func (r *progressReporter) finish(ctx context.Context, stats *Stats, err error) {
	r.current.Files, r.current.Dirs, r.current.Bytes = stats.Files, stats.Dirs, stats.Bytes
	r.current.Current = ""
	r.current.UpdatedAt = time.Now()
	if err != nil {
		r.current.State = StateFailed
		r.current.Error = err.Error()
	} else {
		r.current.State = StateDone
	}
	r.write(ctx)
}

// ReconcileInterrupted marks a scan that was still recorded as running when the
// process stopped.
//
// Called once at startup, before any scan begins. Nothing can be scanning at
// that moment, so a record still saying "running" was left by a process that
// died - which is the one thing a dead process could not write for itself.
func ReconcileInterrupted(ctx context.Context, db *store.DB) (Progress, bool, error) {
	p, ok, err := ScanProgress(ctx, db)
	if err != nil || !ok || !p.Running() {
		return Progress{}, false, err
	}
	p.State = StateInterrupted
	encoded, err := json.Marshal(p)
	if err != nil {
		return Progress{}, false, err
	}
	if err := db.SetSetting(ctx, progressKey, string(encoded)); err != nil {
		return Progress{}, false, err
	}
	return p, true, nil
}

// update records progress, writing at most once per interval.
func (r *progressReporter) update(ctx context.Context, stats *Stats, currentPath string) {
	if time.Since(r.lastAt) < progressInterval {
		return
	}
	r.flush(ctx, stats, currentPath)
}

// flush writes progress unconditionally.
func (r *progressReporter) flush(ctx context.Context, stats *Stats, currentPath string) {
	r.lastAt = time.Now()
	r.current.Files = stats.Files
	r.current.Dirs = stats.Dirs
	r.current.Bytes = stats.Bytes
	r.current.Current = currentPath
	r.current.UpdatedAt = r.lastAt
	r.write(ctx)

	// Also logged, because `docker compose logs -f` is what an operator
	// actually has open while wondering whether anything is happening.
	r.scanner.log.Info("scanning",
		"user", r.current.User, "files", stats.Files, "dirs", stats.Dirs,
		"bytes", stats.Bytes, "at", currentPath,
		"rate", fmt.Sprintf("%.0f/s", r.current.Rate()),
		"elapsed", r.current.Elapsed().Round(time.Second))
}

func (r *progressReporter) write(ctx context.Context) {
	encoded, err := json.Marshal(r.current)
	if err != nil {
		return
	}
	if err := r.scanner.db.SetSetting(ctx, progressKey, string(encoded)); err != nil {
		r.scanner.log.Debug("could not record scan progress", "error", err)
	}
}
