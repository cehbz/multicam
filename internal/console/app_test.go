package console

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image/png"
	"net/http"
	"strings"
	"testing"
)

// served answers a GET of target from the console for one camera, failing
// unless it is 200 with Content-Type ct and Cache-Control cc.
func served(t *testing.T, target, ct, cc string) string {
	t.Helper()
	_, _, console := oneBody(t)
	rec := get(console, target)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d", target, rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != ct {
		t.Errorf("GET %s: Content-Type %q, want %q", target, got, ct)
	}
	if got := rec.Header().Get("Cache-Control"); got != cc {
		t.Errorf("GET %s: Cache-Control %q, want %q", target, got, cc)
	}
	return rec.Body.String()
}

func TestPageIsNeverCached(t *testing.T) {
	served(t, "/", "text/html; charset=utf-8", "no-store")
}

func TestPageLinksTheManifestAndTheAppScript(t *testing.T) {
	page := served(t, "/", "text/html; charset=utf-8", "no-store")
	head, _, ok := strings.Cut(page, "<header>")
	if !ok {
		t.Fatalf("page has no <header>: %q", page)
	}
	for _, want := range []string{
		`<link rel="manifest" href="/manifest.webmanifest">`,
		`<script src="/app.js" defer></script>`,
	} {
		if !strings.Contains(head, want) {
			t.Errorf("page's head lacks %s: %q", want, head)
		}
	}
}

// manifest is the part of the web app manifest Chrome installs from.
type manifest struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ShortName   string `json:"short_name"`
	StartURL    string `json:"start_url"`
	Scope       string `json:"scope"`
	Display     string `json:"display"`
	Orientation string `json:"orientation"`
	Icons       []struct {
		Src     string `json:"src"`
		Sizes   string `json:"sizes"`
		Type    string `json:"type"`
		Purpose string `json:"purpose"`
	} `json:"icons"`
}

func TestManifestInstallsAFullscreenAppOpeningTheConsole(t *testing.T) {
	body := served(t, "/manifest.webmanifest", "application/manifest+json", "no-cache")
	var m manifest
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("manifest: %v: %q", err, body)
	}
	if m.Name != "multicam" || m.ShortName != "multicam" {
		t.Errorf("name %q, short name %q; want multicam", m.Name, m.ShortName)
	}
	if m.ID != "/" || m.StartURL != "/" || m.Scope != "/" {
		t.Errorf("id %q, start_url %q, scope %q; want /", m.ID, m.StartURL, m.Scope)
	}
	if m.Display != "fullscreen" {
		t.Errorf("display %q, want fullscreen", m.Display)
	}
	if m.Orientation != "" {
		t.Errorf("orientation locked to %q", m.Orientation)
	}
	sizes := map[string]bool{}
	for _, icon := range m.Icons {
		if icon.Type != "image/png" {
			t.Errorf("icon %s: type %q, want image/png", icon.Src, icon.Type)
		}
		if !strings.Contains(icon.Purpose, "maskable") {
			t.Errorf("icon %s: purpose %q, want maskable", icon.Src, icon.Purpose)
		}
		sizes[icon.Sizes] = true
	}
	for _, want := range []string{"192x192", "512x512"} {
		if !sizes[want] {
			t.Errorf("manifest has no %s icon: %q", want, body)
		}
	}
}

func TestEachIconIsAPNGOfItsSize(t *testing.T) {
	var m manifest
	if err := json.Unmarshal([]byte(served(t, "/manifest.webmanifest", "application/manifest+json", "no-cache")), &m); err != nil {
		t.Fatal(err)
	}
	for _, icon := range m.Icons {
		img, err := png.Decode(bytes.NewReader([]byte(served(t, icon.Src, "image/png", "no-cache"))))
		if err != nil {
			t.Fatalf("icon %s: %v", icon.Src, err)
		}
		b := img.Bounds()
		if got := fmt.Sprintf("%dx%d", b.Dx(), b.Dy()); got != icon.Sizes {
			t.Errorf("icon %s is %s, manifest says %s", icon.Src, got, icon.Sizes)
		}
	}
}

func TestAppScriptRegistersTheWorkerAndHoldsTheScreenWhileCharging(t *testing.T) {
	js := served(t, "/app.js", "text/javascript; charset=utf-8", "no-cache")
	for _, want := range []string{
		// The worker is fetched past the HTTP cache at every update check.
		"navigator.serviceWorker.register('/sw.js', {updateViaCache: 'none'})",
		// The lock follows the battery's charging and the page's visibility.
		"navigator.getBattery()", "chargingchange", "visibilitychange",
		"navigator.wakeLock.request('screen')", ".release()",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app script lacks %s: %q", want, js)
		}
	}
}

func TestWorkerAnswersAFailedNavigationWithTheOfflinePage(t *testing.T) {
	js := served(t, "/sw.js", "text/javascript; charset=utf-8", "no-cache")
	for _, want := range []string{
		// Only navigations are answered, from the network, the offline page
		// when the network fails.
		"request.mode !== 'navigate'", "fetch(e.request).catch(",
		// A new worker takes over the open console at once.
		"skipWaiting()", "clients.claim()",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("worker lacks %s: %q", want, js)
		}
	}
	// Nothing goes in the Cache API: the offline page is the worker's own.
	if strings.Contains(js, "caches") {
		t.Errorf("worker uses the Cache API: %q", js)
	}
	page, err := json.Marshal(offlinePage)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(js, "const offline = "+string(page)+";") {
		t.Errorf("worker does not carry the offline page: %q", js)
	}
}

func TestOfflinePageLoadsTheConsoleOnceTheServerAnswers(t *testing.T) {
	for _, want := range []string{"<title>multicam</title>", "fetch(location.href, {cache: 'no-store'})", "location.reload()", "2000"} {
		if !strings.Contains(offlinePage, want) {
			t.Errorf("offline page lacks %s: %q", want, offlinePage)
		}
	}
}
