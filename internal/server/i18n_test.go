package server

import (
	"net/http"
	"strings"
	"testing"
)

// getWithHeaders fetches a page with particular request headers, which is how
// language and platform are decided.
func (h *harness) getWithHeaders(t *testing.T, path string, headers map[string]string) (int, string) {
	t.Helper()
	resp := h.do("GET", path, "", "", "", headers)
	return resp.StatusCode, readBody(t, resp)
}

// TestLandingPageReplacesThe404: the address somebody is handed is the root,
// and until now that was a 404 for anybody who pasted it into a browser.
func TestLandingPageReplacesThe404(t *testing.T) {
	h := newHarness(t)
	status, body := h.getWithHeaders(t, "/", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	// The address to give the app is the thing the page exists to convey.
	if !strings.Contains(body, "https://mirage.test") {
		t.Errorf("the landing page does not show the server address:\n%s", body)
	}
	if !strings.Contains(body, "nextcloud.com/install") &&
		!strings.Contains(body, "apps.apple.com") {
		t.Errorf("the landing page offers no way to get the app:\n%s", body)
	}
}

// TestLandingPageOffersThePlatformsApp: somebody on a phone wants the app, not
// a page explaining that apps exist.
func TestLandingPageOffersThePlatformsApp(t *testing.T) {
	h := newHarness(t)
	for agent, want := range map[string]string{
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X)": "apps.apple.com",
		"Mozilla/5.0 (Linux; Android 14; Pixel 8)":               "play.google.com",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)":        "nextcloud.com/install",
	} {
		_, body := h.getWithHeaders(t, "/", map[string]string{"User-Agent": agent})
		// The others are listed too, so this checks the primary button rather
		// than mere presence anywhere on the page.
		primary := betweenMarkers(body, `class="btn" href="`, `"`)
		if !strings.Contains(primary, want) {
			t.Errorf("agent %q was offered %q, want a %s link", agent, primary, want)
		}
	}
}

// TestPagesAreTranslated is the point of the exercise: somebody who reads
// French is handed a link and should not meet a wall of English.
func TestPagesAreTranslated(t *testing.T) {
	h := newHarness(t)
	french := map[string]string{"Accept-Language": "fr-FR,fr;q=0.9,en;q=0.8"}

	_, landing := h.getWithHeaders(t, "/", french)
	for _, want := range []string{"Vos fichiers", "Installez l", "Indiquez-lui cette adresse"} {
		if !strings.Contains(landing, want) {
			t.Errorf("the landing page is not in French; missing %q", want)
		}
	}
	if !strings.Contains(landing, `lang="fr"`) {
		t.Error("the document does not declare its language")
	}

	_, login := h.getWithHeaders(t, "/web/login", french)
	for _, want := range []string{"Identifiant", "Mot de passe", "Se connecter"} {
		if !strings.Contains(login, want) {
			t.Errorf("the sign-in page is not in French; missing %q", want)
		}
	}
}

// TestEnglishRemainsTheDefault for a browser that asks for something else, or
// says nothing at all.
func TestEnglishRemainsTheDefault(t *testing.T) {
	h := newHarness(t)
	for name, headers := range map[string]map[string]string{
		"no header":       nil,
		"an unknown one":  {"Accept-Language": "de-DE,de;q=0.9"},
		"a malformed one": {"Accept-Language": ";;;q=notanumber"},
		"English wanted":  {"Accept-Language": "en-GB,en;q=0.9"},
	} {
		_, body := h.getWithHeaders(t, "/web/login", headers)
		if !strings.Contains(body, "Username") {
			t.Errorf("%s did not get English:\n%s", name, body)
		}
	}
}

// TestLanguageFollowsTheWeights: a browser listing several languages is asking
// for the one with the highest weight, not the first one that matches.
func TestLanguageFollowsTheWeights(t *testing.T) {
	h := newHarness(t)
	_, body := h.getWithHeaders(t, "/web/login",
		map[string]string{"Accept-Language": "en;q=0.2,fr;q=0.9"})
	if !strings.Contains(body, "Identifiant") {
		t.Errorf("the higher-weighted language was not chosen:\n%s", body)
	}
}

// TestSignedInVisitorsSkipTheInstructions: somebody who already has a session
// wants their files, not a page telling them how to get started.
func TestSignedInVisitorsSkipTheInstructions(t *testing.T) {
	h := newHarness(t)
	c := webClient(t, h, "alice", alicePassword)
	resp, err := c.Get(h.http.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want a redirect to the files", resp.StatusCode)
	}
}

// TestTranslatedPagesAreComplete guards the catalogue: a key with no English
// text renders as the key itself, which looks like a bug to everybody.
func TestTranslatedPagesAreComplete(t *testing.T) {
	h := newHarness(t)
	c := webClient(t, h, "alice", alicePassword)
	for _, path := range []string{"/web/", "/web/trash", "/web/profile"} {
		for _, headers := range []map[string]string{
			nil, {"Accept-Language": "fr"},
		} {
			req, _ := http.NewRequest("GET", h.http.URL+path, nil)
			for k, v := range headers {
				req.Header.Set(k, v)
			}
			resp, err := c.Do(req)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			body := readBody(t, resp)
			// An untranslated key looks like "profile.password" on the page.
			for _, marker := range []string{"profile.", "browse.", "trash.", "versions.", "landing."} {
				if strings.Contains(body, ">"+marker) {
					t.Errorf("GET %s (%v) shows a raw message key starting %q", path, headers, marker)
				}
			}
		}
	}
}
