package server

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

// TestRescanIntervalIsAGapNotASchedule documents the behaviour the timer gives
// and a ticker does not.
//
// A ticker fires on a fixed schedule, so a scan lasting longer than the
// interval leaves a tick already waiting the moment it finishes, and the next
// scan starts immediately. On a share where a pass takes far longer than any
// sensible interval that is continuous scanning with no idle time at all -
// which looks, from the outside, exactly like a scan that never ends.
func TestRescanIntervalIsAGapNotASchedule(t *testing.T) {
	const interval = 40 * time.Millisecond
	const scanTime = 100 * time.Millisecond // deliberately longer than the interval

	// What a ticker does: ticks accumulate during the scan.
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	time.Sleep(scanTime)
	select {
	case <-ticker.C:
		// Expected: a tick was already waiting before the scan "finished".
	default:
		t.Fatal("a ticker did not queue a tick during a long scan; the premise is wrong")
	}

	// What the timer does: reset after the scan, so the wait starts fresh.
	timer := time.NewTimer(interval)
	defer timer.Stop()
	<-timer.C
	time.Sleep(scanTime)
	timer.Reset(interval)
	select {
	case <-timer.C:
		t.Fatal("the timer fired immediately after a long scan; the gap was not honoured")
	default:
		// Expected: the next scan waits a full interval.
	}
}

// TestScanningCannotBecomeContinuous is the property that matters on a large
// share: a pass that takes longer than the gap configured after it must not
// cause the next to begin immediately. Spinning disks that never rest, for a
// share that is mostly archives, is a real cost and not a theoretical one.
func TestScanningCannotBecomeContinuous(t *testing.T) {
	s := &Server{log: discardLogger()}

	tests := []struct {
		name     string
		interval time.Duration
		took     time.Duration
		wantMin  time.Duration
	}{
		// Comfortably inside the interval: the configured gap stands.
		{"quick pass on a small share", 5 * time.Minute, 2 * time.Second, 5 * time.Minute},
		// Right at the edge of the budget: still the configured gap.
		{"pass using a quarter of the budget", 5 * time.Minute, 75 * time.Second, 5 * time.Minute},
		// Longer than the interval: the gap has to widen, or the next pass
		// starts the instant this one ends.
		{"pass longer than the interval", 5 * time.Minute, 10 * time.Minute, 30 * time.Minute},
		// A full walk of a very large share.
		{"forty minute walk, six hour interval", 6 * time.Hour, 40 * time.Minute, 6 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := s.nextGap("test pass", tc.interval, tc.took)
			if got < tc.wantMin {
				t.Errorf("gap = %v, want at least %v", got, tc.wantMin)
			}
			// Whatever the numbers, the share of time spent scanning stays
			// bounded - which is the whole point.
			duty := float64(tc.took) / float64(tc.took+got)
			if duty > maxDutyCycle+0.01 {
				t.Errorf("would scan %.0f%% of the time, want at most %.0f%%",
					duty*100, maxDutyCycle*100)
			}
		})
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
