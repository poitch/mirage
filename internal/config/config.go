// Package config loads and validates the Mirage server configuration.
//
// The config file is the source of truth for the mapping of Mirage users onto
// NAS home directories and their filesystem ownership. Everything else about a
// user (password hash, app passwords, the file index) lives in the database and
// is reconciled against this file at startup.
package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/poitch/mirage/internal/account"
	"github.com/poitch/mirage/internal/fsx"
	"gopkg.in/yaml.v3"
)

// Config is the top-level server configuration.
type Config struct {
	Server   Server   `yaml:"server"`
	Database Database `yaml:"database"`
	Storage  Storage  `yaml:"storage"`
	// Users is optional; see the note on User. An empty list means accounts are
	// managed through the admin page instead.
	Users []User `yaml:"users"`
}

// ManagesUsers reports whether the config file declares the account list. When
// it does not, nothing is reconciled from it and the database stands alone.
func (c *Config) ManagesUsers() bool { return len(c.Users) > 0 }

// Server holds HTTP listener settings.
type Server struct {
	// Listen is the address the HTTP server binds to, e.g. ":8080".
	Listen string `yaml:"listen"`
	// ExternalURL is the origin clients reach the server on. It is baked into
	// the Login Flow v2 and notify_push URLs handed to clients, so it must be
	// the address they can actually reach, not the container-internal one.
	ExternalURL string `yaml:"external_url"`
	// AdvertisedVersion is the Nextcloud server version Mirage claims to be.
	//
	// Clients gate features on it and may warn about a server they consider
	// end-of-life, so it is configurable: a client release can change its mind
	// about what it accepts, and that should be fixable without a rebuild.
	AdvertisedVersion string `yaml:"advertised_version"`
}

// Database holds index database settings.
type Database struct {
	// Path is the SQLite file. It holds only rebuildable index data plus
	// credentials; user files never live here.
	Path string `yaml:"path"`
}

// Storage holds settings governing how files are written to the NAS.
type Storage struct {
	// FileMode and DirMode are applied to every file and directory Mirage
	// creates, after ownership is set from the owning user's UID/GID.
	FileMode FileMode `yaml:"file_mode"`
	DirMode  FileMode `yaml:"dir_mode"`
	// RescanInterval is how often the full reconciliation walk runs. This is
	// the backstop that catches out-of-band changes the watcher missed, so it
	// should stay enabled even when the watcher is healthy. Zero disables it.
	RescanInterval Duration `yaml:"rescan_interval"`
	// Watcher enables the fsnotify watcher. Disabling it leaves reconciliation
	// entirely to RescanInterval, which is slower but still correct.
	Watcher bool `yaml:"watcher"`
	// QuickRescanInterval is the gap between quick passes, which use directory
	// timestamps to find files added, removed or renamed without stat'ing every
	// file in the share.
	//
	// This is what keeps a file dropped over SMB appearing on clients in
	// minutes on a share too large to walk in full at that frequency, and too
	// large for the kernel to watch every directory of. Zero disables it.
	QuickRescanInterval Duration `yaml:"quick_rescan_interval"`
	// MaxWatches caps how many directories the filesystem watcher may watch.
	//
	// Watches are a fixed kernel resource and a large share has far more
	// directories than any limit allows, so they are spent on the most recently
	// changed directories rather than on whichever come first. Zero derives a
	// figure from the kernel's own limit.
	MaxWatches int `yaml:"max_watches"`
	// Exclude lists entry names that are not indexed or synced, matched against
	// the name at any depth using filepath.Match syntax.
	//
	// Nothing is excluded by default: these are the user's files, and only they
	// can say which of them are worth syncing. The reason to set it is cost -
	// a source checkout's .svn directory or a node_modules tree can hold
	// millions of tiny files that dominate a scan and are rebuilt anyway.
	Exclude []string `yaml:"exclude"`
}

// User maps a Mirage account onto a directory on the NAS filesystem.
//
// Declaring users here is optional. When the list is empty, accounts are
// managed entirely through the admin page and the database is authoritative;
// when it is populated, this file wins and the admin page reflects it but
// cannot override it. Mixing the two is possible but confusing, so pick one.
type User struct {
	Username    string `yaml:"username"`
	DisplayName string `yaml:"display_name"`
	// Home is the absolute path, as seen from inside the container, of the
	// directory backing this user. Typically a bind mount of
	// /volumeN/homes/<nas_user>.
	Home string `yaml:"home"`
	// UID and GID are stamped onto every file and directory Mirage creates for
	// this user, so the NAS sees native ownership over SMB and File Station.
	UID int `yaml:"uid"`
	GID int `yaml:"gid"`
	// Quota is the storage limit in bytes. Zero means unlimited.
	Quota int64 `yaml:"quota"`
}

// Default returns a Config populated with the defaults applied before the file
// is unmarshalled over it.
func Default() Config {
	return Config{
		Server: Server{
			Listen:            ":8080",
			AdvertisedVersion: "31.0.0",
		},
		Database: Database{Path: "/var/lib/mirage/mirage.db"},
		Storage: Storage{
			FileMode:            0o640,
			DirMode:             0o750,
			RescanInterval:      Duration(6 * time.Hour),
			QuickRescanInterval: Duration(5 * time.Minute),
			Watcher:             true,
		},
	}
}

// Load reads, parses and validates the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := Default()
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &cfg, nil
}

// advertisedVersionRe matches a three-part version such as "31.0.0".
var advertisedVersionRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// Validate checks the configuration for errors that would otherwise surface as
// confusing runtime failures.
func (c *Config) Validate() error {
	if c.Server.Listen == "" {
		return fmt.Errorf("server.listen must not be empty")
	}
	if c.Server.ExternalURL == "" {
		return fmt.Errorf("server.external_url is required: clients are handed this URL during login and push setup")
	}
	u, err := url.Parse(c.Server.ExternalURL)
	if err != nil {
		return fmt.Errorf("server.external_url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("server.external_url must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("server.external_url must include a host")
	}
	c.Server.ExternalURL = strings.TrimRight(c.Server.ExternalURL, "/")

	if !advertisedVersionRe.MatchString(c.Server.AdvertisedVersion) {
		return fmt.Errorf("server.advertised_version %q must look like MAJOR.MINOR.MICRO",
			c.Server.AdvertisedVersion)
	}

	if c.Database.Path == "" {
		return fmt.Errorf("database.path must not be empty")
	}
	if c.Storage.RescanInterval < 0 {
		return fmt.Errorf("storage.rescan_interval must not be negative")
	}
	if c.Storage.QuickRescanInterval < 0 {
		return fmt.Errorf("storage.quick_rescan_interval must not be negative")
	}
	if c.Storage.MaxWatches < 0 {
		return fmt.Errorf("storage.max_watches must not be negative (use 0 to derive it)")
	}
	// Compiled here so a malformed pattern is a startup error rather than
	// something that silently matches nothing for the life of the server.
	if _, err := fsx.NewExcluder(c.Storage.Exclude); err != nil {
		return fmt.Errorf("storage.exclude: %w", err)
	}
	// The same rules apply to accounts created through the admin page, so they
	// live in one place rather than being restated here.
	seen := make([]account.Mapping, 0, len(c.Users))
	for i := range c.Users {
		u := &c.Users[i]
		if err := account.ValidateUsername(u.Username); err != nil {
			return fmt.Errorf("users[%d]: %w", i, err)
		}
		home, err := account.ValidateHome(u.Home)
		if err != nil {
			return fmt.Errorf("users[%d] (%s): %w", i, u.Username, err)
		}
		u.Home = home
		if err := account.ValidateOwnership(u.UID, u.GID); err != nil {
			return fmt.Errorf("users[%d] (%s): %w", i, u.Username, err)
		}
		if err := account.CheckConflicts(account.Mapping{Username: u.Username, Home: u.Home}, seen); err != nil {
			return fmt.Errorf("users[%d] (%s): %w", i, u.Username, err)
		}
		seen = append(seen, account.Mapping{Username: u.Username, Home: u.Home})

		if u.Quota < 0 {
			return fmt.Errorf("users[%d] (%s): quota must not be negative (use 0 for unlimited)", i, u.Username)
		}
		if u.DisplayName == "" {
			u.DisplayName = u.Username
		}
	}
	return nil
}

// Duration is a time span parsed from a string such as "15m".
//
// It exists because yaml.v3 will decode a duration string into time.Duration
// but rejects a bare integer, including 0 - and 0 is exactly what someone
// writes to turn a periodic task off. Accepting it avoids a config that fails
// to load for following its own documentation.
type Duration time.Duration

// UnmarshalYAML parses a duration string, or a literal 0.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err == nil {
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("%q is not a valid duration: use a unit, such as \"30s\", \"15m\" or \"2h\"", s)
		}
		*d = Duration(parsed)
		return nil
	}

	var n int64
	if err := value.Decode(&n); err != nil {
		return fmt.Errorf("expected a duration such as \"15m\", got %q", value.Value)
	}
	if n != 0 {
		// A bare number has no obvious unit, and guessing one would silently
		// produce an interval orders of magnitude off.
		return fmt.Errorf("duration %d needs a unit: write %ds, %dm or %dh", n, n, n, n)
	}
	*d = 0
	return nil
}

// MarshalYAML renders the duration back out as a string.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// String renders the duration.
func (d Duration) String() string { return time.Duration(d).String() }

// FileMode is a Unix permission bitmask parsed from an octal string such as
// "0640". It is a distinct type because YAML's own integer parsing would read
// a bare 0640 as decimal, silently producing the wrong permissions.
type FileMode os.FileMode

// UnmarshalYAML parses an octal permission string.
func (m *FileMode) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("mode must be a quoted octal string such as \"0640\": %w", err)
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(s, "0o"), 8, 32)
	if err != nil {
		return fmt.Errorf("mode %q is not valid octal: %w", s, err)
	}
	if n > 0o7777 {
		return fmt.Errorf("mode %q is out of range", s)
	}
	*m = FileMode(n)
	return nil
}

// MarshalYAML renders the mode back out as a quoted octal string.
func (m FileMode) MarshalYAML() (any, error) { return fmt.Sprintf("%#o", uint32(m)), nil }

// Perm returns the mode as an os.FileMode.
func (m FileMode) Perm() os.FileMode { return os.FileMode(m).Perm() }

// String renders the mode in octal.
func (m FileMode) String() string { return fmt.Sprintf("%#o", uint32(m)) }
