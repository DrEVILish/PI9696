package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
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

func TestDefaultThemeIsFTL(t *testing.T) {
	themeSlug = defaultThemeSlug
	if got := currentTheme(); got != defaultThemeSlug {
		t.Fatalf("currentTheme() = %q, want %q", got, defaultThemeSlug)
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
	if !strings.Contains(body, `<html data-theme="`+defaultThemeSlug+`">`) {
		t.Errorf("expected data-theme=%s on <html>", defaultThemeSlug)
	}
	if !strings.Contains(body, `href="/static/themes/`+defaultThemeSlug+`.css?v=`) {
		t.Errorf("expected the %s bundle to be linked", defaultThemeSlug)
	}
	// App-owned custom properties are the meter's geometry and its
	// green/yellow/red bands. v4's .meter-fill consumes them.
	for _, want := range []string{
		"--meter-h:120px",
		"--meter-low:var(--success,#0aff9d)",
		"--meter-mid:var(--warning,#ffe400)",
		"--meter-high:var(--danger,#ff2a2a)",
		"background:var(--surface-2,#08192b)",
		"background:var(--input-bg,#08192b)",
		"box-shadow:var(--panel-shadow,0 0 20px rgba(0,180,255,0.08)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("lost %q", want)
		}
	}

	// The dashboard must not re-declare any custom property that ftl-themes
	// itself declares. v4 dropped the ftl- prefix, so the app's old bridge
	// aliases (--glow, --panel, --dim, --rec, --idle, --orange) became
	// --accent, --surface, --muted, --danger, --success, --warning and
	// silently overrode the theme; its --text and --border aliases became
	// self-referential and resolved to nothing. Reading the theme's tokens
	// directly is the fix, and this is what keeps it fixed.
	core := embeddedThemeCSS(t)
	themeOwned := map[string]bool{}
	for _, m := range regexp.MustCompile(`(--[a-z0-9-]+)\s*:`).FindAllStringSubmatch(core, -1) {
		themeOwned[m[1]] = true
	}
	if len(themeOwned) == 0 {
		t.Fatal("parsed no custom properties out of the ftl-themes core bundle")
	}
	for _, m := range regexp.MustCompile(`(--[a-z0-9-]+)\s*:`).FindAllStringSubmatch(body, -1) {
		if themeOwned[m[1]] {
			t.Errorf("dashboard declares %s, which ftl-themes also declares - it will override the theme", m[1])
		}
	}
}

// embeddedThemeCSS returns the ftl-themes core bundle the app embeds, so a
// test can compare the app's own declarations against what the library ships.
func embeddedThemeCSS(t *testing.T) string {
	t.Helper()
	data, err := embeddedThemes.ReadFile("third_party/ftl-themes/dist/core.css")
	if err != nil {
		t.Fatalf("read embedded core.css: %v", err)
	}
	return string(data)
}

func TestThemeBundleServedAndUnknownRejected(t *testing.T) {
	mux := newRemoteMux()
	cookie := sessionCookie(t, mux)
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/static/themes/lcars.css", http.StatusOK},
		{"/static/themes/matrix.css", http.StatusOK},
		{"/static/themes/core.css", http.StatusOK},
		{"/static/themes/icons/generic.svg", http.StatusOK},
		{"/static/themes/icons/xbmc.svg", http.StatusOK},
		{"/static/themes/icons/windows95.svg", http.StatusOK},
		{"/static/themes/icons/nope.svg", http.StatusNotFound},
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
	defer func() { themeSlug = defaultThemeSlug }()
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
	if !strings.Contains(body, `href="/static/themes/lcars.css?v=`) {
		t.Error("expected the lcars bundle to be linked")
	}
	// An unknown persisted slug must fall back to the default theme.
	themeSlug = "removed-theme"
	if got := currentTheme(); got != defaultThemeSlug {
		t.Errorf("unknown slug should fall back to %q, got %q", defaultThemeSlug, got)
	}
}

// The user-facing action: POST the picker and get back both the refreshed
// setting row and an out-of-band <link> swap, so the theme applies without a
// reload that would drop the live meter and telemetry sockets.
func TestThemePostSwapsStylesheetOutOfBand(t *testing.T) {
	defer func() { themeSlug = defaultThemeSlug }()
	themeSlug = defaultThemeSlug
	mux := newRemoteMux()
	cookie := sessionCookie(t, mux)

	// Resolve LCARS's picker index from the manifest (order is upstream's).
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
	if !strings.Contains(body, `href="/static/themes/lcars.css?v=`) ||
		!strings.Contains(body, `hx-swap-oob="outerHTML"`) {
		t.Errorf("expected an OOB stylesheet swap, got:\n%s", body)
	}
	if !strings.Contains(body, `teleCPU=teleRAM=teleTemp=teleDisk=telePalette=null`) {
		t.Error("theme swap should drop charts so the next push rebuilds them in the new palette")
	}
	if themeSlug != "lcars" {
		t.Errorf("themeSlug = %q, want lcars", themeSlug)
	}

	// Switching back to idx 0 links a real bundle, never href="".
	req = httptest.NewRequest("POST", "/api/settings/theme", strings.NewReader("idx=0"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if strings.Contains(rr.Body.String(), `href=""`) {
		t.Error(`switching back must link a bundle, not emit href=""`)
	}
	first := availableThemes()[0].Slug
	if themeSlug != first {
		t.Errorf("themeSlug = %q, want %q", themeSlug, first)
	}
}

func TestLoginPageCarriesActiveTheme(t *testing.T) {
	defer func() { themeSlug = defaultThemeSlug }()
	mux := newRemoteMux()

	// Default: marker + bundle link, so --ftl-* tokens the page reads resolve.
	themeSlug = defaultThemeSlug
	req := httptest.NewRequest("GET", "/login", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	body := rr.Body.String()
	if !strings.Contains(body, `<html data-theme="`+defaultThemeSlug+`">`) {
		t.Errorf("login page should carry data-theme=%s", defaultThemeSlug)
	}
	if !strings.Contains(body, `href="/static/themes/`+defaultThemeSlug+`.css?v=`) {
		t.Errorf("login page should link the %s bundle", defaultThemeSlug)
	}

	// Switching persists: marker + bundle link follow the new theme.
	themeSlug = "matrix"
	if currentTheme() != "matrix" {
		t.Skip("matrix bundle not embedded (submodule uninitialized?)")
	}
	req = httptest.NewRequest("GET", "/login", nil)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	body = rr.Body.String()
	if !strings.Contains(body, `<html data-theme="matrix">`) {
		t.Error("login page should carry the active theme marker")
	}
	if !strings.Contains(body, `href="/static/themes/matrix.css?v=`) {
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
		`<body class="app">`,
		`class="deck app-bar"`,
		`<aside class="app-rail" aria-hidden="true"></aside>`,
		`<main class="app-main">`,
		`class="meter-footer app-status"`,
		`main.app-main{display:contents}`,
		`.app-rail:empty{display:none}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing shell hook %q", want)
		}
	}
}

func TestPreviewRendersWithoutPersisting(t *testing.T) {
	defer func() { themeSlug = defaultThemeSlug }()
	themeSlug = defaultThemeSlug
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

	body := get("/?preview=matrix")
	if !strings.Contains(body, `<html data-theme="matrix">`) ||
		!strings.Contains(body, `href="/static/themes/matrix.css?v=`) {
		t.Error("preview should render the requested bundle")
	}
	if themeSlug != defaultThemeSlug {
		t.Errorf("preview must not persist: themeSlug = %q", themeSlug)
	}
	body = get("/?preview=nope")
	if !strings.Contains(body, `<html data-theme="`+defaultThemeSlug+`">`) {
		t.Error("unknown preview slug should fall back to the persisted theme")
	}
}

func TestLayoutBridgeKeepsBuiltInGeometry(t *testing.T) {
	themeSlug = defaultThemeSlug
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

func TestUnknownPersistedThemeFallsBackToDefault(t *testing.T) {
	defer func() { themeSlug = defaultThemeSlug }()
	// A theme removed upstream (e.g. winxp-zune) must degrade to the
	// default theme, never to a broken page or a stale link.
	themeSlug = "winxp-zune"
	if got := currentTheme(); got != defaultThemeSlug {
		t.Fatalf("currentTheme() = %q for a removed slug, want %q", got, defaultThemeSlug)
	}
	mux := newRemoteMux()
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(sessionCookie(t, mux))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	body := rr.Body.String()
	if !strings.Contains(body, `<html data-theme="`+defaultThemeSlug+`">`) {
		t.Errorf("removed persisted theme should render data-theme=%s", defaultThemeSlug)
	}
	if !strings.Contains(body, `href="/static/themes/`+defaultThemeSlug+`.css?v=`) {
		t.Errorf("removed persisted theme should link the %s bundle", defaultThemeSlug)
	}
	if !strings.Contains(body, `/static/themes/icons/`+defaultThemeSlug+`.svg`) {
		t.Errorf("dashboard should reference the %s icon sprite", defaultThemeSlug)
	}
}

func TestDualClassMarkupPresent(t *testing.T) {
	themeSlug = defaultThemeSlug
	mux := newRemoteMux()
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(sessionCookie(t, mux))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	body := rr.Body.String()
	// Dual-class hooks: inert under none (no bundle linked), styled by
	// the theme bundle whenever one is active. If a hook is dropped from
	// the markup, that surface silently stops theming.
	for _, want := range []string{
		`class="field-row"`,
		`class="switch"`,
		`class="panel left panel"`,
		`class="icon-btn btn btn-icon"`,
		`'transport-row transport'`,
		`is-pause`,
		`is-play`,
		`class="tabs settings-tabs"`,
		`data-pane="pane-display"`,
		`settings-pane`,
		`class="input-group"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard lost dual-class hook %q", want)
		}
	}
}
