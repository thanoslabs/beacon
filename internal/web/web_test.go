package web_test

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/thanoslabs/beacon/internal/web"
)

// built is a build output as Vite writes one.
var built = fstest.MapFS{
	"index.html":              {Data: []byte(`<!doctype html><html><head><script type="module" src="/assets/index-a1b2c3.js"></script></head><body><div id="root"></div></body></html>`)},
	"assets/index-a1b2c3.js":  {Data: []byte(strings.Repeat("console.log('beacon');\n", 200))},
	"assets/index-d4e5f6.css": {Data: []byte("body{margin:0}")},
	"theme.js":                {Data: []byte("document.documentElement.dataset.theme='dark'")},
	".gitkeep":                {Data: []byte("kept")},
}

// theAPI stands for the server's answer to an address of the API that no
// route takes.
var theAPI = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = io.WriteString(w, `{"error":"authentication is required"}`)
})

// answer is what the handler said to a request, with the body read.
type answer struct {
	StatusCode int
	Header     http.Header
	Body       string
}

func get(t *testing.T, h http.Handler, method, target string, headers ...string) answer {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return answer{StatusCode: rec.Code, Header: rec.Header(), Body: rec.Body.String()}
}

func body(_ *testing.T, resp answer) string { return resp.Body }

func newHandler(t *testing.T) *web.Handler {
	t.Helper()
	h, err := web.New(built, theAPI)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Built() {
		t.Fatal("a build output with index.html is not seen as built")
	}
	return h
}

// Every address that is a page of the app is answered with the shell, which
// is never kept without asking again.
func TestPagesAreAnsweredWithTheShell(t *testing.T) {
	h := newHandler(t)
	for _, target := range []string{"/", "/alerts", "/alerts/9b2d6c1e-0a4f-4a57-8a0c-5f3c7d2e1b90", "/schedules/platform-primary?week=2026-12-01", "/no-such-page", "/index.html", "/apiary", "/authors"} {
		resp := get(t, h, "GET", target)
		page := body(t, resp)
		if resp.StatusCode != http.StatusOK || !strings.Contains(page, `<div id="root">`) {
			t.Errorf("GET %s = %d %q, want the shell", target, resp.StatusCode, page)
		}
		if got := resp.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
			t.Errorf("GET %s: Content-Type %q", target, got)
		}
		if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
			t.Errorf("GET %s: Cache-Control %q, want no-cache: a new release must be picked up at the next visit", target, got)
		}
	}
}

// An address of the API that no route takes must answer as the API does. A
// page there would make a client's mistake look like a success.
func TestTheAPIIsNeverAnsweredWithThePage(t *testing.T) {
	h := newHandler(t)
	for _, target := range []string{"/api", "/api/", "/api/v1/no-such-thing", "/api/v2/alerts", "/api/v1/alerts/x/y/z", "/auth", "/auth/", "/auth/logout", "/auth/no-such-thing"} {
		if !web.Reserved(target) {
			t.Errorf("%s is not reserved", target)
		}
		resp := get(t, h, "GET", target)
		answer := body(t, resp)
		if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(answer, "authentication is required") || strings.Contains(answer, "<") {
			t.Errorf("GET %s = %d %q, want the API's own refusal", target, resp.StatusCode, answer)
		}
		if resp.Header.Get("Content-Type") != "application/json" {
			t.Errorf("GET %s: Content-Type %q", target, resp.Header.Get("Content-Type"))
		}
	}
}

// The files with a hash in their name never change and are kept for good; a
// missing one is 404, never the page.
func TestAssets(t *testing.T) {
	h := newHandler(t)
	resp := get(t, h, "GET", "/assets/index-a1b2c3.js")
	if script := body(t, resp); resp.StatusCode != http.StatusOK || !strings.HasPrefix(script, "console.log") {
		t.Fatalf("GET the script = %d %q", resp.StatusCode, script)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/javascript; charset=utf-8" {
		t.Errorf("Content-Type of the script = %q", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control of a hashed file = %q", got)
	}
	resp = get(t, h, "GET", "/assets/index-d4e5f6.css")
	if got := resp.Header.Get("Content-Type"); got != "text/css; charset=utf-8" || body(t, resp) != "body{margin:0}" {
		t.Errorf("Content-Type of the style sheet = %q", got)
	}

	for _, target := range []string{"/assets/index-000000.js", "/assets/", "/assets/../assets/nothing.css"} {
		resp := get(t, h, "GET", target)
		if page := body(t, resp); resp.StatusCode != http.StatusNotFound || strings.Contains(page, "root") {
			t.Errorf("GET %s = %d %q, want 404 and not the page", target, resp.StatusCode, page)
		}
	}

	// A file beside the page has no hash in its name, so it is asked about
	// again each time.
	resp = get(t, h, "GET", "/theme.js")
	if script := body(t, resp); resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-cache" || script == "" {
		t.Errorf("GET /theme.js = %d, Cache-Control %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	// What keeps the directory in git is not served.
	resp = get(t, h, "GET", "/.gitkeep")
	if page := body(t, resp); strings.Contains(page, "kept") {
		t.Errorf("GET /.gitkeep answered the file")
	}
}

// The headers are on everything the handler answers itself: the page, the
// files and a missing file.
func TestSecurityHeaders(t *testing.T) {
	h := newHandler(t)
	for _, target := range []string{"/", "/alerts", "/assets/index-a1b2c3.js", "/assets/missing.js", "/theme.js"} {
		resp := get(t, h, "GET", target)
		csp := resp.Header.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self'", "connect-src 'self'", "frame-ancestors 'none'", "base-uri 'none'", "form-action 'self'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("GET %s: the Content-Security-Policy %q lacks %q", target, csp, want)
			}
		}
		// Nothing may weaken it: no inline script or style, no eval, no
		// other host.
		for _, never := range []string{"unsafe-inline", "unsafe-eval", "http:", "https:", "*"} {
			if strings.Contains(csp, never) {
				t.Errorf("GET %s: the Content-Security-Policy %q has %q", target, csp, never)
			}
		}
		for name, want := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer", "Cross-Origin-Opener-Policy": "same-origin"} {
			if got := resp.Header.Get(name); got != want {
				t.Errorf("GET %s: %s = %q, want %q", target, name, got, want)
			}
		}
	}
}

func TestCompressionAndRevalidation(t *testing.T) {
	h := newHandler(t)
	plain := body(t, get(t, h, "GET", "/assets/index-a1b2c3.js"))

	resp := get(t, h, "GET", "/assets/index-a1b2c3.js", "Accept-Encoding", "br, gzip")
	if resp.Header.Get("Content-Encoding") != "gzip" || !strings.Contains(resp.Header.Get("Vary"), "Accept-Encoding") {
		t.Fatalf("Content-Encoding %q, Vary %q", resp.Header.Get("Content-Encoding"), resp.Header.Get("Vary"))
	}
	zr, err := gzip.NewReader(strings.NewReader(resp.Body))
	if err != nil {
		t.Fatal(err)
	}
	unpacked, err := io.ReadAll(zr)
	if err != nil || string(unpacked) != plain {
		t.Errorf("the compressed script is not the script: %v", err)
	}
	if resp := get(t, h, "GET", "/assets/index-a1b2c3.js", "Accept-Encoding", "gzip;q=0"); resp.Header.Get("Content-Encoding") != "" {
		t.Error("gzip was sent to a client that refuses it")
	}
	// Compressing a few bytes saves nothing.
	if resp := get(t, h, "GET", "/assets/index-d4e5f6.css", "Accept-Encoding", "gzip"); resp.Header.Get("Content-Encoding") != "" {
		t.Error("a file that compression makes larger was compressed")
	}

	etag := get(t, h, "GET", "/").Header.Get("ETag")
	if etag == "" {
		t.Fatal("the page has no ETag")
	}
	resp = get(t, h, "GET", "/alerts", "If-None-Match", etag)
	if page := body(t, resp); resp.StatusCode != http.StatusNotModified || page != "" {
		t.Errorf("revalidating the page = %d %q, want 304", resp.StatusCode, page)
	}
	if resp := get(t, h, "HEAD", "/"); resp.StatusCode != http.StatusOK || body(t, resp) != "" {
		t.Errorf("HEAD / = %d with a body", resp.StatusCode)
	}
}

// A binary built before the app says so, and still gives the API its
// addresses.
func TestWithoutABuild(t *testing.T) {
	h, err := web.New(fstest.MapFS{".gitkeep": {Data: []byte("kept")}}, theAPI)
	if err != nil {
		t.Fatal(err)
	}
	if h.Built() {
		t.Fatal("an empty build output is seen as built")
	}
	resp := get(t, h, "GET", "/alerts")
	if page := body(t, resp); resp.StatusCode != http.StatusOK || !strings.Contains(page, "built without the web interface") || !strings.Contains(page, "npm run build") {
		t.Errorf("GET /alerts = %d %q", resp.StatusCode, page)
	}
	if resp.Header.Get("Content-Security-Policy") == "" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("the notice lacks its headers: %v", resp.Header)
	}
	if resp := get(t, h, "GET", "/api/v1/nothing"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /api/v1/nothing = %d, want the API's refusal", resp.StatusCode)
	}
}

// TestEmbedded checks the app that is in this build of the binary. Without a
// build it is skipped, except in CI, where the app is built first: a release
// without the web interface must not pass.
func TestEmbedded(t *testing.T) {
	h, err := web.New(web.Dist(), theAPI)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Built() {
		if os.Getenv("CI") != "" {
			t.Fatal("the web interface is not built; in CI it is built before the Go code, so that the binary under test is the one that is released")
		}
		t.Skip("the web interface is not built (npm run build, in web/); skipping the test of the embedded app")
	}
	page := body(t, get(t, h, "GET", "/"))
	if !strings.Contains(page, `<div id="root">`) {
		t.Fatalf("the embedded page has no root element: %q", page)
	}
	// The policy allows no inline script and no other host, so the page
	// must need neither.
	if strings.Contains(page, "<script>") || strings.Contains(page, "<style") || strings.Contains(page, "http://") || strings.Contains(page, "https://") {
		t.Errorf("the embedded page has an inline script or style, or names another host:\n%s", page)
	}
	// Every file the page names is in the binary.
	for _, part := range strings.Split(page, `"`) {
		if !strings.HasPrefix(part, "/assets/") && !strings.HasSuffix(part, ".js") && !strings.HasSuffix(part, ".svg") {
			continue
		}
		if !strings.HasPrefix(part, "/") {
			continue
		}
		if resp := get(t, h, "GET", part); resp.StatusCode != http.StatusOK {
			t.Errorf("the page names %s, which the binary answers with %d", part, resp.StatusCode)
		}
	}
}
