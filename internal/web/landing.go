package web

import (
	"net/http"
	"strings"
)

// The landing page exists because somebody who is handed this server's address
// and opens it in a browser has, until now, been shown a 404. That person is
// usually a family member who has been told "your files are on here" and has no
// idea what a WebDAV endpoint is. They need three things: the app, the address,
// and their password.

// client is one platform's Nextcloud app. Name is filled in at render time
// from nameKey, so the list reads in the reader's language.
type client struct {
	Name     string
	URL      string
	labelKey string
	nameKey  string
}

// clients are the official apps, by platform.
//
// The store links rather than the download page, because a person on a phone
// wants the app rather than a page explaining that apps exist.
var (
	clientIOS = client{
		URL:      "https://apps.apple.com/app/nextcloud/id1125420102",
		labelKey: "client.ios", nameKey: "client.name.ios",
	}
	clientAndroid = client{
		URL:      "https://play.google.com/store/apps/details?id=com.nextcloud.client",
		labelKey: "client.android", nameKey: "client.name.android",
	}
	clientDesktop = client{
		URL:      "https://nextcloud.com/install/#install-clients",
		labelKey: "client.desktop", nameKey: "client.name.desktop",
	}
)

// allClients is the fixed order the alternatives are listed in. A map would
// reorder them between refreshes, which reads as a page that cannot make up
// its mind.
var allClients = []client{clientIOS, clientAndroid, clientDesktop}

// landing shows somebody how to connect a device.
func (s *Site) landing(w http.ResponseWriter, r *http.Request) {
	// Somebody already signed in wants their files, not the instructions.
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if _, ok := s.sessions.lookup(cookie.Value); ok {
			http.Redirect(w, r, rootPath, http.StatusSeeOther)
			return
		}
	}

	l := languageFor(r)
	base := pageData{Lang: l}
	platformKey, primary := platformFor(r.UserAgent())

	data := pageData{
		Lang:        l,
		Title:       base.T("landing.title"),
		ClientLabel: base.T(primary.labelKey),
		ClientURL:   primary.URL,
		ServerURL:   s.externalURL,
	}
	if platformKey != "" {
		data.PlatformName = base.T(platformKey)
	}
	for _, c := range allClients {
		if c.URL == primary.URL {
			continue
		}
		data.OtherClients = append(data.OtherClients,
			client{Name: base.T(c.nameKey), URL: c.URL})
	}
	s.render(w, r, "landing.html", http.StatusOK, data)
}

// platformFor guesses which app to offer.
//
// A guess, and treated as one: the other platforms are listed beside it, so
// being wrong costs a glance rather than a dead end.
func platformFor(agent string) (string, client) {
	switch {
	case strings.Contains(agent, "iPhone"):
		return "platform.iphone", clientIOS
	case strings.Contains(agent, "iPad"):
		return "platform.ipad", clientIOS
	case strings.Contains(agent, "Android"):
		return "platform.android", clientAndroid
	case strings.Contains(agent, "Macintosh"):
		return "platform.mac", clientDesktop
	case strings.Contains(agent, "Windows"):
		return "platform.windows", clientDesktop
	case strings.Contains(agent, "Linux"):
		return "platform.linux", clientDesktop
	}
	return "", clientDesktop
}
