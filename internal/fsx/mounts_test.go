package fsx

import (
	"runtime"
	"strings"
	"testing"
)

func TestIsSystemMount(t *testing.T) {
	for _, p := range []string{"/proc", "/proc/sys", "/sys/fs/cgroup", "/dev/shm", "/etc/mirage", "/var/lib/mirage"} {
		if !isSystemMount(p) {
			t.Errorf("isSystemMount(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"/volumes/homes", "/homes", "/mnt/data", "/etcetera", "/development"} {
		if isSystemMount(p) {
			t.Errorf("isSystemMount(%q) = true, want false", p)
		}
	}
}

// TestDataMountsIsSafeEverywhere: the admin page calls this on every render, so
// it must degrade to nothing rather than fail where /proc is absent.
func TestDataMountsIsSafeEverywhere(t *testing.T) {
	got := DataMounts()
	t.Logf("DataMounts() = %v", got)
	if runtime.GOOS != "linux" {
		if got != nil {
			t.Errorf("DataMounts() = %v on %s, want nil", got, runtime.GOOS)
		}
		return
	}
	for _, p := range got {
		if !strings.HasPrefix(p, "/") {
			t.Errorf("DataMounts() returned a relative path %q", p)
		}
		if isSystemMount(p) {
			t.Errorf("DataMounts() returned system mount %q", p)
		}
	}
}
