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

	// Resolve LCARS's picker index (manifest order, variants beneath their theme).
	idx := themeChoiceIndex("lcars", "")
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
	if !strings.Contains(body, `teleCPU=teleRAM=teleTemp=teleDisk=teleSub=telePalette=null`) {
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

// Palette variants (CONTRACT.md): offered beneath their theme, rendered as
// <html data-variant>, persisted beside the theme and only ever one the
// theme lists.
func postTheme(t *testing.T, mux http.Handler, cookie *http.Cookie, idx int) string {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/settings/theme", strings.NewReader("idx="+strconv.Itoa(idx)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST theme idx=%d: status %d", idx, rr.Code)
	}
	return rr.Body.String()
}

func TestThemePickerOffersVariantsBeneathTheirTheme(t *testing.T) {
	defer func() { themeSlug, themeVariant = defaultThemeSlug, "" }()
	themeSlug, themeVariant = defaultThemeSlug, ""
	var buf strings.Builder
	themeFragmentTmpl.Execute(&buf, themePicker())
	html := buf.String()
	// LCARS has variants: an optgroup holding its own palette, then each variant.
	group := regexp.MustCompile(`(?s)<optgroup label="LCARS">(.*?)</optgroup>`).FindStringSubmatch(html)
	if group == nil {
		t.Fatalf("no LCARS optgroup in picker:\n%s", html)
	}
	labels := regexp.MustCompile(`>([^<]+)</option>`).FindAllStringSubmatch(group[1], -1)
	got := []string{}
	for _, l := range labels {
		got = append(got, l[1])
	}
	if strings.Join(got, "|") != "LCARS|Voyager / DS9|Picard (25th century)" {
		t.Errorf("LCARS group options = %v", got)
	}
	// A theme without variants stays a plain option, and every choice is listed once.
	if strings.Contains(html, `<optgroup label="Matrix">`) {
		t.Error("a theme without variants must not get an optgroup")
	}
	if n := strings.Count(html, "<option "); n != len(themeChoices()) {
		t.Errorf("%d options for %d choices", n, len(themeChoices()))
	}
	// The selected option is the active (theme, variant).
	themeSlug, themeVariant = "lcars", "picard"
	buf.Reset()
	themeFragmentTmpl.Execute(&buf, themePicker())
	want := `<option value="` + strconv.Itoa(themeChoiceIndex("lcars", "picard")) + `" selected>Picard (25th century)</option>`
	if !strings.Contains(buf.String(), want) {
		t.Errorf("picker should select the active variant, want %s", want)
	}
}

func TestSelectingVariantRendersAndSwapsDataVariant(t *testing.T) {
	defer func() { themeSlug, themeVariant = defaultThemeSlug, "" }()
	themeSlug, themeVariant = defaultThemeSlug, ""
	mux := newRemoteMux()
	cookie := sessionCookie(t, mux)

	// Theme and variant in one choice: new bundle plus data-variant.
	body := postTheme(t, mux, cookie, themeChoiceIndex("lcars", "voyager"))
	if themeSlug != "lcars" || themeVariant != "voyager" {
		t.Fatalf("state = %q/%q, want lcars/voyager", themeSlug, themeVariant)
	}
	if !strings.Contains(body, `href="/static/themes/lcars.css?v=`) ||
		!strings.Contains(body, `document.documentElement.setAttribute("data-variant","voyager")`) {
		t.Errorf("expected a bundle swap carrying data-variant, got:\n%s", body)
	}
	// Same theme, other variant: no stylesheet swap, only the attribute.
	body = postTheme(t, mux, cookie, themeChoiceIndex("lcars", "picard"))
	if strings.Contains(body, `id="themecss"`) {
		t.Error("switching variant within a theme must not relink the bundle")
	}
	if !strings.Contains(body, `document.documentElement.setAttribute("data-variant","picard")`) {
		t.Errorf("expected a data-variant update, got:\n%s", body)
	}
	// Back to the theme's own palette removes the attribute.
	body = postTheme(t, mux, cookie, themeChoiceIndex("lcars", ""))
	if !strings.Contains(body, `document.documentElement.removeAttribute("data-variant")`) || themeVariant != "" {
		t.Errorf("own palette should remove data-variant (variant=%q):\n%s", themeVariant, body)
	}
	// Another theme drops the variant, also on the page.
	postTheme(t, mux, cookie, themeChoiceIndex("lcars", "voyager"))
	body = postTheme(t, mux, cookie, themeChoiceIndex("matrix", ""))
	if themeVariant != "" || !strings.Contains(body, `removeAttribute("data-variant")`) {
		t.Errorf("a theme change must clear the variant (variant=%q)", themeVariant)
	}

	// The dashboard renders the persisted variant on <html>.
	themeSlug, themeVariant = "lcars", "voyager"
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), `<html data-theme="lcars" data-variant="voyager">`) {
		t.Error(`dashboard should open with <html data-theme="lcars" data-variant="voyager">`)
	}
}

func TestVariantMustBelongToItsTheme(t *testing.T) {
	defer func() { themeSlug, themeVariant = defaultThemeSlug, "" }()
	// A stale variant (theme changed elsewhere, or dropped upstream) is ignored.
	themeSlug, themeVariant = "matrix", "voyager"
	if v := currentThemeVariant(); v != "" {
		t.Errorf("currentThemeVariant() = %q for a variant matrix doesn't list", v)
	}
	if got := displayHTMLTag("matrix", currentThemeVariant(), ""); strings.Contains(string(got), "data-variant") {
		t.Errorf("tag %s should carry no data-variant", got)
	}
	if themeHasVariant("lcars", "") || !themeHasVariant("lcars", "picard") || themeHasVariant("nope", "picard") {
		t.Error("themeHasVariant mismatch")
	}
}

func TestPreviewVariantWithoutPersisting(t *testing.T) {
	defer func() { themeSlug, themeVariant = defaultThemeSlug, "" }()
	themeSlug, themeVariant = defaultThemeSlug, ""
	mux := newRemoteMux()
	cookie := sessionCookie(t, mux)
	get := func(target string) string {
		req := httptest.NewRequest("GET", target, nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		return rr.Body.String()
	}
	if !strings.Contains(get("/?preview=winxp-luna&variant=olive"), `<html data-theme="winxp-luna" data-variant="olive">`) {
		t.Error("preview should render the requested variant")
	}
	if !strings.Contains(get("/?preview=winxp-luna&variant=voyager"), `<html data-theme="winxp-luna">`) {
		t.Error("a variant of another theme must not be previewed")
	}
	if themeSlug != defaultThemeSlug || themeVariant != "" {
		t.Errorf("preview must not persist: %q/%q", themeSlug, themeVariant)
	}
}

// Theme tint (CONTRACT.md, required when a theme declares one): a colour
// control beside the theme choice, rendered as an inline token on <html>,
// stored per theme, cleared by choosing a preset, hidden otherwise.
func resetThemeState() {
	mutex.Lock()
	themeSlug, themeVariant, themeTints = defaultThemeSlug, "", map[string]string{}
	displayDensityIdx = 0
	mutex.Unlock()
}

func postForm(t *testing.T, mux http.Handler, cookie *http.Cookie, path, form string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

func dashboardHTML(t *testing.T, mux http.Handler, cookie *http.Cookie, target string) string {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr.Body.String()
}

func TestTintControlOnlyForThemesThatDeclareOne(t *testing.T) {
	defer resetThemeState()
	resetThemeState()
	if themeTintSpec("win7-aero") == nil || themeTintSpec("matrix") != nil {
		t.Fatal("manifest: win7-aero should declare a tint and matrix none")
	}
	mux := newRemoteMux()
	cookie := sessionCookie(t, mux)
	themeSlug = "matrix"
	if body := dashboardHTML(t, mux, cookie, "/"); !strings.Contains(body, `<div id="tint" hidden></div>`) || strings.Contains(body, `id="tintcolor"`) {
		t.Error("a theme without tint must hide the control (keeping the swap target)")
	}
	themeSlug = "win7-aero"
	body := dashboardHTML(t, mux, cookie, "/")
	for _, want := range []string{
		`<label class="label" for="tintcolor">Window Color</label>`,
		`type="color" name="tint" value="#74b8fc"`,
		`hx-post="/api/settings/tint"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("win7-aero dashboard missing %s", want)
		}
	}
	if !regexp.MustCompile(`oninput="document.documentElement.style.setProperty\((&#34;|&quot;|")--aero-tint`).MatchString(body) {
		t.Error("the colour input should preview the tint token live")
	}
}

func TestTintStoredPerThemeAndRendered(t *testing.T) {
	defer resetThemeState()
	resetThemeState()
	mux := newRemoteMux()
	cookie := sessionCookie(t, mux)
	themeSlug = "win7-aero"

	code, body := postForm(t, mux, cookie, "/api/settings/tint", "tint=%236E3BA1")
	if code != http.StatusOK || themeTints["win7-aero"] != "#6e3ba1" {
		t.Fatalf("status %d, stored %q", code, themeTints["win7-aero"])
	}
	if !strings.Contains(body, `document.documentElement.style.setProperty("--aero-tint","#6e3ba1")`) {
		t.Errorf("response should apply the tint at once:\n%s", body)
	}
	if !strings.Contains(body, `value="#6e3ba1"`) || !strings.Contains(body, `name="reset"`) {
		t.Error("cell should show the custom colour and a Default button")
	}
	// Inline on <html>, sharing one style attribute with density.
	displayDensityIdx = 1
	if got := dashboardHTML(t, mux, cookie, "/"); !strings.Contains(got, `<html data-theme="win7-aero" style="--density:0.85;--aero-tint:#6e3ba1">`) {
		t.Error("dashboard should render the tint token inline on <html> beside density")
	}
	displayDensityIdx = 0

	// Malformed colours and themes without a tint are refused.
	for _, form := range []string{"tint=red", "tint=%23abc", "tint=%236e3ba1%22onload", "tint="} {
		if code, _ := postForm(t, mux, cookie, "/api/settings/tint", form); code != http.StatusBadRequest {
			t.Errorf("%s accepted (status %d)", form, code)
		}
	}
	themeSlug = "matrix"
	if code, _ := postForm(t, mux, cookie, "/api/settings/tint", "tint=%23112233"); code != http.StatusBadRequest {
		t.Error("a theme without tint must refuse one")
	}
	if strings.Contains(dashboardHTML(t, mux, cookie, "/"), "--aero-tint") {
		t.Error("a theme without tint must not render another theme's token")
	}
	// Reset clears the custom colour.
	themeSlug = "win7-aero"
	_, body = postForm(t, mux, cookie, "/api/settings/tint", "reset=1")
	if _, ok := themeTints["win7-aero"]; ok || !strings.Contains(body, `removeProperty("--aero-tint")`) || strings.Contains(body, `setProperty("--aero-tint"`) {
		t.Errorf("reset should clear the stored tint and the inline token:\n%s", body)
	}
}

func TestTintFollowsThemeAndPresetClearsIt(t *testing.T) {
	defer resetThemeState()
	resetThemeState()
	mux := newRemoteMux()
	cookie := sessionCookie(t, mux)

	// Switching to the tinted theme shows the control and applies its colour.
	themeTints["win7-aero"] = "#6e3ba1"
	_, body := postForm(t, mux, cookie, "/api/settings/theme", "idx="+strconv.Itoa(themeChoiceIndex("win7-aero", "")))
	if !strings.Contains(body, `<div id="tint" class="setting-cell" hx-swap-oob="outerHTML">`) ||
		!strings.Contains(body, `setProperty("--aero-tint","#6e3ba1")`) {
		t.Errorf("switching to win7-aero should swap in the tint control and apply the stored colour:\n%s", body)
	}
	// Leaving it hides the control and drops the token; the colour stays stored.
	_, body = postForm(t, mux, cookie, "/api/settings/theme", "idx="+strconv.Itoa(themeChoiceIndex("matrix", "")))
	if !strings.Contains(body, `<div id="tint" hidden hx-swap-oob="outerHTML"></div>`) ||
		!strings.Contains(body, `removeProperty("--aero-tint")`) || strings.Contains(body, `setProperty("--aero-tint"`) {
		t.Errorf("leaving win7-aero should hide the control and drop the token:\n%s", body)
	}
	if themeTints["win7-aero"] != "#6e3ba1" {
		t.Error("the colour is stored per theme and survives switching away")
	}
	// Choosing a preset (a sub-theme) clears the custom colour so it shows.
	postForm(t, mux, cookie, "/api/settings/theme", "idx="+strconv.Itoa(themeChoiceIndex("win7-aero", "")))
	_, body = postForm(t, mux, cookie, "/api/settings/theme", "idx="+strconv.Itoa(themeChoiceIndex("win7-aero", "twilight")))
	if _, ok := themeTints["win7-aero"]; ok {
		t.Error("choosing a preset must clear the custom tint")
	}
	if !strings.Contains(body, `setAttribute("data-variant","twilight")`) || !strings.Contains(body, `removeProperty("--aero-tint")`) {
		t.Errorf("preset should set data-variant and drop the inline tint:\n%s", body)
	}
}

func TestTintPreviewAndPersistence(t *testing.T) {
	defer resetThemeState()
	resetThemeState()
	mux := newRemoteMux()
	cookie := sessionCookie(t, mux)
	if !strings.Contains(dashboardHTML(t, mux, cookie, "/?preview=win7-aero&tint=%23ff8800"), `<html data-theme="win7-aero" style="--aero-tint:#ff8800">`) {
		t.Error("preview should accept &tint=")
	}
	if len(themeTints) != 0 {
		t.Error("preview must not persist a tint")
	}
	got := validThemeTints(map[string]string{
		"win7-aero": "#ABCDEF", "matrix": "#123456", "nope": "#123456",
	})
	if len(got) != 1 || got["win7-aero"] != "#abcdef" {
		t.Errorf("validThemeTints = %v", got)
	}
	if v := validThemeTints(map[string]string{"win7-aero": "url(x)"}); len(v) != 0 {
		t.Errorf("malformed stored colour kept: %v", v)
	}
}

// htmx 4.0's settle step showed a text/number field's OLD value after an
// Enter-save (it copies outgoing attributes, sets the live value, refocuses,
// and the focused input then ignores the real value). The dashboard turns
// settle off; keep it off. Browser-level check: test/ui/settings-roundtrip.js.
func TestDashboardDisablesHtmxSettle(t *testing.T) {
	initTestHardware(t)
	rec := httptest.NewRecorder()
	handleDashboard(rec, httptest.NewRequest("GET", "/", nil))
	body := rec.Body.String()
	meta := `<meta name="htmx-config" content='{"defaultSettleDelay":0}'>`
	i, j := strings.Index(body, meta), strings.Index(body, `<script src="/static/htmax.min.js">`)
	if i < 0 {
		t.Fatal("dashboard no longer sets htmx defaultSettleDelay 0")
	}
	if j < 0 || i > j {
		t.Fatal("htmx-config meta must come before htmax.min.js, which reads it at load")
	}
}

// The WebUI level meters were dead after the ftl-themes v4 migration: the
// channel strips were built with .meter-fill/.meter-peak but the update
// code still queried .vu-fill/.vu-peak, found nothing and never moved a
// bar. Every class the update code looks up must be one the strips carry,
// and the level must be set where ftl-themes' .meter contract reads it.
func TestDashboardMeterUpdateMatchesStripMarkup(t *testing.T) {
	mux := newRemoteMux()
	body := dashboardHTML(t, mux, sessionCookie(t, mux), "/")
	start := strings.Index(body, "function ensureChannels(")
	end := strings.Index(body, "if (meterBadge)") // html/template strips JS comments
	if start < 0 || end < start {
		t.Fatal("meter JS not found in the dashboard")
	}
	js := body[start:end]
	for _, m := range regexp.MustCompile(`chMeters\.querySelector\('\.([a-z-]+)\[`).FindAllStringSubmatch(js, -1) {
		// Once in the query, at least once more in the strip markup
		// (which html/template JS-escapes, so match the bare class name).
		if strings.Count(js, m[1]) < 2 {
			t.Errorf("update code queries .%s, which the channel strips never carry", m[1])
		}
	}
	if strings.Contains(js, "vu-fill") || strings.Contains(js, "vu-peak") {
		t.Error("meter JS still uses the pre-v4 .vu-fill/.vu-peak classes")
	}
	if !strings.Contains(js, "track.style.setProperty('--meter-level'") {
		t.Error("level not set on the .meter element, where ftl-themes reads --meter-level")
	}
}

// The WebUI transport keys mirror the panel lamps: unlit at rest, REC lit
// only while recording, PLAY lit only while playing and flashing while
// paused (steady under reduced motion).
func TestDashboardTransportLampsFollowState(t *testing.T) {
	mux := newRemoteMux()
	body := dashboardHTML(t, mux, sessionCookie(t, mux), "/")
	for _, want := range []string{
		`.transport-row .record,.transport-row .play{border-color:var(--border);color:var(--muted)}`,
		`.transport-row.is-rec .record{`,
		`.transport-row.is-play .play,.transport-row.is-pause .play{`,
		`.transport-row.is-pause .play{animation:pi-lamp-flash`,
		`@media (prefers-reduced-motion:reduce){.transport-row.is-pause .play{animation:none`,
		`rec: !!m.recording`,
		`(transportState.rec ? ' is-rec' : '')`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard lacks %q", want)
		}
	}
	for _, stale := range []string{`.transport-row .record{border-color:var(--danger)`, `.transport-row .play{border-color:var(--success)`} {
		if strings.Contains(body, stale) {
			t.Errorf("dashboard still lights a key at rest: %q", stale)
		}
	}
}
