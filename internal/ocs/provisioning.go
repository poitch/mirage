package ocs

import (
	"net/http"

	"github.com/poitch/mirage/internal/auth"
	"github.com/poitch/mirage/internal/store"
)

// provisioningUser is the shape the provisioning API returns for an account.
//
// It overlaps with cloud/user but is not the same: the mobile apps read the
// display name from displayname here and from display-name there, and populate
// their account screen from whichever they got.
type provisioningUser struct {
	ID          string   `xml:"id" json:"id"`
	Enabled     bool     `xml:"enabled" json:"enabled"`
	DisplayName string   `xml:"displayname" json:"displayname"`
	Email       string   `xml:"email" json:"email"`
	Quota       quota    `xml:"quota" json:"quota"`
	Groups      []string `xml:"groups>element" json:"groups"`
	Language    string   `xml:"language" json:"language"`
	Backend     string   `xml:"backend" json:"backend"`
}

// UserDetails serves /ocs/v{1,2}.php/cloud/users/{userid}.
//
// Nextcloud lets an administrator read any account through this. Mirage has no
// administrator role on the sync side - the admin page is separate and does not
// issue app passwords - so an account may only read itself, and asking for
// somebody else is refused rather than answered.
func (s *Service) UserDetails(v Version) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller := auth.MustUser(r.Context())
		if r.PathValue("userid") != caller.Username {
			WriteError(w, r, v, StatusForbidden, "you may only read your own account")
			return
		}

		used, err := s.usage(r.Context(), caller)
		if err != nil {
			s.log.Error("could not determine storage usage", "user", caller.Username, "error", err)
			used = 0
		}

		Write(w, r, v, provisioningUser{
			ID:          caller.Username,
			Enabled:     !caller.Disabled,
			DisplayName: caller.DisplayName,
			Quota:       accountQuota(caller, used),
			Groups:      []string{},
			Language:    "en",
			Backend:     "Database",
		})
	}
}

// accountQuota renders an account's usage in the form OCS reports it.
func accountQuota(u store.User, used int64) quota {
	if u.Quota <= 0 {
		return quota{Used: used, Quota: quotaUnlimited, Total: quotaUnlimited, Free: quotaUnlimited}
	}
	free := max(u.Quota-used, 0)
	return quota{
		Free:     free,
		Used:     used,
		Total:    u.Quota,
		Quota:    u.Quota,
		Relative: float64(used) / float64(u.Quota) * 100,
	}
}

// NavigationApps serves /ocs/v{1,2}.php/core/navigation/apps.
//
// The mobile apps ask what web applications the server offers so they can build
// a menu. Mirage has none - it is a sync server with no web interface - so the
// honest answer is an empty list, which the apps render as no menu.
func (s *Service) NavigationApps(v Version) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		Write(w, r, v, []struct{}{})
	}
}

// termsResponse is what the terms_of_service app returns.
type termsResponse struct {
	Terms       []struct{} `xml:"terms>element" json:"terms"`
	HasSigned   bool       `xml:"hasSigned" json:"hasSigned"`
	NotSigned   []struct{} `xml:"notSigned>element" json:"notSigned"`
	AdminTerms  []struct{} `xml:"adminTerms>element" json:"adminTerms"`
	AdminLegacy bool       `xml:"adminLegacy" json:"adminLegacy"`
}

// Terms serves /ocs/v{1,2}.php/apps/terms_of_service/terms.
//
// Clients ask this before sync to find out whether the server is withholding
// access until somebody accepts a document. Mirage never does, so it answers
// that there is nothing to sign. Leaving the endpoint unimplemented left the
// iOS app retrying it on every connection.
func (s *Service) Terms(v Version) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		Write(w, r, v, termsResponse{
			Terms:      []struct{}{},
			HasSigned:  true,
			NotSigned:  []struct{}{},
			AdminTerms: []struct{}{},
		})
	}
}

// DirectEditing serves /ocs/v{1,2}.php/apps/files/api/v1/directEditing.
//
// The apps ask which editors the server offers so they can open a document
// without downloading it. Mirage has none - it stores files and syncs them -
// so the honest answer is an empty set, which the apps read as "download it and
// edit it locally".
func (s *Service) DirectEditing(v Version) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		Write(w, r, v, directEditing{
			Editors:   map[string]struct{}{},
			Creators:  map[string]struct{}{},
			Templates: map[string]struct{}{},
		})
	}
}

// directEditing is the shape the apps expect: three maps keyed by editor id.
// Empty ones are a valid answer, where a missing endpoint is not.
type directEditing struct {
	Editors   map[string]struct{} `xml:"editors" json:"editors"`
	Creators  map[string]struct{} `xml:"creators" json:"creators"`
	Templates map[string]struct{} `xml:"templates" json:"templates"`
}

// DashboardWidgets serves /ocs/v{1,2}.php/apps/dashboard/api/v1/widgets.
//
// Nextcloud's dashboard is a web page of panels. Mirage has no web application
// to put panels in, so it offers none - and saying so stops the apps asking on
// every connection.
func (s *Service) DashboardWidgets(v Version) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		Write(w, r, v, map[string]struct{}{})
	}
}

// PushRegistration serves the notifications app's device registration.
//
// Nextcloud registers a device with a hosted proxy that then delivers push
// notifications through Apple and Google. Mirage has no such proxy, and
// pretending the registration succeeded would leave the app waiting for
// notifications that never arrive. Refusing it makes the app fall back to
// notify_push and polling, which do work here.
func (s *Service) PushRegistration(v Version) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, v, StatusNotFound, "this server does not relay push notifications")
	}
}
