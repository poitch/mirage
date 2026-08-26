package admin

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/poitch/mirage/internal/store"
)

// bytesPerGB is the multiplier for the quota field, which is expressed in
// gibibytes because nobody wants to type 107374182400.
const bytesPerGB = 1024 * 1024 * 1024

// parseUserForm reads the account form.
//
// It returns the raw submitted values alongside the parsed mapping so that a
// rejected form can be redisplayed with what was typed, rather than blanked.
func parseUserForm(r *http.Request) (userForm, store.UserMapping, error) {
	if err := r.ParseForm(); err != nil {
		return userForm{}, store.UserMapping{}, fmt.Errorf("that form could not be read")
	}
	form := userForm{
		Username:    strings.TrimSpace(r.PostFormValue("username")),
		DisplayName: strings.TrimSpace(r.PostFormValue("display_name")),
		Home:        strings.TrimSpace(r.PostFormValue("home")),
		UID:         strings.TrimSpace(r.PostFormValue("uid")),
		GID:         strings.TrimSpace(r.PostFormValue("gid")),
		QuotaGB:     strings.TrimSpace(r.PostFormValue("quota_gb")),
	}

	uid, err := strconv.Atoi(form.UID)
	if err != nil {
		return form, store.UserMapping{}, fmt.Errorf("uid must be a number; find it with `id %s` on the NAS", form.Username)
	}
	gid, err := strconv.Atoi(form.GID)
	if err != nil {
		return form, store.UserMapping{}, fmt.Errorf("gid must be a number; find it with `id %s` on the NAS", form.Username)
	}

	var quota int64
	if form.QuotaGB != "" {
		gb, err := strconv.ParseFloat(form.QuotaGB, 64)
		if err != nil || gb < 0 {
			return form, store.UserMapping{}, fmt.Errorf("quota must be a number of GB, or blank for unlimited")
		}
		quota = int64(gb * bytesPerGB)
	}

	return form, store.UserMapping{
		Username:    form.Username,
		DisplayName: form.DisplayName,
		Home:        form.Home,
		UID:         uid,
		GID:         gid,
		Quota:       quota,
	}, nil
}

// quotaToGB renders a stored quota for the form, blank meaning unlimited.
func quotaToGB(quota int64) string {
	if quota <= 0 {
		return ""
	}
	gb := float64(quota) / bytesPerGB
	return strconv.FormatFloat(gb, 'f', -1, 64)
}

// humanBytes renders a byte count for display.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
