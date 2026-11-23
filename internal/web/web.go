// Package web serves the web interface: a single-page app that is built from
// the web directory of the repository into dist and embedded in the binary
// from there, so that an install stays one binary.
//
// What it serves is the same for everyone and says nothing about an install:
// the files of an open-source project. Everything the app shows comes from
// the API, which asks for a session. That is why its route can be public
// although Beacon is closed by default.
//
// A binary can be built before the app has been (go build needs no Node). It
// then says so, in the log at start and to a browser, and the API works as
// ever.
package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// The build output, as Vite writes it: index.html, the files of web/public,
// and assets/, whose file names carry a hash of their content.
//
//go:embed all:dist
var embedded embed.FS

// Dist returns the embedded build output.
func Dist() fs.FS {
	dist, err := fs.Sub(embedded, "dist")
	if err != nil {
		// The directory is embedded above, so this cannot happen.
		panic(err)
	}
	return dist
}

// Pattern is the route of the app: every GET that no other route takes.
const Pattern = "GET /"

// shellName is the page that loads the app. It is answered for every address
// that is a page of the app, which finds out which one from the address.
const shellName = "index.html"

// assetsPrefix is where the files with a hash in their name are. They never
// change, so a browser may keep them for good.
const assetsPrefix = "assets/"

// ContentSecurityPolicy is what a page of the app may load and do: only what
// this server serves, with no inline script and no inline style, in no frame,
// and talking to nothing but this server. An alert's text that somehow got
// rendered as markup could therefore still run nothing.
const ContentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; " +
	"connect-src 'self'; manifest-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// notBuilt is answered in place of the app by a binary that has none.
const notBuilt = `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Beacon</title></head>
<body>
<h1>Beacon</h1>
<p>This binary was built without the web interface. The API works: see /api/v1/openapi.yaml.</p>
<p>To include the web interface, run <code>npm ci</code> and <code>npm run build</code> in the <code>web</code> directory, then build Beacon again. The release image always includes it.</p>
</body>
</html>
`

// file is one file of the build output, read once.
type file struct {
	contentType string
	body        []byte
	// gzipped is the body compressed, or nil when that saves nothing.
	gzipped []byte
	etag    string
}

// Handler serves the app.
type Handler struct {
	files map[string]*file
	// reserved answers the addresses that belong to the API: those must
	// never be answered with the app.
	reserved http.Handler
}

// New returns a handler that serves the build output in dist. reserved
// answers every request under /api/ and /auth/ that reaches the handler,
// which are the requests no API route took: an unknown address there must
// answer as the API does, with JSON and 401 to a stranger, not with a page.
func New(dist fs.FS, reserved http.Handler) (*Handler, error) {
	h := &Handler{files: map[string]*file{}, reserved: reserved}
	err := fs.WalkDir(dist, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		// Files whose name starts with a dot are not part of the app.
		if strings.HasPrefix(path.Base(name), ".") {
			return nil
		}
		body, err := fs.ReadFile(dist, name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		f := &file{contentType: contentType(name), body: body, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
		if compressible(name) {
			var buf bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			if _, err := zw.Write(body); err != nil {
				return err
			}
			if err := zw.Close(); err != nil {
				return err
			}
			if buf.Len() < len(body) {
				f.gzipped = buf.Bytes()
			}
		}
		h.files[name] = f
		return nil
	})
	if err != nil {
		return nil, err
	}
	return h, nil
}

// Built reports whether the binary has the app.
func (h *Handler) Built() bool {
	return h.files[shellName] != nil
}

// Reserved reports whether an address belongs to the API and the sign-in
// routes, and so must never be answered with the app.
func Reserved(urlPath string) bool {
	for _, prefix := range []string{"/api", "/auth"} {
		if urlPath == prefix || strings.HasPrefix(urlPath, prefix+"/") {
			return true
		}
	}
	return false
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if Reserved(r.URL.Path) {
		h.reserved.ServeHTTP(w, r)
		return
	}
	header := w.Header()
	header.Set("Content-Security-Policy", ContentSecurityPolicy)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
	// Addresses of the app name alerts and schedules; a link from an alert
	// to a runbook must not tell the runbook's site where it was followed.
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("Cross-Origin-Opener-Policy", "same-origin")

	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if f, ok := h.files[name]; ok && name != shellName {
		if strings.HasPrefix(name, assetsPrefix) {
			header.Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			header.Set("Cache-Control", "no-cache")
		}
		f.serve(w, r, http.StatusOK)
		return
	}
	// A file that is missing must not be answered with the page: a browser
	// that asked for a script would be given markup.
	if strings.HasPrefix(name+"/", assetsPrefix) {
		header.Set("Cache-Control", "no-store")
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// Every other address is a page of the app. The page is always
	// revalidated, so a new release is picked up at the next visit.
	header.Set("Cache-Control", "no-cache")
	if shell, ok := h.files[shellName]; ok {
		shell.serve(w, r, http.StatusOK)
		return
	}
	// The notice is a page like the app's, with the same status, so that
	// what follows a sign-in does not depend on how the binary was built.
	header.Set("Cache-Control", "no-store")
	(&file{contentType: "text/html; charset=utf-8", body: []byte(notBuilt)}).serve(w, r, http.StatusOK)
}

// serve writes the file, compressed when the client takes that, or answers
// that the client's copy is still good.
func (f *file) serve(w http.ResponseWriter, r *http.Request, status int) {
	header := w.Header()
	header.Set("Content-Type", f.contentType)
	header.Add("Vary", "Accept-Encoding")
	if f.etag != "" {
		header.Set("ETag", f.etag)
		if status == http.StatusOK && r.Header.Get("If-None-Match") == f.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	body := f.body
	if f.gzipped != nil && acceptsGzip(r) {
		header.Set("Content-Encoding", "gzip")
		body = f.gzipped
	}
	header.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		encoding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.TrimSpace(encoding) == "gzip" && strings.ReplaceAll(params, " ", "") != "q=0" {
			return true
		}
	}
	return false
}

// contentType is decided by the file's extension, from a list of what a
// build holds, so that it does not depend on the machine's own table.
func contentType(name string) string {
	switch path.Ext(name) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".json", ".map":
		return "application/json"
	case ".txt":
		return "text/plain; charset=utf-8"
	case ".png":
		return "image/png"
	case ".ico":
		return "image/x-icon"
	case ".woff2":
		return "font/woff2"
	}
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}

func compressible(name string) bool {
	switch path.Ext(name) {
	case ".html", ".js", ".css", ".svg", ".json", ".map", ".txt":
		return true
	}
	return false
}
