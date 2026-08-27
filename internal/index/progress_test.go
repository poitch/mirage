package index

import (
	"context"
	"testing"
	"time"
)

// TestProgressDistinguishesInterruptedFromRunning matters because the two look
// identical in the record: an unfinished scan. Progress stops being written the
// instant the process stops, so a stale timestamp is the only signal that a
// scan was interrupted rather than still working.
func TestProgressDistinguishesInterruptedFromRunning(t *testing.T) {
	now := time.Now()
	running := Progress{StartedAt: now.Add(-time.Minute), UpdatedAt: now}
	if running.Stale() {
		t.Error("a scan updated just now was reported as interrupted")
	}

	interrupted := Progress{StartedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)}
	if !interrupted.Stale() {
		t.Error("a scan last updated an hour ago was reported as still running")
	}

	// A finished scan is never stale, however long ago it ran.
	finished := Progress{StartedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour), Done: true}
	if finished.Stale() {
		t.Error("a completed scan was reported as interrupted")
	}
}

func TestProgressRate(t *testing.T) {
	p := Progress{
		Files:     900,
		Dirs:      100,
		StartedAt: time.Now().Add(-10 * time.Second),
		UpdatedAt: time.Now(),
	}
	if rate := p.Rate(); rate < 90 || rate > 110 {
		t.Errorf("Rate() = %.0f, want about 100/s", rate)
	}
	// A zero-length scan must not divide by zero.
	instant := Progress{Files: 5, StartedAt: time.Now(), UpdatedAt: time.Now()}
	if rate := instant.Rate(); rate < 0 {
		t.Errorf("Rate() = %v for an instantaneous scan", rate)
	}
}

// TestScanRecordsProgress checks the whole path: a scan writes progress, and
// another reader can see it afterwards.
func TestScanRecordsProgress(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, ok, err := ScanProgress(ctx, f.db); err != nil {
		t.Fatalf("ScanProgress before any scan: %v", err)
	} else if ok {
		t.Error("progress was reported before any scan ran")
	}

	f.scan(t)

	p, ok, err := ScanProgress(ctx, f.db)
	if err != nil {
		t.Fatalf("ScanProgress: %v", err)
	}
	if !ok {
		t.Fatal("no progress recorded after a scan")
	}
	if !p.Done {
		t.Error("progress does not report the scan as finished")
	}
	if p.Files != 3 {
		t.Errorf("Files = %d, want 3", p.Files)
	}
	if p.User != "alice" {
		t.Errorf("User = %q, want alice", p.User)
	}
}
