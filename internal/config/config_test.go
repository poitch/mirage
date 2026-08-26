package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mirage.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

const validConfig = `
server:
  listen: ":9000"
  external_url: "https://nas.example.com/"
database:
  path: /data/mirage.db
storage:
  file_mode: "0644"
  dir_mode: "0755"
users:
  - username: alice
    home: /volumes/homes/nas_alice
    uid: 1026
    gid: 100
  - username: bob
    display_name: Bob Bobson
    home: /volumes/homes/nas_bob
    uid: 1027
    gid: 100
    quota: 1073741824
`

func TestLoadValid(t *testing.T) {
	cfg, err := Load(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Listen != ":9000" {
		t.Errorf("listen = %q, want :9000", cfg.Server.Listen)
	}
	// The trailing slash must be stripped, or every URL handed to a client
	// ends up with a doubled separator.
	if cfg.Server.ExternalURL != "https://nas.example.com" {
		t.Errorf("external_url = %q, want no trailing slash", cfg.Server.ExternalURL)
	}
	if got := cfg.Storage.FileMode.Perm(); got != 0o644 {
		t.Errorf("file_mode = %#o, want 0644", got)
	}
	if got := cfg.Storage.DirMode.Perm(); got != 0o755 {
		t.Errorf("dir_mode = %#o, want 0755", got)
	}
	// Unset fields must fall back to defaults rather than zero values.
	if cfg.Storage.RescanInterval.Duration() != 15*time.Minute {
		t.Errorf("rescan_interval = %v, want 15m default", cfg.Storage.RescanInterval)
	}
	if !cfg.Storage.Watcher {
		t.Error("watcher should default to true")
	}
	if cfg.Users[0].DisplayName != "alice" {
		t.Errorf("display_name = %q, want fallback to username", cfg.Users[0].DisplayName)
	}
	if cfg.Users[1].DisplayName != "Bob Bobson" {
		t.Errorf("display_name = %q, want Bob Bobson", cfg.Users[1].DisplayName)
	}
}

// TestFileModeIsOctal guards the trap that makes this a custom type: YAML would
// read a bare 0644 as decimal 644, which is mode 0o1204 - world-writable.
func TestFileModeIsOctal(t *testing.T) {
	cfg, err := Load(writeConfig(t, strings.Replace(validConfig,
		`file_mode: "0644"`, `file_mode: "0600"`, 1)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Storage.FileMode.Perm(); got != 0o600 {
		t.Fatalf("file_mode = %#o, want 0600", got)
	}
}

// TestRescanIntervalParsing covers the case that motivated a custom type:
// yaml.v3 decodes a duration string into time.Duration but rejects a bare
// integer, so "rescan_interval: 0" - the documented way to turn the periodic
// scan off - would otherwise fail to load.
func TestRescanIntervalParsing(t *testing.T) {
	tests := []struct {
		value string
		want  time.Duration
	}{
		{`"30s"`, 30 * time.Second},
		{`30s`, 30 * time.Second},
		{`"2h"`, 2 * time.Hour},
		{`1h30m`, 90 * time.Minute},
		{`0`, 0},
		{`"0s"`, 0},
	}
	for _, tc := range tests {
		body := strings.Replace(validConfig, `  dir_mode: "0755"`,
			"  dir_mode: \"0755\"\n  rescan_interval: "+tc.value, 1)
		cfg, err := Load(writeConfig(t, body))
		if err != nil {
			t.Errorf("rescan_interval: %s -> %v", tc.value, err)
			continue
		}
		if got := cfg.Storage.RescanInterval.Duration(); got != tc.want {
			t.Errorf("rescan_interval: %s -> %v, want %v", tc.value, got, tc.want)
		}
	}
}

// TestRescanIntervalRejectsUnitlessNumbers: guessing a unit for a bare number
// could set an interval orders of magnitude off, so it must be an error.
func TestRescanIntervalRejectsUnitlessNumbers(t *testing.T) {
	for _, value := range []string{"30", "900", `"soon"`, `"30 seconds"`} {
		body := strings.Replace(validConfig, `  dir_mode: "0755"`,
			"  dir_mode: \"0755\"\n  rescan_interval: "+value, 1)
		_, err := Load(writeConfig(t, body))
		if err == nil {
			t.Errorf("rescan_interval: %s was accepted; it should require a unit", value)
			continue
		}
		if !strings.Contains(err.Error(), "unit") && !strings.Contains(err.Error(), "valid duration") {
			t.Errorf("rescan_interval: %s -> %v; error should explain the unit requirement", value, err)
		}
	}
}

// TestUsersAreOptional: accounts can be managed entirely through the admin
// page, so a config that declares none is valid rather than an error.
func TestUsersAreOptional(t *testing.T) {
	cfg, err := Load(writeConfig(t,
		"server:\n  external_url: https://x.example.com\ndatabase:\n  path: /tmp/x.db\n"))
	if err != nil {
		t.Fatalf("a config with no users should load: %v", err)
	}
	if cfg.ManagesUsers() {
		t.Error("ManagesUsers should be false when no users are declared")
	}
}

func TestManagesUsersWhenDeclared(t *testing.T) {
	cfg, err := Load(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.ManagesUsers() {
		t.Error("ManagesUsers should be true when the config declares users")
	}
}

func TestValidationErrors(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantHint string
	}{
		{
			name:     "missing external_url",
			body:     strings.Replace(validConfig, `external_url: "https://nas.example.com/"`, "", 1),
			wantHint: "external_url",
		},
		{
			name:     "duplicate username",
			body:     strings.Replace(validConfig, "username: bob", "username: alice", 1),
			wantHint: "already exists",
		},
		{
			// Two users pointing at one directory is a tenant-isolation breach,
			// so it must fail loudly at startup rather than at request time.
			name:     "shared home directory",
			body:     strings.Replace(validConfig, "/volumes/homes/nas_bob", "/volumes/homes/nas_alice", 1),
			wantHint: "already uses",
		},
		{
			// So is nesting: the outer user would see the inner user's files,
			// and no amount of path confinement would prevent it, because
			// nothing is escaping any directory.
			name:     "home nested inside another",
			body:     strings.Replace(validConfig, "/volumes/homes/nas_bob", "/volumes/homes/nas_alice/shared", 1),
			wantHint: "would be able to see these files",
		},
		{
			name:     "home containing another",
			body:     strings.Replace(validConfig, "home: /volumes/homes/nas_bob", "home: /volumes/homes", 1),
			wantHint: "would be visible here",
		},
		{
			// A username of ".." would land in the WebDAV path as a traversal
			// segment. The character class permits dots for names like
			// "first.last", which makes this a member of it by accident.
			name:     "username made only of dots",
			body:     strings.Replace(validConfig, "username: alice", `username: ".."`, 1),
			wantHint: "path segment",
		},
		{
			name:     "relative home",
			body:     strings.Replace(validConfig, "/volumes/homes/nas_alice", "homes/nas_alice", 1),
			wantHint: "absolute path",
		},
		{
			// A slash in a username would let it escape its URL path segment.
			name:     "username with slash",
			body:     strings.Replace(validConfig, "username: alice", `username: "a/../bob"`, 1),
			wantHint: "username",
		},

		{
			name:     "unknown field",
			body:     validConfig + "\nbogus_field: 1\n",
			wantHint: "bogus_field",
		},
		{
			name:     "bad scheme",
			body:     strings.Replace(validConfig, "https://nas.example.com/", "ftp://nas.example.com", 1),
			wantHint: "http or https",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.body))
			if err == nil {
				t.Fatalf("expected an error mentioning %q, got none", tc.wantHint)
			}
			if !strings.Contains(err.Error(), tc.wantHint) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.wantHint)
			}
		})
	}
}
