package ocs

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/poitch/mirage/internal/auth"
	"github.com/poitch/mirage/internal/store"
)

// ProductName is what Mirage reports itself as. Clients display it and use it
// to tell Nextcloud from ownCloud, so it must remain recognisable as a
// Nextcloud-compatible server.
const ProductName = "Nextcloud"

// UsageFunc reports a user's current storage consumption in bytes. It is
// injected so the OCS layer does not need to know how usage is tracked.
type UsageFunc func(ctx context.Context, user store.User) (int64, error)

// Service serves the OCS endpoints and status.php.
type Service struct {
	version  ServerVersion
	auth     *auth.Authenticator
	db       *store.DB
	log      *slog.Logger
	usage    UsageFunc
	features Features
}

// Features toggles the capabilities Mirage advertises. Announcing something
// unimplemented makes clients surface controls that then fail, so each flag is
// turned on only when its milestone lands.
type Features struct {
	Trashbin   bool
	Versioning bool
}

// NewService builds the OCS service. advertisedVersion is the Nextcloud version
// string Mirage claims; usage may be nil, in which case zero is reported.
func NewService(advertisedVersion string, features Features, a *auth.Authenticator,
	db *store.DB, log *slog.Logger, usage UsageFunc) (*Service, error) {

	v, err := parseVersion(advertisedVersion)
	if err != nil {
		return nil, err
	}
	if usage == nil {
		usage = func(context.Context, store.User) (int64, error) { return 0, nil }
	}
	s := &Service{version: v, auth: a, db: db, log: log, usage: usage}
	s.features = features
	return s, nil
}

// ServerVersion is the version quadruple clients read.
type ServerVersion struct {
	Major           int    `xml:"major" json:"major"`
	Minor           int    `xml:"minor" json:"minor"`
	Micro           int    `xml:"micro" json:"micro"`
	String          string `xml:"string" json:"string"`
	Edition         string `xml:"edition" json:"edition"`
	ExtendedSupport bool   `xml:"extendedSupport" json:"extendedSupport"`
}

func parseVersion(s string) (ServerVersion, error) {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return ServerVersion{}, errors.New("version must look like MAJOR.MINOR.MICRO")
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return ServerVersion{}, errors.New("version must be numeric")
		}
		nums[i] = n
	}
	return ServerVersion{
		Major: nums[0], Minor: nums[1], Micro: nums[2],
		String: s, Edition: "",
	}, nil
}

// Status serves /status.php, the first request any client makes. A client that
// cannot parse this never gets as far as trying to authenticate.
func (s *Service) Status(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Clients may probe this cross-origin during setup.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	//nolint:errcheck // connection gone
	json.NewEncoder(w).Encode(map[string]any{
		"installed":       true,
		"maintenance":     false,
		"needsDbUpgrade":  false,
		"version":         s.version.String + ".0",
		"versionstring":   s.version.String,
		"edition":         "",
		"productname":     ProductName,
		"extendedSupport": false,
	})
}

// NoContent serves /index.php/204, the connectivity probe mobile clients use to
// distinguish a real server from a captive portal.
func (s *Service) NoContent(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

type capabilitiesData struct {
	Version      ServerVersion `xml:"version" json:"version"`
	Capabilities capabilities  `xml:"capabilities" json:"capabilities"`
}

type capabilities struct {
	Core      coreCaps     `xml:"core" json:"core"`
	DAV       davCaps      `xml:"dav" json:"dav"`
	Files     filesCaps    `xml:"files" json:"files"`
	Checksums checksumCaps `xml:"checksums" json:"checksums"`
}

type coreCaps struct {
	// PollInterval is how often a client re-checks for changes when it has no
	// push connection. It drops to near-irrelevant once notify_push lands.
	PollInterval int        `xml:"pollinterval" json:"pollinterval"`
	WebDAVRoot   string     `xml:"webdav-root" json:"webdav-root"`
	Bruteforce   bruteforce `xml:"bruteforce" json:"bruteforce"`
}

type bruteforce struct {
	Delay int `xml:"delay" json:"delay"`
}

type davCaps struct {
	// Chunking "1.0" means the chunked upload v2 protocol. The value is a
	// protocol marker, not a version of the server.
	Chunking string `xml:"chunking" json:"chunking"`
}

type filesCaps struct {
	BigFileChunking bool `xml:"bigfilechunking" json:"bigfilechunking"`
	Undelete        bool `xml:"undelete" json:"undelete"`
	Versioning      bool `xml:"versioning" json:"versioning"`
}

type checksumCaps struct {
	SupportedTypes      []string `xml:"supportedTypes>element" json:"supportedTypes"`
	PreferredUploadType string   `xml:"preferredUploadType" json:"preferredUploadType"`
}

// Capabilities serves /ocs/v{1,2}.php/cloud/capabilities.
func (s *Service) Capabilities(v Version) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		Write(w, r, v, capabilitiesData{
			Version: s.version,
			Capabilities: capabilities{
				Core: coreCaps{
					PollInterval: 60,
					WebDAVRoot:   "remote.php/webdav",
					Bruteforce:   bruteforce{Delay: 0},
				},
				DAV: davCaps{Chunking: "1.0"},
				Files: filesCaps{
					BigFileChunking: true,
					Undelete:        s.features.Trashbin,
					Versioning:      s.features.Versioning,
				},
				Checksums: checksumCaps{
					SupportedTypes:      []string{"SHA1", "MD5"},
					PreferredUploadType: "SHA1",
				},
			},
		})
	}
}

type userData struct {
	ID          string `xml:"id" json:"id"`
	DisplayName string `xml:"display-name" json:"display-name"`
	// Clients disagree on which spelling to read, so both are sent.
	DisplayNameAlt string `xml:"displayname" json:"displayname"`
	Email          string `xml:"email" json:"email"`
	Enabled        bool   `xml:"enabled" json:"enabled"`
	Quota          quota  `xml:"quota" json:"quota"`
}

// quota mirrors Nextcloud's shape, including its sentinel values: Quota is
// -3 for an unlimited account, and Free and Total are then meaningless.
type quota struct {
	Free     int64   `xml:"free" json:"free"`
	Used     int64   `xml:"used" json:"used"`
	Total    int64   `xml:"total" json:"total"`
	Relative float64 `xml:"relative" json:"relative"`
	Quota    int64   `xml:"quota" json:"quota"`
}

// quotaUnlimited is the sentinel Nextcloud uses for an account with no limit.
const quotaUnlimited = -3

// User serves /ocs/v{1,2}.php/cloud/user for the authenticated account.
func (s *Service) User(v Version) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := auth.MustUser(r.Context())

		used, err := s.usage(r.Context(), u)
		if err != nil {
			s.log.Error("could not determine storage usage", "user", u.Username, "error", err)
			used = 0
		}

		q := quota{Used: used, Quota: quotaUnlimited, Total: quotaUnlimited, Free: quotaUnlimited}
		if u.Quota > 0 {
			free := u.Quota - used
			if free < 0 {
				free = 0
			}
			q = quota{
				Free:     free,
				Used:     used,
				Total:    u.Quota,
				Quota:    u.Quota,
				Relative: float64(used) / float64(u.Quota) * 100,
			}
		}

		Write(w, r, v, userData{
			ID:             u.Username,
			DisplayName:    u.DisplayName,
			DisplayNameAlt: u.DisplayName,
			Enabled:        !u.Disabled,
			Quota:          q,
		})
	}
}

// DeleteAppPassword serves DELETE /ocs/v2.php/core/apppassword, which clients
// call when an account is removed so the device token stops working.
func (s *Service) DeleteAppPassword(v Version) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := auth.MustUser(r.Context())

		// The token to revoke is the one that authenticated this request.
		_, secret, ok := r.BasicAuth()
		if !ok {
			WriteError(w, r, v, StatusBadRequest, "no app password presented")
			return
		}
		err := s.db.DeleteAppPassword(r.Context(), u.ID, auth.HashToken(secret))
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.log.Error("could not delete app password", "user", u.Username, "error", err)
			WriteError(w, r, v, StatusError, "could not delete app password")
			return
		}
		// A token that was already gone is still the outcome the client wanted.
		s.auth.Forget(u.Username)
		Write(w, r, v, struct{}{})
	}
}
