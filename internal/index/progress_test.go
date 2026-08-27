package index

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestInterruptionIsRecordedNotInferred is the property that matters: a scan
// that stopped is identified by what was written down, not by how long ago it
// last spoke. A busy scan and a dead one are indistinguishable by timing - the
// gap between updates depends entirely on what is being walked - so any
// threshold either libels a healthy scan or misses a dead one.
func TestInterruptionIsRecordedNotInferred(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	// A scan that has been silent for an hour but is genuinely still running.
	writeProgress(t, f, Progress{
		User: "alice", State: StateRunning, Files: 500_000,
		Current:   "04.Archives/huge",
		StartedAt: time.Now().Add(-2 * time.Hour),
		UpdatedAt: time.Now().Add(-time.Hour),
	})
	p, _, err := ScanProgress(ctx, f.db)
	if err != nil {
		t.Fatalf("ScanProgress: %v", err)
	}
	if !p.Running() {
		t.Error("a long-silent but running scan was not reported as running")
	}

	// Only a restart settles it, because only then is it certain that nothing
	// is scanning.
	was, ok, err := ReconcileInterrupted(ctx, f.db)
	if err != nil {
		t.Fatalf("ReconcileInterrupted: %v", err)
	}
	if !ok {
		t.Fatal("a scan left marked running was not reconciled")
	}
	if was.Files != 500_000 || was.Current != "04.Archives/huge" {
		t.Errorf("reconciliation lost how far the scan got: %+v", was)
	}

	p, _, _ = ScanProgress(ctx, f.db)
	if p.State != StateInterrupted {
		t.Errorf("State = %q, want %q", p.State, StateInterrupted)
	}

	// Reconciling again must not disturb a settled record.
	if _, ok, _ := ReconcileInterrupted(ctx, f.db); ok {
		t.Error("an already-reconciled scan was reconciled a second time")
	}
}

// TestFinishedScanIsNeverReconciled: a completed scan stays completed no matter
// how long ago it ran or how many restarts follow.
func TestFinishedScanIsNeverReconciled(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	writeProgress(t, f, Progress{
		User: "alice", State: StateDone, Files: 10,
		StartedAt: time.Now().Add(-72 * time.Hour),
		UpdatedAt: time.Now().Add(-72 * time.Hour),
	})
	if _, ok, err := ReconcileInterrupted(ctx, f.db); err != nil || ok {
		t.Errorf("a finished scan was reconciled as interrupted (ok=%v, err=%v)", ok, err)
	}
}

func writeProgress(t *testing.T, f *fixture, p Progress) {
	t.Helper()
	encoded, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := f.db.SetSetting(context.Background(), progressKey, string(encoded)); err != nil {
		t.Fatalf("SetSetting: %v", err)
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
	if p.State != StateDone {
		t.Errorf("State = %q, want %q", p.State, StateDone)
	}
	if p.Files != 3 {
		t.Errorf("Files = %d, want 3", p.Files)
	}
	if p.User != "alice" {
		t.Errorf("User = %q, want alice", p.User)
	}
}
