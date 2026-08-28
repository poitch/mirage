package ocs

import (
	"html/template"
	"net/http"
	"path"
	"strings"
)

// filesPage explains where a file is, for the one case that reaches it.
//
// The desktop client turns a search result into "open this folder in the file
// manager" and only opens a browser when it cannot find the folder among the
// ones it is syncing - which means the person has that folder deselected on
// this device. So this page exists to answer the question they are actually
// asking, which is "then where is it?", rather than to be a file manager.
//
// It is served without authentication, and deliberately shows nothing it was
// not given: the path is read out of the query string the caller supplied, and
// nothing is looked up. There is no account here to leak.
var filesPage = template.Must(template.New("files").Parse(`<!doctype html>
<html lang="en"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{ if .Path }}{{ .Name }} &middot; Mirage{{ else }}Mirage{{ end }}</title>
<style>
  :root { color-scheme: light dark; --fg:#1a1a1a; --muted:#6b6b70; --bg:#fbfbfc; --card:#fff; --line:#e3e3e7; }
  @media (prefers-color-scheme: dark) {
    :root { --fg:#e8e8ea; --muted:#9a9aa2; --bg:#161618; --card:#1e1e21; --line:#303036; }
  }
  * { box-sizing: border-box; }
  body {
    margin: 0; min-height: 100vh; display: grid; place-items: center; padding: 1.5rem;
    background: var(--bg); color: var(--fg);
    font: 15px/1.55 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
  }
  main { width: 100%; max-width: 33rem; background: var(--card); border: 1px solid var(--line);
         border-radius: 12px; padding: 1.6rem 1.8rem; }
  h1 { margin: 0 0 .3rem; font-size: 1.15rem; }
  p { margin: .55rem 0; color: var(--muted); font-size: .92rem; }
  .path { margin: 1rem 0; padding: .7rem .85rem; background: var(--bg); border: 1px solid var(--line);
          border-radius: 8px; font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
          font-size: .88rem; word-break: break-all; color: var(--fg); }
  .name { font-weight: 600; }
</style>
</head><body>
<main>
{{ if .Path }}
  <h1>{{ .Name }}</h1>
  <p>Mirage has no web interface, so there is nothing to open here. The file is at:</p>
  <div class="path">{{ .Dir }}{{ if ne .Dir "/" }}/{{ end }}<span class="name">{{ .Name }}</span></div>
  <p>
    Your client sent you here because it could not find that folder among the ones it
    is syncing on this device &mdash; most likely it is deselected. Turn it on in the
    client&rsquo;s folder settings, or open the file on a device that syncs it.
  </p>
{{ else }}
  <h1>Mirage</h1>
  <p>
    This is a sync server with no web interface. Use the Nextcloud desktop or mobile
    client to reach your files.
  </p>
{{ end }}
</main>
</body></html>`))

// FilesPage answers the address search results point at.
func (s *Service) FilesPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dir := strings.TrimSpace(q.Get("dir"))
	name := strings.TrimSpace(q.Get("scrollto"))

	data := struct{ Dir, Name, Path string }{}
	// Only rendered when both are present and the name is a single component;
	// anything else is somebody hand-editing the URL, and gets the plain page.
	if dir != "" && name != "" && name == path.Base(name) && !strings.ContainsAny(name, `/\`) {
		data.Dir = path.Clean("/" + dir)
		data.Name = name
		data.Path = data.Dir + "/" + name
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	if err := filesPage.Execute(w, data); err != nil {
		s.log.Warn("could not render the file location page", "error", err)
	}
}
