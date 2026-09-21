package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// The adoption promise: with no theme selected the dashboard is unchanged.

// sessionCookie performs a real login so the dashboard routes are reachable.
func sessionCookie(t *testing.T, mux http.Handler) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest("POST", "/login", strings.NewReader("token="+remoteToken))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "192.0.2.1:12345"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == remoteSessionCookie {
			return c
		}
	}
	t.Fatalf("login did not issue a session cookie (status %d)", rec.Code)
	return nil
}

func TestDefaultThemeRendersBuiltInLook(t *testing.T) {
	themeSlug = themeNone
	if got := currentTheme(); got != themeNone {
		t.Fatalf("currentTheme() = %q, want %q", got, themeNone)
	}
	mux := newRemoteMux()
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(sessionCookie(t, mux))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	body := rr.Body.String()
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	if !strings.Contains(body, `<html data-theme="none">`) {
		t.Error("expected data-theme=none on <html>")
	}
	if !strings.Contains(body, `<link id="themecss" rel="stylesheet"></head>`) {
		t.Error("expected a theme <link> with NO href when no theme is selected: href=\"\" would make the browser fetch the page itself as CSS")
	}
	// The bridge must keep every original literal as its fallback.
	for _, want := range []string{
		"--glow:var(--ftl-accent,#00d9ff)",
		"--panel:var(--ftl-surface,#0a1526)",
		"--border:var(--ftl-border,#0f3a5c)",
		"--text:var(--ftl-text,#cfeeff)",
		"--dim:var(--ftl-muted,#5b8aa8)",
		"--rec:var(--ftl-danger,#ff3355)",
		"--idle:var(--ftl-success,#2bffb0)",
		"--orange:var(--ftl-warning,#ff8c1a)",
		"--meter-h:120px",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("bridge lost %q", want)
		}
	}
}

func TestThemeBundleServedAndUnknownRejected(t *testing.T) {
	mux := newRemoteMux()
	cookie := sessionCookie(t, mux)
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/static/themes/lcars.css", http.StatusOK},
		{"/static/themes/blue-future.css", http.StatusOK},
		{"/static/themes/nope.css", http.StatusNotFound},
		{"/static/themes/none.css", http.StatusNotFound},
		{"/static/assets/fonts/Antonio-Bold.woff2", http.StatusOK},
		{"/static/assets/fonts/evil.txt", http.StatusNotFound},
	} {
		req := httptest.NewRequest("GET", tc.path, nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != tc.want {
			t.Errorf("%s = %d, want %d", tc.path, rr.Code, tc.want)
		}
	}
	// A bundle must resolve its font as a sibling of the themes dir.
	req := httptest.NewRequest("GET", "/static/themes/lcars.css", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), "../assets/fonts/Antonio-Regular.woff2") {
		t.Error("lcars bundle should reference ../assets/fonts/…")
	}
}

func TestSelectingThemeLinksItAndPersists(t *testing.T) {
	defer func() { themeSlug = themeNone }()
	themeSlug = "lcars"
	if got := currentTheme(); got != "lcars" {
		t.Fatalf("currentTheme() = %q", got)
	}
	mux := newRemoteMux()
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(sessionCookie(t, mux))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	body := rr.Body.String()
	if !strings.Contains(body, `<html data-theme="lcars">`) {
		t.Error("expected data-theme=lcars")
	}
	if !strings.Contains(body, `href="/static/themes/lcars.css"`) {
		t.Error("expected the lcars bundle to be linked")
	}
	// An unknown persisted slug must fall back to the built-in look.
	themeSlug = "removed-theme"
	if got := currentTheme(); got != themeNone {
		t.Errorf("unknown slug should fall back to %q, got %q", themeNone, got)
	}
}

// The user-facing action: POST the picker and get back both the refreshed
// setting row and an out-of-band <link> swap, so the theme applies without a
// reload that would drop the live meter and telemetry sockets.
func TestThemePostSwapsStylesheetOutOfBand(t *testing.T) {
	defer func() { themeSlug = themeNone }()
	themeSlug = themeNone
	mux := newRemoteMux()
	cookie := sessionCookie(t, mux)

	// index 4 is LCARS in the manifest order asserted by the picker markup.
	idx := -1
	for i, th := range availableThemes() {
		if th.Slug == "lcars" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatal("lcars missing from the manifest")
	}

	req := httptest.NewRequest("POST", "/api/settings/theme",
		strings.NewReader("idx="+strconv.Itoa(idx)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `<div id="theme" class="setting-cell">`) {
		t.Error("expected the setting row back for the hx-swap target")
	}
	if !strings.Contains(body, `href="/static/themes/lcars.css"`) ||
		!strings.Contains(body, `hx-swap-oob="outerHTML"`) {
		t.Errorf("expected an OOB stylesheet swap, got:\n%s", body)
	}
	if themeSlug != "lcars" {
		t.Errorf("themeSlug = %q, want lcars", themeSlug)
	}

	// Switching back must drop the href entirely, not emit href="".
	req = httptest.NewRequest("POST", "/api/settings/theme", strings.NewReader("idx=0"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if strings.Contains(rr.Body.String(), `href=""`) {
		t.Error(`switching back to the built-in look must omit href, not emit href=""`)
	}
	if themeSlug != themeNone {
		t.Errorf("themeSlug = %q, want %q", themeSlug, themeNone)
	}
}

func TestLoginPageCarriesActiveTheme(t *testing.T) {
	defer func() { themeSlug = themeNone }()
	mux := newRemoteMux()

	// Unthemed: marker "none", no theme stylesheet.
	themeSlug = themeNone
	req := httptest.NewRequest("GET", "/login", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	body := rr.Body.String()
	if !strings.Contains(body, `<html data-theme="none">`) {
		t.Error("login page should carry data-theme=none when unthemed")
	}
	if strings.Contains(body, `/static/themes/`) {
		t.Error("login page must not link a theme stylesheet when unthemed")
	}

	// Themed: marker + bundle link, so --ftl-* tokens the page reads resolve.
	themeSlug = "blue-future"
	if currentTheme() != "blue-future" {
		t.Skip("blue-future bundle not embedded (submodule uninitialized?)")
	}
	req = httptest.NewRequest("GET", "/login", nil)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	body = rr.Body.String()
	if !strings.Contains(body, `<html data-theme="blue-future">`) {
		t.Error("login page should carry the active theme marker")
	}
	if !strings.Contains(body, `href="/static/themes/blue-future.css"`) {
		t.Error("login page should link the active theme bundle")
	}
}

func TestDashboardCarriesAppShell(t *testing.T) {
	mux := newRemoteMux()
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(sessionCookie(t, mux))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	body := rr.Body.String()
	// Shell slots (ftl-themes#3): dual-classed so the built-in look keeps
	// matching its own selectors while layout themes gain regions.
	for _, want := range []string{
		`<body class="ftl-app">`,
		`class="deck ftl-app-bar"`,
		`<aside class="ftl-app-rail" aria-hidden="true"></aside>`,
		`<main class="ftl-app-main">`,
		`class="meter-footer ftl-app-status"`,
		`main.ftl-app-main{display:contents}`,
		`.ftl-app-rail:empty{display:none}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing shell hook %q", want)
		}
	}
}

func TestPreviewRendersWithoutPersisting(t *testing.T) {
	defer func() { themeSlug = themeNone }()
	themeSlug = themeNone
	mux := newRemoteMux()
	cookie := sessionCookie(t, mux)

	get := func(target string) string {
		req := httptest.NewRequest("GET", target, nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", target, rr.Code)
		}
		return rr.Body.String()
	}

	body := get("/?preview=blue-future")
	if !strings.Contains(body, `<html data-theme="blue-future">`) ||
		!strings.Contains(body, `href="/static/themes/blue-future.css"`) {
		t.Error("preview should render the requested bundle")
	}
	if themeSlug != themeNone {
		t.Errorf("preview must not persist: themeSlug = %q", themeSlug)
	}
	body = get("/?preview=nope")
	if !strings.Contains(body, `<html data-theme="none">`) {
		t.Error("unknown preview slug should fall back to the persisted theme")
	}
}

func TestLayoutBridgeKeepsBuiltInGeometry(t *testing.T) {
	themeSlug = themeNone
	mux := newRemoteMux()
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(sessionCookie(t, mux))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	body := rr.Body.String()
	// Layout bridge: identical-fallback vars themes may turn. Unthemed,
	// every fallback is the long-standing geometry.
	for _, want := range []string{
		`grid-template-columns:var(--pi-columns,1fr 1.6fr 1fr)`,
		`gap:var(--pi-gap,1.2em)`,
		`flex-direction:var(--pi-deck-dir,row)`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("layout bridge lost %q", want)
		}
	}
}
