package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/skip2/go-qrcode"
	"golang.org/x/net/websocket"

	"pi9696/hardware"
)

// remoteToken gates every route except /login. It's generated fresh at each
// process startup (never persisted to disk) and shown on the OLED via
// Settings -> Remote Access, so reading it requires physical/console access
// to the device - the same trust model as the rest of this app's local-only
// controls, just extended to the LAN.
//
// It's short (8 chars from a 32-symbol alphabet, ~40 bits of entropy)
// because it has to fit on a 256px-wide OLED line and be typeable from a
// phone; loginLimiter's lockout is what keeps that from being brute-forceable
// over the network in practice, not the raw length. Displayed (OLED, login
// page) as two groups of 4 for readability - see formatToken.
var remoteToken string

const (
	remoteTokenLength   = 8
	remoteTokenAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ" // Crockford-style, no 0/O/1/I/L - avoids exactly the characters most likely to be misread on a small OLED
)

func generateRemoteToken() string {
	b := make([]byte, remoteTokenLength)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("Failed to generate remote control token: %v", err)
	}
	out := make([]byte, len(b))
	for i, c := range b {
		out[i] = remoteTokenAlphabet[int(c)%len(remoteTokenAlphabet)]
	}
	return string(out)
}

// generateRemoteSessionID produces the cookie value: a longer, full-entropy
// hex string, not the short operator-typed token. It's never shown to the
// user and never equals the token, so a session cookie can't be guessed from
// the token (and vice versa).
func generateRemoteSessionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// formatToken renders a token as two groups of 4 for readability, e.g.
// "K7M2QX9F" -> "K7M2 QX9F". Display-only - the raw unseparated string is
// what's actually stored/compared.
func formatToken(t string) string {
	if len(t) != remoteTokenLength {
		return t
	}
	return t[:4] + " " + t[4:]
}

// normalizeToken strips whatever separator a user typed between the two
// groups, so "K7M2 QX9F", "K7M2-QX9F", and "K7M2QX9F" all compare equal.
// Uppercased first: the alphabet is uppercase-only, so a lowercase direct
// POST (curl, QR ?t= URL) must not 401.
func normalizeToken(s string) string {
	return strings.NewReplacer(" ", "", "-", "").Replace(strings.ToUpper(s))
}

const remoteSessionCookie = "pi9696_session"

// loginLimiter blunts online guessing of the short token: 5 failed attempts
// from an IP locks that IP out for a minute. Deliberately separate from the
// app's UI mutex - this only ever guards its own map, never app state.
type loginLimiter struct {
	mu       sync.Mutex
	failures map[string]int
	lockedAt map[string]time.Time
	seenAt   map[string]time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{failures: make(map[string]int), lockedAt: make(map[string]time.Time), seenAt: make(map[string]time.Time)}
}

func (l *loginLimiter) allowed(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if until, ok := l.lockedAt[ip]; ok {
		if time.Since(until) < 60*time.Second {
			return false
		}
		delete(l.lockedAt, ip)
		delete(l.failures, ip)
	}
	// Sweep stale entries: sessions.create prunes its own map, but
	// probed-never-locked IPs would otherwise grow these maps forever.
	// O(n) over attacker IPs per login attempt - logins are rare.
	for probe, at := range l.seenAt {
		if time.Since(at) >= 60*time.Second {
			if _, locked := l.lockedAt[probe]; !locked {
				delete(l.seenAt, probe)
				delete(l.failures, probe)
			}
		}
	}
	l.seenAt[ip] = time.Now()
	return true
}

func (l *loginLimiter) recordFailure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures[ip]++
	l.seenAt[ip] = time.Now()
	if l.failures[ip] >= 5 {
		l.lockedAt[ip] = time.Now()
	}
}

func (l *loginLimiter) recordSuccess(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, ip)
	delete(l.lockedAt, ip)
	delete(l.seenAt, ip)
}

var loginLimit = newLoginLimiter()

// sessionStore holds issued session IDs with their expiry. It is deliberately
// a separate in-memory store (not the token itself): the login token is the
// secret the operator reads off the OLED and types in, and it never becomes
// the cookie value. Instead, a fresh random session ID is minted at login and
// stored here with a lifetime, so (a) the cookie doesn't carry the token, and
// (b) sessions actually expire server-side - the browser's MaxAge alone only
// tells the client when to drop the cookie, not the server when to stop
// accepting it. A process restart clears all sessions (fresh token + empty
// store), which is acceptable for a device that re-logs-in after a reboot.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]time.Time // session ID -> expiry
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: make(map[string]time.Time)}
}

// create mints a new session ID valid for the given lifetime. Each login
// also sweeps out every already-expired entry: valid() only prunes lazily,
// when the same expired ID is presented again, so entries that expire and
// are never re-presented would otherwise sit in the map forever on this
// long-running device. Sweeping here bounds the map to (live sessions +
// logins since the last login) with no extra timer.
func (s *sessionStore) create(ttl time.Duration) string {
	id, err := generateRemoteSessionID()
	if err != nil {
		// Session IDs use the same crypto/rand source as the token; a failure
		// here is fatal (there's no safe fallback for a bearer credential).
		log.Fatalf("Failed to generate session ID: %v", err)
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, exp := range s.sessions {
		if now.After(exp) {
			delete(s.sessions, k)
		}
	}
	s.sessions[id] = now.Add(ttl)
	return id
}

// valid reports whether id is a live, unexpired session. Expired entries are
// pruned lazily here when they're presented again; entries that expire and
// are never re-presented are swept by the next login (see create).
func (s *sessionStore) valid(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[id]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.sessions, id)
		return false
	}
	return true
}

// revoke removes a session (logout), so a logged-out cookie can't be replayed.
func (s *sessionStore) revoke(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

var sessions = newSessionStore()

// sessionLifetime is how long a login session lasts server-side.
const sessionLifetime = 12 * time.Hour

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// validSession checks the cookie against the server-side session store (a
// constant-time lookup isn't needed here - sessions are random IDs looked up
// in a map, not a secret compared byte-by-byte).
func validSession(r *http.Request) bool {
	c, err := r.Cookie(remoteSessionCookie)
	if err != nil {
		return false
	}
	return sessions.valid(c.Value)
}

func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !validSession(r) {
			// WS handshakes and API/fetch callers can't follow a 303
			// (handshake just fails, <img> renders login HTML): give
			// them a 401 they can act on instead.
			if strings.HasPrefix(r.URL.Path, "/ws/") || strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, "session expired", http.StatusUnauthorized)
				return
			}
			// Preserve the query (the OLED access-QR encodes /?t=<token>) so a
			// scanned code still pre-fills the login boxes after the 303.
			target := "/login"
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

// pi9696LogoSVG is a small inline vector wordmark shared by the login and
// dashboard pages - a chrome-gradient italic wordmark with speed-line
// flourishes and thin accent bars, styled after 1980s corporate logotypes
// (Tandy/Compaq/NBC-era). An inline SVG needs no raster asset shipped or
// fetched, which matters on a device that has to work with no internet
// access, and it scales cleanly from the login page's large mark down to
// the dashboard header's small one.
const pi9696LogoSVG = `<svg class="logo-svg" viewBox="0 0 320 110" xmlns="http://www.w3.org/2000/svg">
<defs>
<linearGradient id="chrome" x1="0" y1="0" x2="0" y2="1">
<stop offset="0%" stop-color="#eafcff"/><stop offset="30%" stop-color="#00d9ff"/>
<stop offset="70%" stop-color="#0066ff"/><stop offset="100%" stop-color="#021a33"/>
</linearGradient>
<linearGradient id="bar" x1="0" y1="0" x2="1" y2="0">
<stop offset="0%" stop-color="#0066ff" stop-opacity="0"/><stop offset="50%" stop-color="#00d9ff"/>
<stop offset="100%" stop-color="#0066ff" stop-opacity="0"/>
</linearGradient>
</defs>
<g stroke="#123a52" stroke-width="1.5" opacity="0.7">
<line x1="4" y1="96" x2="70" y2="80"/><line x1="4" y1="104" x2="86" y2="88"/>
<line x1="316" y1="96" x2="250" y2="80"/><line x1="316" y1="104" x2="234" y2="88"/>
</g>
<rect x="30" y="24" width="260" height="2" fill="url(#bar)"/>
<text x="160" y="72" text-anchor="middle" font-family="Arial, Helvetica, sans-serif" font-size="52" font-weight="900" font-style="italic" fill="url(#chrome)" textLength="260" lengthAdjust="spacingAndGlyphs">PI9696</text>
<rect x="30" y="82" width="260" height="2" fill="url(#bar)"/>
<text x="160" y="102" text-anchor="middle" font-family="Arial, Helvetica, sans-serif" font-size="10" fill="#5b8aa8" textLength="260" lengthAdjust="spacingAndGlyphs">MULTITRACK RECORDER</text>
</svg>`

// pi9696IconSVG is the PWA/home-screen icon - a square mark reusing the
// dashboard's reel-hub motif (see the .reel SVGs in dashboardTmpl) rather
// than inventing a second visual language just for the icon. Served as-is
// (image/svg+xml): every modern mobile browser that supports "Add to Home
// Screen" for a PWA accepts an SVG manifest icon, so no PNG rasterizer
// dependency is needed.
const pi9696IconSVG = `<svg viewBox="0 0 192 192" xmlns="http://www.w3.org/2000/svg">
<defs><linearGradient id="g" x1="0" y1="0" x2="0" y2="1">
<stop offset="0%" stop-color="#00d9ff"/><stop offset="100%" stop-color="#0066ff"/>
</linearGradient></defs>
<rect width="192" height="192" rx="34" fill="#020509"/>
<circle cx="96" cy="96" r="74" fill="none" stroke="url(#g)" stroke-width="6"/>
<g fill="none" stroke="url(#g)" stroke-width="6">
<path d="M96 96 L68 40 Q96 24 124 40 Z" transform="rotate(0 96 96)"/>
<path d="M96 96 L68 40 Q96 24 124 40 Z" transform="rotate(120 96 96)"/>
<path d="M96 96 L68 40 Q96 24 124 40 Z" transform="rotate(240 96 96)"/>
</g>
<circle cx="96" cy="96" r="17" fill="url(#g)"/>
</svg>`

func handleIcon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	fmt.Fprint(w, pi9696IconSVG)
}

// manifestTmpl embeds the current device name so an installed home-screen
// icon reflects a renamed unit without a rebuild. start_url ("/") requires
// auth like every other route - opening the installed app when the session
// cookie has expired just lands on /login, same as any bookmark would.
var manifestTmpl = template.Must(template.New("manifest").Parse(`{
  "name": "{{.DeviceName}} Remote",
  "short_name": "{{.DeviceName}}",
  "start_url": "/",
  "display": "standalone",
  "background_color": "#020509",
  "theme_color": "#00d9ff",
  "icons": [{"src": "/icon.svg", "sizes": "any", "type": "image/svg+xml", "purpose": "any"}]
}`))

func handleManifest(w http.ResponseWriter, r *http.Request) {
	mutex.Lock()
	name := deviceName
	mutex.Unlock()
	w.Header().Set("Content-Type", "application/manifest+json")
	manifestTmpl.Execute(w, struct{ DeviceName string }{name})
}

type loginPageData struct {
	Error, DeviceName string
	Logo              template.HTML
	// Theme/ThemeCSS mirror the dashboard's opt-in theming so the login
	// page is the same product, not a stranger: data-theme scopes the
	// --ftl-* tokens (e.g. --ftl-font) the stylesheet reads. Unthemed
	// renders exactly as before (no marker beyond "none", no href).
	Theme, ThemeCSS string
	// Boxes pre-fills the 8 token inputs so a failed attempt (or a QR
	// prefill) isn't wiped by the error re-render.
	Boxes [8]string
}

// boxesFromToken splits a normalized token into per-box characters.
func boxesFromToken(t string) (b [8]string) {
	for i, c := range t {
		if i >= 8 {
			break
		}
		b[i] = string(c)
	}
	return b
}

// loginPageTheme returns the dashboard's active theme for the login page.
func loginPageTheme() (string, string) {
	active := currentTheme()
	css := ""
	if active != themeNone {
		css = "/static/themes/" + active + ".css"
	}
	return active, css
}

var loginPageTmpl = template.Must(template.New("login").Parse(`<!DOCTYPE html>
<html data-theme="{{.Theme}}"><head><title>{{.DeviceName}} Remote</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<link rel="manifest" href="/manifest.json">
<link rel="icon" href="/icon.svg" type="image/svg+xml">
{{if .ThemeCSS}}<link rel="stylesheet" href="{{.ThemeCSS}}">{{end}}
<style>
body{font-family:var(--ftl-font,"Consolas",monospace);background:radial-gradient(ellipse at center,#0a1a2e,#020509 75%);color:#cfeeff;display:flex;flex-direction:column;align-items:center;justify-content:center;min-height:100vh;margin:0;gap:2em}
.logo-svg{width:480px;max-width:85vw;display:block}
form{background:#0a1526;padding:2em 3em;border-radius:10px;border:1px solid #0f3a5c;box-shadow:0 0 30px rgba(0,180,255,0.15);text-align:center}
.token-row{display:flex;align-items:center;justify-content:center;gap:0.4em;margin-bottom:1em}
.token-row input{font-family:inherit;font-size:1.3em;width:1.4em;padding:0.4em 0;background:#08192b;color:#cfeeff;border:1px solid #0f3a5c;border-radius:4px;text-align:center;text-transform:uppercase}
.token-row input:focus{outline:none;border-color:#00d9ff;box-shadow:0 0 8px #00d9ff}
.token-row .dash{color:#5b8aa8;font-size:1.3em}
button{font-family:inherit;font-size:1.1em;padding:0.5em 1.2em;background:#08192b;color:#00d9ff;border:1px solid #0f3a5c;border-radius:4px;cursor:pointer}
button:hover{border-color:#00d9ff;box-shadow:0 0 8px #00d9ff}
.err{color:#ff3355}
.hint{color:#5b8aa8;font-size:0.85em;margin-top:1em}

@media (max-width: 480px) {
  form{padding:1.5em 1.2em}
  .token-row{gap:0.25em}
  .token-row input{width:1.1em;font-size:1.1em}
}
</style></head>
<body>
{{.Logo}}
<form method="POST" action="/login" id="loginForm">
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
<div class="token-row" id="tokenRow">
<input maxlength="1" autofocus autocomplete="off" value="{{index .Boxes 0}}">
<input maxlength="1" autocomplete="off" value="{{index .Boxes 1}}">
<input maxlength="1" autocomplete="off" value="{{index .Boxes 2}}">
<input maxlength="1" autocomplete="off" value="{{index .Boxes 3}}">
<span class="dash">-</span>
<input maxlength="1" autocomplete="off" value="{{index .Boxes 4}}">
<input maxlength="1" autocomplete="off" value="{{index .Boxes 5}}">
<input maxlength="1" autocomplete="off" value="{{index .Boxes 6}}">
<input maxlength="1" autocomplete="off" value="{{index .Boxes 7}}">
</div>
<input type="hidden" name="token" id="tokenValue">
<noscript><p><input name="token" maxlength="9" autocomplete="off" placeholder="XXXXXXXX" style="text-transform:uppercase"></p></noscript>
<button type="submit">Enter</button>
<p class="hint">8-character code shown on the OLED (Settings &rarr; Remote Access)</p>
</form>
<script>
// One box per character, auto-advancing focus, joined into the hidden
// "token" field the existing /login handler already expects (it strips
// separators itself via normalizeToken, so the dash here is display-only).
var boxes = document.querySelectorAll('#tokenRow input');
boxes.forEach(function(box, i) {
  box.addEventListener('input', function() {
    box.value = box.value.toUpperCase();
    if (box.value && i < boxes.length - 1) boxes[i + 1].focus();
  });
  box.addEventListener('keydown', function(e) {
    if (e.key === 'Backspace' && !box.value && i > 0) boxes[i - 1].focus();
  });
  box.addEventListener('paste', function(e) {
    e.preventDefault();
    var chars = (e.clipboardData.getData('text') || '').replace(/[^A-Za-z0-9]/g, '').toUpperCase().split('');
    for (var j = 0; j < chars.length && i + j < boxes.length; j++) boxes[i + j].value = chars[j];
    boxes[Math.min(i + chars.length, boxes.length - 1)].focus();
  });
});
// The OLED's access-QR encodes a URL with "?t=<token>"; pre-fill the boxes
// so scanning the code lands the operator one click from Enter.
(function() {
  var t = new URLSearchParams(window.location.search).get('t');
  if (t && t.length === 8 && /^[A-Za-z0-9]+$/.test(t)) {
    t = t.toUpperCase();
    for (var j = 0; j < t.length; j++) boxes[j].value = t[j];
    boxes[boxes.length - 1].focus();
  }
})();
document.getElementById('loginForm').addEventListener('submit', function() {
  document.getElementById('tokenValue').value = Array.from(boxes).map(function(b) { return b.value; }).join('');
});
</script>
</body></html>`))

func handleLoginGet(w http.ResponseWriter, r *http.Request) {
	mutex.Lock()
	name := deviceName
	lastLoginPage = time.Now()
	mutex.Unlock()
	theme, css := loginPageTheme()
	loginPageTmpl.Execute(w, loginPageData{DeviceName: name, Logo: template.HTML(pi9696LogoSVG), Theme: theme, ThemeCSS: css})
}

func handleLoginPost(w http.ResponseWriter, r *http.Request) {
	mutex.Lock()
	name := deviceName
	mutex.Unlock()
	ip := clientIP(r)
	if !loginLimit.allowed(ip) {
		w.WriteHeader(http.StatusTooManyRequests)
		theme, css := loginPageTheme()
		loginPageTmpl.Execute(w, loginPageData{Error: "Too many attempts, wait a minute", DeviceName: name, Logo: template.HTML(pi9696LogoSVG), Theme: theme, ThemeCSS: css})
		return
	}

	submitted := normalizeToken(r.FormValue("token"))
	if subtle.ConstantTimeCompare([]byte(submitted), []byte(remoteToken)) != 1 {
		loginLimit.recordFailure(ip)
		w.WriteHeader(http.StatusUnauthorized)
		theme, css := loginPageTheme()
		loginPageTmpl.Execute(w, loginPageData{Error: "Invalid token", DeviceName: name, Logo: template.HTML(pi9696LogoSVG), Theme: theme, ThemeCSS: css, Boxes: boxesFromToken(submitted)})
		return
	}

	loginLimit.recordSuccess(ip)
	// Mint a fresh session ID; the cookie value is never the token.
	sessionID := sessions.create(sessionLifetime)
	http.SetCookie(w, &http.Cookie{
		Name:     remoteSessionCookie,
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		// No Secure flag: this server is plain HTTP (see PROJECT_STATUS.md's
		// remote-control notes for why, and what that means for LAN
		// eavesdropping risk). The server-side lifetime is enforced by
		// sessionStore.valid, not this client-side MaxAge.
		MaxAge: int(sessionLifetime / time.Second),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	// Revoke server-side so a captured cookie can't be replayed after logout.
	if c, err := r.Cookie(remoteSessionCookie); err == nil {
		sessions.revoke(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: remoteSessionCookie, Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- Themes ---------------------------------------------------------------
//
// The dashboard ships with its own look baked into the inline stylesheet
// below; a theme from the ftl-themes submodule is an OPT-IN overlay on top
// of it. With no theme selected (the default, and every existing install
// after upgrade) nothing extra is linked and the page renders exactly as it
// did before: the inline :root block's var(--ftl-*, <original value>)
// fallbacks resolve to the original values because no --ftl-* token exists.
//
// Selecting a theme links one self-contained bundle, which defines the
// --ftl-* tokens and so re-colours every existing rule through those same
// fallbacks - plus the app shell, which re-lays-out the page.

// themeNone is the slug meaning "no theme file; use the built-in look".
const themeNone = "none"

// themeSlug names the bundle the dashboard links, or themeNone for the
// built-in look - the default, so an upgraded unit keeps rendering exactly
// as it did. Persisted in the unit's config like every other setting, so the
// choice follows the device rather than one browser. Guarded by mutex.
var themeSlug = themeNone

// themeManifest mirrors ftl-themes' dist/themes.json entries.
type themeManifest struct {
	Slug        string `json:"slug"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// themeAssetPath maps a path inside the submodule to its embed path.
func themeAssetPath(rel string) string { return "third_party/ftl-themes/" + rel }

var (
	themeListOnce sync.Once
	themeList     []themeManifest
)

// availableThemes reads the embedded manifest once. The "none" entry is
// synthesised: it is this device's own look, not one of ftl-themes'.
func availableThemes() []themeManifest {
	themeListOnce.Do(func() {
		themeList = []themeManifest{{Slug: themeNone, Label: "Built-in", Description: "The unit's own look"}}
		data, err := embeddedThemes.ReadFile(themeAssetPath("dist/themes.json"))
		if err != nil {
			logWarnf("theme manifest unreadable: %v", err)
			return
		}
		var parsed []themeManifest
		if err := json.Unmarshal(data, &parsed); err != nil {
			logWarnf("theme manifest unparseable: %v", err)
			return
		}
		themeList = append(themeList, parsed...)
	})
	return themeList
}

// isKnownTheme reports whether a slug names a bundle that is actually
// embedded. Guards the static route and any persisted value.
func isKnownTheme(slug string) bool {
	if slug == "" || slug == themeNone {
		return false
	}
	for _, t := range availableThemes() {
		if t.Slug == slug && t.Slug != themeNone {
			return true
		}
	}
	return false
}

// currentTheme returns the active slug, or themeNone. Caller holds no lock.
func currentTheme() string {
	mutex.Lock()
	defer mutex.Unlock()
	if !isKnownTheme(themeSlug) {
		return themeNone
	}
	return themeSlug
}

func themeOptionsView() optionsView {
	all := availableThemes()
	opts := make([]string, 0, len(all))
	idx := 0
	active := currentTheme()
	for i, t := range all {
		opts = append(opts, t.Label)
		if t.Slug == active {
			idx = i
		}
	}
	return optionsView{Options: opts, Idx: idx}
}

func themeSelect() selectView {
	return settingSelect("theme", "/api/settings/theme", "Theme", "", themeOptionsView)
}

// handleAPISettingsTheme persists the chosen theme and tells the page to
// swap its stylesheet. The theme lives in the unit's config beside every
// other setting, so it follows the device rather than the browser - the
// front panel and the web UI share one state, as they do for brightness.
func handleAPISettingsTheme(w http.ResponseWriter, r *http.Request) {
	changed := false
	if idx, err := strconv.Atoi(r.FormValue("idx")); err == nil {
		all := availableThemes()
		if idx >= 0 && idx < len(all) {
			mutex.Lock()
			if themeSlug != all[idx].Slug {
				themeSlug = all[idx].Slug
				changed = true
				settingChanged()
			}
			mutex.Unlock()
		}
	}
	selectFragmentTmpl.Execute(w, themeSelect())
	if changed {
		// Out-of-band swap of the <link> and the <html data-theme> marker so
		// the change is visible immediately, without a reload that would lose
		// the live meter and telemetry sockets.
		active := currentTheme()
		// No href attribute at all when unthemed: href="" would resolve to the
		// dashboard's own URL and the browser would fetch the page as CSS.
		href := ""
		if active != themeNone {
			href = fmt.Sprintf(" href=%q", "/static/themes/"+active+".css")
		}
		fmt.Fprintf(w, "\n<link id=\"themecss\" rel=\"stylesheet\"%s hx-swap-oob=\"outerHTML\">", href)
		// Charts snapshot the palette at creation (see telePaletteInit), so
		// drop them: the next telemetry push (<=2s) rebuilds them in the new
		// theme's colors instead of keeping stale ones until a reload.
		fmt.Fprintf(w, "\n<script>document.documentElement.setAttribute(\"data-theme\",%q);teleCPU=teleRAM=teleTemp=teleDisk=telePalette=null</script>", active)
	}
}

type dashboardData struct {
	DeviceName           string
	Logo                 template.HTML
	Theme                string
	ThemeCSS             string
	ThemeFragment        template.HTML
	VURangeFragment      template.HTML
	PeakHoldFragment     template.HTML
	SampleRateFragment   template.HTML
	ChannelCountFragment template.HTML
	TagFragment          template.HTML
	PrefixFragment       template.HTML
	TransportFragment    template.HTML
	LogLevelFragment     template.HTML
	BrightnessFragment   template.HTML
	AutoDimFragment      template.HTML
	MonitorFragment      template.HTML
	DemoFragment         template.HTML
	WifiEnabled          bool
	WifiSSID             string
	WifiPassword         string
	WifiQRFragment       template.HTML
	TransportIcon        bool
}

// optionsView is the shared shape behind the settings-dropdown option lists
// - both the initial dashboard render and each setting's htmx POST handler
// read the same options through this, so there's exactly one place that
// builds each list (see the selectView descriptors below).
type optionsView struct {
	Options []string
	Idx     int
}

// selectView carries one settings-dropdown fragment. The six select-based
// settings (meter range, peak hold, sample rate, tag, transport mode, log
// level) render identical markup, differing only in anchor id, endpoint,
// label, option labels, and selected index - so one template serves them all.
// Suffix is appended verbatim to every option label (vuRange's "dBFS").
// Transport keeps its own fragment: its original markup hardcodes two
// options, one per line, which this single-option-per-line-free layout
// can't express byte-identically.
type selectView struct {
	Id      string
	Post    string
	Label   string
	Suffix  string
	Options []string
	Idx     int
}

func settingSelect(id, post, label, suffix string, opts func() optionsView) selectView {
	v := opts()
	return selectView{Id: id, Post: post, Label: label, Suffix: suffix, Options: v.Options, Idx: v.Idx}
}

func vuRangeSelect() selectView {
	return settingSelect("vurange", "/api/settings/vu-range", "Meter Range", "dBFS", vuRangeOptionsView)
}

func peakHoldSelect() selectView {
	return settingSelect("peakhold", "/api/settings/peak-hold", "Peak Hold", "", peakHoldOptionsView)
}

func sampleRateSelect() selectView {
	return settingSelect("samplerate", "/api/settings/sample-rate", "Sample Rate", "", sampleRateOptionsView)
}

func tagSelect() selectView {
	return settingSelect("tag", "/api/settings/tag", "Tag", "", tagOptionsView)
}

var transportFragmentTmpl = template.Must(template.New("transport").Parse(`<div id="transportmode" class="setting-cell">
<div class="setting-row">
<form hx-post="/api/settings/transport-mode" hx-target="#transportmode" hx-swap="outerHTML">
<label>Transport Buttons</label>
<select name="idx" onchange="this.form.requestSubmit()">
<option value="0" {{if eq .Idx 0}}selected{{end}}>Icon</option>
<option value="1" {{if eq .Idx 1}}selected{{end}}>Text</option>
</select>
</form>
</div>
</div>`))

func logLevelSelect() selectView {
	return settingSelect("loglevel", "/api/settings/log-level", "Log Level", "", logLevelOptionsView)
}

var selectFragmentTmpl = template.Must(template.New("setting-select").Parse(`<div id="{{.Id}}" class="setting-cell">
<div class="setting-row">
<form hx-post="{{.Post}}" hx-target="#{{.Id}}" hx-swap="outerHTML">
<label>{{.Label}}</label>
<select name="idx" onchange="this.form.requestSubmit()">
{{range $i, $v := .Options}}<option value="{{$i}}" {{if eq $i $.Idx}}selected{{end}}>{{$v}}{{$.Suffix}}</option>{{end}}
</select>
</form>
</div>
</div>`))

func vuRangeOptionsView() optionsView {
	mutex.Lock()
	defer mutex.Unlock()
	opts := make([]string, len(vuRangeOptions))
	for i, v := range vuRangeOptions {
		opts[i] = fmt.Sprintf("%d", int(v))
	}
	return optionsView{Options: opts, Idx: vuRangeIdx}
}

func peakHoldOptionsView() optionsView {
	mutex.Lock()
	defer mutex.Unlock()
	opts := make([]string, len(peakHoldOptions))
	for i, d := range peakHoldOptions {
		if d == 0 {
			opts[i] = "Off"
		} else {
			opts[i] = d.String()
		}
	}
	return optionsView{Options: opts, Idx: peakHoldIdx}
}

func sampleRateOptionsView() optionsView {
	mutex.Lock()
	defer mutex.Unlock()
	opts := make([]string, len(sampleRates))
	for i, r := range sampleRates {
		opts[i] = fmt.Sprintf("%dkHz", r/1000)
	}
	return optionsView{Options: opts, Idx: sampleRateIdx}
}

// currentChannelCountView carries the data the channel-count setting needs:
// a direct numeric input (1..MaxChannelCount) rather than a dropdown, since
// the legal range is wide and every value is valid.
type channelCountView struct {
	Count int
	Max   int
}

func currentChannelCountView() channelCountView {
	mutex.Lock()
	defer mutex.Unlock()
	return channelCountView{Count: channelCount, Max: MaxChannelCount}
}

func tagOptionsView() optionsView {
	mutex.Lock()
	defer mutex.Unlock()
	opts := make([]string, len(tagPresets))
	for i, t := range tagPresets {
		if t == "" {
			opts[i] = "None"
		} else {
			opts[i] = t
		}
	}
	return optionsView{Options: opts, Idx: tagPresetIdx}
}

// brightnessFragmentTmpl is the Display -> Brightness setting: a 0-100
// slider (the OLED's continuous brightness control) that live-updates its
// readout while dragging and submits on release, so the panel responds only
// when the operator finishes moving it.
var brightnessFragmentTmpl = template.Must(template.New("brightness").Parse(`<div id="brightness" class="setting-cell">
<div class="setting-row">
<form hx-post="/api/settings/brightness" hx-target="#brightness" hx-swap="outerHTML">
<label for="brightnessRange">Brightness</label>
<span class="hint" id="brightnessVal">{{.Pct}}%</span>
<input id="brightnessRange" class="styled-range" type="range" name="pct" min="0" max="100" step="1" value="{{.Pct}}" oninput="document.getElementById('brightnessVal').textContent=this.value+'%'" onchange="this.form.requestSubmit()" title="Panel brightness 0-100%">
</form>
</div>
</div>`))

// autoDimFragmentTmpl is the Display -> Auto Dim setting: an on/off switch
// for the dim-then-off idle behavior. Mirrors the WiFi switch markup.
var autoDimFragmentTmpl = template.Must(template.New("autodim").Parse(`<div id="autodim" class="setting-cell">
<div class="setting-row setting-row--switch">
<form hx-post="/api/settings/dim" hx-target="#autodim" hx-swap="outerHTML">
<label for="autoDimToggle">Auto Dim</label>
<label class="sci-switch" for="autoDimToggle">
<input id="autoDimToggle" name="enabled" type="checkbox" {{if .Enabled}}checked{{end}} onchange="this.form.requestSubmit()">
<span class="sci-switch-track"><span class="sci-thumb"></span></span>
<span class="switch-readout" data-on="AUTO" data-off="MANUAL"></span>
</label>
</form>
</div>
</div>`))

// brightnessView/autoDimView carry the exact values the fragments render, so
// both the dashboard and the htmx POST handlers share one template.
type brightnessView struct {
	Pct int
}

type autoDimView struct {
	Enabled bool
}

func brightnessViewData() brightnessView {
	mutex.Lock()
	defer mutex.Unlock()
	return brightnessView{Pct: oledBrightnessPct}
}

func autoDimViewData() autoDimView {
	mutex.Lock()
	defer mutex.Unlock()
	return autoDimView{Enabled: autoDimEnabled}
}

func handleAPISettingsBrightness(w http.ResponseWriter, r *http.Request) {
	if pct, err := strconv.Atoi(r.FormValue("pct")); err == nil {
		mutex.Lock()
		if pct >= 0 && pct <= 100 {
			oledBrightnessPct = pct
			noteActivity() // a WebUI brightness change is input - wake the panel
			if hwManager != nil {
				hwManager.SetBrightness(oledBrightnessPct)
			}
			settingChanged()
		}
		mutex.Unlock()
	}
	brightnessFragmentTmpl.Execute(w, brightnessViewData())
}

func handleAPISettingsAutoDim(w http.ResponseWriter, r *http.Request) {
	enabled := r.FormValue("enabled") != ""
	mutex.Lock()
	autoDimEnabled = enabled
	// Either way this is an input while the panel may be dimmed/off: wake it
	// (turning dim off restores the screen, turning it on restarts the idle
	// clock from full brightness).
	noteActivity()
	settingChanged()
	mutex.Unlock()
	autoDimFragmentTmpl.Execute(w, autoDimViewData())
}

// demoFragmentTmpl is the Demo -> Demo Mode setting: an on/off switch for
// the simulated-audio demonstration mode. Mirrors the Auto Dim switch.
var demoFragmentTmpl = template.Must(template.New("demo").Parse(`<div id="demo" class="setting-cell">
<div class="setting-row setting-row--switch">
<form hx-post="/api/settings/demo" hx-target="#demo" hx-swap="outerHTML">
<label for="demoToggle">Demo Mode</label>
<label class="sci-switch" for="demoToggle">
<input id="demoToggle" name="enabled" type="checkbox" {{if .Enabled}}checked{{end}} onchange="this.form.requestSubmit()">
<span class="sci-switch-track"><span class="sci-thumb"></span></span>
<span class="switch-readout" data-on="DEMO" data-off="LIVE"></span>
</label>
</form>
</div>
</div>`))

// demoView carries the demo-mode flag the fragment renders, so both the
// dashboard and the htmx POST handler share one template.
type demoView struct {
	Enabled bool
}

func demoViewData() demoView {
	mutex.Lock()
	defer mutex.Unlock()
	return demoView{Enabled: demoMode}
}

func handleAPISettingsDemoMode(w http.ResponseWriter, r *http.Request) {
	enabled := r.FormValue("enabled") != ""
	mutex.Lock()
	setDemoModeLocked(enabled)
	noteActivity()
	mutex.Unlock()
	demoFragmentTmpl.Execute(w, demoViewData())
}

// monitorFragmentTmpl is the Audio -> Monitoring setting: an on/off switch
// for the input monitor. Mirrors the Demo Mode switch.
var monitorFragmentTmpl = template.Must(template.New("monitor").Parse(`<div id="monitor" class="setting-cell">
<div class="setting-row setting-row--switch">
<form hx-post="/api/settings/monitor" hx-target="#monitor" hx-swap="outerHTML">
<label for="monitorToggle">Monitoring</label>
<label class="sci-switch" for="monitorToggle">
<input id="monitorToggle" name="enabled" type="checkbox" {{if .Enabled}}checked{{end}} onchange="this.form.requestSubmit()">
<span class="sci-switch-track"><span class="sci-thumb"></span></span>
<span class="switch-readout" data-on="ON" data-off="OFF"></span>
</label>
</form>
</div>
</div>`))

// monitorView carries the monitoring flag the fragment renders, so both the
// dashboard and the htmx POST handler share one template.
type monitorView struct {
	Enabled bool
}

func monitorViewData() monitorView {
	mutex.Lock()
	defer mutex.Unlock()
	return monitorView{Enabled: monitoring}
}

func handleAPISettingsMonitor(w http.ResponseWriter, r *http.Request) {
	enabled := r.FormValue("enabled") != ""
	var done chan struct{}
	mutex.Lock()
	if enabled {
		startMonitor()
		// startMonitor no-ops when Inferno is down/recording: only claim
		// an auto session that actually exists (see doStartInferno).
		if monitoring {
			autoMonitor = true
		}
	} else {
		autoMonitor = false
		done = monitorDone
		stopMonitor()
	}
	noteActivity()
	mutex.Unlock()
	// stopMonitor only signals; the reaper clears the flag. Wait for it
	// (mutex-free) so the re-rendered switch reflects the real state.
	if done != nil {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}
	monitorFragmentTmpl.Execute(w, monitorViewData())
}

var channelCountFragmentTmpl = template.Must(template.New("channelcount").Parse(`<div id="channelcount" class="setting-cell">
<div class="setting-row">
<form hx-post="/api/settings/channels" hx-target="#channelcount" hx-swap="outerHTML">
<label for="channelsInput">Channels</label>
<input id="channelsInput" type="number" name="count" min="1" max="{{.Max}}" step="1" value="{{.Count}}" onchange="this.form.requestSubmit()" title="Number of input channels">
<span class="hint">1–{{.Max}}</span>
</form>
</div>
</div>`))

// prefixView carries the current filename prefix for the free-text Prefix
// field. A blank value renders an empty box with the default as placeholder;
// submitting clears a custom prefix back to the default.
type prefixView struct {
	Prefix string
}

// filePrefixFragmentTmpl is the WebUI free-text Prefix field (the OLED gets
// the preset-list picker instead, per the Round 3 design decision: WebUI
// text field + OLED presets). It posts the literal prefix, validated to a
// filename-safe charset server-side.
var filePrefixFragmentTmpl = template.Must(template.New("fileprefix").Parse(`<div id="fileprefix" class="setting-cell">
<div class="setting-row">
<form hx-post="/api/settings/prefix" hx-target="#fileprefix" hx-swap="outerHTML" hx-status:400="target:#prefix-error">
<label for="filePrefixInput">Prefix</label>
<input id="filePrefixInput" name="prefix" type="text" value="{{.Prefix}}" maxlength="32" placeholder="recording" pattern="[A-Za-z0-9 -]+" title="Letters, numbers, spaces and - only (no underscores)">
<span class="hint">file_YYYYMMDD…</span>
<button type="submit" class="btn-primary">Save</button>
</form>
<div id="prefix-error"></div>
</div>
</div>`))

func filePrefixView() prefixView {
	mutex.Lock()
	defer mutex.Unlock()
	return prefixView{Prefix: filePrefix}
}

func handleAPISettingsPrefix(w http.ResponseWriter, r *http.Request) {
	prefix := strings.TrimSpace(r.FormValue("prefix"))
	if prefix == "" || isValidFilePrefix(prefix) {
		mutex.Lock()
		filePrefix = prefix
		settingChanged()
		mutex.Unlock()
		logInfof("recording prefix set to %q via remote", effectiveFilePrefix())
	} else {
		logWarnf("rejected invalid recording prefix %q via remote", prefix)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `<span class="err">Letters, numbers, spaces and - only (max 32)</span>`)
		return
	}
	filePrefixFragmentTmpl.Execute(w, filePrefixView())
	// OOB swap: clear any stale validation error from #prefix-error on success.
	fmt.Fprint(w, "\n<div id=\"prefix-error\" hx-swap-oob=\"innerHTML\"></div>")
}

// transportSelect switches the main transport buttons between icon
// SVG glyphs and text labels (see the ICON/TEXT setting). A full fragment,
// so the settings modal stays consistent with the other setting rows.

func transportOptionsView() optionsView {
	idx := 0
	if transportMode == "text" {
		idx = 1
	}
	return optionsView{Options: []string{"Icon", "Text"}, Idx: idx}
}

func handleAPISettingsTransportMode(w http.ResponseWriter, r *http.Request) {
	if idx, err := strconv.Atoi(r.FormValue("idx")); err == nil {
		mutex.Lock()
		if idx == 0 {
			transportMode = "icon"
		} else if idx == 1 {
			transportMode = "text"
		} else {
			mutex.Unlock()
			transportFragmentTmpl.Execute(w, transportOptionsView())
			return
		}
		settingChanged()
		mutex.Unlock()
	}
	transportFragmentTmpl.Execute(w, transportOptionsView())
}

// wifiQRView carries the data needed to render the WiFi join QR code in the
// dashboard settings modal. The QR is generated via skip2/go-qrcode into a
// base64 PNG so it can be embedded directly in the HTML without extra
// endpoints.
type wifiQRView struct {
	Enabled  bool
	SSID     string
	Password string
	QRBase64 string
}

var wifiQRFragmentTmpl = template.Must(template.New("wifiqr").Parse(`
<div id="wifiqr">
{{if .Enabled}}
<div class="wifi-qr-row">
  <div class="wifi-qr-info">
    <p><strong>SSID:</strong> {{.SSID}}</p>
  </div>
  <div class="wifi-qr-img">
    <img src="data:image/png;base64,{{.QRBase64}}" alt="WiFi QR Code" title="Scan to join">
  </div>
</div>
{{else}}
<p class="dim">WiFi AP is off. Enable it below to start broadcasting.</p>
{{end}}
</div>
`))

func handleAPISettingsVURange(w http.ResponseWriter, r *http.Request) {
	if idx, err := strconv.Atoi(r.FormValue("idx")); err == nil {
		mutex.Lock()
		if idx >= 0 && idx < len(vuRangeOptions) {
			vuRangeIdx = idx
			settingChanged()
		}
		mutex.Unlock()
	}
	selectFragmentTmpl.Execute(w, vuRangeSelect())
}

func logLevelOptionsView() optionsView {
	return optionsView{Options: logLevelNames, Idx: int(currentLogLevel())}
}

func handleAPISettingsLogLevel(w http.ResponseWriter, r *http.Request) {
	if idx, err := strconv.Atoi(r.FormValue("idx")); err == nil {
		mutex.Lock()
		if idx >= 0 && idx < len(logLevelNames) {
			setLogLevel(LogLevel(idx))
		}
		mutex.Unlock()
	}
	selectFragmentTmpl.Execute(w, logLevelSelect())
}

func handleAPISettingsPeakHold(w http.ResponseWriter, r *http.Request) {
	if idx, err := strconv.Atoi(r.FormValue("idx")); err == nil {
		mutex.Lock()
		if idx >= 0 && idx < len(peakHoldOptions) {
			peakHoldIdx = idx
			settingChanged()
		}
		mutex.Unlock()
	}
	selectFragmentTmpl.Execute(w, peakHoldSelect())
}

func handleAPISettingsSampleRate(w http.ResponseWriter, r *http.Request) {
	if idx, err := strconv.Atoi(r.FormValue("idx")); err == nil {
		mutex.Lock()
		if idx >= 0 && idx < len(sampleRates) {
			sampleRateIdx = idx
			checkInfernoRestart()
			settingChanged()
		}
		mutex.Unlock()
	}
	selectFragmentTmpl.Execute(w, sampleRateSelect())
}

func handleAPISettingsChannels(w http.ResponseWriter, r *http.Request) {
	if n, err := strconv.Atoi(r.FormValue("count")); err == nil {
		mutex.Lock()
		if n >= 1 && n <= MaxChannelCount {
			channelCount = n
			// Relaunch Inferno with the new channel count if it's running
			// (the worker's restart path re-starts monitoring too), so the
			// running instance - and with it the live VU count - always
			// matches what the settings page shows.
			checkInfernoRestart()
			settingChanged()
		}
		mutex.Unlock()
	}
	channelCountFragmentTmpl.Execute(w, currentChannelCountView())
}

func handleAPISettingsTag(w http.ResponseWriter, r *http.Request) {
	if idx, err := strconv.Atoi(r.FormValue("idx")); err == nil {
		mutex.Lock()
		if idx >= 0 && idx < len(tagPresets) {
			tagPresetIdx = idx
			settingChanged()
		}
		mutex.Unlock()
	}
	selectFragmentTmpl.Execute(w, tagSelect())
}

// handleAPISettingsWiFi updates the WiFi access point configuration from the
// web dashboard (SSID, password, enabled toggle). Requires authentication.
func handleAPISettingsWiFi(w http.ResponseWriter, r *http.Request) {
	ssid := strings.TrimSpace(r.FormValue("ssid"))
	pass := r.FormValue("password")
	enabled := r.FormValue("enabled") == "on"

	mutex.Lock()
	if enabled {
		// Only a save that turns the AP on needs credentials; switching it
		// off must work even when the form fields were cleared.
		if ssid == "" {
			mutex.Unlock()
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `<span class="err">SSID required</span>`)
			return
		}
		if len(pass) < 8 {
			mutex.Unlock()
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `<span class="err">Password must be at least 8 characters</span>`)
			return
		}
		if len(ssid) > 32 {
			mutex.Unlock()
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `<span class="err">SSID must be at most 32 characters</span>`)
			return
		}
		wifiSSID = ssid
		wifiPassword = pass
	}
	wifiEnabled = enabled
	persistConfig()
	mutex.Unlock()

	// Re-apply the AP configuration (hostapd restart)
	go applyWifiConfig(wifiSSID, wifiPassword, wifiEnabled)

	// Return updated fragment with new QR code
	var qrBuf bytes.Buffer
	var qrBase64 string
	if enabled {
		if code, err := qrcode.New(fmt.Sprintf("WIFI:T:WPA;S:%s;P:%s;;", escapeWifiField(ssid), escapeWifiField(pass)), qrcode.Medium); err == nil {
			png, _ := code.PNG(256)
			qrBase64 = base64.StdEncoding.EncodeToString(png)
		}
	}
	wifiQRFragmentTmpl.Execute(&qrBuf, wifiQRView{enabled, ssid, pass, qrBase64})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(qrBuf.Bytes())
	// OOB swap: clear any stale validation error from #wifi-error on success.
	fmt.Fprint(w, "\n<div id=\"wifi-error\" hx-swap-oob=\"innerHTML\"></div>")
}

// handleAPIConfigExport writes the non-secret config profile to the USB drive
// (configExportName) and reports the outcome into the settings modal's status
// line - always a 200 so htmx shows the message; the text carries failure.
func handleAPIConfigExport(w http.ResponseWriter, r *http.Request) {
	mutex.Lock()
	err := exportConfig()
	mutex.Unlock()
	if err != nil {
		logErrorf("web config export: %v", err)
		fmt.Fprintf(w, "<span class=\"err\">Export failed: %s</span>", html.EscapeString(err.Error()))
		return
	}
	fmt.Fprint(w, "<span class=\"ok\">Config exported to USB drive.</span>")
}

// handleAPIConfigImport loads the USB config profile and applies it. On
// success the page is refreshed (HX-Refresh) so every settings row shows the
// imported values; on failure the message stays in the modal's status line.
func handleAPIConfigImport(w http.ResponseWriter, r *http.Request) {
	mutex.Lock()
	err := importConfig()
	mutex.Unlock()
	if err != nil {
		logErrorf("web config import: %v", err)
		fmt.Fprintf(w, "<span class=\"err\">Import failed: %s</span>", html.EscapeString(err.Error()))
		return
	}
	w.Header().Set("HX-Refresh", "true")
	fmt.Fprint(w, "<span class=\"ok\">Config imported from USB - reloading...</span>")
}

// The dashboard's header ("deck") mirrors the physical front panel left to
// right - logo, OLED, rotary encoder, transport buttons - reusing the exact
// same onEncoderRotate/onEncoderClick/onButtonPress functions physical
// hardware calls, so every existing guard (recording/playback mutual
// exclusion, confirmation dialogs) applies identically. No standalone
// "hold" button: on the real unit that's a long-press of the same encoder
// button, not a separate control, so it has no web equivalent either.
// Status/config/recordings (read-only info) sit in the three-column body;
// the reel transport follows it and the level meters stay pinned to the footer.
var dashboardTmpl = template.Must(template.New("dashboard").Parse(`<!DOCTYPE html>
<html data-theme="{{.Theme}}"><head><title>{{.DeviceName}} Remote</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<link rel="manifest" href="/manifest.json">
<link rel="icon" href="/icon.svg" type="image/svg+xml">
<link rel="apple-touch-icon" href="/icon.svg">
<meta name="theme-color" content="#00d9ff">
<meta name="apple-mobile-web-app-capable" content="yes">
<meta name="apple-mobile-web-app-status-bar-style" content="black-translucent">
<script src="/static/htmax.min.js"></script>
<link rel="stylesheet" href="/static/uPlot.min.css">
<script src="/static/uPlot.iife.min.js"></script>
<style>
/* Theme bridge: each built-in variable reads its ftl-themes token and
   falls back to the value it has always had. With no theme linked no
   --ftl-* token exists, every fallback applies, and the dashboard is
   byte-identical to before. With a theme linked, every rule below
   re-colours through these same names - no rule needed editing.
   --meter-h and the JS-set --vu-* stay app-owned: they are geometry
   and live signal data, not theming. */
:root{--glow:var(--ftl-accent,#00d9ff);--panel:var(--ftl-surface,#0a1526);--border:var(--ftl-border,#0f3a5c);--text:var(--ftl-text,#cfeeff);--dim:var(--ftl-muted,#5b8aa8);--rec:var(--ftl-danger,#ff3355);--idle:var(--ftl-success,#2bffb0);--orange:var(--ftl-warning,#ff8c1a);--meter-h:120px}
*{box-sizing:border-box}
body{font-family:"Consolas",monospace;background:radial-gradient(ellipse at top,#0a1a2e,#020509 70%);background-attachment:fixed;color:var(--text);margin:0;padding:0 1.5em 260px}
h2{font-size:0.8em;letter-spacing:0.2em;text-transform:uppercase;color:var(--dim);border-bottom:1px solid var(--border);padding-bottom:0.4em;margin:0 0 0.8em}
a{color:var(--glow)}
input{font-family:inherit;background:#08192b;color:var(--text);border:1px solid var(--border);border-radius:4px;padding:0.4em}
button{font-family:inherit;font-size:0.95em;padding:0.5em 1em;background:#08192b;color:var(--glow);border:1px solid var(--border);border-radius:5px;cursor:pointer;letter-spacing:0.05em}
button:hover{border-color:var(--glow);box-shadow:0 0 8px var(--glow)}
button:active{background:#0f2a44}
button:disabled{opacity:0.35;cursor:default;box-shadow:none}
button:focus-visible,input:focus-visible,select:focus-visible{outline:1px solid var(--glow);outline-offset:2px}
.ok{color:var(--idle)}
.err{color:var(--rec)}
.rec{color:var(--rec);font-weight:bold;text-shadow:0 0 8px var(--rec)}
.idle{color:var(--idle)}
table{border-collapse:collapse;width:100%;font-size:0.82em}
td,th{padding:0.3em 0.5em;border-bottom:1px solid var(--border)}
th{color:var(--dim);text-transform:uppercase;font-size:0.72em;letter-spacing:0.08em;text-align:left}

/* Panels get HUD corner brackets - the recurring "sci-fi readout" motif
   tying the three columns together. */
.panel{position:relative;background:var(--panel);border:1px solid var(--border);border-radius:10px;padding:1em 1.2em;box-shadow:0 0 20px rgba(0,180,255,0.08),inset 0 0 30px rgba(0,180,255,0.03)}
.panel::before,.panel::after{content:'';position:absolute;width:14px;height:14px;border:2px solid var(--glow);opacity:0.55}
.panel::before{top:-1px;left:-1px;border-right:none;border-bottom:none}
.panel::after{bottom:-1px;right:-1px;border-left:none;border-top:none}
.left{text-align:left}
.center{text-align:center}
.right{text-align:right}
.right table{text-align:right}
.right td:first-child{text-align:left;color:var(--dim)}

/* Header deck: [logo] [OLED] [rotary] [transport], matching the physical
   front panel's left-to-right layout. Settings/logout sit apart, top right,
   since they're not physical-panel controls.
   Every component here is sized fluidly (clamp()/vw) so as the viewport
   narrows the whole deck shrinks in BOTH width and height together on one
   row - no overflow, no horizontal scroll - instead of only dropping to a
   small size at one fixed breakpoint. */
header.deck{position:relative;display:flex;flex-direction:var(--pi-deck-dir,row);align-items:center;justify-content:center;gap:clamp(0.3em,1.2vw,1.6em);flex-wrap:nowrap;padding:clamp(0.6em,1.2vw,1.2em) clamp(0.5em,2.5vw,5.5em);border-bottom:1px solid var(--border);margin-bottom:1.5em}
.deck-logo .logo-svg{width:clamp(0px,11vw,255px)}
.oled-frame{background:#000;border:2px solid var(--border);border-radius:6px;padding:clamp(3px,0.6vw,8px);display:inline-block;box-shadow:0 0 25px rgba(0,180,255,0.15)}
.oled-frame img{width:clamp(170px,32vw,440px);height:auto;aspect-ratio:4/1;image-rendering:pixelated;display:block}
.encoder-row{display:flex;align-items:center;gap:clamp(0.2em,0.5vw,0.5em)}
.encoder-row button{font-size:clamp(0.7em,1.5vw,1.3em);width:clamp(1.3em,2.6vw,2.3em);padding:0.2em 0}
.encoder-row .click{border-radius:50%;width:clamp(1.3em,2.6vw,2.3em);height:clamp(1.3em,2.6vw,2.3em);padding:0}
/* Transport buttons: equal-sized icon squares, 60% of the OLED frame's
   110px rendered height (see .oled-frame img above). Both dimensions shrink
   with the viewport so the row always fits. */
.transport-row{display:flex;gap:clamp(0.2em,0.5vw,0.6em)}
.transport-row button{width:clamp(30px,4.6vw,66px);height:clamp(30px,4.6vw,66px);padding:0;display:flex;align-items:center;justify-content:center}
.transport-row button svg{width:clamp(14px,2.2vw,30px);height:clamp(14px,2.2vw,30px)}
.transport-row .record{border-color:var(--rec)}
.transport-row .record svg{fill:var(--rec)}
.transport-row .stop svg{fill:var(--glow)}
.transport-row .play{border-color:var(--idle)}
.transport-row .play svg{fill:var(--idle)}
/* The PLAY transport doubles as PAUSE while a track is running (see
   renderTransportRow) - two bars instead of the play triangle. */
.transport-row .play.pause svg{fill:var(--glow);stroke:var(--glow)}
.transport-row .play.pause{border-color:var(--orange)}
/* Text mode: the same transport keys but labelled instead of icon glyphs.
   Buttons stretch to fit and the label takes the accent colour the icon had. */
.transport-row.text button{width:auto;min-width:clamp(2em,3.2vw,3.4em);font-size:clamp(0.55em,0.95vw,0.85em);letter-spacing:0.08em;padding:0 0.3em}
.transport-row.text .record{color:var(--rec)}
.transport-row.text .stop{color:var(--glow)}
.transport-row.text .play{color:var(--idle)}
.transport-row.text .play.pause{color:var(--orange)}
.header-actions{position:absolute;top:0.8em;right:clamp(0.5em,2vw,1.5em);display:flex;gap:0.5em}
/* .icon-btn is applied to both a <button> (Settings) and an <a> (Log out)
   - the base button{} rule above only targets <button>, so colors/border
   are repeated here rather than relied on from that selector. */
.icon-btn{width:clamp(1.8em,2.6vw,2.2em);height:clamp(1.8em,2.6vw,2.2em);border-radius:50%;padding:0;display:flex;align-items:center;justify-content:center;background:#08192b;color:var(--glow);border:1px solid var(--border);cursor:pointer;text-decoration:none}
.icon-btn svg{width:clamp(14px,1.8vw,18px);height:clamp(14px,1.8vw,18px);stroke:var(--glow)}
.icon-btn:hover{border-color:var(--orange)}
.icon-btn:hover svg{stroke:var(--orange)}
/* Conn lamp: broadcast-pin showing the telemetry socket state - glow blue
   while the server pushes, error red while disconnected. A span, not a
   button: no pointer affordance, and no hover recolor (it must never read
   as a control). The bi-broadcast-pin glyph is a fill path, so it takes
   color rather than stroke. */
.icon-btn.conn{cursor:default}
.icon-btn.conn:hover{border-color:var(--border)}
.icon-btn.conn svg{stroke:none}
.icon-btn.conn.on{color:var(--glow)}
.icon-btn.conn.on svg{fill:var(--glow);filter:drop-shadow(0 0 3px rgba(0,217,255,0.8))}
.icon-btn.conn.off{color:var(--rec)}
.icon-btn.conn.off svg{fill:var(--rec)}
/* Download ALL: a small labeled action in the Recordings heading - text,
   not just an icon, so its function reads at a glance. */
.dl-all{float:right;font-size:0.7em;letter-spacing:0.08em;color:var(--glow);background:#08192b;border:1px solid var(--border);border-radius:5px;padding:0.15em 0.5em;text-decoration:none;font-weight:normal}
.dl-all:hover{border-color:var(--glow)}

.grid{display:grid;grid-template-columns:var(--pi-columns,1fr 1.6fr 1fr);gap:var(--pi-gap,1.2em)}
/* Layout bridge (companion to the :root color bridge above): the structural
   knobs a layout theme may turn, with the built-in geometry as fallback.
   Themes set --pi-* under their own scope to reflow the page (e.g. a wall
   panel stacking to one column); unthemed, every fallback applies and the
   layout is byte-identical. Deliberately a short list - deck internals,
   meter footer and modal sheets stay app-owned (see blue-future's
   stays-app-side section); regions themselves are shell-arranged. */

/* ftl-app shell none-case (ftl-themes#3): with no theme linked these hooks
   must generate zero boxes, so the built-in look renders exactly as before
   the shell existed. display:contents dissolves the wrapper (children lay
   out against body as they always did); the decorative rail is always empty
   in this app, so it never displays. Linked themes override both. */
main.ftl-app-main{display:contents}
.ftl-app-rail:empty{display:none}

.recordings-section{padding:0 1.2em 1.2em}
.recordings-section h2{margin:1.2em 0 0.6em;font-size:1.1em;letter-spacing:0.05em}
.recordings-table{width:100%;border-collapse:collapse;font-size:0.85em}
.recordings-table th,.recordings-table td{padding:0.6em 0.8em;text-align:left;border-bottom:1px solid var(--border);white-space:nowrap}
.recordings-table th{color:var(--glow);font-weight:600;font-size:0.7em;letter-spacing:0.1em;text-transform:uppercase;background:#08162a;position:sticky;top:0;z-index:10}
.recordings-table tr:hover td{background:#08162a}
.recordings-table td:last-child{text-align:right}
.recordings-table .dl-link{color:var(--glow);text-decoration:none;border:1px solid var(--border);border-radius:4px;padding:0.2em 0.6em;font-size:0.85em;white-space:nowrap}
.recordings-table .dl-link:hover{border-color:var(--glow);background:rgba(0,217,255,0.1)}
.recordings-table .empty{color:var(--dim);font-style:italic;padding:2em;text-align:center}

/* Scrollable table wrapper for narrow viewports */
.recordings-wrap{overflow-x:auto;max-width:100%}

@media (min-width:801px){
  body{min-height:100vh;display:flex;flex-direction:column}
  .grid{flex:1;min-height:0;align-self:stretch}
  .panel{display:flex;flex-direction:column;min-height:0;overflow:hidden}
  .panel h2{flex:none}
  .recordings-section{flex:0 0 auto}
  #recordings{overflow-y:auto;max-height:30vh}
}

/* Modals: the settings sheet and the stop-recording confirmation. */
.modal-backdrop{display:none;position:fixed;inset:0;background:rgba(2,6,10,0.75);z-index:200;align-items:center;justify-content:center}
.modal-backdrop.open{display:flex}
.modal{background:var(--panel);border:1px solid var(--border);border-radius:12px;box-shadow:0 0 30px rgba(0,180,255,0.2);min-width:20em}
.modal h2{border:none;margin:0}
.modal-close{background:none;border:none;color:var(--dim);font-size:1.4em;line-height:1;cursor:pointer;padding:0.2em;border-radius:6px}
.modal-close:hover{color:var(--glow)}
/* Stop-recording confirmation (touch devices - see stopModal in the body).
   Large hit targets for a fat-finger confirm/cancel. */
.modal--confirm{padding:1.4em 1.8em;position:relative}
.stop-prompt{color:var(--dim);margin:1.2em 0}
.stop-actions{display:flex;gap:0.8em;justify-content:flex-end}
.stop-actions button{min-width:7em;padding:0.8em 1em}
.modal--confirm .modal-close{position:absolute;top:0.9em;right:0.9em}

/* Settings modal: a sheet with a fixed header bar and a scrollable body, so
   a long setting list never runs past the viewport edge. Setting families are
   grouped under section titles and laid out on a responsive 2-column grid. */
.modal--settings{width:min(680px,94vw);max-height:88vh;display:flex;flex-direction:column}
.modal--settings .modal-head{display:flex;align-items:center;justify-content:space-between;gap:1em;padding:1.1em 1.4em;border-bottom:1px solid var(--border)}
.modal--settings .modal-body{padding:0.9em 1.4em 1.4em;overflow-y:auto}
.settings-group{display:grid;grid-template-columns:repeat(auto-fit,minmax(240px,1fr));gap:0.7em;padding:1em 0 0.4em}
/* The first group sits right under the header bar, which already provides
   top padding - drop the leading gap there so the sheet's vertical rhythm is
   even instead of stacking every group with an identical extra top pad. */
.modal-body>.settings-group:first-child{padding-top:0}
.settings-group-title{grid-column:1/-1;margin:0 0 0.2em;font-size:0.7em;letter-spacing:0.2em;text-transform:uppercase;color:var(--glow);border-bottom:1px solid var(--border);padding-bottom:0.4em}
.setting-row{display:flex;align-items:center;gap:0.8em;background:#08162a;border:1px solid var(--border);border-radius:8px;padding:0.55em 0.8em}
.setting-row:hover{border-color:#1b5380}
.setting-row form{display:flex;align-items:center;gap:0.8em;flex:1;width:100%}
.setting-row label{flex:1;color:var(--dim);font-size:0.82em;letter-spacing:0.03em;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.setting-row select{font-family:inherit;background:#020509;color:var(--text);border:1px solid var(--border);border-radius:6px;padding:0.42em 0.7em;min-width:9em;cursor:pointer}
.setting-row input[type="text"],.setting-row input[type="password"],.setting-row input[type="number"]{flex:1;min-width:0;background:#020509;color:var(--text);border:1px solid var(--border);border-radius:6px;padding:0.45em 0.7em}
.setting-row input[type="number"]{flex:none;width:6em;font-family:inherit}
.setting-row .hint{flex:none;font-size:0.78em;color:var(--dim);letter-spacing:0.05em}
.setting-row--switch input[type="checkbox"]{width:1.3em;height:1.3em;accent-color:var(--glow);cursor:pointer}
.btn-primary{background:transparent;color:var(--glow);border:1px solid var(--glow);border-radius:6px;font-size:0.82em;letter-spacing:0.06em;padding:0.45em 1em;cursor:pointer}
.btn-primary:hover{background:rgba(0,217,255,0.12);box-shadow:0 0 10px rgba(0,217,255,0.4)}
/* Paired SSID/Password row: two labelled fields sit side by side within one
   setting card. */
.setting-row--pair{gap:0.8em 1.2em;flex-wrap:wrap}
.setting-row--pair .field{flex:1 1 42%;display:flex;align-items:center;gap:0.6em;min-width:0}
.setting-row--pair .field label{flex:none;width:auto;max-width:8em}
.setting-row--pair .field input{flex:1;min-width:0}
/* SciFi toggle switch for the WiFi access-point: a squared HUD-style rail
   with a chamfered thumb whose diode lights up when the link goes live,
   plus an ONLINE/OFFLINE status readout that swaps as the switch flips. */
.sci-switch{position:relative;display:inline-flex;align-items:center;gap:0.7em;flex:none;cursor:pointer}
.sci-switch input{position:absolute;opacity:0;width:0;height:0}
.sci-switch-track{position:relative;display:block;width:4.4em;height:1.9em;padding:2px;background:#02050a;border:1px solid var(--border);border-radius:3px;box-shadow:inset 0 0 12px rgba(0,180,255,0.08);transition:border-color 0.15s,box-shadow 0.15s}
.sci-thumb{display:block;width:1.45em;height:1.45em;background:#0d2b4a;border:1px solid var(--border);border-radius:2px;transform:translateX(0);transition:transform 0.18s ease,background 0.18s,border-color 0.18s;position:relative}
.sci-thumb::before{content:'';position:absolute;inset:3px;background:#071426;border-radius:1px}
.sci-thumb::after{content:'';position:absolute;left:50%;top:50%;width:4px;height:4px;border-radius:50%;background:#fff;opacity:0.35;transform:translate(-50%,-50%);box-shadow:0 0 5px #fff;transition:opacity 0.18s,background 0.18s,box-shadow 0.18s}
.sci-switch input:checked + .sci-switch-track{border-color:var(--glow);box-shadow:inset 0 0 12px rgba(0,217,255,0.22),0 0 10px rgba(0,217,255,0.25)}
.sci-switch input:checked + .sci-switch-track .sci-thumb{transform:translateX(2.45em);background:#0e3a5c;border-color:var(--glow)}
.sci-switch input:checked + .sci-switch-track .sci-thumb::before{background:#062036}
.sci-switch input:checked + .sci-switch-track .sci-thumb::after{background:var(--glow);box-shadow:0 0 6px var(--glow);opacity:1}
.sci-switch input:focus-visible + .sci-switch-track{outline:1px solid var(--glow);outline-offset:2px}
.switch-readout{font-size:0.82em;letter-spacing:0.08em;position:relative;min-width:5em;text-align:center;color:#2c4a66}
.switch-readout::after{content:attr(data-off)}
.sci-switch input:checked ~ .switch-readout{color:var(--glow);text-shadow:0 0 6px rgba(0,217,255,0.6)}
.sci-switch input:checked ~ .switch-readout::after{content:attr(data-on)}

/* Continuous OLED brightness slider: a wide Sci-Fi range control with a
   glowing track and thumb, sized so a single row holds label + live % readout
   + slider (the readout is updated inline by the fragment's oninput). */
.styled-range{-webkit-appearance:none;appearance:none;flex:1 1 auto;min-width:0;height:1.6em;background:transparent;cursor:pointer}
.styled-range::-webkit-slider-runnable-track{height:4px;border-radius:2px;background:linear-gradient(90deg,#0e3a5c,var(--glow))}
.styled-range::-webkit-slider-thumb{-webkit-appearance:none;appearance:none;width:14px;height:14px;margin-top:-5px;border-radius:3px;background:#02050a;border:1px solid var(--glow);box-shadow:0 0 8px rgba(0,217,255,0.5)}
.styled-range::-moz-range-track{height:4px;border-radius:2px;background:linear-gradient(90deg,#0e3a5c,var(--glow))}
.styled-range::-moz-range-thumb{width:14px;height:14px;border-radius:3px;background:#02050a;border:1px solid var(--glow);box-shadow:0 0 8px rgba(0,217,255,0.5)}
.styled-range:focus-visible{outline:1px solid var(--glow);outline-offset:2px}

/* The rack-mount reel-to-reel transport lives in the normal document flow
   right below the three-column grid and scrolls with the page. The level
   meters are NOT here - they live in the pinned, collapsible footer (see
   .meter-footer) so they stay in view while you drive the controls. */
/* transport-deck lives INSIDE the "Transport Status" panel, so it's a plain
   fluid container - the panel itself provides the HUD frame and corner
   brackets, and the .r2r reel SVG below it is the live visual state of the
   transport (reels spin when running, tape path glows, head shows time). */
.transport-deck{width:100%;margin:0 0 0.6em;padding:0}
/* Scale the SVG to fit the card in both dimensions: width:auto-driven by the
   container (100%) and the intrinsic 820x265 aspect-ratio, capped with
   max-width so on large screens (where the widened centre column can exceed
   the deck's natural size) it shrinks *proportionally* instead of being
   clamped by a separate max-height - a max-height alongside width:100% +
   aspect-ratio would let the two constraints fight and distort the reels.
   height:auto keeps width -> height from the aspect-ratio, never fighting it. */
.r2r{display:block;width:100%;max-width:760px;height:auto;margin:0 auto}
.r2r .plate{fill:url(#deckBg)}
.r2r .plate-bezel{fill:none;stroke:rgba(0,217,255,0.28);stroke-width:1.5}
.r2r .plate-screw{fill:var(--ftl-deck-well,#0d1c31);stroke:rgba(0,217,255,0.35);stroke-width:1}
.r2r .deck-grid{fill:none;stroke:rgba(15,58,92,0.55);stroke-width:1}
.r2r .deck-corner{fill:none;stroke:var(--glow);stroke-width:2;opacity:0.45}
/* Reels: a near-black engineering-grade flange disc (gradient so the face
   reads as machined metal rather than flat), with a thin trim ring that
   lights up as the reel spins, faint tape windings and a bright hub. Only
   the inner spindle group (.reel-spin) rotates so the winding looks like
   it's turning while the plate and take-off point stay put. Flat fills go
   through --ftl-deck-* (fallbacks = the reference values) so layout themes
   can reskin the metalwork; glow accents stay on --glow. */
.r2r .reel-disc{fill:var(--ftl-deck-face,#112842);stroke:var(--ftl-deck-trim,#1d5c8f);stroke-width:2}
.r2r .reel-ring{fill:none;stroke:var(--ftl-deck-trim,#1d5c8f);stroke-width:1.5}
.r2r .reel-g.spinning .reel-ring{stroke:rgba(0,217,255,0.5);filter:drop-shadow(0 0 4px rgba(0,217,255,0.6))}
.r2r .reel-wind{fill:none;stroke:var(--glow);stroke-width:2;opacity:0.35}
.r2r .reel-hub{fill:var(--ftl-deck-hub,#11304a);stroke:var(--glow);stroke-width:1.5;opacity:0.85}
.r2r .reel-g.spinning .reel-hub{fill:var(--glow);filter:drop-shadow(0 0 5px rgba(0,217,255,0.6))}
.r2r .reel-center{fill:var(--ftl-deck-well,#071729)}
.r2r .reel-spoke{fill:var(--ftl-deck-spoke,#16345c);stroke:rgba(0,217,255,0.5);stroke-width:1.2}
.r2r .reel-g.spinning .reel-spoke{stroke:rgba(0,217,255,0.75)}
.r2r .reel-spin{transform-box:fill-box;transform-origin:center}
.r2r .reel-g.spinning .reel-spin{animation:spin 2.2s linear infinite}
.r2r .reel-g#reelL.spinning .reel-spin{animation-direction:reverse}
@keyframes spin{to{transform:rotate(360deg)}}
/* The tape path: angled runs from each reel down to the head block plus the
   straight run across the head gap. A darker under-shadow gives the glowing
   tape depth; one stroked path carries the travelling pulse (dash animation)
   from supply reel, over the head, to the take-up reel exactly like real
   tape. */
.r2r .tape-shadow{fill:none;stroke:var(--ftl-deck-shadow,#04121f);stroke-width:6;stroke-linecap:round;stroke-linejoin:round;opacity:0.9}
.r2r .tape{fill:none;stroke:var(--ftl-deck-tape,#14507e);stroke-width:3;stroke-linecap:round;stroke-linejoin:round;opacity:0.8}
.r2r .tape.active{stroke:var(--glow);stroke-width:3;opacity:0.85;stroke-dasharray:22 14;animation:tapeflow 0.55s linear infinite;filter:drop-shadow(0 0 5px rgba(0,217,255,0.45))}
@keyframes tapeflow{to{stroke-dashoffset:-36}}
/* Guide idlers: lit rims so the tape path reads at a glance. */
.r2r .guide{fill:var(--ftl-deck-well,#0a1830);stroke:rgba(0,217,255,0.55);stroke-width:1.5}
/* The read/write head block: a chamfered angular castle rising out of the
   tape gap, with glowing trim rails on its mounting cheeks, the red centre
   gap line and the large 7-segment digital time counter in its display
   window. The centre gap line turns recording-red while a take is running
   (.r2r.rec). */
.r2r .head-plate{fill:url(#headFace);stroke:var(--border);stroke-width:1.5}
.r2r .head-edge{fill:none;stroke:rgba(0,217,255,0.25);stroke-width:1}
.r2r .head-gap{fill:none;stroke:var(--glow);stroke-width:3;stroke-linecap:round;opacity:0.55}
.r2r.run .head-gap{opacity:0.75}
.r2r.rec .head-gap{stroke:var(--rec);opacity:0.95;filter:drop-shadow(0 0 5px rgba(255,51,85,0.8))}
.r2r .head-window{fill:#050d1a;stroke:#16456e;stroke-width:1.5}
.r2r .head-win-grid{fill:none;stroke:rgba(0,217,255,0.07);stroke-width:1}
/* Bottom HUD band: a thin status rail with system lamps and micro labels,
   matching the larger panel HUD motif (corner brackets + glow). */
.r2r .hud-band{fill:none;stroke:var(--border);stroke-width:1}
.r2r .hud-lamp{fill:#11304a}
.r2r .hud-lamp.on{fill:var(--idle);filter:drop-shadow(0 0 3px var(--idle))}
.r2r .hud-lamp.rec{fill:var(--rec);filter:drop-shadow(0 0 3px var(--rec))}
.r2r #linkLamp{fill:#15324a}
.r2r #linkLamp.on{fill:rgba(0,217,255,0.9);filter:drop-shadow(0 0 3px rgba(0,217,255,0.8))}
.r2r .hud-text{fill:var(--dim);font-size:9px;letter-spacing:0.22em;font-family:"Consolas",monospace}
/* The lit 7-segment time display. Every segment is an SVG line (see the
   buildSeg7 JS); the dim .s7 shows all segments faintly so the display
   reads as a proper 7-segment counter even for unlit digits. The whole display
   is skewed to the right for an italic, forward-leaning readout. */
#seg7{font-style:italic}
#seg7 .s7{stroke:rgba(0,180,255,0.16);stroke-width:2.5;stroke-linecap:round}
#seg7 .s7.on{stroke:var(--glow);filter:drop-shadow(0 0 4px rgba(0,217,255,0.75))}
#seg7 .s7-dot{fill:rgba(0,180,255,0.16)}
#seg7 .s7-dot.on{fill:var(--glow);filter:drop-shadow(0 0 4px rgba(0,217,255,0.75))}

/* Pinned meter footer: always visible at the bottom of the viewport so the
   VU levels stay on screen while you operate the transport, with a slim
   header bar that collapses/expands the meter bank on demand. */
.meter-footer{position:fixed;left:0;right:0;bottom:0;z-index:150;background:rgba(3,8,15,0.94);border-top:1px solid var(--border);box-shadow:0 -8px 30px rgba(0,180,255,0.10);backdrop-filter:blur(2px)}
.meter-bar{display:flex;align-items:center;gap:1em;padding:0.3em 1.2em;border-bottom:1px solid var(--border)}
.meter-title{font-size:0.7em;letter-spacing:0.25em;color:var(--dim);text-transform:uppercase}
.meter-badge{font-size:0.62em;letter-spacing:0.12em;color:var(--glow);border:1px solid var(--border);border-radius:10px;padding:0.05em 0.6em}
.meter-caret{width:1.9em;height:1.9em;border-radius:50%;margin-left:auto}
.meter-body{padding:0.7em 1em;transition:max-height 0.25s ease,opacity 0.25s ease,padding 0.25s ease;max-height:220px;overflow:hidden}
.meter-footer.collapsed .meter-body{max-height:0;padding-top:0;padding-bottom:0;opacity:0}
/* The meter bank itself - a shared dB-FS scale (standard audio-meter log
   taper, see VU_CURVE/vuPct in the script) beside one meter per channel. */
.meter-bridge{display:flex;align-items:stretch;justify-content:center;gap:0.8em;max-width:1300px;margin:0 auto;background:#050c16;border:1px solid var(--border);border-radius:10px;padding:0.7em 1em;box-shadow:inset 0 0 24px rgba(0,180,255,0.06)}
/* The dB scale column and every meter track share the exact same inner
   height so a given dB reading lands on the same pixel row in each. The
   scale uses a transparent 1px border (see below) so its content box equals
   the tracks' full height. */
.db-scale{position:relative;height:var(--meter-h);width:2.6em;flex:none;border:1px solid transparent}
.db-scale span{position:absolute;left:0;right:0.3em;text-align:right;transform:translateY(50%);font-size:0.6em;color:var(--dim);font-weight:bold}
.db-scale span::after{content:'';position:absolute;right:0;top:50%;width:100%;height:1px;background:rgba(0,217,255,0.25);transform:translateY(50%)}
.ch-meters{display:flex;justify-content:center;gap:0.6em;overflow-x:auto;padding-bottom:2px}
.ch-meter{display:flex;flex-direction:column;align-items:center;gap:0.25em;flex:none}
/* No real border on the track: the fill's height% and the scale labels' % are
   then resolved against the same full --meter-h box, so the top of the fill
   touches exactly the same row as the matching dB tick text beside it. A
   ring is drawn via box-shadow instead so the darker background still reads
   as a channel well. */
.vu-track{position:relative;width:14px;height:var(--meter-h);background:#020509;box-shadow:inset 0 0 0 1px var(--border);border-radius:2px}
/* The meter bands honor the design colors at absolute dBFS: green below
   -18dBFS, yellow -18..-6dBFS, red above -6dBFS. The stops are percentages
   of the track, computed in JS from the configured floor via the same
   vuPct() curve used for the ticks and fills (--vu-g / --vu-r below), so a
   floor change repositions the bands instead of leaving them tuned to one
   range. The gradient is sized to the full --meter-h track (not the fill's
   own height) and pinned to the bottom, so the fill only reveals the band up
   to its current level - a low bar reads green, a mid bar yellow, a hot bar
   red - instead of the whole green->red ramp compressing into every bar
   regardless of level. */
.vu-fill{position:absolute;bottom:0;left:1px;right:1px;height:0%;background:linear-gradient(to top,#0aff9d 0%,#0aff9d var(--vu-g,58%),#ffe400 var(--vu-g,58%),#ffe400 var(--vu-r,90%),#ff2a2a var(--vu-r,90%),#ff2a2a 100%);background-size:100% var(--meter-h);background-position:0 100%;background-repeat:no-repeat;box-shadow:0 0 8px rgba(0,255,180,0.35)}
.vu-peak{position:absolute;left:1px;right:1px;height:2px;background:#fff;box-shadow:0 0 6px #fff}
.ch-label{font-size:0.6em;color:var(--dim);letter-spacing:0.04em}

/* Telemetry panel: collapsible system stats with per-core mini graphs */
.sys-readout{margin-top:.5em;font-size:.72em;color:var(--dim)}
.sys-readout p{margin:.25em 0}
.sys-graphs{margin-top:.6em}
.sys-graphs h3{font-size:.68em;letter-spacing:.18em;text-transform:uppercase;color:var(--dim);margin:.7em 0 .2em}
.sys-graphs .uplot{width:100%}
.sys-graphs .u-legend{font-size:.68em;color:var(--dim);background:transparent;border:none;padding-left:0}
.sys-graphs .u-legend th{font-weight:normal}
.sys-graphs .u-legend .u-value{color:var(--text)}
.sys-wait{font-size:.72em;color:var(--dim)}

/* Mobile: stack the three-column grid, let the fixed OLED frame shrink to
    the viewport instead of overflowing it, and give the header/footer more
    vertical room now that their contents wrap onto more lines. */
/* Mobile: the header already scales fluidly with vw widths (see the clamp()
    rules above), so here we only need to clear the fixed decorations that
    would crowd out the OLED and controls on a phone: hide the logo and the
    meter footer badge, tighten gutters, and collapse the status columns to a
    single column. The OLED, encoder, and transport keep shrinking with the
    viewport so the single-row deck never overflows. */
@media (max-width: 800px) {
  body{padding:0 0.4em 200px}
  header.deck{flex-wrap:wrap}
  .header-actions{top:0.6em;right:0.6em}
  .deck-logo{flex:1 0 100%;text-align:center;display:block}
  .deck-logo .logo-svg{width:clamp(80px,20vw,200px)}
  .oled-frame{flex:0 0 auto}
  .encoder-row{flex:0 0 auto}
  .transport-row{flex:1 0 100%;justify-content:center}
  .oled-frame img{width:min(190px,40vw);height:auto;aspect-ratio:4/1}
  .grid{grid-template-columns:1fr}
  .transport-deck{padding:0}
  .meter-title{font-size:0.6em}
  .meter-badge{display:none}
  .meter-body{padding:0.6em 0.5em}
}

body.meters-collapsed{padding-bottom:4em}

@media (prefers-reduced-motion: reduce) {
  .r2r .reel-g.spinning .reel-spin,
  .r2r .tape.active,
  .meter-body{animation:none;transition:none}
  .sci-switch-track,.sci-thumb{transition:none}
}

/* WiFi settings panel */
.wifi-qr-row{display:flex;align-items:center;gap:1em;flex-wrap:wrap}
.wifi-qr-info p{margin:0.2em 0;font-size:0.85em}
.wifi-qr-img img{width:180px;height:180px;image-rendering:pixelated;border:1px solid var(--border);border-radius:4px}
/* Only when a theme is active: let the theme's own page background show
   through instead of the built-in gradient. With data-theme="none" this
   selector never matches and the dashboard paints exactly as before. */
html[data-theme]:not([data-theme="none"]) body{background:transparent}
</style>
<link id="themecss" rel="stylesheet"{{if .ThemeCSS}} href="{{.ThemeCSS}}"{{end}}></head>
<body class="ftl-app">

<!-- ftl-app shell (ftl-themes#3): dual-classed regions so layout themes can
     arrange the page while the built-in look (no theme linked) renders
     exactly as before - the app's own selectors keep matching, and the
     none-case rules below neutralize the new hooks. -->
<header class="deck ftl-app-bar">
  <div class="deck-logo">{{.Logo}}</div>
  <div class="oled-frame"><img id="oled" src="/api/display.png" alt="OLED display" onerror="if(!this.dataset.r){this.dataset.r=1;location.reload()}"></div>
  <div class="encoder-row">
    <button hx-post="/api/input/encoder/left">&#9664;</button>
    <button class="click" hx-post="/api/input/encoder/click">&#9679;</button>
    <button hx-post="/api/input/encoder/right">&#9654;</button>
  </div>
  <div class="transport-row" id="transportRow"></div>
  <div class="header-actions">
    <span class="icon-btn conn off" id="connLamp" title="Server disconnected">
      <svg viewBox="0 0 16 16" fill="currentColor"><path d="M3.05 3.05a7 7 0 0 0 0 9.9.5.5 0 0 1-.707.707 8 8 0 0 1 0-11.314.5.5 0 0 1 .707.707m2.122 2.122a4 4 0 0 0 0 5.656.5.5 0 1 1-.708.708 5 5 0 0 1 0-7.072.5.5 0 0 1 .708.708m5.656-.708a.5.5 0 0 1 .708 0 5 5 0 0 1 0 7.072.5.5 0 1 1-.708-.708 4 4 0 0 0 0-5.656.5.5 0 0 1 0-.708m2.122-2.12a.5.5 0 0 1 .707 0 8 8 0 0 1 0 11.313.5.5 0 0 1-.707-.707 7 7 0 0 0 0-9.9.5.5 0 0 1 0-.707zM6 8a2 2 0 1 1 2.5 1.937V15.5a.5.5 0 0 1-1 0V9.937A2 2 0 0 1 6 8"/></svg>
    </span>
    <button class="icon-btn" id="settingsBtn" type="button" title="Settings">
      <svg viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 00.33 1.82l.06.06a2 2 0 11-2.83 2.83l-.06-.06a1.65 1.65 0 00-1.82-.33 1.65 1.65 0 00-1 1.51V21a2 2 0 01-4 0v-.09a1.65 1.65 0 00-1-1.51 1.65 1.65 0 00-1.82.33l-.06.06a2 2 0 11-2.83-2.83l.06-.06a1.65 1.65 0 00.33-1.82 1.65 1.65 0 00-1.51-1H3a2 2 0 010-4h.09a1.65 1.65 0 001.51-1 1.65 1.65 0 00-.33-1.82l-.06-.06a2 2 0 112.83-2.83l.06.06a1.65 1.65 0 001.82.33H9a1.65 1.65 0 001-1.51V3a2 2 0 014 0v.09a1.65 1.65 0 001 1.51 1.65 1.65 0 001.82-.33l.06-.06a2 2 0 112.83 2.83l-.06.06a1.65 1.65 0 00-.33 1.82V9a1.65 1.65 0 001.51 1H21a2 2 0 010 4h-.09a1.65 1.65 0 00-1.51 1z"/></svg>
    </button>
    <form action="/logout" method="POST" style="display:inline;margin:0">
      <button class="icon-btn" title="Log out" aria-label="Log out">
      <svg viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M16 17l5-5-5-5M21 12H9M12 19H5a2 2 0 01-2-2V7a2 2 0 012-2h7"/></svg>
      </button>
    </form>
  </div>
</header>

<aside class="ftl-app-rail" aria-hidden="true"></aside>

<main class="ftl-app-main">
<div class="grid">

  <div class="panel left">
    <h2>System</h2>
    <div class="sys-graphs">
      <h3>CPU %</h3>
      <div id="cpuChart"><span class="sys-wait">collecting&hellip;</span></div>
      <h3>RAM MB</h3>
      <div id="ramChart"><span class="sys-wait">collecting&hellip;</span></div>
      <h3>Temp &deg;C</h3>
      <div id="tempChart"><span class="sys-wait">collecting&hellip;</span></div>
      <h3>Disk free GB</h3>
      <div id="diskChart"><span class="sys-wait">collecting&hellip;</span></div>
      <pre id="teleHist" hidden></pre>
    </div>
  </div>

  <div class="panel center">
    <h2>Transport Status</h2>
    <div class="transport-deck">
      <svg class="r2r" viewBox="0 0 820 265" preserveAspectRatio="xMidYMid meet" role="img" aria-labelledby="transportTitle transportDesc">
        <title id="transportTitle">Reel-to-reel transport</title>
        <desc id="transportDesc">Two tape reels connected by an angled tape path and a read/write head time display.</desc>
        <defs>
          <linearGradient id="deckBg" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stop-color="#132947"/>
            <stop offset="55%" stop-color="#0d1c31"/>
            <stop offset="100%" stop-color="#0a1526"/>
          </linearGradient>
          <linearGradient id="headFace" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stop-color="#142f4d"/>
            <stop offset="100%" stop-color="#0a1a30"/>
          </linearGradient>
        </defs>

        <rect class="plate" x="2" y="2" width="816" height="261" rx="10"/>
        <rect class="plate-bezel" x="2" y="2" width="816" height="261" rx="10"/>

        <g class="deck-grid">
          <path d="M40 26 H780 M40 248 H780"/>
          <path d="M90 26 V248 M730 26 V248" opacity="0.5"/>
        </g>

        <circle class="plate-screw" cx="18" cy="18" r="4"/>
        <circle class="plate-screw" cx="802" cy="18" r="4"/>
        <circle class="plate-screw" cx="18" cy="247" r="4"/>
        <circle class="plate-screw" cx="802" cy="247" r="4"/>

        <path class="tape-shadow" d="M211 161 L330 196 L490 196 L609 161"/>
        <path class="tape" id="tapePath" d="M211 159 L330 194 L490 194 L609 159"/>

        <g class="reel-g" id="reelL" transform="translate(170,105) scale(1.25) translate(-170,-105)">
          <circle class="reel-ring" cx="170" cy="105" r="61"/>
          <circle class="reel-disc" cx="170" cy="105" r="58"/>
          <g class="reel-spin">
            <path class="reel-wind" d="M170 105 m0 -54 a54 54 0 0 1 0 108 a54 54 0 0 1 0 -108"/>
            <path class="reel-wind" d="M170 105 m0 -46 a46 46 0 0 1 0 92 a46 46 0 0 1 0 -92"/>
            <path class="reel-wind" d="M170 105 m0 -38 a38 38 0 0 1 0 76 a38 38 0 0 1 0 -76"/>
            <path class="reel-wind" d="M170 105 m0 -30 a30 30 0 0 1 0 60 a30 30 0 0 1 0 -60"/>
            <path class="reel-spoke" d="M170 105 L161 64 Q170 59 179 64 Z"/>
            <path class="reel-spoke" d="M170 105 L161 64 Q170 59 179 64 Z" transform="rotate(120 170 105)"/>
            <path class="reel-spoke" d="M170 105 L161 64 Q170 59 179 64 Z" transform="rotate(240 170 105)"/>
            <circle class="reel-hub" cx="170" cy="105" r="13"/>
            <circle class="reel-center" cx="170" cy="105" r="5"/>
          </g>
        </g>
        <g class="reel-g" id="reelR" transform="translate(650,105) scale(1.25) translate(-650,-105)">
          <circle class="reel-ring" cx="650" cy="105" r="61"/>
          <circle class="reel-disc" cx="650" cy="105" r="58"/>
          <g class="reel-spin">
            <path class="reel-wind" d="M650 105 m0 -54 a54 54 0 0 1 0 108 a54 54 0 0 1 0 -108"/>
            <path class="reel-wind" d="M650 105 m0 -46 a46 46 0 0 1 0 92 a46 46 0 0 1 0 -92"/>
            <path class="reel-wind" d="M650 105 m0 -38 a38 38 0 0 1 0 76 a38 38 0 0 1 0 -76"/>
            <path class="reel-wind" d="M650 105 m0 -30 a30 30 0 0 1 0 60 a30 30 0 0 1 0 -60"/>
            <path class="reel-spoke" d="M650 105 L641 64 Q650 59 659 64 Z"/>
            <path class="reel-spoke" d="M650 105 L641 64 Q650 59 659 64 Z" transform="rotate(120 650 105)"/>
            <path class="reel-spoke" d="M650 105 L641 64 Q650 59 659 64 Z" transform="rotate(240 650 105)"/>
            <circle class="reel-hub" cx="650" cy="105" r="13"/>
            <circle class="reel-center" cx="650" cy="105" r="5"/>
          </g>
        </g>

        <text class="hud-text" x="410" y="122" text-anchor="middle">PI9696</text>

        <circle class="guide" cx="221" cy="162" r="7"/>
        <circle class="guide" cx="599" cy="162" r="7"/>
        <circle class="guide" cx="330" cy="194" r="7"/>
        <circle class="guide" cx="490" cy="194" r="7"/>

        <g class="head">
          <polygon class="head-plate" points="272,252 272,198 288,184 532,184 548,198 548,252"/>
          <path class="head-edge" d="M284 198 V246 M536 198 V246"/>
          <path class="head-gap" d="M402 190 L418 190"/>
          <rect class="head-window" x="300" y="198" width="220" height="48" rx="3"/>
          <path class="head-win-grid" d="M304 210 H516 M304 222 H516 M304 234 H516 M304 246 H516 M324 198 V246 M348 198 V246 M372 198 V246 M396 198 V246 M420 198 V246 M444 198 V246 M468 198 V246 M492 198 V246"/>
          <g id="seg7" transform="translate(312,202) skewX(-10) scale(2.12)"></g>
        </g>

        <text class="hud-text" x="146" y="24">SUPPLY</text>
        <text class="hud-text" x="614" y="24">TAKE-UP</text>

        <g class="hud">
          <path class="hud-band" d="M40 249 H780"/>
          <circle class="hud-lamp" id="sysLamp" cx="54" cy="255" r="2.5"/>
          <text class="hud-text" x="66" y="258">SYS</text>
          <text class="hud-text" x="352" y="258">TRANSPORT</text>
          <text class="hud-text" x="620" y="258">INFERNO-LINK</text>
          <circle class="hud-lamp" id="linkLamp" cx="706" cy="255" r="2.5"/>
        </g>

        <path class="deck-corner" d="M14 30 V14 H30"/>
        <path class="deck-corner" d="M806 14 H790 V30"/>
        <path class="deck-corner" d="M14 235 V251 H30"/>
        <path class="deck-corner" d="M806 251 V235 H790"/>
      </svg>
    </div>
    <div id="status">Loading...</div>
    <div id="teleSock" hx-ext="ws" hx-ws:connect="/ws/telemetry" hx-target="#status" hx-swap="innerHTML" hidden></div>
  </div>

  <div class="panel right">
    <h2>Status</h2>
    <div id="config" hx-get="/api/config" hx-trigger="load" hx-swap="innerHTML">Loading...</div>
  </div>

</div>

<div class="recordings-section">
  <h2>Recordings <a class="dl-all" href="/download-all" title="Download every recording as one ZIP archive (with a manifest.txt listing each file)">Download ALL (.zip)</a></h2>
  <div id="recordings" hx-get="/api/recordings" hx-trigger="load" hx-swap="innerHTML">Loading...</div>
</div>
</main>

<footer class="meter-footer ftl-app-status" id="meterFooter">
  <div class="meter-bar">
    <span class="meter-title">Level meters</span>
    <span class="meter-badge" id="meterBadge">--</span>
    <button class="icon-btn meter-caret" id="meterToggle" type="button" title="Collapse/expand meters" aria-label="Collapse or expand level meters" aria-controls="meterBody" aria-expanded="true">
      <svg id="meterCaretSvg" viewBox="0 0 24 24" fill="none" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M6 9l6 6 6-6"/></svg>
    </button>
  </div>
  <div class="meter-body" id="meterBody">
    <div class="meter-bridge">
      <div class="db-scale" id="dbScale"></div>
      <div class="ch-meters" id="chMeters"></div>
    </div>
  </div>
</footer>

<div class="modal-backdrop" id="settingsModal">
  <div class="modal modal--settings">
    <div class="modal-head">
      <h2>Unit Settings</h2>
      <button class="modal-close" id="settingsClose" type="button" aria-label="Close settings">&times;</button>
    </div>
    <div class="modal-body">
      <section class="settings-group">
        <h3 class="settings-group-title">Device</h3>
        <div id="devicename" class="setting-cell">
          <div class="setting-row">
            <form hx-post="/api/device-name" hx-target="#devicename" hx-swap="outerHTML">
              <label for="deviceNameInput">Unit Name</label>
              <input id="deviceNameInput" name="name" value="{{.DeviceName}}" maxlength="32" pattern="[A-Za-z0-9 _-]+" title="Letters, numbers, spaces, - and _ only">
              <button type="submit" class="btn-primary">Save</button>
            </form>
          </div>
        </div>
      </section>

      <section class="settings-group">
        <h3 class="settings-group-title">Audio</h3>
        {{.SampleRateFragment}}
        {{.ChannelCountFragment}}
        {{.MonitorFragment}}
      </section>

      <section class="settings-group">
        <h3 class="settings-group-title">Metering</h3>
        {{.VURangeFragment}}
        {{.PeakHoldFragment}}
      </section>

      <section class="settings-group">
        <h3 class="settings-group-title">Metadata</h3>
        {{.PrefixFragment}}
        {{.TagFragment}}
      </section>

      <section class="settings-group">
        <h3 class="settings-group-title">Transport</h3>
        {{.TransportFragment}}
      </section>

      <section class="settings-group">
        <h3 class="settings-group-title">Display</h3>
        {{.ThemeFragment}}
        {{.BrightnessFragment}}
        {{.AutoDimFragment}}
      </section>

      <section class="settings-group">
        <h3 class="settings-group-title">Demo</h3>
        {{.DemoFragment}}
      </section>

      <section class="settings-group">
        <h3 class="settings-group-title">Logging</h3>
        {{.LogLevelFragment}}
      </section>

      <section class="settings-group">
        <h3 class="settings-group-title">Config</h3>
        <div class="setting-row setting-row--pair">
          <button hx-post="/api/config/export" hx-target="#config-msg" class="btn-primary">Export to USB</button>
          <button hx-post="/api/config/import" hx-target="#config-msg" class="btn-primary">Import from USB</button>
        </div>
        <div class="setting-row" id="config-msg"></div>
      </section>

      <section class="settings-group">
        <h3 class="settings-group-title">Network</h3>
        <div id="wifi-settings">
          {{.WifiQRFragment}}
          <form hx-post="/api/settings/wifi" hx-target="#wifiqr" hx-swap="outerHTML" hx-status:400="target:#wifi-error">
            <div class="setting-row setting-row--pair">
              <span class="field">
                <label for="wifiSsid">SSID</label>
                <input id="wifiSsid" name="ssid" value="{{.WifiSSID}}" maxlength="32" required>
              </span>
              <span class="field">
                <label for="wifiPass">Password</label>
                <input id="wifiPass" name="password" type="password" value="{{.WifiPassword}}" minlength="8" maxlength="63" required>
              </span>
            </div>
            <div class="setting-row setting-row--switch">
              <label for="wifiEnabled">Access Point</label>
              <label class="sci-switch" for="wifiEnabled">
                <input id="wifiEnabled" name="enabled" type="checkbox" {{if .WifiEnabled}}checked{{end}}>
                <span class="sci-switch-track"><span class="sci-thumb"></span></span>
                <span class="switch-readout" data-on="ONLINE" data-off="OFFLINE"></span>
              </label>
            </div>
            <button type="submit" class="btn-primary">Save WiFi</button>
          </form>
          <div id="wifi-error"></div>
        </div>
      </section>
    </div>
  </div>
</div>

<div class="modal-backdrop" id="stopModal">
  <div class="modal modal--confirm">
    <button class="modal-close" id="stopModalClose" type="button" aria-label="Close">&times;</button>
    <h2>Stop Recording?</h2>
    <p class="stop-prompt">The current take is still being written. Stop it now?</p>
    <div class="stop-actions">
      <button id="stopConfirm" class="rec" type="button">Stop</button>
      <button id="stopCancel" type="button">Keep recording</button>
    </div>
  </div>
</div>

<script>
// The OLED mirror reloads on framebuffer generation change (see
// displaySeq in the meter payload), not on a blind interval - static
// screens cost zero image fetches, and this shares eth0 with Inferno's
// audio-over-IP traffic.

var settingsBtn = document.getElementById('settingsBtn');
var settingsModal = document.getElementById('settingsModal');
settingsBtn.addEventListener('click', function() { settingsModal.classList.add('open'); });
document.getElementById('settingsClose').addEventListener('click', function() { settingsModal.classList.remove('open'); });
settingsModal.addEventListener('click', function(e) { if (e.target === settingsModal) settingsModal.classList.remove('open'); });

// Transport buttons are drawn by JS so they can switch between ICON and TEXT
// mode (Settings -> Transport Buttons) and, for the PLAY key, between a play
// triangle and a pause glyph while a track runs. ICON_MODE is seeded from the
// server (persisted setting); transportState is kept in sync by applyMeter.
var ICON_MODE = {{.TransportIcon}};
var transportState = { playing: false, paused: false };
function playGlyph()  { return '<svg viewBox="0 0 16 16"><polygon points="4,2 14,8 4,14"/></svg>'; }
function pauseGlyph() { return '<svg viewBox="0 0 16 16"><rect x="3.5" y="2.5" width="3.4" height="11"/><rect x="9.1" y="2.5" width="3.4" height="11"/></svg>'; }
function transportBtn(cls, post, title, label) {
  return '<button class="' + cls + '" hx-post="' + post + '" title="' + title + '">' + label + '</button>';
}
function renderTransportRow() {
  var row = document.getElementById('transportRow');
  var pause = transportState.playing || transportState.paused;
  var title = transportState.paused ? 'Resume' : (transportState.playing ? 'Pause' : 'Play');
  var html = '';
  if (ICON_MODE) {
    html += transportBtn('record', '/api/input/button/record', 'Record', '<svg viewBox="0 0 16 16"><circle cx="8" cy="8" r="6"/></svg>');
    html += '<button class="stop" data-stop title="Stop"><svg viewBox="0 0 16 16"><rect x="3" y="3" width="10" height="10"/></svg></button>';
    html += transportBtn(pause ? 'play pause' : 'play', '/api/input/button/play', title, pause ? pauseGlyph() : playGlyph());
  } else {
    html += transportBtn('record', '/api/input/button/record', 'Record', 'REC');
    html += '<button class="stop" data-stop title="Stop">STOP</button>';
    html += transportBtn(pause ? 'play pause' : 'play', '/api/input/button/play', title, pause ? 'II' : '>');
  }
  row.className = 'transport-row' + (ICON_MODE ? '' : ' text');
  row.innerHTML = html;
  // These controls are recreated after htmx's initial DOM scan whenever the
  // play/pause state or button style changes, so explicitly process the new
  // hx-post nodes.
  if (window.htmx) htmx.process(row);
}
renderTransportRow();

// When the Transport Buttons setting changes in the settings modal, htmx
// swaps the fragment - listen for that and re-seed ICON_MODE from the
// select's own value (0=icon,1=text) so the header updates instantly.
document.body.addEventListener('htmx:after:swap', function(e) {
  // htmx 4 shape is {ctx, cancelled}: target lives on detail.ctx.target
  // (same fallback as the teleHist listener below).
  var d = e.detail || {}, t = d.target || (d.ctx && d.ctx.target);
  var id = t && (t.id || t);
  if (id === 'transportmode' || id === '#transportmode') {
    var sel = (t.querySelector ? t : document.getElementById('transportmode')).querySelector('select[name="idx"]');
    if (sel) { ICON_MODE = (sel.value === '0'); renderTransportRow(); }
  }
});

// Touch screens: stopping a take can be a fat-finger accident, so the STOP
// transport and the status-panel Stop button first open a confirmation modal
// instead of tearing the recording down immediately. Desktop (non-touch)
// keeps the one-tap behaviour. The transport STOP is a plain button handled
// here; the status-panel Stop (data-record-stop, an hx-post button) is only
// intercepted on touch devices.
var isTouch = ('ontouchstart' in window) || navigator.maxTouchPoints > 0;
var stopModal = document.getElementById('stopModal');
var stopPending = null;
function confirmStop(action) {
  if (!isTouch) { action(); return; }
  stopPending = action;
  stopModal.classList.add('open');
}
document.getElementById('stopConfirm').addEventListener('click', function() {
  stopModal.classList.remove('open');
  if (stopPending) { var a = stopPending; stopPending = null; a(); }
});
document.getElementById('stopCancel').addEventListener('click', function() { stopModal.classList.remove('open'); stopPending = null; });
document.getElementById('stopModalClose').addEventListener('click', function() { stopModal.classList.remove('open'); stopPending = null; });
stopModal.addEventListener('click', function(e) { if (e.target === stopModal) { stopModal.classList.remove('open'); stopPending = null; } });
// Capture this before htmx sees the status-panel stop button. Otherwise its
// hx-post listener can submit the stop request before the touch confirmation
// handler at the document bubble phase gets a chance to cancel it.
document.addEventListener('click', function(e) {
  var stopBtn = e.target.closest ? e.target.closest('[data-stop]') : null;
  var recStopBtn = e.target.closest ? e.target.closest('[data-record-stop]') : null;
  if (stopBtn) {
    e.preventDefault();
    e.stopPropagation();
    confirmStop(function(){ fetch('/api/input/button/stop', { method: 'POST' }); });
  } else if (recStopBtn && isTouch) {
    e.preventDefault();
    e.stopPropagation();
    confirmStop(function(){ fetch('/api/record/stop', { method: 'POST' }); });
  }
}, true);

// Standard audio-meter log taper: 0dBFS at the top, the configured floor
// (see FLOOR below, updated from every meter message's floorDB - Settings
// -> Meter Range) at the bottom, with more of the scale's height given to
// the top of the range than the bottom. VU_CURVE is expressed as {fraction
// of the range from floor (0) to 0dBFS (1), display %} pairs, mirroring
// main.go's idleVUCurve exactly, so a config change on the OLED reshapes
// this scale identically rather than the two drifting apart.
var VU_CURVE = [[0, 0], [0.1667, 7], [0.3333, 15], [0.4444, 22], [0.5556, 30], [0.6667, 40], [0.7333, 48], [0.8, 58], [0.8667, 70], [0.9, 78], [0.9333, 85], [0.9667, 92], [1, 100]];
var FLOOR = -90;
function vuPct(db) {
  var frac = (db - FLOOR) / (0 - FLOOR);
  if (frac <= 0) return VU_CURVE[0][1];
  if (frac >= 1) return 100;
  for (var i = 1; i < VU_CURVE.length; i++) {
    if (frac <= VU_CURVE[i][0]) {
      var lo = VU_CURVE[i - 1], hi = VU_CURVE[i];
      var t = (frac - lo[0]) / (hi[0] - lo[0]);
      return lo[1] + t * (hi[1] - lo[1]);
    }
  }
  return 100;
}

// rebuildDbScale redraws the tick labels whenever the configured floor
// changes (including on first load) - ticks sit at the same curve
// fractions used for VU_CURVE's own breakpoints, so every tick lines up
// exactly with where that dB value's fill reaches.
var dbScale = document.getElementById('dbScale');
var scaleFloor = null;
function rebuildDbScale(floor) {
  if (floor === scaleFloor) return;
  scaleFloor = floor;
  dbScale.innerHTML = '';
  [0, 0.1667, 0.3333, 0.5556, 0.8, 1].forEach(function(frac) {
    var db = Math.round(floor + frac * (0 - floor));
    var span = document.createElement('span');
    span.style.bottom = (frac * 100) + '%';
    span.textContent = db;
    dbScale.appendChild(span);
  });
  // Position the green->yellow and yellow->red meter bands at the design's
  // absolute thresholds (-18 / -6 dBFS) mapped through the current floor.
  var root = document.documentElement;
  root.style.setProperty('--vu-g', vuPct(-18) + '%');
  root.style.setProperty('--vu-r', vuPct(-6) + '%');
}

// ---- 7-segment time display (inline SVG segments, italic via skewX) ----
var svgNS = 'http://www.w3.org/2000/svg';
// Segment endpoints per digit (each digit is 10x18, segments stroke-width 2, round caps)
// a: top horiz, g: middle, d: bottom horiz, f: top-left vert, b: top-right vert, e: bot-left, c: bot-right
var SEG = {
  a: {x1:3, y1:2, x2:7, y2:2},
  g: {x1:3, y1:9, x2:7, y2:9},
  d: {x1:3, y1:16, x2:7, y2:16},
  f: {x1:2, y1:3, x2:2, y2:7},
  b: {x1:8, y1:3, x2:8, y2:7},
  e: {x1:2, y1:10, x2:2, y2:14},
  c: {x1:8, y1:10, x2:8, y2:14}
};
var SEG_ON = {
  '0': 'abcfed', '1': 'bc', '2': 'abged', '3': 'abgcd', '4': 'fgbc', '5': 'afgcd', '6': 'afgedc', '7': 'abc',
  '8': 'abcdefg', '9': 'abcfgd', ':': 'dots'
};
var seg7Root = document.getElementById('seg7');
var lastSeg7 = '';

function buildSeg7() {
  if (!seg7Root) return;
  seg7Root.innerHTML = '';
  // 8 glyph positions: 6 digits + 2 colons (HH:MM:SS)
  var x = 0;
  for (var p = 0; p < 8; p++) {
    var g = document.createElementNS(svgNS, 'g');
    var tx = x, ty = 0, sc = 1;
    // Seconds digits (positions 6,7) render at 75%, bottom-aligned with
    // the HH:MM pair (ty 3.5 = full 17px baseline minus scaled 13.5px).
    if (p >= 6) { tx = x + 1.25; ty = 3.5; sc = 0.75; }
    g.setAttribute('transform', 'translate(' + tx + ',' + ty + ') scale(' + sc + ')');
    g.setAttribute('data-pos', p);
    var isColon = (p === 2 || p === 5);
    if (isColon) {
      var dot1 = document.createElementNS(svgNS, 'circle');
      dot1.setAttribute('cx', 5); dot1.setAttribute('cy', 4); dot1.setAttribute('r', 2);
      dot1.className.baseVal = 's7-dot';
      var dot2 = document.createElementNS(svgNS, 'circle');
      dot2.setAttribute('cx', 5); dot2.setAttribute('cy', 14); dot2.setAttribute('r', 2);
      dot2.className.baseVal = 's7-dot';
      g.appendChild(dot1); g.appendChild(dot2);
    } else {
      var segs = 'abgfedc';
      for (var si = 0; si < segs.length; si++) {
        var s = segs[si];
        var ln = document.createElementNS(svgNS, 'line');
        ln.setAttribute('x1', SEG[s].x1); ln.setAttribute('y1', SEG[s].y1);
        ln.setAttribute('x2', SEG[s].x2); ln.setAttribute('y2', SEG[s].y2);
        ln.setAttribute('stroke-width', '2');
        ln.setAttribute('stroke-linecap', 'round');
        ln.className.baseVal = 's7';
        ln.setAttribute('data-seg', s);
        g.appendChild(ln);
      }
    }
    seg7Root.appendChild(g);
    x += isColon ? 10 : 12;
  }
}

function setSeg7(str) {
  if (!seg7Root) return;
  var display = (typeof str === 'string' && /^\d{2}:\d{2}:\d{2}$/.test(str)) ? str : '00:00:00';
  if (display === lastSeg7) return;
  lastSeg7 = display;
  var nodes = seg7Root.querySelectorAll('g[data-pos]');
  var idx = 0;
  for (var p = 0; p < display.length && idx < nodes.length; p++) {
    var ch = display[p];
    var isColon = (ch === ':');
    var g = nodes[idx++];
    if (isColon) {
      var dots = g.querySelectorAll('.s7-dot');
      dots.forEach(function(d) { d.classList.add('on'); });
    } else {
      var on = SEG_ON[ch] || '';
      var lines = g.querySelectorAll('line.s7');
      lines.forEach(function(l) {
        var seg = l.getAttribute('data-seg');
        l.classList.toggle('on', on.indexOf(seg) >= 0);
      });
    }
  }
}

// Initialize seg7 on load. This avoids a blank/ghost-only head window before
// the first WebSocket packet arrives.
buildSeg7();
setSeg7('00:00:00');

// ---- Pinned meter footer collapse/expand ----
var meterFooter = document.getElementById('meterFooter');
var meterToggle = document.getElementById('meterToggle');
var meterBody = document.getElementById('meterBody');
var meterCaret = document.getElementById('meterCaretSvg');
if (meterToggle && meterFooter && meterBody && meterCaret) {
  var collapsed = localStorage.getItem('pi9696_meterCollapsed') === '1';
  function applyCollapse() {
    meterFooter.classList.toggle('collapsed', collapsed);
    document.body.classList.toggle('meters-collapsed', collapsed);
    meterCaret.style.transform = collapsed ? 'rotate(-90deg)' : '';
    meterToggle.setAttribute('aria-expanded', !collapsed);
  }
  applyCollapse();
  meterToggle.addEventListener('click', function() {
    collapsed = !collapsed;
    localStorage.setItem('pi9696_meterCollapsed', collapsed ? '1' : '0');
    applyCollapse();
  });
}

// Update meter badge (stereo/dual-mono indicator)
var meterBadge = document.getElementById('meterBadge');

var chMeters = document.getElementById('chMeters');
var chCount = -1;
function ensureChannels(n) {
  if (n === chCount) return;
  chMeters.innerHTML = '';
  for (var i = 1; i <= n; i++) {
    var el = document.createElement('div');
    el.className = 'ch-meter';
    el.innerHTML = '<div class="vu-track"><div class="vu-peak" data-i="' + i + '"></div><div class="vu-fill" data-i="' + i + '"></div></div><div class="ch-label">' + i + '</div>';
    chMeters.appendChild(el);
  }
  chCount = n;
}

function applyMeter(m) {
  FLOOR = m.floorDB;
  rebuildDbScale(m.floorDB);

  // OLED mirror: reload only when the panel framebuffer actually changed.
  if (m.displaySeq !== oledSeq) {
    oledSeq = m.displaySeq;
    document.getElementById('oled').src = '/api/display.png?t=' + Date.now();
  }

  var paused = !!m.paused;
  // Reels and the tape-path pulse stop moving while paused (frozen transport)
  // but the head display still shows the frozen elapsed time rather than
  // going blank.
  var moving = m.recording || m.playing;
  var showing = m.recording || m.playing || paused;
  document.querySelectorAll('.reel-g').forEach(function(el) { el.classList.toggle('spinning', moving); });
  document.getElementById('tapePath').classList.toggle('active', moving);
  setSeg7(showing ? m.elapsed : null);

  // Deck-level recording state: the head gap line and lamps go red while a
  // take is running, the sys lamp glows green whenever the unit is moving.
  var deck = document.querySelector('.r2r');
  if (deck) { deck.classList.toggle('rec', !!m.recording); deck.classList.toggle('run', moving); }
  var sysLamp = document.getElementById('sysLamp');
  if (sysLamp) {
    sysLamp.classList.toggle('on', moving);
    sysLamp.classList.toggle('rec', !!m.recording);
  }
  var linkLamp = document.getElementById('linkLamp');
  if (linkLamp) linkLamp.classList.toggle('on', !!m.infernoUp);

  // Rebuild the transport row only when the play/pause state actually flips,
  // so the play triangle toggles to a pause glyph exactly when the state does.
  var next = { playing: !!m.playing, paused: paused };
  if (next.playing !== transportState.playing || next.paused !== transportState.paused) {
    transportState = next;
    renderTransportRow();
  }

  var channels = Array.isArray(m.channels) ? m.channels : [];
  ensureChannels(channels.length);
  channels.forEach(function(c, idx) {
    var i = idx + 1;
    var fill = chMeters.querySelector('.vu-fill[data-i="' + i + '"]');
    var peak = chMeters.querySelector('.vu-peak[data-i="' + i + '"]');
    if (fill) fill.style.height = vuPct(c.rmsDB) + '%';
    if (peak) peak.style.bottom = vuPct(c.peakDB) + '%';
  });

  // Meter footer badge: stereo/dual-mono indicator
  if (meterBadge) {
    meterBadge.textContent = channels.length === 2 ? 'STEREO' : (channels.length === 1 ? 'MONO' : channels.length + 'CH');
  }
}

// WebSocket push instead of polling /api/meter: the server streams a
// snapshot every 100ms (see handleWSMeter) over one persistent connection
// rather than the dashboard opening a new HTTP request per tick. If the
// socket drops it reconnects after a second; there is no polling fallback.
function connectMeterSocket() {
  var proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  var ws = new WebSocket(proto + '//' + location.host + '/ws/meter');
  ws.onmessage = function(ev) { applyMeter(JSON.parse(ev.data)); };
  // A 401-capable world (see requireAuth): an expired session now fails the
  // handshake instead of spinning reconnects forever - check once and offer
  // the login page rather than a dead red lamp.
  ws.onclose = function() {
    fetch('/api/status').then(function(r) {
      if (r.status === 401) { location.href = '/login'; return; }
      setTimeout(connectMeterSocket, 1000);
    }).catch(function() { setTimeout(connectMeterSocket, 1000); });
  };
  ws.onerror = function() { ws.close(); };
}
// System graphs: uPlot history driven by the hx-ws telemetry socket (see
// #teleSock), not by polling - the server pushes a status swap plus a
// history JSON swap every 2s, and applyTeleHist redraws from the swap.
// Charts appear once 2+ samples exist. Missing uPlot file degrades to the
// collecting placeholder.
var teleCPU = null, teleRAM = null, teleTemp = null, teleDisk = null;
// teleCSS reads a bridge token's resolved value so charts follow the active
// theme (none-case: the :root literal; themed: the theme's value through the
// bridge). Falls back to the literal when tokens are unavailable.
function teleCSS(name, fallback) {
  try {
    var v = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
    return v || fallback;
  } catch (e) { return fallback; }
}
var telePalette = null; // built lazily at chart init, after styles resolve
function telePaletteInit() {
  if (!telePalette) telePalette = [
    teleCSS('--glow', '#00d9ff'), teleCSS('--idle', '#2bffb0'),
    teleCSS('--orange', '#ff8c1a'), teleCSS('--rec', '#ff3355'),
    teleCSS('--dim', '#5b8aa8'), teleCSS('--text', '#cfeeff')
  ];
  return telePalette;
}
// uPlot's built-in time axis is 12h + am/pm; the unit standard is 24h.
function teleTimeValues(self, ticks) {
  function p(n) { return (n < 10 ? '0' : '') + n; }
  return ticks.map(function(t) {
    var d = new Date(t * 1000);
    return p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds());
  });
}
function teleOpts(extraSeries, ymin, ymax, h) {
  var dim = teleCSS('--dim', '#5b8aa8');
  var font = '9px ' + teleCSS('--ftl-font', 'Consolas,monospace');
  var o = {
    width: 300, height: h || 90,
    series: [{}].concat(extraSeries),
    cursor: {show: false},
    legend: {show: true},
    axes: [
      {stroke: dim, font: font, grid: {stroke: 'rgba(0,217,255,0.12)', width: 1}, values: teleTimeValues},
      {stroke: dim, font: font, grid: {stroke: 'rgba(0,217,255,0.12)', width: 1}}
    ]
  };
  if (ymin !== null) o.scales = {y: {range: [ymin, ymax]}};
  return o;
}
function teleWidth(el) {
  var w = el.clientWidth || 300;
  return w > 0 ? w : 300;
}
function teleSize(chart, el, h) {
  if (chart) chart.setSize({width: teleWidth(el), height: h});
}
function initTeleCharts(ncores) {
  var cpuEl = document.getElementById('cpuChart');
  var ramEl = document.getElementById('ramChart');
  var tempEl = document.getElementById('tempChart');
  var diskEl = document.getElementById('diskChart');
  if (!cpuEl || !ramEl || !tempEl || !diskEl) return false;
  cpuEl.innerHTML = ''; ramEl.innerHTML = ''; tempEl.innerHTML = ''; diskEl.innerHTML = '';
  var cpuSeries = [];
  var pal = telePaletteInit();
  for (var i = 0; i < ncores; i++) {
    cpuSeries.push({label: 'CPU' + i, stroke: pal[i % pal.length], width: 1.5});
  }
  var dummy = [[0, 1]];
  for (var i = 0; i < ncores; i++) dummy.push([0, 0]);
  teleCPU = new uPlot(teleOpts(cpuSeries, 0, 100, 90), dummy, cpuEl);
  teleRAM = new uPlot(teleOpts([
    {label: 'App MB', stroke: pal[0], width: 1.5, fill: 'rgba(0,217,255,0.10)'},
    {label: 'Sys MB', stroke: pal[2], width: 1.5}
  ], null, null, 90), [[0, 1], [0, 0], [0, 0]], ramEl);
  teleTemp = new uPlot(teleOpts([{label: 'Temp C', stroke: pal[2], width: 1.5}], null, null, 56), [[0, 1], [0, 0]], tempEl);
  teleDisk = new uPlot(teleOpts([{label: 'Free GB', stroke: pal[1], width: 1.5}], 0, null, 56), [[0, 1], [0, 0]], diskEl);
  teleSize(teleCPU, cpuEl, 90); teleSize(teleRAM, ramEl, 90);
  teleSize(teleTemp, tempEl, 56); teleSize(teleDisk, diskEl, 56);
  // Armed once: every re-init (core-count change, theme switch) would
  // otherwise stack another resize listener and run setSize N times.
  if (!window.teleResizeArmed) {
    window.teleResizeArmed = true;
    window.addEventListener('resize', function() {
      teleSize(teleCPU, document.getElementById('cpuChart'), 90);
      teleSize(teleRAM, document.getElementById('ramChart'), 90);
      teleSize(teleTemp, document.getElementById('tempChart'), 56);
      teleSize(teleDisk, document.getElementById('diskChart'), 56);
    });
  }
  return true;
}
// Hidden tabs skip redraws (item: don't burn cycles on an unseen panel);
// the latest payload is applied on return.
var teleHidden = document.hidden, telePending = null;
document.addEventListener('visibilitychange', function() {
  teleHidden = document.hidden;
  if (!teleHidden && telePending) { var h = telePending; telePending = null; applyTeleHist(h); }
});
function applyTeleHist(h) {
  if (!window.uPlot || !h || !h.t || h.t.length < 2) return;
  var ncores = (h.cores && h.cores.length > 0 && h.cores[0]) ? h.cores[0].length : 0;
  // Core count can only change across reboots; rebuild charts if it did.
  if (!teleCPU && !initTeleCharts(ncores)) return;
  if (teleCPU && teleCPU.series.length - 1 !== ncores) {
    teleCPU = null; teleRAM = null; teleTemp = null; teleDisk = null;
    if (!initTeleCharts(ncores)) return;
  }
  var cols = [h.t];
  for (var i = 0; i < ncores; i++) {
    cols.push(h.cores.map(function(row) { return (row && i < row.length) ? row[i] : null; }));
  }
  teleCPU.setData(cols);
  teleRAM.setData([h.t, h.ramApp, h.ramSys]);
  if (h.temp) teleTemp.setData([h.t, h.temp]);
  if (h.disk) teleDisk.setData([h.t, h.disk]);
}
document.body.addEventListener('htmx:after:swap', function(e) {
  // WS-driven swaps carry no detail.target (htmx 4 shape is {ctx,
  // cancelled}) - the swapped selector lives on detail.ctx.target as a
  // string like "#teleHist", while plain swaps still use detail.target.
  var d = e.detail || {}, t = d.target || (d.ctx && d.ctx.target);
  var id = t && (t.id || t);
  if (id === 'teleHist' || id === '#teleHist') {
    var raw = document.getElementById('teleHist').textContent;
    if (teleHidden) { telePending = raw; return; }
    try { applyTeleHist(JSON.parse(raw)); } catch (err) {}
  }
});
// Conn lamp: blue while the telemetry socket pushes, error red while the
// server is unreachable. hx-ws fires on the socket element; listen there
// and on document in case the bundle retargets.
function setConnLamp(on) {
  var lamp = document.getElementById('connLamp');
  if (!lamp) return;
  lamp.classList.toggle('on', !!on);
  lamp.classList.toggle('off', !on);
  lamp.title = on ? 'Server connected' : 'Server disconnected';
}
function watchConnLamp(el) {
  if (!el || !el.addEventListener) return;
  el.addEventListener('htmx:ws:after:connection', function() { setConnLamp(true); });
  el.addEventListener('htmx:ws:close', function() { setConnLamp(false); });
  el.addEventListener('htmx:ws:error', function() { setConnLamp(false); });
}
watchConnLamp(document);
watchConnLamp(document.getElementById('teleSock'));
// OLED mirror reloads on framebuffer generation change (see displaySeq),
// not on a blind interval.
var oledSeq = -1;
connectMeterSocket();
</script>
</body></html>`))

func handleDashboard(w http.ResponseWriter, r *http.Request) {
	mutex.Lock()
	name := deviceName
	wifiEn := wifiEnabled
	wifiS := wifiSSID
	wifiP := wifiPassword
	transportIcon := transportMode != "text"
	mutex.Unlock()

	activeTheme := currentTheme()
	activeThemeCSS := ""
	if activeTheme != themeNone {
		activeThemeCSS = "/static/themes/" + activeTheme + ".css"
	}
	// ?preview=<slug> renders a theme for this browser only, without
	// persisting it: try-before-apply on shared hardware, where selecting
	// rewrites the unit's look for every browser. Unknown slugs fall back
	// to the persisted theme, never to an error page.
	if pv := r.URL.Query().Get("preview"); isKnownTheme(pv) {
		activeTheme = pv
		activeThemeCSS = "/static/themes/" + pv + ".css"
	}

	var vuBuf, holdBuf, srBuf, chBuf, tagBuf, prefixBuf, transportBuf, logLevelBuf, brightnessBuf, autoDimBuf, monitorBuf, demoBuf, qrBuf, themeBuf bytes.Buffer
	selectFragmentTmpl.Execute(&vuBuf, vuRangeSelect())
	selectFragmentTmpl.Execute(&holdBuf, peakHoldSelect())
	selectFragmentTmpl.Execute(&srBuf, sampleRateSelect())
	channelCountFragmentTmpl.Execute(&chBuf, currentChannelCountView())
	selectFragmentTmpl.Execute(&tagBuf, tagSelect())
	filePrefixFragmentTmpl.Execute(&prefixBuf, filePrefixView())
	transportFragmentTmpl.Execute(&transportBuf, transportOptionsView())
	selectFragmentTmpl.Execute(&logLevelBuf, logLevelSelect())
	selectFragmentTmpl.Execute(&themeBuf, themeSelect())
	brightnessFragmentTmpl.Execute(&brightnessBuf, brightnessViewData())
	autoDimFragmentTmpl.Execute(&autoDimBuf, autoDimViewData())
	demoFragmentTmpl.Execute(&demoBuf, demoViewData())
	monitorFragmentTmpl.Execute(&monitorBuf, monitorViewData())

	// Generate WiFi QR code as base64 PNG for the settings modal
	var qrBase64 string
	if wifiEn && wifiS != "" {
		if code, err := qrcode.New(wifiQRContent(), qrcode.Medium); err == nil {
			png, _ := code.PNG(256)
			qrBase64 = base64.StdEncoding.EncodeToString(png)
		}
	}
	wifiQRFragmentTmpl.Execute(&qrBuf, wifiQRView{wifiEn, wifiS, wifiP, qrBase64})

	dashboardTmpl.Execute(w, dashboardData{
		DeviceName:           name,
		Logo:                 template.HTML(pi9696LogoSVG),
		Theme:                activeTheme,
		ThemeCSS:             activeThemeCSS,
		VURangeFragment:      template.HTML(vuBuf.String()),
		PeakHoldFragment:     template.HTML(holdBuf.String()),
		SampleRateFragment:   template.HTML(srBuf.String()),
		ChannelCountFragment: template.HTML(chBuf.String()),
		TagFragment:          template.HTML(tagBuf.String()),
		PrefixFragment:       template.HTML(prefixBuf.String()),
		TransportFragment:    template.HTML(transportBuf.String()),
		LogLevelFragment:     template.HTML(logLevelBuf.String()),
		ThemeFragment:        template.HTML(themeBuf.String()),
		BrightnessFragment:   template.HTML(brightnessBuf.String()),
		AutoDimFragment:      template.HTML(autoDimBuf.String()),
		MonitorFragment:      template.HTML(monitorBuf.String()),
		DemoFragment:         template.HTML(demoBuf.String()),
		WifiEnabled:          wifiEn,
		WifiSSID:             wifiS,
		WifiPassword:         wifiP,
		WifiQRFragment:       template.HTML(qrBuf.String()),
		TransportIcon:        transportIcon,
	})
}

// isValidDeviceName restricts the web-settable unit name to a small safe
// charset. It ends up in three places that each have their own risk if left
// unvalidated: an INFERNO_NAME env var passed to a subprocess (env values
// aren't shell-parsed, so injection isn't possible there, but a name full of
// control characters would still be a bad idea to hand to another process),
// an html/template-escaped page (safe either way, template does the
// escaping), and log lines (unescaped - control chars could forge log
// entries).
func isValidDeviceName(name string) bool {
	if name == "" || len(name) > 32 {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == ' ' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func handleAPIDeviceName(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if isValidDeviceName(name) {
		mutex.Lock()
		deviceName = name
		persistConfig()
		mutex.Unlock()
		logInfof("Device name changed to %q via remote", name)
	}

	mutex.Lock()
	current := deviceName
	mutex.Unlock()
	// Wrap in the outer #devicename div: the form targets it with
	// outerHTML, and a bare <form> response would destroy the target so
	// the name is editable exactly once per page load.
	fmt.Fprintf(w, `<div id="devicename" class="setting-cell"><div class="setting-row"><form hx-post="/api/device-name" hx-target="#devicename" hx-swap="outerHTML">
<input name="name" value="%s" maxlength="32" pattern="[A-Za-z0-9 _-]+" title="Letters, numbers, spaces, - and _ only">
<button type="submit">Save</button>
</form></div></div>`, template.HTMLEscapeString(current))
}

// handleDisplayPNG mirrors the OLED exactly - encoded from the same packed
// framebuffer real hardware receives (see TTFDisplay.EncodePNG), not a
// separate HTML/CSS reimplementation of the layout that could drift from
// what render() actually draws.
//
// Encodes into an in-memory buffer under the lock, then writes to the
// response after releasing it. png.Encode writes straight through
// http.ResponseWriter to the socket - encoding directly into w while
// holding mutex would block render(), every encoder/button callback
// (physical and remote), and infernoWorker's recording guard
// for as long as a slow or stalled client's network write took, the same
// class of bug already fixed once for stopRecording().
func handleDisplayPNG(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	mutex.Lock()
	err := hwManager.EncodePNG(&buf)
	mutex.Unlock()
	if err != nil {
		logErrorf("Failed to encode display PNG: %v", err)
		http.Error(w, "failed to encode display", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	buf.WriteTo(w)
}

// The input handlers below call the exact same functions
// setupHardwareCallbacks wires to physical encoder/button interrupts - a
// remote click is indistinguishable, from the state machine's point of
// view, from a real one. That's what keeps every existing guard (recording
// blocks playback and vice versa, confirmation dialogs before delete/
// format/shutdown/restart) intact without reimplementing them here.

func handleInputEncoderRotate(direction int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		onEncoderRotate(direction)
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleInputEncoderClick(w http.ResponseWriter, r *http.Request) {
	onEncoderClick()
	w.WriteHeader(http.StatusNoContent)
}

func handleInputEncoderHold(w http.ResponseWriter, r *http.Request) {
	onEncoderHold()
	w.WriteHeader(http.StatusNoContent)
}

func handleInputButton(bt hardware.ButtonType) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		onButtonPress(bt)
		w.WriteHeader(http.StatusNoContent)
	}
}

var configTmpl = template.Must(template.New("config").Parse(`
<table>
<tr><td>Sample Rate</td><td>{{.SampleRate}}kHz</td></tr>
<tr><td>Channels</td><td>{{.Channels}}</td></tr>
<tr><td>Format</td><td>{{.Format}}</td></tr>
<tr><td>Tag</td><td>{{.Tag}}</td></tr>
<tr><td>Inferno</td><td>{{.Inferno}}</td></tr>
<tr><td>Network</td><td>{{.Network}}</td></tr>
</table>
`))

type configView struct {
	SampleRate int
	Channels   int
	Format     string
	Tag        string
	Inferno    string
	Network    string
}

// handleAPIConfig is read-only by design: mutating settings goes through
// the encoder/button controls above (the same state machine the OLED menu
// system already guards), not a second, parallel settings form here.
func handleAPIConfig(w http.ResponseWriter, r *http.Request) {
	html, err := renderConfigHTML()
	if err != nil {
		http.Error(w, "config unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
}

// renderConfigHTML renders the dashboard Status panel; shared by the
// one-shot GET (initial paint) and the telemetry-socket push.
func renderConfigHTML() (string, error) {
	mutex.Lock()
	v := configView{
		SampleRate: sampleRates[sampleRateIdx] / 1000,
		Channels:   channelCount,
		Format:     "WAV",
		Tag:        tagStatusText(),
		Inferno:    getInfernoStatusText(),
	}
	mutex.Unlock()

	_, v.Network = hwManager.Network.GetNetworkStatus()

	var buf bytes.Buffer
	if err := configTmpl.Execute(&buf, v); err != nil {
		return "", err
	}
	return buf.String(), nil
}

var statusTmpl = template.Must(template.New("status").Parse(`
{{if .Recording}}<p class="rec">&#9679; RECORDING - {{.Elapsed}}</p>
<p>{{.Meter}}</p>
<button hx-post="/api/record/stop" hx-target="#status" hx-swap="innerHTML" data-record-stop>Stop</button>
 {{else if .Playing}}<p>&#9654; Playing back - {{.Elapsed}}</p>
 {{else if .Paused}}<p>&#10074;&#10074; Paused - {{.Elapsed}}</p>
 {{else if .MonOutput}}<p class="idle">&#9654; Monitoring output - playing {{.Format}} {{.SampleRate}}kHz {{.Channels}}ch {{.Elapsed}}</p>
 {{else if .Monitoring}}<p class="idle">&#128266; Monitoring input - {{.Format}} {{.SampleRate}}kHz {{.Channels}}ch</p>
{{else}}<p class="idle">Idle - {{.Format}} {{.SampleRate}}kHz {{.Channels}}ch</p>
{{if not .InfernoUp}}<p>(Inferno not running &mdash; build the Inferno binary and restart)</p>{{end}}
{{if .DemoMode}}<p>(Demo mode &mdash; simulated audio)</p>{{end}}
{{end}}
<div class="sys-readout">
<p>Uptime {{.Uptime}} &middot; v{{.AppVersion}}</p>
<p>Temp {{if ge .CPUTemp 0.0}}{{printf "%.0f" .CPUTemp}}&deg;{{else}}&mdash;{{end}}</p>
<p>Disk /rec: {{printf "%.0f" .DiskTotal}}GB / {{printf "%.0f" .DiskFree}}GB free &middot; record: {{.RecordTime}}</p>
</div>`))

type statusView struct {
	Recording   bool
	Playing     bool
	Paused      bool
	Monitoring  bool
	MonOutput   bool
	Elapsed     string
	Meter       string
	Format      string
	SampleRate  int
	Channels    int
	InfernoUp   bool
	DemoMode    bool
	Uptime      string
	AppVersion  string
	CPUPerCore  []float64
	RAMApp      float64
	RAMInferno  float64
	RAMSysUsed  float64
	RAMSysTotal float64
	CPUTemp     float64
	DiskTotal   float64
	DiskFree    float64
	RecordTime  string
}

func currentStatusView() statusView {
	mutex.Lock()
	v := statusView{
		Recording:  isRecording,
		Playing:    currentState == StatePlaying,
		Paused:     currentState == StatePaused,
		Monitoring: monitoring,
		MonOutput:  monitoringOutput,
		Format:     "WAV",
		SampleRate: sampleRates[sampleRateIdx] / 1000,
		Channels:   channelCount,
		InfernoUp:  infernoUp(),
		DemoMode:   demoMode,
	}
	switch {
	case v.Recording:
		v.Elapsed = formatDuration(time.Since(recordStart))
		v.Meter = formatMeter()
	case v.Playing:
		v.Elapsed = formatDuration(time.Since(playbackStart))
	case v.Paused:
		v.Elapsed = formatDuration(playbackPausedElapsed)
	}
	mutex.Unlock()

	t := snapshotTelemetry()
	v.Uptime, v.AppVersion, v.CPUPerCore = t.Uptime, t.AppVersion, t.CPUPerCore
	v.RAMApp, v.RAMInferno, v.RAMSysUsed, v.RAMSysTotal = t.RAMApp, t.RAMInferno, t.RAMSysUsed, t.RAMSysTotal
	v.CPUTemp, v.DiskTotal, v.DiskFree, v.RecordTime = t.CPUTemp, t.DiskTotal, t.DiskFree, t.RecordTime
	return v
}

func renderStatusHTML() (string, error) {
	var buf bytes.Buffer
	if err := statusTmpl.Execute(&buf, currentStatusView()); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	statusTmpl.Execute(w, currentStatusView())
}

// telemetryHistView is the dashboard graphs' feed: parallel arrays, newest
// last, sampled every teleHistStep (see appendTelemetryHist).
type telemetryHistView struct {
	T      []int64     `json:"t"`
	CPU    []float64   `json:"cpu"`
	Cores  [][]float64 `json:"cores"`
	RAMApp []float64   `json:"ramApp"`
	RAMSys []float64   `json:"ramSys"`
	Temp   []float64   `json:"temp"`
	Disk   []float64   `json:"disk"`
}

func currentTelemetryHist() telemetryHistView {
	mutex.Lock()
	defer mutex.Unlock()
	return telemetryHistView{
		T:      append([]int64(nil), teleHistT...),
		CPU:    append([]float64(nil), teleHistCPU...),
		Cores:  append([][]float64(nil), teleHistCores...),
		RAMApp: append([]float64(nil), teleHistRAMApp...),
		RAMSys: append([]float64(nil), teleHistRAMSys...),
		Temp:   append([]float64(nil), teleHistTemp...),
		Disk:   append([]float64(nil), teleHistDisk...),
	}
}

func handleAPITelemetry(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(currentTelemetryHist())
}

// teleWSHub tracks dashboard telemetry sockets; guarded by teleWSMu (never
// the app mutex - broadcast renders status HTML which takes it).
var teleWSHub = map[*websocket.Conn]bool{}
var teleWSMu sync.Mutex

// teleWSMessage is the hx-ws wire shape: target selects the swap element,
// content is HTML for #status or the history JSON for #teleHist.
type teleWSMessage struct {
	Target  string `json:"target"`
	Content string `json:"content"`
}

func teleWSSend(ws *websocket.Conn, target, content string) bool {
	if err := ws.SetWriteDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
		return false
	}
	return websocket.JSON.Send(ws, teleWSMessage{Target: target, Content: content}) == nil
}

// buildTelemetryWSMessages renders one broadcast round: status HTML plus the
// history JSON payload. Factored for tests (no socket needed).
func buildTelemetryWSMessages() (status, hist string, err error) {
	status, err = renderStatusHTML()
	if err != nil {
		return "", "", err
	}
	raw, err := json.Marshal(currentTelemetryHist())
	if err != nil {
		return "", "", err
	}
	return status, string(raw), nil
}

func broadcastTelemetry() {
	status, hist, err := buildTelemetryWSMessages()
	if err != nil {
		return
	}
	teleWSMu.Lock()
	defer teleWSMu.Unlock()
	config, recs, configChanged, recsChanged := panelWSMessages()
	for ws := range teleWSHub {
		if !teleWSSend(ws, "#status", status) || !teleWSSend(ws, "#teleHist", hist) {
			ws.Close()
			delete(teleWSHub, ws)
			continue
		}
		if configChanged && !teleWSSend(ws, "#config", config) {
			ws.Close()
			delete(teleWSHub, ws)
			continue
		}
		if recsChanged && !teleWSSend(ws, "#recordings", recs) {
			ws.Close()
			delete(teleWSHub, ws)
		}
	}
}

// lastPanelConfig/lastPanelRecs are the last pushed panel bodies; the
// Status and Recordings panels only go out over the socket when they
// actually changed, so an idle dashboard costs no re-renders. Guarded by
// teleWSMu - panelWSMessages renders (taking the app mutex inside), so
// never call it with the app mutex already held.
var lastPanelConfig, lastPanelRecs string

// panelWSMessages renders both panels, stores the new bodies, and reports
// whether each changed since the last push. Call with teleWSMu held.
func panelWSMessages() (config, recs string, configChanged, recsChanged bool) {
	if c, err := renderConfigHTML(); err == nil {
		config, configChanged = c, c != lastPanelConfig
		lastPanelConfig = c
	}
	if r, err := renderRecordingsHTML(); err == nil {
		recs, recsChanged = r, r != lastPanelRecs
		lastPanelRecs = r
	}
	return config, recs, configChanged, recsChanged
}

func handleWSTelemetry(ws *websocket.Conn) {
	teleWSMu.Lock()
	teleWSHub[ws] = true
	teleWSMu.Unlock()
	defer func() {
		teleWSMu.Lock()
		delete(teleWSHub, ws)
		teleWSMu.Unlock()
		ws.Close()
	}()
	// Instant first paint so a fresh dashboard never waits a full tick:
	// status + history plus both panels (a new socket hasn't seen anything,
	// so send unconditionally - this also seeds the change cache).
	if status, hist, err := buildTelemetryWSMessages(); err == nil {
		if !teleWSSend(ws, "#status", status) || !teleWSSend(ws, "#teleHist", hist) {
			return
		}
	}
	teleWSMu.Lock()
	config, recs, _, _ := panelWSMessages()
	teleWSMu.Unlock()
	if config != "" && !teleWSSend(ws, "#config", config) {
		return
	}
	if recs != "" && !teleWSSend(ws, "#recordings", recs) {
		return
	}
	// Read to EOF purely to notice the client going away; frames are ignored.
	var discard any
	for {
		if websocket.JSON.Receive(ws, &discard) != nil {
			return
		}
	}
}

func telemetryWSLoop() {
	ticker := time.NewTicker(teleHistStep)
	defer ticker.Stop()
	for range ticker.C {
		broadcastTelemetry()
	}
}

// handleAPIMeter is deliberately separate from handleAPIStatus: the VU
// hologram needs numeric dB values on a fast (~150ms) push to look live,
// while the 2s telemetry-socket push is fine for everything else on the
// dashboard. meterPeakDB/meterRMSDB fall back to meterSilence whenever nothing is
// recording (see main.go's stopRecording/meterReader), so the hologram
// correctly goes quiet rather than showing a stale level.
type channelLevel struct {
	PeakDB float64 `json:"peakDB"`
	RMSDB  float64 `json:"rmsDB"`
}

type meterResponse struct {
	PeakDB     float64        `json:"peakDB"`
	RMSDB      float64        `json:"rmsDB"`
	Recording  bool           `json:"recording"`
	Playing    bool           `json:"playing"`
	Paused     bool           `json:"paused"`
	Monitoring bool           `json:"monitoring"`
	MonOutput  bool           `json:"monOutput"`
	InfernoUp  bool           `json:"infernoUp"`
	Elapsed    string         `json:"elapsed"`
	Channels   []channelLevel `json:"channels"`
	FloorDB    float64        `json:"floorDB"`
	// DisplaySeq is the OLED framebuffer generation (see render) so the
	// dashboard mirror reloads on change instead of polling blindly.
	DisplaySeq uint64 `json:"displaySeq"`
}

// jsonSafeDB coerces a dB level to a JSON-encodable value. encoding/json will
// not marshal +/-Inf or NaN (the WebSocket meter and /api/meter would then
// return an empty 200 instead of a payload), so clamp any non-finite reading
// to the silence sentinel before it reaches the wire. meterReader in main.go
// already sanitizes at ingestion, but this guarantees the JSON sink can never
// fail even if a non-finite value comes from some other path (meter math,
// decay ballistics, etc.).
func jsonSafeDB(v float64) float64 {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return meterSilence
	}
	return v
}

// currentMeterResponse builds a meterResponse snapshot under the app mutex -
// shared by the plain-HTTP /api/meter handler (kept for anything that wants
// a one-shot read) and the /ws/meter push loop below, so there's exactly
// one place that assembles this payload.
func currentMeterResponse() meterResponse {
	mutex.Lock()
	defer mutex.Unlock()

	resp := meterResponse{
		PeakDB:     jsonSafeDB(meterPeakDB),
		RMSDB:      jsonSafeDB(meterRMSDB),
		Recording:  isRecording,
		Playing:    currentState == StatePlaying,
		Paused:     currentState == StatePaused,
		Monitoring: monitoring,
		MonOutput:  monitoringOutput,
		InfernoUp:  infernoUp(),
		FloorDB:    vuRangeOptions[vuRangeIdx],
		DisplaySeq: displaySeq,
	}
	// Always size the meter bank to the configured channel count. During
	// recording, the peak/RMS arrays are exactly channelCount (startRecording
	// sizes them to it), and at idle/monitoring they follow it too, so this
	// is normally a no-op - but while a channel change is still in flight
	// (Inferno restart is deferred if a take is running), sizing to
	// channelCount means the VU count tracks the setting immediately instead
	// of staying pinned to the old running server. The per-index bounds
	// checks below keep channels beyond the current arrays silent rather
	// than reading out of range.
	count := channelCount
	resp.Channels = make([]channelLevel, count)
	for i := range resp.Channels {
		if i < len(meterChannelPeakHeld) && i < len(meterChannelRMS) {
			resp.Channels[i] = channelLevel{PeakDB: jsonSafeDB(meterChannelPeakHeld[i]), RMSDB: jsonSafeDB(meterChannelRMS[i])}
		} else {
			resp.Channels[i] = channelLevel{PeakDB: meterSilence, RMSDB: meterSilence}
		}
	}
	switch {
	case resp.Recording:
		resp.Elapsed = formatDuration(time.Since(recordStart))
	case resp.Playing:
		resp.Elapsed = formatDuration(time.Since(playbackStart))
	case resp.Paused:
		resp.Elapsed = formatDuration(playbackPausedElapsed)
	}
	return resp
}

func handleAPIMeter(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(currentMeterResponse())
}

// handleWSMeter pushes a meter snapshot every 100ms over a WebSocket rather
// than making the dashboard poll /api/meter - fewer requests, and headroom
// to tighten the interval later without adding HTTP request overhead per
// tick. Falls back to nothing fancier than closing on the first send error
// (client navigated away, network dropped) - the dashboard's reconnect
// logic (see the WS setup in dashboardTmpl) is what handles that, not this
// loop retrying.
// wsWriteTimeout bounds how long a single meter-frame write may block. It's
// a var so tests can tighten it.
var wsWriteTimeout = 5 * time.Second

// wsMeterSend pushes one meter snapshot, arming a fresh write deadline first.
// It returns false when the connection is done and the push loop should exit.
// The deadline matters: a client that vanishes without closing (power loss,
// silent network drop) never triggers a read error, and a deadline-less Send
// can block on it indefinitely - leaking this goroutine per stale connection
// and keeping the connection "active" so remoteControlLoop's Shutdown can't
// drain it (a clean close by the client errors the write immediately; the
// deadline covers the silent-vanish case at the cost of one timed-out write).
func wsMeterSend(ws *websocket.Conn) bool {
	if err := ws.SetWriteDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
		return false
	}
	return websocket.JSON.Send(ws, currentMeterResponse()) == nil
}

func handleWSMeter(ws *websocket.Conn) {
	defer ws.Close()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		if !wsMeterSend(ws) {
			return
		}
	}
}

func handleAPIRecordStart(w http.ResponseWriter, r *http.Request) {
	mutex.Lock()
	// Use the same gate as the physical Record button so the WebUI can't
	// start a take that the hardware would refuse (low disk, or from a state
	// other than idle) - see startRecordingGuarded.
	startRecordingGuarded()
	mutex.Unlock()
	handleAPIStatus(w, r)
}

func handleAPIRecordStop(w http.ResponseWriter, r *http.Request) {
	mutex.Lock()
	if isRecording {
		stopRecording()
	}
	mutex.Unlock()
	handleAPIStatus(w, r)
}

func handleAPIMonitorStart(w http.ResponseWriter, r *http.Request) {
	mutex.Lock()
	autoMonitor = true
	startMonitor()
	mutex.Unlock()
	handleAPIStatus(w, r)
}

func handleAPIMonitorStop(w http.ResponseWriter, r *http.Request) {
	mutex.Lock()
	if monitoring {
		autoMonitor = false
		stopMonitor()
	}
	mutex.Unlock()
	handleAPIStatus(w, r)
}

var recordingsTmpl = template.Must(template.New("recordings").Parse(`
<div class="recordings-wrap">
<table class="recordings-table">
<thead><tr><th>File</th><th>Tracks</th><th>Format</th><th>Start</th><th>End</th><th>Duration</th><th></th></tr></thead>
<tbody>
{{if not .}}<tr><td class="empty" colspan="7">None yet.</td></tr>{{else}}
{{range .}}<tr>
<td>{{.Name}}</td>
<td>{{.Channels}}</td>
<td>{{.Format}} {{.SampleRate}}kHz</td>
<td>{{.StartStr}}</td>
<td>{{.EndStr}}</td>
<td>{{.DurationStr}}</td>
<td><a class="dl-link" href="/download/{{.RelPath}}">download</a></td>
</tr>{{end}}
{{end}}
</tbody>
</table>
</div>
`))

type recordingRow struct {
	Name        string
	RelPath     string
	Channels    int
	Format      string
	SampleRate  int
	StartStr    string
	EndStr      string
	DurationStr string
}

// recFilenameRe matches the app's own recording filenames, e.g.
// "recording_20240131_143022_ch2_48kHz.wav" or "Live_20240131_143022_ch2_48kHz.wav"
// (prefix now precedes the fixed timestamp; see startRecording in main.go).
// The prefix is validated to contain no underscore, so everything up to the
// first _ before the timestamp is the prefix and the rest is unambiguous.
// Everything but duration/end time is recoverable straight from the name;
// duration needs the file's actual content (see recordingDuration).
var recFilenameRe = regexp.MustCompile(`^([A-Za-z0-9 -]+)_(\d{8})_(\d{6})_ch(\d+)_(\d+)kHz(-\d+)?\.(\w+)$`)

func recordingDuration(path string, channels, sampleRate int) time.Duration {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}

	const bytesPerSample = 3 // pcm_s24le
	const headerBytes = 44
	dataBytes := info.Size() - headerBytes
	if dataBytes <= 0 || channels <= 0 || sampleRate <= 0 {
		return 0
	}
	return time.Duration(dataBytes) * time.Second / time.Duration(int64(channels)*int64(bytesPerSample)*int64(sampleRate))
}

func buildRecordingRow(path string) recordingRow {
	name := filepath.Base(path)
	row := recordingRow{Name: name}
	// RelPath is the path relative to RecordPath (e.g. "2026-08-30/Live_...wav"),
	// the unique key for download - recordingFiles() globs both the top level
	// and per-day subfolders, so basename alone could collide across days.
	if rel, err := filepath.Rel(RecordPath, path); err == nil {
		row.RelPath = rel
	} else {
		row.RelPath = name
	}

	m := recFilenameRe.FindStringSubmatch(name)
	if m == nil {
		return row
	}
	start, err := time.ParseInLocation("20060102 150405", m[2]+" "+m[3], time.Local)
	if err != nil {
		return row
	}
	channels, _ := strconv.Atoi(m[4])
	sampleRate, _ := strconv.Atoi(m[5])
	format := strings.ToUpper(m[7])

	row.Channels = channels
	row.SampleRate = sampleRate
	row.Format = format
	row.StartStr = start.Format("2006-01-02 15:04:05")

	// sampleRate here is the kHz value parsed from the filename (m[5] is the
	// "48" in 48kHz) and is stored for display; recordingDuration computes
	// bytes-per-second from the actual sample rate, so convert kHz -> Hz.
	dur := recordingDuration(path, channels, sampleRate*1000)
	if dur > 0 {
		row.DurationStr = formatDuration(dur)
		row.EndStr = start.Add(dur).Format("2006-01-02 15:04:05")
	} else {
		row.DurationStr = "-"
		row.EndStr = "-"
	}
	return row
}

func handleAPIRecordings(w http.ResponseWriter, r *http.Request) {
	html, err := renderRecordingsHTML()
	if err != nil {
		http.Error(w, "recordings unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
}

// renderRecordingsHTML renders the dashboard recordings list; shared by the
// one-shot GET (initial paint) and the telemetry-socket push.
func renderRecordingsHTML() (string, error) {
	var rows []recordingRow
	for _, f := range recordingFiles() {
		rows = append(rows, buildRecordingRow(f))
	}
	var buf bytes.Buffer
	if err := recordingsTmpl.Execute(&buf, rows); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// handleDownload only serves files that appear in the app's own current
// recordingFiles() listing - whitelisting against reality rather than just
// filepath.Base()-sanitizing the request, so a path like "../../etc/passwd"
// is rejected outright rather than relying on string-cleaning alone. It keys
// on the path relative to RecordPath (the recordingRow.RelPath the list uses
// for its download link), which is unique even though the basename may be
// shared across per-day subfolders.
func handleDownload(w http.ResponseWriter, r *http.Request) {
	rel := r.PathValue("filepath")
	if rel == "" || strings.Contains(rel, "..") {
		http.NotFound(w, r)
		return
	}
	for _, f := range recordingFiles() {
		if relPath, err := filepath.Rel(RecordPath, f); err == nil && relPath == rel {
			http.ServeFile(w, r, f)
			return
		}
	}
	http.NotFound(w, r)
}

// handleDownloadAll streams every finished recording as a single ZIP bundle,
// with a small manifest.txt describing each file. It never loads the files
// into memory - each one is opened and copied into the archive as it's
// encountered - so a very large set (multi-channel high-sample-rate takes can
// be many GB each) is written to the client streaming, not buffered. Zip entry
// names use the recording's path relative to RecordPath (the same unique key
// the list uses for per-file download), so per-day subfolders are preserved
// and same-named files across days don't collide.
func handleDownloadAll(w http.ResponseWriter, r *http.Request) {
	files := recordingFiles()
	if len(files) == 0 {
		// A bare 404 reads as "broken" - the common case is simply an empty
		// /rec (fresh unit, or a dev box). Say so, with a way back.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><head><title>Download ALL</title></head><body>`+
			`<p>No recordings to download yet.</p>`+
			`<p><a href="/">Back to dashboard</a></p></body></html>`)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="pi9696-recordings-%s.zip"`, time.Now().Format("20060102_150405")))

	if err := writeRecordingZip(w, RecordPath, files); err != nil {
		logErrorf("download-all: %v", err)
	}
}

// writeRecordingZip streams every file in files as a single ZIP archive with a
// manifest.txt describing each one. Each file is opened and copied into the
// archive as it's encountered, never buffered whole, so a very large set
// (multi-channel high-sample-rate takes can be many GB each) is written
// streaming. Zip entry names use each file's path relative to base (the
// recording root), so per-day subfolders are preserved and same-named files
// across days don't collide. base is the path prefix against which rel is
// computed; it's a parameter so the same streaming logic is testable against a
// temp directory without touching the real RecordPath.
func writeRecordingZip(dst io.Writer, base string, files []string) error {
	zw := zip.NewWriter(dst)
	defer zw.Close()

	var manifest strings.Builder
	fmt.Fprintf(&manifest, "PI9696 recording bundle\nGenerated: %s\nFiles: %d\n\n", time.Now().Format("2006-01-02 15:04:05"), len(files))
	fmt.Fprintf(&manifest, "%-60s %12s %8s %6s %8s %10s  %s\n", "Path", "Size(bytes)", "Channels", "Rate", "Format", "Duration", "Start")

	for _, f := range files {
		rel, err := filepath.Rel(base, f)
		if err != nil || strings.Contains(rel, "..") {
			continue
		}
		entry := filepath.ToSlash(rel)

		info, err := os.Stat(f)
		if err != nil {
			continue
		}

		hdr := &zip.FileHeader{Name: entry, Method: zip.Deflate}
		hdr.SetModTime(info.ModTime())
		wc, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		in, err := os.Open(f)
		if err != nil {
			continue
		}
		_, copyErr := io.Copy(wc, in)
		in.Close()
		if copyErr != nil {
			return copyErr
		}

		row := buildRecordingRow(f)
		fmt.Fprintf(&manifest, "%-60s %12d %8d %6d %8s %10s  %s\n",
			entry, info.Size(), row.Channels, row.SampleRate, row.Format, row.DurationStr, row.StartStr)
	}

	mw, err := zw.Create("manifest.txt")
	if err != nil {
		return err
	}
	if _, err := mw.Write([]byte(manifest.String())); err != nil {
		return err
	}
	return nil
}

//go:embed web/htmax.min.js web/uPlot.iife.min.js web/uPlot.min.css
var embeddedWeb embed.FS

// Theme bundles come from the ftl-themes submodule rather than web/: they are
// version-pinned with the repo instead of downloaded at install time, so the
// dashboard cannot end up serving a theme that disagrees with this binary.
// Each dist/<slug>.css is self-contained (reset + components + app shell +
// theme); the fonts sit alongside because a bundle references them as
// ../assets/fonts/... relative to its own served path.
//
//go:embed third_party/ftl-themes/dist/*.css third_party/ftl-themes/dist/themes.json third_party/ftl-themes/assets/fonts/*.woff2
var embeddedThemes embed.FS

// serveEmbeddedStatic serves a pinned vendored asset with immutable caching,
// from whichever embedded set holds it (web/ for the vendored JS/CSS,
// third_party/ for the theme bundles and their fonts).
func serveEmbeddedStatic(w http.ResponseWriter, r *http.Request, src embed.FS, path, contentType string) {
	data, err := src.ReadFile(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Write(data)
}

func newRemoteMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /manifest.json", handleManifest)
	mux.HandleFunc("GET /icon.svg", handleIcon)
	mux.HandleFunc("GET /login", handleLoginGet)
	mux.HandleFunc("POST /login", handleLoginPost)
	mux.HandleFunc("POST /logout", requireAuth(handleLogout))
	// State change lives on POST (logout-CSRF via top-level navigation);
	// plain GETs just land back on the dashboard.
	mux.HandleFunc("GET /logout", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}))
	mux.HandleFunc("GET /", requireAuth(handleDashboard))
	mux.HandleFunc("GET /api/status", requireAuth(handleAPIStatus))
	mux.HandleFunc("GET /api/telemetry", requireAuth(handleAPITelemetry))
	mux.HandleFunc("GET /api/meter", requireAuth(handleAPIMeter))
	mux.HandleFunc("GET /ws/meter", requireAuth(websocket.Handler(handleWSMeter).ServeHTTP))
	mux.HandleFunc("GET /ws/telemetry", requireAuth(websocket.Handler(handleWSTelemetry).ServeHTTP))
	mux.HandleFunc("GET /api/config", requireAuth(handleAPIConfig))
	mux.HandleFunc("POST /api/device-name", requireAuth(handleAPIDeviceName))
	mux.HandleFunc("POST /api/settings/vu-range", requireAuth(handleAPISettingsVURange))
	mux.HandleFunc("POST /api/settings/peak-hold", requireAuth(handleAPISettingsPeakHold))
	mux.HandleFunc("POST /api/settings/log-level", requireAuth(handleAPISettingsLogLevel))
	mux.HandleFunc("POST /api/settings/theme", requireAuth(handleAPISettingsTheme))
	mux.HandleFunc("POST /api/settings/brightness", requireAuth(handleAPISettingsBrightness))
	mux.HandleFunc("POST /api/settings/dim", requireAuth(handleAPISettingsAutoDim))
	mux.HandleFunc("POST /api/settings/demo", requireAuth(handleAPISettingsDemoMode))
	mux.HandleFunc("POST /api/settings/monitor", requireAuth(handleAPISettingsMonitor))
	mux.HandleFunc("POST /api/settings/sample-rate", requireAuth(handleAPISettingsSampleRate))
	mux.HandleFunc("POST /api/settings/channels", requireAuth(handleAPISettingsChannels))
	mux.HandleFunc("POST /api/settings/tag", requireAuth(handleAPISettingsTag))
	mux.HandleFunc("POST /api/settings/prefix", requireAuth(handleAPISettingsPrefix))
	mux.HandleFunc("POST /api/settings/transport-mode", requireAuth(handleAPISettingsTransportMode))
	mux.HandleFunc("POST /api/settings/wifi", requireAuth(handleAPISettingsWiFi))
	mux.HandleFunc("POST /api/config/export", requireAuth(handleAPIConfigExport))
	mux.HandleFunc("POST /api/config/import", requireAuth(handleAPIConfigImport))
	mux.HandleFunc("POST /api/record/start", requireAuth(handleAPIRecordStart))
	mux.HandleFunc("POST /api/record/stop", requireAuth(handleAPIRecordStop))
	mux.HandleFunc("POST /api/monitor/start", requireAuth(handleAPIMonitorStart))
	mux.HandleFunc("POST /api/monitor/stop", requireAuth(handleAPIMonitorStop))
	mux.HandleFunc("GET /api/recordings", requireAuth(handleAPIRecordings))
	mux.HandleFunc("GET /download-all", requireAuth(handleDownloadAll))
	mux.HandleFunc("GET /download/{filepath...}", requireAuth(handleDownload))

	mux.HandleFunc("GET /api/display.png", requireAuth(handleDisplayPNG))
	mux.HandleFunc("POST /api/input/encoder/left", requireAuth(handleInputEncoderRotate(-1)))
	mux.HandleFunc("POST /api/input/encoder/right", requireAuth(handleInputEncoderRotate(1)))
	mux.HandleFunc("POST /api/input/encoder/click", requireAuth(handleInputEncoderClick))
	mux.HandleFunc("POST /api/input/encoder/hold", requireAuth(handleInputEncoderHold))
	mux.HandleFunc("POST /api/input/button/record", requireAuth(handleInputButton(hardware.RecordButton)))
	mux.HandleFunc("POST /api/input/button/stop", requireAuth(handleInputButton(hardware.StopButton)))
	mux.HandleFunc("POST /api/input/button/play", requireAuth(handleInputButton(hardware.PlayButton)))

	// htmax.min.js (htmx 4.0 plus its bundled extensions) is vendored at
	// install time (pinned to 4.0.0) rather than referencing an
	// external CDN at runtime - this device shouldn't depend on internet
	// access, only its own LAN, to serve its control page. Embedded so the
	// page works no matter what directory the binary runs from (the old
	// relative web/ path 404d everything outside the service's
	// WorkingDirectory); immutable cache headers since the bytes are pinned.
	mux.HandleFunc("GET /static/htmax.min.js", func(w http.ResponseWriter, r *http.Request) {
		serveEmbeddedStatic(w, r, embeddedWeb, "web/htmax.min.js", "text/javascript")
	})
	mux.HandleFunc("GET /static/uPlot.iife.min.js", func(w http.ResponseWriter, r *http.Request) {
		serveEmbeddedStatic(w, r, embeddedWeb, "web/uPlot.iife.min.js", "text/javascript")
	})
	mux.HandleFunc("GET /static/uPlot.min.css", func(w http.ResponseWriter, r *http.Request) {
		serveEmbeddedStatic(w, r, embeddedWeb, "web/uPlot.min.css", "text/css")
	})

	// Theme bundles and their fonts, served as siblings (/static/themes/x.css
	// resolves ../assets/fonts/... to /static/assets/fonts/...), which is the
	// layout ftl-themes' CONTRACT.md requires. Only a slug that exists in the
	// embedded set is served, so a stale or hand-typed slug 404s rather than
	// escaping the embed with a traversal.
	// A ServeMux wildcard must span a whole path segment, so the ".css" is
	// matched here rather than in the pattern.
	mux.HandleFunc("GET /static/themes/{file}", func(w http.ResponseWriter, r *http.Request) {
		slug, ok := strings.CutSuffix(r.PathValue("file"), ".css")
		if !ok || !isKnownTheme(slug) {
			http.NotFound(w, r)
			return
		}
		serveEmbeddedStatic(w, r, embeddedThemes, themeAssetPath("dist/"+slug+".css"), "text/css")
	})
	mux.HandleFunc("GET /static/assets/fonts/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !strings.HasSuffix(name, ".woff2") || strings.ContainsAny(name, "/\\") {
			http.NotFound(w, r)
			return
		}
		serveEmbeddedStatic(w, r, embeddedThemes, themeAssetPath("assets/fonts/"+name), "font/woff2")
	})

	return mux
}

const remoteControlPort = "8080"

// startRemoteServer binds to the given address (normally "0.0.0.0" from
// remoteControlLoop, or a specific host via PI9696_REMOTE_BIND for dev
// testing). Binding 0.0.0.0 serves the control surface on every interface -
// the Round 3 design decision, replacing the old eth0-only constraint; access
// is still gated by the token/session auth.
func startRemoteServer(ip string) (*http.Server, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(ip, remoteControlPort))
	if err != nil {
		return nil, err
	}

	srv := &http.Server{Handler: newRemoteMux()}
	go func() {
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			logErrorf("Remote control server error: %v", err)
		}
	}()

	logInfof("Remote control server listening on http://%s", listener.Addr())
	return srv, nil
}

// anyInterfaceIP returns the first non-loopback IPv4 address on any up
// interface, or "" if none. This is the "serve on any interface" view that
// the eth0-only NetworkDetector can't give: the remote control server must
// run whenever *any* interface (eth0, wlan0 AP/client, USB gadget, etc.) has
// an address, not just eth0 (Round 3 design decision).
func anyInterfaceIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok {
				ip := ipnet.IP.To4()
				if ip != nil && !ip.IsLoopback() {
					return ip.String()
				}
			}
		}
	}
	return ""
}

// remoteControlLoop (re)binds the remote control server to every up interface
// (0.0.0.0) and tears it down when no interface has an address, mirroring
// networkMonitorLoop's polling approach but kept entirely separate from the
// app mutex: binding/shutting down a listener is not instant, and this must
// never block render()/input handling the way pre-worker Inferno start/stop
// used to.
func remoteControlLoop() {
	var currentServer *http.Server
	var wasUp bool

	for {
		up := anyInterfaceIP() != ""
		if up != wasUp {
			if currentServer != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				err := currentServer.Shutdown(ctx)
				cancel()
				if err != nil {
					// Graceful drain timed out - long-lived connections
					// (the WebSocket meter push, a dashboard that stopped
					// reading) don't count as idle. Force-close so a
					// stopped interface can't leave the old server's
					// connections half-open; their handlers exit on the
					// resulting write errors.
					currentServer.Close()
				}
				currentServer = nil
				logInfof("Remote control server stopped")
			}

			wasUp = up
			if up {
				srv, err := startRemoteServer("0.0.0.0")
				if err != nil {
					logErrorf("Failed to start remote control server: %v", err)
					wasUp = false // retry on the next tick
				} else {
					currentServer = srv
				}
			}
		}

		time.Sleep(5 * time.Second)
	}
}

// remoteAccessInfo is what Settings -> Remote Access shows on the OLED.
// It picks any reachable address (not just eth0) since the server now binds
// every interface.
func remoteAccessInfo() []string {
	ip := anyInterfaceIP()
	if ip == "" {
		return []string{"Remote Access", "Not available", "(no interface has an IP)"}
	}
	return []string{
		"Remote Access",
		fmt.Sprintf("http://%s:%s", ip, remoteControlPort),
		"Token: " + formatToken(remoteToken),
	}
}
