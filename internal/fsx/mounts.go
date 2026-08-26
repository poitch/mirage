package fsx

import (
	"bufio"
	"os"
	"sort"
	"strings"
)

// systemMounts are paths whose contents are the container's own machinery
// rather than anywhere a user's files could sensibly live.
var systemMounts = []string{
	"/proc", "/sys", "/dev", "/run", "/etc", "/tmp",
	"/var/lib/mirage", "/var/log", "/var/run",
}

// DataMounts lists paths mounted into the container that could plausibly hold
// user files.
//
// Nothing inside a container can discover the host path a mount came from, so
// the admin page cannot honestly say "/volume1/homes appears here as
// /volumes/homes" - the volume number varies by NAS, and guessing produces
// instructions that are confidently wrong. What it can do is report which
// paths were mounted in, which is the part the operator actually needs.
//
// Returns nil where /proc is unavailable, such as on macOS during development.
func DataMounts() []string {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	defer f.Close()

	seen := make(map[string]bool)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		// mountinfo columns: ID parentID major:minor root mountpoint ...
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		point := fields[4]
		if point == "/" || seen[point] || isSystemMount(point) {
			continue
		}
		info, err := os.Stat(point)
		if err != nil || !info.IsDir() {
			continue
		}
		seen[point] = true
	}
	if scanner.Err() != nil {
		return nil
	}

	out := make([]string, 0, len(seen))
	for point := range seen {
		out = append(out, point)
	}
	sort.Strings(out)
	return out
}

func isSystemMount(point string) bool {
	for _, prefix := range systemMounts {
		if point == prefix || strings.HasPrefix(point, prefix+"/") {
			return true
		}
	}
	return false
}
