package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"io"
	"log"
	"math"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
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
// process startup (never persisted to disk), printed to the journal, and shown on the OLED via
// Settings -> Remote Access, so reading it requires physical/console access
// to the device - the same trust model as the rest of this app's local-only
// controls, just extended to the LAN.
//
// It's short (8 chars from a 31-symbol alphabet, ~39.6 bits of entropy)
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
	// Rejection sampling: 256 is not a multiple of the 31-symbol alphabet,
	// so a plain byte%31 made the first 8 symbols slightly likelier. Bytes
	// at or above the largest multiple of 31 are discarded and redrawn.
	limit := 256 - 256%len(remoteTokenAlphabet)
	out := make([]byte, 0, remoteTokenLength)
	b := make([]byte, remoteTokenLength*2)
	for len(out) < remoteTokenLength {
		if _, err := rand.Read(b); err != nil {
			log.Fatalf("Failed to generate remote control token: %v", err)
		}
		for _, c := range b {
			if int(c) < limit && len(out) < remoteTokenLength {
				out = append(out, remoteTokenAlphabet[int(c)%len(remoteTokenAlphabet)])
			}
		}
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
// POST (curl, QR #t= URL typed by hand) must not 401.
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
	// Reserve the attempt here, in the same critical section as the
	// check. Counting only on failure let a burst of N concurrent POSTs all
	// pass this check before any failure was recorded: N guesses per
	// lockout window instead of loginMaxAttempts. A success clears the
	// reservation (recordSuccess).
	if l.failures[ip] >= loginMaxAttempts {
		l.lockedAt[ip] = time.Now()
		return false
	}
	l.failures[ip]++
	l.seenAt[ip] = time.Now()
	return true
}

// loginMaxAttempts is how many login attempts an address gets before a
// minute's lockout.
const loginMaxAttempts = 5

// recordFailure closes a failed attempt that allowed already counted; the
// last permitted one starts the lockout.
func (l *loginLimiter) recordFailure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seenAt[ip] = time.Now()
	if l.failures[ip] >= loginMaxAttempts {
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

// revokeAll drops every session: used by token rotation so sessions minted
// under a compromised token don't survive it (logout alone revokes only the
// presented cookie).
func (s *sessionStore) revokeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = make(map[string]time.Time)
}

var sessions = newSessionStore()

// sessionLifetime is how long a login session lasts server-side.
const sessionLifetime = 12 * time.Hour

// clientIP is the address the login limiter keys on. Normally the TCP
// peer. When the peer is a reverse proxy listed in PI9696_TRUSTED_PROXIES
// (both units sit behind one), every client would otherwise share the
// proxy's address, so five bad tokens from anyone locked everyone out for
// a minute, repeatably. For a trusted peer the client is the rightmost
// X-Forwarded-For entry that is not itself a trusted proxy (entries to its
// left are client-supplied and spoofable). Untrusted peers' headers are
// ignored, so the default - no list - is the old behaviour.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !isTrustedProxy(host) {
		return host
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop := strings.TrimSpace(hops[i])
		if _, err := netip.ParseAddr(hop); err != nil {
			break // malformed chain: trust nothing further left
		}
		if !isTrustedProxy(hop) {
			return hop
		}
	}
	return host
}

// trustedProxies parses PI9696_TRUSTED_PROXIES once: comma-separated
// addresses or CIDR prefixes of reverse proxies whose X-Forwarded-For may
// be believed. A var so tests can substitute a list.
var trustedProxies = sync.OnceValue(func() []netip.Prefix {
	list, bad := parseTrustedProxies(os.Getenv("PI9696_TRUSTED_PROXIES"))
	for _, b := range bad {
		logErrorf("PI9696_TRUSTED_PROXIES: ignoring %q (not an address or CIDR prefix)", b)
	}
	return list
})

// parseTrustedProxies returns the valid prefixes in s (a bare address is a
// single-address prefix) and the entries it could not parse.
func parseTrustedProxies(s string) (list []netip.Prefix, bad []string) {
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if p, err := netip.ParsePrefix(f); err == nil {
			list = append(list, p.Masked())
		} else if a, err := netip.ParseAddr(f); err == nil {
			list = append(list, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
		} else {
			bad = append(bad, f)
		}
	}
	return list, bad
}

func isTrustedProxy(ip string) bool {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	a = a.Unmap()
	for _, p := range trustedProxies() {
		if p.Contains(a) {
			return true
		}
	}
	return false
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
			// Redirect bare, without the query: the access-QR prefill token
			// travels in the fragment (/#t=...), which browsers preserve
			// across this 303 and never send to the server - keeping it
			// out of server and proxy logs and the login POST (it does
			// stay in the browser's own history).
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		// Mutations and WS handshakes additionally need a same-origin
		// request: the cookie is SameSite=Strict, but old/non-conforming
		// clients and x/net/websocket's default-accept handshake don't
		// honor that, and a foreign page driving an authenticated browser
		// is exactly the CSRF shape.
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions ||
			strings.HasPrefix(r.URL.Path, "/ws/") {
			if !checkSameOrigin(r) {
				http.Error(w, "cross-origin request rejected", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}

// checkSameOrigin reports whether a request's Origin (or Referer fallback)
// matches the request's own host. Absent headers (curl, tests, same-origin
// navigations) pass - this is defense in depth behind SameSite=Strict, not
// a boundary: it stops conforming browsers (which always send Origin on
// POST/WS) from driving the device cross-origin.
func checkSameOrigin(r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		return strings.EqualFold(u.Host, r.Host)
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		u, err := url.Parse(ref)
		if err != nil {
			return false
		}
		return strings.EqualFold(u.Host, r.Host)
	}
	return true
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

// manifestTmpl is static JSON with the device name filled in so an installed
// home-screen icon reflects a renamed unit without a rebuild. Rendered with
// encoding/json (not html/template): the name validator excludes
// JSON-significant chars today, but the wrong engine invites a future break.
func handleManifest(w http.ResponseWriter, r *http.Request) {
	mutex.Lock()
	name := deviceName
	mutex.Unlock()
	w.Header().Set("Content-Type", "application/manifest+json")
	manifest := map[string]any{
		"name":             name + " Remote",
		"short_name":       name,
		"start_url":        "/",
		"display":          "standalone",
		"background_color": "#020509",
		"theme_color":      "#00d9ff",
		"icons":            []map[string]string{{"src": "/icon.svg", "sizes": "any", "type": "image/svg+xml", "purpose": "any"}},
	}
	// start_url ("/") requires auth like every other route - opening the
	// installed app when the session cookie has expired just lands on
	// /login, same as any bookmark would.
	if err := json.NewEncoder(w).Encode(manifest); err != nil {
		logDebugf("manifest encode: %v", err)
	}
}

type loginPageData struct {
	Error, DeviceName string
	Logo              template.HTML
	// Theme/ThemeCSS mirror the dashboard's opt-in theming so the login
	// page is the same product, not a stranger: data-theme scopes the
	// design tokens (e.g. --font) the stylesheet reads. Unthemed
	// renders exactly as before (no marker beyond "none", no href).
	// CoreVersion cache-busts the always-linked core.css.
	Theme, ThemeCSS, CoreVersion string
	// HTMLTag is the prebuilt <html> open tag carrying data-theme and the
	// library display options (motion/contrast/density).
	HTMLTag template.HTML
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
	return active, themeCSSHref(active)
}

var loginPageTmpl = template.Must(template.New("login").Parse(`<!DOCTYPE html>
{{.HTMLTag}}<head><title>{{.DeviceName}} Remote</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<link rel="manifest" href="/manifest.json">
<link rel="icon" href="/icon.svg" type="image/svg+xml">
<!-- Core is always linked (reset + shared components, no tokens of its own);
   the active theme bundle on top supplies the palette. -->
<link rel="stylesheet" href="/static/themes/core.css?v={{.CoreVersion}}">
<link rel="stylesheet" href="{{.ThemeCSS}}">
<style>
body{font-family:var(--font,"Consolas",monospace);background:radial-gradient(ellipse at center,var(--surface,#0a1a2e),var(--bg,#020509) 75%);color:var(--text,#cfeeff);display:flex;flex-direction:column;align-items:center;justify-content:center;min-height:100vh;margin:0;gap:2em}
.logo-svg{width:480px;max-width:85vw;display:block}
/* Box geometry is the shared .panel; the centered layout is app-owned. */
form.panel{text-align:center;min-width:20em}
.token-row{display:flex;align-items:center;justify-content:center;gap:var(--space-2xs,0.4em);margin-bottom:var(--space-m,1em)}
.token-row .input{font-size:1.3em;width:1.4em;padding:0.4em 0;text-align:center;text-transform:uppercase}
.token-row .dash{color:var(--muted,#5b8aa8);font-size:1.3em}
.err{color:var(--danger,#ff3355)}
.hint{color:var(--muted,#5b8aa8);font-size:0.85em;margin-top:1em}

@media (max-width: 480px) {
  form.panel{padding:1.5em 1.2em}
  .token-row{gap:0.25em}
  .token-row .input{width:1.1em;font-size:1.1em}
}
</style></head>
<body>
{{.Logo}}
<form method="POST" action="/login" id="loginForm" class="panel">
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
<div class="token-row" id="tokenRow">
<input class="input" maxlength="1" autofocus autocomplete="off" value="{{index .Boxes 0}}">
<input class="input" maxlength="1" autocomplete="off" value="{{index .Boxes 1}}">
<input class="input" maxlength="1" autocomplete="off" value="{{index .Boxes 2}}">
<input class="input" maxlength="1" autocomplete="off" value="{{index .Boxes 3}}">
<span class="dash">-</span>
<input class="input" maxlength="1" autocomplete="off" value="{{index .Boxes 4}}">
<input class="input" maxlength="1" autocomplete="off" value="{{index .Boxes 5}}">
<input class="input" maxlength="1" autocomplete="off" value="{{index .Boxes 6}}">
<input class="input" maxlength="1" autocomplete="off" value="{{index .Boxes 7}}">
</div>
<input type="hidden" name="token" id="tokenValue">
<noscript><p><input name="token" maxlength="9" autocomplete="off" placeholder="XXXXXXXX" style="text-transform:uppercase"></p></noscript>
<button type="submit" class="btn btn-primary">Enter</button>
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
// The OLED's access-QR encodes a URL with "#t=<token>"; pre-fill the boxes
// so scanning the code lands the operator one click from Enter. The fragment
// is never sent to the server, so unlike the old ?t= query it stays out of
// browser history and proxy logs.
(function() {
  var m = /[#&?]t=([A-Za-z0-9]{8})/.exec(window.location.hash);
  var t = m && m[1];
  if (t) {
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
	// Only start the show-token window if it isn't already running: a
	// crawler/attacker GETting /login in a loop must not extend it (the
	// token would stay parked on the panel indefinitely).
	if !loginTokenFreshLocked() {
		lastLoginPage = time.Now()
	}
	mutex.Unlock()
	writeLoginPage(w, loginPageData{})
}

// writeLoginPage fills in the page-invariant fields (name, logo, theme) and
// renders; error/box state rides in d.
// renderFragment executes a page or htmx fragment template into the
// response. These calls used to drop the error: a failing template sent a
// truncated 200 with nothing logged. Headers are usually already sent by
// then, so the error can only be logged, but now it always is.
func renderFragment(w io.Writer, t *template.Template, data any) {
	if err := t.Execute(w, data); err != nil {
		logErrorf("render %s: %v", t.Name(), err)
	}
}

func writeLoginPage(w http.ResponseWriter, d loginPageData) {
	// Snapshot under the app mutex: a WebUI rename writes deviceName under
	// it, and reading a string mid-write is a data race. No caller holds the
	// mutex here (loginPageTheme below takes it too).
	mutex.Lock()
	d.DeviceName = deviceName
	mutex.Unlock()
	d.Logo = template.HTML(pi9696LogoSVG)
	d.Theme, d.ThemeCSS = loginPageTheme()
	d.CoreVersion = themeBuildVersion()
	d.HTMLTag = displayHTMLTag(currentTheme(), currentThemeVariant(), currentThemeTint(currentTheme()))
	if err := loginPageTmpl.Execute(w, d); err != nil {
		logDebugf("login render: %v", err)
	}
}

func handleLoginPost(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !loginLimit.allowed(ip) {
		w.WriteHeader(http.StatusTooManyRequests)
		writeLoginPage(w, loginPageData{Error: "Too many attempts, wait a minute"})
		return
	}

	submitted := normalizeToken(r.FormValue("token"))
	// Rotation writes remoteToken under the app mutex; reading it bare here
	// raced with that write.
	mutex.Lock()
	token := remoteToken
	mutex.Unlock()
	if subtle.ConstantTimeCompare([]byte(submitted), []byte(token)) != 1 {
		loginLimit.recordFailure(ip)
		w.WriteHeader(http.StatusUnauthorized)
		writeLoginPage(w, loginPageData{Error: "Invalid token", Boxes: boxesFromToken(submitted)})
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
		// No Secure flag: this server is plain HTTP (see README's Auth &
		// Security notes for why, and what that means for LAN
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

// handleAPIRotateToken mints a fresh access token and revokes every session
// (including the caller's): sessions from a compromised token must not
// survive rotation. Redirects to /login with the new token in the fragment,
// so the operator's boxes pre-fill and they're one click from logging back
// in - while the token itself never touches a query string or the server log.
func handleAPIRotateToken(w http.ResponseWriter, r *http.Request) {
	mutex.Lock()
	remoteToken = generateRemoteToken()
	sessions.revokeAll()
	noteActivity()
	newToken := remoteToken
	mutex.Unlock()
	logInfof("access token rotated, all sessions revoked")
	fmt.Fprintln(os.Stderr, "remote access code (rotated):", formatToken(newToken))
	http.SetCookie(w, &http.Cookie{Name: remoteSessionCookie, Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login#t="+newToken, http.StatusSeeOther)
}

// --- Themes ---------------------------------------------------------------
//
// The dashboard reads ftl-themes' tokens directly (--accent, --surface,
// --text, --muted, --danger, --success, --warning, --space-*). Selecting a
// theme links one self-contained bundle that defines them, so every rule
// re-colours with no rule needing editing; the app also re-lays-out the page
// through the library's app-shell hooks.
//
// v4 dropped the ftl- prefix from every token, so the app deliberately
// declares none of them itself: core.css already supplies the baseline set,
// and a same-named declaration would silently override the theme (or, for
// --text/--border, become self-referential and resolve to nothing).
// TestDefaultThemeIsFTL enforces that.

// defaultThemeSlug is the ftl-themes bundle a fresh or upgraded unit links:
// xbmc's blue-on-navy is the closest shipping palette to the old built-in
// look. blue-future (the baseline sci-fi HUD this dashboard was extracted
// from) takes over once upstream un-archives it - see ftl-themes#49.
const defaultThemeSlug = "xbmc"

// themeSlug names the bundle the dashboard links. Persisted in the unit's
// config like every other setting, so the choice follows the device rather
// than one browser. Guarded by mutex.
var themeSlug = defaultThemeSlug

// themeVariant is the palette variant (CONTRACT.md "Palette variants") of
// themeSlug, rendered as <html data-variant>; "" is the theme's own palette.
// Persisted beside the theme and only ever one the theme lists. Guarded by
// mutex.
var themeVariant string

// themeTints is the user-chosen tint colour per theme slug (CONTRACT.md
// "Theme tint"): only themes whose manifest declares a tint, only #rrggbb.
// Absent means the theme's default or the chosen preset shows. Guarded by
// mutex.
var themeTints = map[string]string{}

var tintColorRE = regexp.MustCompile(`^#[0-9a-f]{6}$`)

// normalizeTint returns a colour as lowercase #rrggbb, or "" if it isn't one.
func normalizeTint(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if !tintColorRE.MatchString(v) {
		return ""
	}
	return v
}

// validThemeTints keeps the entries of a loaded map that name a theme with
// a tint and hold a valid colour.
func validThemeTints(in map[string]string) map[string]string {
	out := map[string]string{}
	for slug, v := range in {
		if themeTintSpec(slug) != nil {
			if c := normalizeTint(v); c != "" {
				out[slug] = c
			}
		}
	}
	return out
}

func copyThemeTints(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// themeTintSpec returns a theme's tint declaration, nil if it has none.
func themeTintSpec(slug string) *themeTintEntry {
	for _, t := range availableThemes() {
		if t.Slug == slug {
			return t.Tint
		}
	}
	return nil
}

// currentThemeTint returns the stored tint colour for slug, "" when none
// is stored or the theme declares no tint (a tint is never rendered for a
// theme without one).
func currentThemeTint(slug string) string {
	if themeTintSpec(slug) == nil {
		return ""
	}
	mutex.Lock()
	defer mutex.Unlock()
	return themeTints[slug]
}

// tintTokens lists every tint token the manifest declares, so a theme
// change can remove the previous theme's inline token.
func tintTokens() []string {
	var out []string
	for _, t := range availableThemes() {
		if t.Tint != nil {
			out = append(out, t.Tint.Token)
		}
	}
	return out
}

// Library display options (CONTRACT.md "User display options"): browser
// switches an app may offer beside the theme, applied as documentElement
// attributes/style. Guarded by the app mutex; persisted like every setting.
var (
	displayMotion     = "full"     // or "reduced" -> <html data-motion="reduced">
	displayContrast   = "standard" // or "high"    -> <html data-contrast="high">
	displayDensityIdx = 0          // 0 Normal, 1 Compact (0.85), 2 Comfortable (1.15)
)

var displayDensityValues = [3]string{"1", "0.85", "1.15"}

// displayHTMLTag renders the whole <html> open tag: data-theme plus the
// library display options (attributes only when non-default, density as an
// inline custom property - the contract's documented override path, which
// beats any theme). Built server-side because html/template refuses
// dynamic content between a tag's attributes.
func displayHTMLTag(theme, variant, tint string) template.HTML {
	mutex.Lock()
	motion, contrast, density := displayMotion, displayContrast, displayDensityValues[displayDensityIdx]
	mutex.Unlock()
	var b strings.Builder
	b.WriteString(`<html data-theme="` + html.EscapeString(theme) + `"`)
	if variant != "" {
		b.WriteString(` data-variant="` + html.EscapeString(variant) + `"`)
	}
	if motion == "reduced" {
		b.WriteString(` data-motion="reduced"`)
	}
	if contrast == "high" {
		b.WriteString(` data-contrast="high"`)
	}
	// One style attribute carries both inline overrides: density, and the
	// theme tint token (inline beats the theme's root block and variants).
	var style []string
	if density != "1" {
		style = append(style, "--density:"+density)
	}
	if spec := themeTintSpec(theme); spec != nil && normalizeTint(tint) != "" {
		style = append(style, spec.Token+":"+normalizeTint(tint))
	}
	if len(style) > 0 {
		b.WriteString(` style="` + html.EscapeString(strings.Join(style, ";")) + `"`)
	}
	b.WriteString(">")
	return template.HTML(b.String())
}

// themeManifest mirrors ftl-themes' dist/themes.json entries. Version is
// the build's content hash (identical across every entry) - used to
// cache-bust theme asset URLs; scheme/luminance/shellAware are picker
// metadata per CONTRACT.md.
type themeManifest struct {
	Slug        string   `json:"slug"`
	Label       string   `json:"label"`
	Description string   `json:"description"`
	Version     string   `json:"version"`
	Scheme      string   `json:"scheme"`
	Luminance   *float64 `json:"luminance"`
	ShellAware  bool     `json:"shellAware"`
	// Variants are the theme's palette variants (sub-themes), offered in
	// the picker beneath the theme. Absent on most themes.
	Variants []themeVariantEntry `json:"variants"`
	// Tint is the theme's user-chosen colour, if it declares one: the app
	// must offer it next to the theme choice (CONTRACT.md "Theme tint").
	Tint *themeTintEntry `json:"tint"`
}

type themeTintEntry struct {
	Token   string `json:"token"`
	Default string `json:"default"`
	Label   string `json:"label"`
}

type themeVariantEntry struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// themeAssetPath maps a path inside the submodule to its embed path.
func themeAssetPath(rel string) string { return "third_party/ftl-themes/" + rel }

var (
	themeListOnce sync.Once
	themeList     []themeManifest
	themeVersionV string // build version from the manifest, for cache-busting
)

// availableThemes reads the embedded manifest once: only ftl-themes
// bundles, no synthesized entries - the dashboard always links one theme.
func availableThemes() []themeManifest {
	themeListOnce.Do(func() {
		themeList = nil
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
		if len(parsed) > 0 {
			themeVersionV = parsed[0].Version
		}
		themeList = append(themeList, parsed...)
	})
	return themeList
}

// themeBuildVersion returns the manifest's build version ("" when the
// manifest was unreadable), for cache-busting theme asset URLs.
func themeBuildVersion() string {
	availableThemes()
	return themeVersionV
}

// themeCSSHref returns the versioned URL for a theme bundle. Version query
// per CONTRACT.md "Cache-busting": a stale cached bundle after an upgrade
// would otherwise 404 its own assets or show old colors.
func themeCSSHref(slug string) string {
	return "/static/themes/" + slug + ".css?v=" + themeBuildVersion()
}

// iconSpriteHref returns the per-theme icon sprite URL.
func iconSpriteHref(slug string) string {
	return "/static/themes/icons/" + slug + ".svg"
}

// isKnownTheme reports whether a slug names a bundle that is actually
// embedded. Guards the static route and any persisted value.
func isKnownTheme(slug string) bool {
	if slug == "" {
		return false
	}
	for _, t := range availableThemes() {
		if t.Slug == slug {
			return true
		}
	}
	return false
}

// themeHasVariant reports whether slug's manifest entry lists variant id.
// "" (the theme's own palette) is not a variant.
func themeHasVariant(slug, id string) bool {
	if id == "" {
		return false
	}
	for _, t := range availableThemes() {
		if t.Slug == slug {
			for _, v := range t.Variants {
				if v.ID == id {
					return true
				}
			}
		}
	}
	return false
}

// currentThemeVariant returns the active palette variant of the active
// theme, "" when none is chosen or it does not belong to that theme.
func currentThemeVariant() string {
	theme := currentTheme()
	mutex.Lock()
	v := themeVariant
	mutex.Unlock()
	if !themeHasVariant(theme, v) {
		return ""
	}
	return v
}

// currentTheme returns the active slug, falling back to the default when
// the persisted value names a bundle that no longer ships (e.g. an
// upstream-archived theme or an old "none"). Caller holds no lock.
func currentTheme() string {
	mutex.Lock()
	defer mutex.Unlock()
	if !isKnownTheme(themeSlug) {
		return defaultThemeSlug
	}
	return themeSlug
}

// themeChoice is one entry of the theme picker: a theme with its own
// palette (Variant "") or one of its palette variants. The picker posts the
// choice's index in themeChoices().
type themeChoice struct {
	Slug, Variant, Label string
}

// themeChoices lists every theme in manifest order, each followed by its
// variants, so a variant always sits beneath its theme (CONTRACT.md).
func themeChoices() []themeChoice {
	var out []themeChoice
	for _, t := range availableThemes() {
		out = append(out, themeChoice{Slug: t.Slug, Label: t.Label})
		for _, v := range t.Variants {
			out = append(out, themeChoice{Slug: t.Slug, Variant: v.ID, Label: v.Label})
		}
	}
	return out
}

// themeChoiceIndex returns the picker index of (slug, variant), -1 if none.
func themeChoiceIndex(slug, variant string) int {
	for i, c := range themeChoices() {
		if c.Slug == slug && c.Variant == variant {
			return i
		}
	}
	return -1
}

// themePickerView groups the choices for the picker template: a theme
// without variants is a plain option, a theme with variants an <optgroup>
// holding its own palette first and then each variant.
type themePickerView struct {
	Groups []themePickerGroup
}

type themePickerGroup struct {
	Label   string // optgroup label; "" for a theme without variants
	Options []themePickerOption
}

type themePickerOption struct {
	Idx      int
	Label    string
	Selected bool
}

func themePicker() themePickerView {
	active := themeChoiceIndex(currentTheme(), currentThemeVariant())
	var v themePickerView
	i := 0
	for _, t := range availableThemes() {
		g := themePickerGroup{}
		if len(t.Variants) > 0 {
			g.Label = t.Label
		}
		g.Options = append(g.Options, themePickerOption{Idx: i, Label: t.Label, Selected: i == active})
		i++
		for _, tv := range t.Variants {
			g.Options = append(g.Options, themePickerOption{Idx: i, Label: tv.Label, Selected: i == active})
			i++
		}
		v.Groups = append(v.Groups, g)
	}
	return v
}

var themeFragmentTmpl = template.Must(template.New("theme-select").Parse(`<div id="theme" class="setting-cell">
<div class="field-row">
<form hx-post="/api/settings/theme" hx-target="#theme" hx-swap="outerHTML">
<label class="label">Theme</label>
<select name="idx" class="select" onchange="this.form.requestSubmit()">
{{range .Groups}}{{if .Label}}<optgroup label="{{.Label}}">{{end}}{{range .Options}}<option value="{{.Idx}}" {{if .Selected}}selected{{end}}>{{.Label}}</option>{{end}}{{if .Label}}</optgroup>{{end}}
{{end}}</select>
</form>
</div>
</div>`))

// The three library display options (CONTRACT.md "User display options"):
// density/motion/contrast ride the documentElement, not the theme, so a
// look stays intact while accessibility needs are met. Select fragments
// post here; the response re-renders the row and OOB-updates the carrier
// attributes so no reload is needed (the live sockets survive). Default
// values REMOVE the carrier, matching an untouched page.
type displayOption struct {
	id      string
	post    string
	label   string
	options []string
	// get returns the current selection index; set applies one.
	get func() int
	set func(int)
	// carrier renders the documentElement update for the applied value.
	carrier func(int) string
}

var displayOptions = []displayOption{
	{
		id: "motion", post: "/api/settings/motion", label: "Motion",
		options: []string{"Full", "Reduced"},
		get:     func() int { return boolIdx(displayMotion == "reduced") },
		set:     func(i int) { displayMotion = map[int]string{0: "full", 1: "reduced"}[i] },
		carrier: func(i int) string {
			if i == 1 {
				return `document.documentElement.setAttribute("data-motion","reduced")`
			}
			return `document.documentElement.removeAttribute("data-motion")`
		},
	},
	{
		id: "contrast", post: "/api/settings/contrast", label: "Contrast",
		options: []string{"Standard", "High"},
		get:     func() int { return boolIdx(displayContrast == "high") },
		set:     func(i int) { displayContrast = map[int]string{0: "standard", 1: "high"}[i] },
		carrier: func(i int) string {
			if i == 1 {
				return `document.documentElement.setAttribute("data-contrast","high")`
			}
			return `document.documentElement.removeAttribute("data-contrast")`
		},
	},
	{
		id: "density", post: "/api/settings/density", label: "Density",
		options: []string{"Normal", "Compact", "Comfortable"},
		get:     func() int { return displayDensityIdx },
		set: func(i int) {
			if i < len(displayDensityValues) {
				displayDensityIdx = i
			}
		},
		carrier: func(i int) string {
			if d := displayDensityValues[i]; d != "1" {
				return `document.documentElement.style.setProperty("--density","` + d + `")`
			}
			return `document.documentElement.style.removeProperty("--density")`
		},
	},
}

func boolIdx(b bool) int {
	if b {
		return 1
	}
	return 0
}

// displayOptBuf returns the render buffer for a display option's select.
func displayOptBuf(id string, motion, contrast, density *bytes.Buffer) *bytes.Buffer {
	switch id {
	case "motion":
		return motion
	case "contrast":
		return contrast
	case "density":
		return density
	}
	return nil
}

func registerDisplayOptionRoutes(mux *http.ServeMux) {
	for _, opt := range displayOptions {
		opt := opt
		mux.HandleFunc("POST "+opt.post, requireAuth(func(w http.ResponseWriter, r *http.Request) {
			idx, err := strconv.Atoi(r.FormValue("idx"))
			if err != nil || idx < 0 || idx >= len(opt.options) {
				http.Error(w, "bad idx", http.StatusBadRequest)
				return
			}
			mutex.Lock()
			opt.set(idx)
			settingChanged()
			mutex.Unlock()
			noteActivity()
			renderFragment(w, selectFragmentTmpl, settingSelect(opt.id, opt.post, opt.label, "", func() optionsView { return optionsView{Options: opt.options, Idx: opt.get()} }))
			// Carrier update rides out-of-band so the applied value takes
			// effect immediately (same pattern as the theme swap).
			fmt.Fprintf(w, "\n<script hx-swap-oob=\"true\">%s</script>", opt.carrier(idx))
		}))
	}
}

// handleAPISettingsTheme persists the chosen theme and tells the page to
// swap its stylesheet. The theme lives in the unit's config beside every
// other setting, so it follows the device rather than the browser - the
// front panel and the web UI share one state, as they do for brightness.
func handleAPISettingsTheme(w http.ResponseWriter, r *http.Request) {
	themeChanged, variantChanged := false, false
	if idx, err := strconv.Atoi(r.FormValue("idx")); err == nil {
		all := themeChoices()
		if idx >= 0 && idx < len(all) {
			c := all[idx]
			mutex.Lock()
			themeChanged = themeSlug != c.Slug
			variantChanged = themeChanged || themeVariant != c.Variant
			themeSlug, themeVariant = c.Slug, c.Variant
			// A preset is a sub-theme: choosing one clears the theme's custom
			// tint so the preset shows (CONTRACT.md "Theme tint" step 3).
			if c.Variant != "" && themeTints[c.Slug] != "" {
				delete(themeTints, c.Slug)
				variantChanged = true
			}
			if variantChanged {
				settingChanged()
			}
			mutex.Unlock()
		}
	}
	renderFragment(w, themeFragmentTmpl, themePicker())
	if variantChanged {
		// The tint control follows the theme (shown only when it declares a
		// tint) and the inline tint token follows the stored colour.
		tv := currentTintView()
		tv.OOB = true
		w.Write([]byte("\n"))
		renderFragment(w, tintFragmentTmpl, tv)
	}
	if variantChanged && !themeChanged {
		// Same bundle, other palette: only the marker attribute changes.
		fmt.Fprintf(w, "\n<script hx-swap-oob=\"true\">%s;%stelePalette=null</script>", variantCarrier(currentThemeVariant()), tintCarrier())
	}
	if themeChanged {
		// Out-of-band swap of the <link> and the <html data-theme> marker so
		// the change is visible immediately, without a reload that would lose
		// the live meter and telemetry sockets.
		active := currentTheme()
		href := fmt.Sprintf(" href=%q", themeCSSHref(active))
		fmt.Fprintf(w, "\n<link id=\"themecss\" rel=\"stylesheet\"%s hx-swap-oob=\"outerHTML\">", href)
		// Charts snapshot the palette at creation (see telePaletteInit), so
		// drop them: the next telemetry push (<=2s) rebuilds them in the new
		// theme's colors instead of keeping stale ones until a reload.
		// hx-swap-oob="true" marks this as out-of-band content to execute
		// in place: a bare <script> after the OOB <link> relied on htmx
		// executing in-swapped scripts, which multi-node responses don't
		// guarantee. The sprite rewrite keeps every .icon <use> pointed
		// at the new theme's icon set (same ids, theme-authored shapes).
		fmt.Fprintf(w, "\n<script hx-swap-oob=\"true\">document.documentElement.setAttribute(\"data-theme\",%q);%s;%sSPRITE=%q;teleCPU=teleRAM=teleTemp=teleDisk=teleSub=telePalette=null;document.querySelectorAll('.icon use').forEach(function(u){u.setAttribute('href',SPRITE)})</script>", active, variantCarrier(currentThemeVariant()), tintCarrier(), iconSpriteHref(active))
	}
}

// tintView is the theme tint setting cell (CONTRACT.md "Theme tint"):
// shown only for a theme that declares a tint, as a plain colour input
// posted like every other setting. OOB marks it as an out-of-band swap
// riding a theme-picker response.
type tintView struct {
	Show   bool
	OOB    bool
	Token  string
	Label  string
	Value  string // colour shown: the stored one, else the theme default
	Custom bool   // a custom colour is stored (Default resets it)
}

func currentTintView() tintView {
	theme := currentTheme()
	spec := themeTintSpec(theme)
	if spec == nil {
		return tintView{}
	}
	v := tintView{Show: true, Token: spec.Token, Label: spec.Label, Value: normalizeTint(spec.Default)}
	if v.Label == "" {
		v.Label = "Theme tint"
	}
	if c := currentThemeTint(theme); c != "" {
		v.Value, v.Custom = c, true
	}
	return v
}

// With no custom colour the input shows what is in effect (a preset's
// colour, read from the stylesheet once it has loaded), like the library's
// own control. Dragging previews live; release saves.
var tintFragmentTmpl = template.Must(template.New("tint").Parse(`{{if .Show}}<div id="tint" class="setting-cell"{{if .OOB}} hx-swap-oob="outerHTML"{{end}}>
<div class="field-row">
<form hx-post="/api/settings/tint" hx-target="#tint" hx-swap="outerHTML">
<label class="label" for="tintcolor">{{.Label}}</label>
<input id="tintcolor" class="tint-picker" type="color" name="tint" value="{{.Value}}" style="inline-size:2.4rem;block-size:2rem;padding:0;cursor:pointer" oninput="document.documentElement.style.setProperty({{.Token}},this.value)" onchange="this.form.requestSubmit()">
{{if .Custom}}<button class="btn btn-secondary btn-sm" type="submit" name="reset" value="1">Default</button>{{else}}<script>(function(){var i=document.getElementById("tintcolor"),v=getComputedStyle(document.documentElement).getPropertyValue({{.Token}}).trim();if(/^#[0-9a-f]{6}$/i.test(v))i.value=v})()</script>{{end}}
</form>
</div>
</div>{{else}}<div id="tint" hidden{{if .OOB}} hx-swap-oob="outerHTML"{{end}}></div>{{end}}`))

// tintCarrier is the documentElement update that applies the active
// theme's tint: drop every tint token, then set the stored colour, if any.
func tintCarrier() string {
	var b strings.Builder
	for _, tok := range tintTokens() {
		fmt.Fprintf(&b, "document.documentElement.style.removeProperty(%q);", tok)
	}
	theme := currentTheme()
	if spec := themeTintSpec(theme); spec != nil {
		if c := currentThemeTint(theme); c != "" {
			fmt.Fprintf(&b, "document.documentElement.style.setProperty(%q,%q);", spec.Token, c)
		}
		// Show the colour now in effect (the custom one, or the preset's),
		// read once the variant above has applied.
		// The tint cell swapped in by the same response settles after this
		// runs (htmx settle delay), and after a theme change the new bundle
		// loads asynchronously: sync again after the settle and on load.
		fmt.Fprintf(&b, `(function(){function s(){var i=document.getElementById("tintcolor"),v=getComputedStyle(document.documentElement).getPropertyValue(%q).trim();if(i&&/^#[0-9a-f]{6}$/i.test(v))i.value=v}s();setTimeout(s,100);var l=document.getElementById("themecss");if(l)l.addEventListener("load",s,{once:true})})();`, spec.Token)
	}
	return b.String()
}

// handleAPISettingsTint stores (or with reset=1 clears) the active theme's
// tint colour. Only a theme that declares a tint accepts one, and only as
// #rrggbb: the colour lands in an inline style on every page.
func handleAPISettingsTint(w http.ResponseWriter, r *http.Request) {
	theme := currentTheme()
	if themeTintSpec(theme) == nil {
		http.Error(w, "this theme has no tint", http.StatusBadRequest)
		return
	}
	colour := ""
	if r.FormValue("reset") == "" {
		if colour = normalizeTint(r.FormValue("tint")); colour == "" {
			http.Error(w, "tint must be #rrggbb", http.StatusBadRequest)
			return
		}
	}
	mutex.Lock()
	if themeTints[theme] != colour {
		if colour == "" {
			delete(themeTints, theme)
		} else {
			themeTints[theme] = colour
		}
		settingChanged()
	}
	mutex.Unlock()
	noteActivity()
	renderFragment(w, tintFragmentTmpl, currentTintView())
	fmt.Fprintf(w, "\n<script hx-swap-oob=\"true\">%stelePalette=null</script>", tintCarrier())
}

// variantCarrier is the documentElement update for a palette variant: set
// data-variant, or remove it for the theme's own palette.
func variantCarrier(variant string) string {
	if variant == "" {
		return `document.documentElement.removeAttribute("data-variant")`
	}
	return fmt.Sprintf(`document.documentElement.setAttribute("data-variant",%q)`, variant)
}

type dashboardData struct {
	DeviceName           string
	Logo                 template.HTML
	Theme                string
	ThemeCSS             string
	CoreVersion          string
	IconSprite           string
	HTMLTag              template.HTML
	ThemeFragment        template.HTML
	TintFragment         template.HTML
	MotionFragment       template.HTML
	ContrastFragment     template.HTML
	DensityFragment      template.HTML
	VURangeFragment      template.HTML
	PeakHoldFragment     template.HTML
	SampleRateFragment   template.HTML
	ChannelCountFragment template.HTML
	TagFragment          template.HTML
	PrefixFragment       template.HTML
	TransportFragment    template.HTML
	HyperdeckFragment    template.HTML
	LogLevelFragment     template.HTML
	BrightnessFragment   template.HTML
	AutoDimFragment      template.HTML
	MonitorFragment      template.HTML
	DemoFragment         template.HTML
	DeviceNameFragment   template.HTML
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
<div class="field-row">
<form hx-post="/api/settings/transport-mode" hx-target="#transportmode" hx-swap="outerHTML">
<label class="label">Transport Buttons</label>
<select name="idx" class="select" onchange="this.form.requestSubmit()">
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
<div class="field-row">
<form hx-post="{{.Post}}" hx-target="#{{.Id}}" hx-swap="outerHTML">
<label class="label">{{.Label}}</label>
<select name="idx" class="select" onchange="this.form.requestSubmit()">
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
<div class="field-row">
<form hx-post="/api/settings/brightness" hx-target="#brightness" hx-swap="outerHTML">
<label class="label" for="brightnessRange">Brightness</label>
<span class="hint field-hint" id="brightnessVal">{{.Pct}}%</span>
<input id="brightnessRange" class="slider" type="range" name="pct" min="0" max="100" step="1" value="{{.Pct}}" oninput="document.getElementById('brightnessVal').textContent=this.value+'%'" onchange="this.form.requestSubmit()" title="Panel brightness 0-100%">
</form>
</div>
</div>`))

// autoDimFragmentTmpl is the Display -> Auto Dim setting: an on/off switch
// for the dim-then-off idle behavior. Mirrors the WiFi switch markup.
var autoDimFragmentTmpl = template.Must(template.New("autodim").Parse(`<div id="autodim" class="setting-cell">
<div class="field-row">
<form hx-post="/api/settings/dim" hx-target="#autodim" hx-swap="outerHTML">
<label class="label" for="autoDimToggle">Auto Dim</label>
<label class="switch" for="autoDimToggle">
<input id="autoDimToggle" name="enabled" type="checkbox" {{if .Enabled}}checked{{end}} onchange="this.form.requestSubmit()">
<span class="switch-track"><span class="switch-thumb"></span></span>
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
	renderFragment(w, brightnessFragmentTmpl, brightnessViewData())
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
	renderFragment(w, autoDimFragmentTmpl, autoDimViewData())
}

// hyperdeckFragmentTmpl is the Transport -> HyperDeck Control setting: an
// on/off switch for the Blackmagic HyperDeck protocol port (TCP 9993).
// Mirrors the Demo Mode switch; the hint states the no-auth caveat so the
// toggle reads as an informed choice, not a footnote.
var hyperdeckFragmentTmpl = template.Must(template.New("hyperdeck").Parse(`<div id="hyperdeck" class="setting-cell">
<div class="field-row">
<form hx-post="/api/settings/hyperdeck" hx-target="#hyperdeck" hx-swap="outerHTML">
<label class="label" for="hyperdeckToggle">HyperDeck Control</label>
<label class="switch" for="hyperdeckToggle">
<input id="hyperdeckToggle" name="enabled" type="checkbox" {{if .Enabled}}checked{{end}} onchange="this.form.requestSubmit()">
<span class="switch-track"><span class="switch-thumb"></span></span>
<span class="switch-readout" data-on="ON" data-off="OFF"></span>
</label>
</form>
</div>
<div class="field-row"><span class="hint field-hint">TCP 9993, Blackmagic protocol, no auth while on</span></div>
</div>`))

type hyperdeckView struct {
	Enabled bool
}

func hyperdeckViewData() hyperdeckView {
	mutex.Lock()
	defer mutex.Unlock()
	return hyperdeckView{Enabled: hyperdeckEnabled}
}

func handleAPISettingsHyperdeck(w http.ResponseWriter, r *http.Request) {
	enabled := r.FormValue("enabled") != ""
	mutex.Lock()
	setHyperdeckEnabledLocked(enabled)
	settingChanged()
	noteActivity()
	mutex.Unlock()
	renderFragment(w, hyperdeckFragmentTmpl, hyperdeckViewData())
}

// demoFragmentTmpl is the Demo -> Demo Mode setting: an on/off switch for
// the simulated-audio demonstration mode. Mirrors the Auto Dim switch.
var demoFragmentTmpl = template.Must(template.New("demo").Parse(`<div id="demo" class="setting-cell">
<div class="field-row">
<form hx-post="/api/settings/demo" hx-target="#demo" hx-swap="outerHTML">
<label class="label" for="demoToggle">Demo Mode</label>
<label class="switch" for="demoToggle">
<input id="demoToggle" name="enabled" type="checkbox" {{if .Enabled}}checked{{end}} onchange="this.form.requestSubmit()">
<span class="switch-track"><span class="switch-thumb"></span></span>
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
	ok := setDemoModeLocked(enabled)
	noteActivity()
	mutex.Unlock()
	if !ok {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `<span class="err">BUSY - STOP FIRST</span>`)
		return
	}
	renderFragment(w, demoFragmentTmpl, demoViewData())
}

// monitorFragmentTmpl is the Audio -> Monitoring setting: an on/off switch
// for the input monitor. Mirrors the Demo Mode switch.
var monitorFragmentTmpl = template.Must(template.New("monitor").Parse(`<div id="monitor" class="setting-cell">
<div class="field-row">
<form hx-post="/api/settings/monitor" hx-target="#monitor" hx-swap="outerHTML">
<label class="label" for="monitorToggle">Monitoring</label>
<label class="switch" for="monitorToggle">
<input id="monitorToggle" name="enabled" type="checkbox" {{if .Enabled}}checked{{end}} onchange="this.form.requestSubmit()">
<span class="switch-track"><span class="switch-thumb"></span></span>
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
	renderFragment(w, monitorFragmentTmpl, monitorViewData())
}

var channelCountFragmentTmpl = template.Must(template.New("channelcount").Parse(`<div id="channelcount" class="setting-cell">
<div class="field-row">
<form hx-post="/api/settings/channels" hx-swap="none" hx-sync="this:queue last" hx-status:400="target:#channels-error swap:innerHTML">
<label class="label" for="channelsInput">Channels</label>
<input id="channelsInput" class="input" type="number" name="count" min="1" max="{{.Max}}" step="1" value="{{.Count}}" onchange="this.form.requestSubmit()" oninput="document.getElementById('channels-error').textContent=''" title="Number of input channels">
<span class="hint field-hint">1–{{.Max}}</span>
</form>
</div>
<div id="channels-error"></div>
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
<div class="field-row">
<form hx-post="/api/settings/prefix" hx-target="#fileprefix" hx-swap="outerHTML" hx-status:400="target:#prefix-error">
<label class="label" for="filePrefixInput">Prefix</label>
<div class="input-group">
<input id="filePrefixInput" class="input" name="prefix" type="text" value="{{.Prefix}}" maxlength="32" placeholder="recording" pattern="[A-Za-z0-9 \-]+" title="Letters, numbers, spaces and - only (no underscores)">
<button type="submit" class="btn btn-secondary">Save</button>
</div>
<span class="hint field-hint">file_YYYYMMDD…</span>
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
	renderFragment(w, filePrefixFragmentTmpl, filePrefixView())
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
			renderFragment(w, transportFragmentTmpl, transportOptionsView())
			return
		}
		settingChanged()
		mutex.Unlock()
	}
	renderFragment(w, transportFragmentTmpl, transportOptionsView())
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
	renderFragment(w, selectFragmentTmpl, vuRangeSelect())
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
	renderFragment(w, selectFragmentTmpl, logLevelSelect())
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
	renderFragment(w, selectFragmentTmpl, peakHoldSelect())
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
	renderFragment(w, selectFragmentTmpl, sampleRateSelect())
}

// handleAPISettingsChannels applies a channel count. The number box is not
// swapped on success (204): replacing an input the operator is still
// spinning overwrote the newer value they had already entered with the
// server's echo of an older one. A refused value answers 400 with a
// message, so the field never silently disagrees with the unit.
func handleAPISettingsChannels(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.FormValue("count"))
	if err != nil || n < 1 || n > MaxChannelCount {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `<span class="err">Channels must be 1-%d</span>`, MaxChannelCount)
		return
	}
	mutex.Lock()
	// Unchanged is a no-op: Enter in the number box fires both its change
	// handler and the form submit, and a repeat save of the same value must
	// not queue a second Inferno restart or config write.
	if n != channelCount {
		channelCount = n
		// Relaunch Inferno with the new channel count if it's running (the
		// worker's restart path re-starts monitoring too), so the running
		// instance - and with it the live VU count - always matches what
		// the settings page shows.
		checkInfernoRestart()
		settingChanged()
	}
	mutex.Unlock()
	w.WriteHeader(http.StatusNoContent)
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
	renderFragment(w, selectFragmentTmpl, tagSelect())
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
		if msg := validateWifiCredentials(ssid, pass); msg != "" {
			mutex.Unlock()
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `<span class="err">%s</span>`, html.EscapeString(msg))
			return
		}
		wifiSSID = ssid
		wifiPassword = pass
	}
	wifiEnabled = enabled
	persistConfig()
	// Capture under the about-to-release lock: the globals are reread by
	// concurrent saves/imports, so passing them after Unlock races.
	ssid, pass = wifiSSID, wifiPassword
	mutex.Unlock()

	// Re-apply the AP configuration (hostapd restart)
	go applyLatestWifiConfig()

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
{{.HTMLTag}}<head><title>{{.DeviceName}} Remote</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<link rel="manifest" href="/manifest.json">
<link rel="icon" href="/icon.svg" type="image/svg+xml">
<link rel="apple-touch-icon" href="/icon.svg">
<!-- Core is always linked (reset + shared components, plus the baseline
   token set); the active theme bundle on top supplies the palette. Both
   links precede the app <style>: the app stylesheet loads after the theme
   so its equal-specificity rules (e.g. the modal backdrop's closed
   display:none) win the cascade. -->
<link rel="stylesheet" href="/static/themes/core.css?v={{.CoreVersion}}">
<link id="themecss" rel="stylesheet" href="{{.ThemeCSS}}">
<meta name="theme-color" content="#00d9ff">
<meta name="apple-mobile-web-app-capable" content="yes">
<meta name="apple-mobile-web-app-status-bar-style" content="black-translucent">
<!-- Settle off: htmx 4.0's settle step copies the outgoing element's
     attributes onto the incoming one (setting the live value too), refocuses
     it, then applies the real attributes - which a focused, now-dirty input
     ignores. A text/number field saved with Enter showed its OLD value while
     the server (and the main window) had the new one. Nothing here uses
     settle transitions. -->
<meta name="htmx-config" content='{"defaultSettleDelay":0}'>
<script src="/static/htmax.min.js"></script>
<link rel="stylesheet" href="/static/uPlot.min.css">
<script src="/static/uPlot.iife.min.js"></script>
<style>
/* The only custom properties the app declares are the meter's geometry and
   its green/yellow/red bands, which ftl-themes' .meter-fill consumes; every
   other colour comes straight from the library. Since v4 removed the ftl-
   prefix, declaring --text/--border here would shadow the theme and
   declaring --accent/--surface/--muted/--danger/--success/--warning would
   do the same - the old --glow/--panel/--dim/--rec/--idle/--orange bridge
   is deliberately gone. See TestDefaultThemeIsFTL. */
:root{--meter-low:var(--success,#0aff9d);--meter-mid:var(--warning,#ffe400);--meter-high:var(--danger,#ff2a2a)}
*{box-sizing:border-box}
body{font-family:"Consolas",monospace;background:radial-gradient(ellipse at top,var(--surface,#0a1a2e),var(--bg,#020509) 70%);background-attachment:fixed;color:var(--text);margin:0;padding:0 1.5em 4em}
h2{font-size:0.8em;letter-spacing:0.2em;text-transform:uppercase;color:var(--muted);border-bottom:1px solid var(--border);padding-bottom:0.4em;margin:0 0 var(--space-s,0.8em)}
a{color:var(--accent)}
input{font-family:inherit;background:var(--input-bg,#08192b);color:var(--text);border:1px solid var(--border);border-radius:4px;padding:0.4em}
:where(button:not(.key,.btn,.tab,.btn-close)){font-family:inherit;font-size:0.95em;padding:0.5em 1em;background:var(--surface-2,#08192b);color:var(--accent);border:1px solid var(--border);border-radius:5px;cursor:pointer;letter-spacing:0.05em}
:where(button:not(.key,.btn,.tab,.btn-close)):hover{border-color:var(--accent);box-shadow:0 0 8px var(--accent)}
:where(button:not(.key,.btn,.tab,.btn-close)):active{background:var(--surface-2,#0f2a44)}
:where(button:not(.key,.btn,.tab,.btn-close)):disabled{opacity:0.35;cursor:default;box-shadow:none}
button:focus-visible,input:focus-visible,select:focus-visible{outline:1px solid var(--accent);outline-offset:2px}
.ok{color:var(--success)}
.err{color:var(--danger)}
.rec{color:var(--danger);font-weight:bold;text-shadow:0 0 8px var(--danger)}
.idle{color:var(--success)}
table{border-collapse:collapse;width:100%;font-size:0.82em}
td,th{padding:0.3em 0.5em;border-bottom:1px solid var(--border)}
th{color:var(--muted);text-transform:uppercase;font-size:0.72em;letter-spacing:0.08em;text-align:left}

/* Panels get HUD corner brackets - the recurring "sci-fi readout" motif
   tying the three columns together. */
.panel{position:relative;background:var(--surface);border:1px solid var(--border);border-radius:10px;padding:var(--space-m,1em) var(--space-l,1.2em);box-shadow:var(--panel-shadow,0 0 20px rgba(0,180,255,0.08),inset 0 0 30px rgba(0,180,255,0.03))}
.panel::before,.panel::after{content:'';position:absolute;width:14px;height:14px;border:2px solid var(--accent);opacity:0.55}
.panel::before{top:-1px;left:-1px;border-right:none;border-bottom:none}
.panel::after{bottom:-1px;right:-1px;border-left:none;border-top:none}
.left{text-align:left}
.center{text-align:center}
.right{text-align:right}
.right table{text-align:right}
.right td:first-child{text-align:left;color:var(--muted)}

/* Header deck: [logo] [OLED] [rotary] [transport], matching the physical
   front panel's left-to-right layout. Settings/logout sit apart, top right,
   since they're not physical-panel controls.
   Every component here is sized fluidly (clamp()/vw) so as the viewport
   narrows the whole deck shrinks in BOTH width and height together on one
   row - no overflow, no horizontal scroll - instead of only dropping to a
   small size at one fixed breakpoint. */
header.deck{position:relative;display:flex;flex-direction:var(--pi-deck-dir,row);align-items:center;justify-content:center;gap:clamp(0.3em,1.2vw,1.6em);flex-wrap:nowrap;padding:clamp(0.6em,1.2vw,1.2em) clamp(0.5em,2.5vw,5.5em);border-bottom:1px solid var(--border);margin-bottom:1.5em}
.deck-logo .logo-svg{width:clamp(0px,11vw,255px)}
.oled-frame{background:#000;border:2px solid var(--border);border-radius:6px;padding:clamp(3px,0.6vw,8px);display:inline-block;box-shadow:var(--panel-shadow,0 0 25px rgba(0,180,255,0.15))}
.oled-frame img{width:clamp(170px,32vw,440px);height:auto;aspect-ratio:4/1;image-rendering:pixelated;display:block}
.encoder-row{display:flex;align-items:center;gap:clamp(0.2em,0.5vw,0.5em)}
.encoder-row button{font-size:clamp(0.7em,1.5vw,1.3em);width:clamp(1.3em,2.6vw,2.3em);padding:0.2em 0}
.encoder-row .click{border-radius:50%;width:clamp(1.3em,2.6vw,2.3em);height:clamp(1.3em,2.6vw,2.3em);padding:0}
/* Transport buttons: equal-sized icon squares, 60% of the OLED frame's
   110px rendered height (see .oled-frame img above). Both dimensions shrink
   with the viewport so the row always fits. Icons come from the theme's
   ftl-themes sprite: stroke inherits each key's color via currentColor. */
.transport-row{display:flex;gap:clamp(0.2em,0.5vw,0.6em)}
.transport-row .icon{width:clamp(14px,2.2vw,30px);height:clamp(14px,2.2vw,30px);min-width:clamp(14px,2.2vw,30px)}
.transport-row button{width:clamp(30px,4.6vw,66px);height:clamp(30px,4.6vw,66px);padding:0;display:flex;align-items:center;justify-content:center}
/* The deck keys are ftl-themes' .key (v5): .key-mute (REC, red),
   .key-sel (STOP, accent) and .key-on (PLAY, green), lit through
   aria-pressed - REC while a take records, PLAY while playing (flashing
   while paused, the panel lamp's ~2 Hz), STOP while stopped. Owner rule:
   a key always shows its colour, dim while its LED is off, and glows
   strongly when on - so the off look and the glow are set through the
   component's own tokens, mixed from each key's --key-lit. */
.transport-row .key{width:clamp(30px,4.6vw,66px);height:clamp(30px,4.6vw,66px);padding:0;font-size:inherit;cursor:pointer;
  --key-fg:color-mix(in srgb,var(--key-lit) 45%,transparent);
  --key-border:color-mix(in srgb,var(--key-lit) 32%,var(--border));
  --key-bg:color-mix(in srgb,var(--key-lit) 6%,var(--surface-2));
  --key-glow:0 0 0.9em 0.15em color-mix(in srgb,var(--key-lit) 75%,transparent),0 0 2.4em 0.3em color-mix(in srgb,var(--key-lit) 35%,transparent)}
.transport-row .key[aria-pressed="true"] svg{filter:drop-shadow(0 0 0.3em var(--key-lit-fg))}
.transport-row .key.is-flashing{animation:pi-lamp-flash 0.5s steps(1,end) infinite alternate}
@keyframes pi-lamp-flash{to{background:var(--key-bg);color:var(--key-fg);border-color:var(--key-border);box-shadow:none}}
@media (prefers-reduced-motion:reduce){.transport-row .key.is-flashing{animation:none;border-style:dashed}}
/* Text mode: the same transport keys but labelled instead of icon glyphs.
   Buttons stretch to fit and the label takes the accent colour the icon had. */
.transport-row.text .key{width:auto;min-width:clamp(2em,3.2vw,3.4em);font-size:clamp(0.55em,0.95vw,0.85em);letter-spacing:0.08em;padding:0 0.3em}

.header-actions{position:absolute;top:0.8em;right:clamp(0.5em,2vw,1.5em);display:flex;gap:0.5em}
/* .icon-btn is applied to both a <button> (Settings) and an <a> (Log out)
   - the base button{} rule above only targets <button>, so colors/border
   are repeated here rather than relied on from that selector. Icons are
   .icon strokes inheriting currentColor; the lamp colors the span. */
.icon-btn{width:clamp(1.8em,2.6vw,2.2em);height:clamp(1.8em,2.6vw,2.2em);border-radius:50%;padding:0;display:flex;align-items:center;justify-content:center;background:var(--surface-2,#08192b);color:var(--accent);border:1px solid var(--border);cursor:pointer;text-decoration:none}
.icon-btn .icon{width:clamp(14px,1.8vw,18px);height:clamp(14px,1.8vw,18px);min-width:clamp(14px,1.8vw,18px)}
.icon-btn:hover{border-color:var(--warning);color:var(--warning)}
/* Conn lamp: broadcast glyph showing the telemetry socket state - glow blue
   while the server pushes, error red while disconnected. A span, not a
   button: no pointer affordance, and no hover recolor (it must never read
   as a control). The icon stroke inherits the span's color. */
/* Server link: the broadcast glyph plus ftl-themes' .lamp - lit in the
   accent while the telemetry socket pushes, .is-error while unreachable. */
.conn{display:inline-flex;align-items:center;gap:0.4em;padding:0 0.3em;color:var(--muted);--lamp-on:var(--accent)}
.conn .icon{width:1.1em;height:1.1em;min-width:1.1em}
/* Download ALL: a small labeled action in the Recordings heading - text,
   not just an icon, so its function reads at a glance. */
.dl-all{float:right;font-size:0.7em;letter-spacing:0.08em;color:var(--accent);background:var(--surface-2,#08192b);border:1px solid var(--border);border-radius:5px;padding:0.15em 0.5em;text-decoration:none;font-weight:normal;display:inline-flex;align-items:center;gap:0.35em}
.dl-all .icon{width:1em;height:1em;min-width:1em}
.dl-all:hover{border-color:var(--accent)}

.grid{display:grid;grid-template-columns:var(--pi-columns,minmax(0,1fr));gap:var(--pi-gap,1.2em)}
.grid>*{min-width:0}
/* Layout bridge (companion to the :root color bridge above): the structural
   knobs a layout theme may turn, with the built-in geometry as fallback.
   Themes set --pi-* under their own scope to reflow the page (e.g. a wall
   panel stacking to one column); unthemed, every fallback applies and the
   layout is byte-identical. Deliberately a short list - deck internals,
   meter footer and modal sheets stay app-owned (see blue-future's
   stays-app-side section); regions themselves are shell-arranged. */

/* app shell none-case (ftl-themes#3): with no theme linked these hooks
   must generate zero boxes: display:contents dissolves the wrapper (children lay
   out against body as they always did); the decorative rail is always empty
   in this app, so it never displays. Linked themes override both. */
main.app-main{display:contents}
.app-rail:empty{display:none}

.recordings-section h2{font-size:1.1em;letter-spacing:0.05em}
/* The former Status panel, under the reel-to-reel (owner layout). */
.deck-status{margin-top:0.6em}
.deck-status table{width:100%}
/* The table is the shared .table.is-sticky (sticky head + themed rows);
   only the download cell's right alignment stays app-owned. */
.recs-dl{text-align:right}
/* One line per take: the table scrolls sideways in its own wrapper rather
   than wrapping dates onto three lines or widening the page. */
.recordings-wrap .table td,.recordings-wrap .table th{white-space:nowrap}
/* Channel names: the summary stays one line; the rename form opens below. */
.recs-chans summary{cursor:pointer;max-width:16em;overflow:hidden;text-overflow:ellipsis;color:var(--accent)}
.chan-form{display:grid;gap:0.3em;margin:0.4em 0;white-space:normal}
.chan-row{display:grid;grid-template-columns:2.2em minmax(8em,14em) auto;align-items:center;gap:0.5em;font-size:0.85em}
.chan-row small{color:var(--muted)}
.recs-note{font-size:0.75em;color:var(--muted);margin:0.6em 0 0}

/* Scrollable table wrapper for narrow viewports */
.recordings-wrap{overflow-x:auto;max-width:100%}

/* Owner layout: one column (Transport Status, then Recordings under the
   reel-to-reel) and no pane scrolls - every pane is as tall as what it
   holds and the page scrolls instead. Only a recordings table wider than
   the screen scrolls sideways, inside its own wrapper. */
.panel.center .deck-status{max-width:56rem;margin-left:auto;margin-right:auto}

/* Modals: the settings sheet and the stop-recording confirmation. The box,
   overlay and close button are the shared modal family; this app keeps
   only the open/close toggle (the overlay is always display:flex) and the
   two sheets' own sizing. */
.modal-backdrop{display:none;z-index:200}
.modal-backdrop.open{display:flex}
.modal--confirm{position:relative}
.stop-prompt{color:var(--muted);margin:var(--space-l,1.2em) 0}
.stop-actions{display:flex;gap:var(--space-s,0.8em);justify-content:flex-end}
.stop-actions button{min-width:7em;padding:0.8em 1em}
.modal--confirm .btn-close{position:absolute;top:0.9em;right:0.9em}

/* Settings modal: a sheet with a fixed header bar and a scrollable body, so
   a long setting list never runs past the viewport edge. Setting families are
   grouped under section titles and laid out on a responsive 2-column grid. */
.modal--settings{width:min(680px,94vw);max-height:88vh;display:flex;flex-direction:column;padding:0}
.modal--settings .modal-header{padding:var(--space-m,1.1em) var(--space-l,1.4em);margin:0;border-bottom:1px solid var(--border)}
/* Settings sheet: the library's vertical tab rail beside scrolling panes.
   The rail is fixed-width, the active pane fills the rest; panes are plain
   group grids, shown one at a time (see selectSettingsTab). */
.modal--settings .modal-body{display:flex;gap:var(--space-s,0.75rem);min-height:0;overflow:hidden;padding:var(--space-s,0.9em) var(--space-l,1.4em) var(--space-l,1.4em)}
.settings-tabs{flex:none;min-width:9.5em;overflow-y:auto}
.settings-panes{flex:1;min-width:0;overflow-y:auto}
.settings-pane{display:none}
.settings-pane.is-active{display:block}
/* Narrow screens: the rail becomes a horizontal scroll row above the pane. */
@media (max-width:800px){
  .modal--settings .modal-body{flex-direction:column;overflow-y:auto}
  .settings-tabs[aria-orientation="vertical"]{flex-direction:row;flex-wrap:nowrap;overflow-x:auto;overflow-y:hidden;min-width:0;border-inline-end:0;border-bottom:1px solid var(--border);padding-inline-end:0}
  .settings-tabs[aria-orientation="vertical"] > .tab{flex:none;border-bottom-color:transparent}
  .settings-tabs[aria-orientation="vertical"] > .tab.is-active{border-bottom:2px solid var(--tab-underline-active,var(--accent));border-inline-end-width:0}
  .settings-panes{overflow:visible}
}
.settings-group{display:grid;grid-template-columns:repeat(auto-fit,minmax(240px,1fr));gap:var(--space-s,0.7em);padding:1em 0 0.4em}
/* Rows/labels/hints/selects/inputs are the shared field-row family; the
   only app-owned pieces are the pieces the library doesn't know: the form
   layout inside a row and the paired SSID/password fields. */
.field-row form{display:flex;align-items:center;gap:var(--space-s,0.8em);flex:1;width:100%}
.field-row form .input-group{flex:1 1 auto;min-width:0;width:auto}
.field-row .select{min-width:9em}
.field-row .input[type="number"]{flex:none;width:6em}
#config-msg:empty{display:none}
.field-row--pair{gap:0.8em 1.2em;flex-wrap:wrap}
.field-row--pair .field{flex:1 1 42%;display:flex;align-items:center;gap:0.6em;min-width:0}
.field-row--pair .field label{flex:none;width:auto;max-width:8em}
.field-row--pair .field input{flex:1;min-width:0}
/* The control itself is the shared .switch. Two small overrides let
   this app's status readout ride inside the same <label>: the switch
   reverts to content width and the track regains a relative box, so the
   readout sits beside the pill instead of under the absolute track. */
.switch:has(.switch-readout){width:auto;gap:0.7em}
.switch:has(.switch-readout) .switch-track{position:relative;inset:auto;width:calc(2.6em*var(--density,1));height:calc(1.4em*var(--density,1))}
.switch-readout{font-size:0.82em;letter-spacing:0.08em;position:relative;min-width:5em;text-align:center;color:var(--muted,#2c4a66)}
.switch-readout::after{content:attr(data-off)}
.switch input:checked ~ .switch-readout{color:var(--accent);text-shadow:0 0 6px rgba(0,217,255,0.6)}
.switch input:checked ~ .switch-readout::after{content:attr(data-on)}

/* Continuous OLED brightness slider: a wide Sci-Fi range control with a
   glowing track and thumb, sized so a single row holds label + live % readout
   + slider (the readout is updated inline by the fragment's oninput). */
/* The brightness slider is the shared .slider; only its flex sizing
   (one row holds label + live % readout + slider) stays app-owned. */
.slider{flex:1 1 auto;min-width:0}

/* Transport deck: ftl-themes' .media-deck (v5), whose reel-to-reel was
   adapted from this very deck. It draws the plate, reels, tape, head and
   transport lamp from theme tokens and owns the motion: the app only puts
   is-recording / is-playing / is-paused on #deck (applyMeter). Reduced
   motion (OS or data-motion) stops every animation. What the component
   does not have yet stays here: the 7-segment counter, the corner
   brackets and the INFERNO-LINK lamp (asked for upstream: ftl-themes#66). */
.transport-deck.media-deck{width:100%;margin:0 0 0.6em;padding:0;background:none;border:0;box-shadow:none}
.transport-deck .media-deck-visual{display:block;width:100%;max-width:760px;height:auto;margin:0 auto}
.media-deck .deck-corner{fill:none;stroke:var(--accent);stroke-width:2;opacity:0.45}
.media-deck.is-recording .device-lamp{filter:drop-shadow(0 0 6px var(--danger))}
.media-deck.is-playing .device-lamp{filter:drop-shadow(0 0 4px var(--success))}
.media-deck #linkLamp{fill:var(--deck-well,var(--surface))}
.media-deck #linkLamp.on{fill:var(--accent);filter:drop-shadow(0 0 3px var(--accent))}
/* Both reels turn the same way, as on a real deck: the theme spins the
   take-up reel clockwise and reverses only the supply reel, so reverse the
   take-up (right) reel too. Only the direction changes; Reduce Motion
   still stops the animation in the theme. */
.media-deck:is(.is-playing,.is-recording) #reelR .reel-spin{animation-direction:reverse}
/* The lit 7-segment time display. Every segment is an SVG line (see the
   buildSeg7 JS); the dim .s7 shows all segments faintly so the display
   reads as a proper 7-segment counter even for unlit digits. The whole display
   is skewed to the right for an italic, forward-leaning readout. */
#seg7{font-style:italic}
#seg7 .s7{stroke:rgba(0,180,255,0.16);stroke-width:2.5;stroke-linecap:round}
#seg7 .s7.on{stroke:var(--accent);filter:drop-shadow(0 0 4px rgba(0,217,255,0.75))}
#seg7 .s7-dot{fill:rgba(0,180,255,0.16)}
#seg7 .s7-dot.on{fill:var(--accent);filter:drop-shadow(0 0 4px rgba(0,217,255,0.75))}

/* Pinned meter footer: always visible at the bottom of the viewport so the
   VU levels stay on screen while you operate the transport, with a slim
   header bar that collapses/expands the meter bank on demand. */
/* Level meters: a full-width band right under the header, as tall as the
   header (--deck-h is measured from it, see the script), collapsible to
   its bar with a large toggle. Owner layout 2026-10-05. */
.meter-band{display:flex;align-items:stretch;gap:var(--space-m,1em);min-height:var(--deck-h,150px);margin:0 0 1.2em;background:rgba(3,8,15,0.6);border:1px solid var(--border);border-radius:10px;padding:0.4em 1em;box-sizing:border-box}
.meter-bar{display:flex;flex-direction:column;align-items:flex-start;justify-content:center;gap:0.5em;flex:none;min-width:7.5em}
.meter-title{font-size:0.7em;letter-spacing:0.25em;color:var(--muted);text-transform:uppercase}
.meter-caret{width:2.9em;height:2.9em;border-radius:50%}
.meter-caret .icon{width:1.6em;height:1.6em;min-width:1.6em}
.meter-body{flex:1;min-width:0;display:flex;align-items:center;overflow:hidden;transition:opacity 0.25s ease}
.meter-body{justify-content:center}

/* Collapsed, the band disappears entirely: the header's large Meters
   button brings it back. */
.meter-band.collapsed{display:none}
/* Collapsed meters: the band's own label + caret, at the header's left
   edge, so opening matches the collapse control. */
.meter-open[hidden]{display:none}
/* In the header's bottom-right corner, clear of the transport keys. */
header.deck .meter-open{position:absolute;left:clamp(0.5em,2vw,1.5em);top:50%;transform:translateY(-50%);min-width:0}
@media (max-width:800px){header.deck .meter-open{position:static;transform:none;flex-direction:row;align-items:center}}
/* Footer: disk space and record time, plus the System pane toggle. */
.app-footer{position:fixed;left:0;right:0;bottom:0;z-index:150;display:flex;align-items:center;gap:var(--space-m,1em);padding:0.45em 1.5em;background:rgba(3,8,15,0.94);border-top:1px solid var(--border);box-shadow:0 -8px 30px rgba(0,180,255,0.10);font-size:0.85em;color:var(--muted)}
.footer-disk{display:inline-flex;align-items:center;gap:0.45em;flex-wrap:wrap}
.footer-disk .progress{width:6em}
.sys-toggle{margin-left:auto;display:inline-flex;align-items:center;gap:0.5em;padding:0.35em 1em;font-size:0.95em}
.sys-toggle .icon{width:1.3em;height:1.3em;min-width:1.3em}
.sys-toggle svg{transform:rotate(180deg);transition:transform 0.2s ease}
.sys-toggle[aria-expanded="true"] svg{transform:none}
/* System: a full-width pane that pops up above the footer; graphs flow
   in a grid that wraps onto more rows rather than cramping. */
.sys-pane{position:fixed;left:0;right:0;bottom:var(--footer-h,2.6em);z-index:149;max-height:72vh;overflow-y:auto;padding:0.6em 1.5em 1em;background:rgba(3,8,15,0.96);border-top:1px solid var(--border);box-shadow:0 -10px 40px rgba(0,180,255,0.14)}
.sys-pane[hidden]{display:none}
.sys-pane h2{margin:0.2em 0 0.4em;font-size:0.8em;letter-spacing:0.25em;text-transform:uppercase;color:var(--muted)}
/* The meter bank is ftl-themes' console (v5): banks of 8 .strip channels
   (owner layout 2026-10-06: more rows instead of scrolling, broken only
   between banks of 8 so no display width wastes space), each bank a
   .mixer led by its own .scale.is-meter legend; the banks wrap.
   A strip is the channel number, a segmented .meter.meter-v in a
   .strip-fader, then a .scribble with the channel's name. The app sets
   only sizes: strips barely wider than the meter (no gaps between
   meters), and a fader length that keeps one row as tall as the header.
   Levels, peaks and the dB ticks are placed with the same taper (vuPct). */
.meter-rows{display:flex;flex-wrap:wrap;justify-content:center;align-items:flex-start;gap:4px;max-width:100%}
.meter-group{--strip-width:2.6rem;--mixer-gap:2px;--meter-thickness:1rem;--fader-length:calc(var(--deck-h,150px) - 5.4rem);flex:none;max-width:100%;overflow-x:visible}
.meter-group .strip{padding:0.25rem 0.1rem;gap:0.2rem}
.meter-group .strip-fader{margin-top:0}
.meter-group .strip-num{font-size:0.62rem;line-height:1;text-align:center;color:var(--muted);font-variant-numeric:tabular-nums}
.meter-group .strip-legend{--strip-width:2.3rem}
.meter-group .scribble{text-align:center;padding:0.12rem 0.1rem}
.meter-group .scribble-name{font-size:0.56rem;letter-spacing:0;white-space:nowrap;text-overflow:ellipsis;cursor:text}
/* Renaming: the input pops out wider than the strip, over its neighbours. */
.meter-group .scribble{position:relative}
.meter-group .chan-label-input{position:absolute;left:50%;bottom:0;transform:translateX(-50%);z-index:5;width:9rem;font-size:0.75rem;padding:0.15em 0.35em;text-transform:none}

/* Telemetry panel: collapsible system stats with per-core mini graphs */
.sys-graphs{display:grid;grid-template-columns:repeat(auto-fill,minmax(min(100%,22rem),1fr));gap:0.6em 1.4em}
.sys-graph{min-width:0}
.sys-graphs h3{font-size:.68em;letter-spacing:.18em;text-transform:uppercase;color:var(--muted);margin:.4em 0 .2em}
.sys-graphs .uplot{width:100%}
.sys-graphs .u-legend{font-size:.68em;color:var(--muted);background:transparent;border:none;padding-left:0}
.sys-graphs .u-legend th{font-weight:normal}
.sys-graphs .u-legend .u-value{color:var(--text)}
.sys-wait{font-size:.72em;color:var(--muted)}

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
  body{padding:0 0.4em 4em}
  header.deck{flex-wrap:wrap}
  /* In-flow on narrow screens: the absolute corner position overlays the
     centered logo once the deck wraps. */
  .header-actions{position:static;margin-left:auto}
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
  .meter-band{height:auto;flex-direction:column}
  .meter-group{--fader-length:7rem}
  .meter-bar{flex-direction:row;align-items:center}
}

@media (prefers-reduced-motion: reduce) {
  .meter-body{animation:none;transition:none}
  .switch-track,.switch-thumb{transition:none}
}

/* WiFi settings panel */
.wifi-qr-row{display:flex;align-items:center;gap:var(--space-m,1em);flex-wrap:wrap}
.wifi-qr-info p{margin:0.2em 0;font-size:0.85em}
.wifi-qr-img img{width:180px;height:180px;image-rendering:pixelated;border:1px solid var(--border);border-radius:4px}
/* The theme owns the page background; the built-in gradient underneath is
   only a fallback while the bundle loads. */
html[data-theme] body{background:transparent}
</style>
</head>
<body class="app">

<!-- app shell (ftl-themes#3): dual-classed regions so layout themes can
     arrange the page while the built-in look (no theme linked) renders
     exactly as before - the app's own selectors keep matching, and the
     none-case rules below neutralize the new hooks. -->
<header class="deck app-bar">
  <div class="deck-logo">{{.Logo}}</div>
  <div class="oled-frame"><img id="oled" src="/api/display.png" alt="OLED display" onerror="if(!this.dataset.r){this.dataset.r=1;location.reload()}"></div>
  <div class="encoder-row">
    <button hx-post="/api/input/encoder/left" aria-label="Encoder left" title="Encoder left"><svg class="icon" aria-hidden="true"><use href="{{.IconSprite}}#icon-chevron-left"/></svg></button>
    <button class="click" hx-post="/api/input/encoder/click" aria-label="Encoder click" title="Encoder click">&#9679;</button>
    <button hx-post="/api/input/encoder/right" aria-label="Encoder right" title="Encoder right"><svg class="icon" aria-hidden="true"><use href="{{.IconSprite}}#icon-chevron-right"/></svg></button>
  </div>
  <div class="transport-row" id="transportRow"></div>
  <div class="meter-open meter-bar" id="meterOpen" hidden>
    <span class="meter-title">Level meters</span>
    <button class="icon-btn meter-caret" id="meterOpenBtn" type="button" title="Show the level meters" aria-label="Show the level meters" aria-controls="meterFooter" aria-expanded="false">
      <svg class="icon" aria-hidden="true" style="transform:rotate(-90deg)"><use href="{{.IconSprite}}#icon-chevron-down"/></svg>
    </button>
  </div>
  <div class="header-actions">
    <span class="conn" id="connLamp" role="status" title="Server disconnected" aria-label="Server disconnected">
      <svg class="icon" aria-hidden="true"><use href="{{.IconSprite}}#icon-broadcast"/></svg><span class="lamp is-error" aria-hidden="true"></span>
    </span>
    <button class="icon-btn btn btn-icon" id="settingsBtn" type="button" title="Settings">
      <svg class="icon" aria-hidden="true"><use href="{{.IconSprite}}#icon-settings"/></svg>
    </button>
    <form action="/logout" method="POST" style="display:inline;margin:0">
      <button class="icon-btn btn btn-icon" title="Log out" aria-label="Log out">
      <svg class="icon" aria-hidden="true"><use href="{{.IconSprite}}#icon-logout"/></svg>
      </button>
    </form>
    <form action="/api/settings/rotate-token" method="POST" style="display:inline;margin:0" onsubmit="return confirm('Rotate the access token? Every session (including this one) is logged out.')">
      <button class="icon-btn btn btn-icon" title="Rotate access token" aria-label="Rotate access token">
      <svg class="icon" aria-hidden="true"><use href="{{.IconSprite}}#icon-refresh"/></svg>
      </button>
    </form>
  </div>
</header>

<aside class="app-rail" aria-hidden="true"></aside>


<main class="app-main">
<section class="meter-band" id="meterFooter" aria-label="Level meters">
  <div class="meter-bar">
    <span class="meter-title">Level meters</span>
    <span class="meter-badge badge badge-accent" id="meterBadge">--</span>
    <button class="icon-btn meter-caret" id="meterToggle" type="button" title="Collapse/expand meters" aria-label="Collapse or expand level meters" aria-controls="meterBody" aria-expanded="true">
      <svg class="icon" id="meterCaretSvg" aria-hidden="true"><use href="{{.IconSprite}}#icon-chevron-down"/></svg>
    </button>
  </div>
  <div class="meter-body" id="meterBody">
    <div class="meter-rows" id="chMeters" role="group" aria-label="Input levels"></div>
  </div>
</section>

<div class="grid">
  <div class="panel center panel">
    <h2>Transport Status</h2>
    <section class="transport-deck media-deck" id="deck" aria-label="Tape transport">
      <svg class="media-deck-visual r2r" viewBox="0 0 820 265" preserveAspectRatio="xMidYMid meet" role="img" aria-labelledby="transportTitle transportDesc">
        <title id="transportTitle">Reel-to-reel transport</title>
        <desc id="transportDesc">Two tape reels connected by an angled tape path and a read/write head time display.</desc>
        <defs>
          <linearGradient id="deckBg" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stop-color="var(--deck-face,#132947)"/>
            <stop offset="55%" stop-color="var(--deck-well,#0d1c31)"/>
            <stop offset="100%" stop-color="var(--bg,#0a1526)"/>
          </linearGradient>
          <linearGradient id="headFace" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stop-color="var(--deck-face,#142f4d)"/>
            <stop offset="100%" stop-color="var(--deck-well,#0a1a30)"/>
          </linearGradient>
        </defs>

        <rect class="plate" x="2" y="2" width="816" height="261" rx="10" fill="url(#deckBg)"/>
        <rect class="plate-bezel" x="2" y="2" width="816" height="261" rx="10"/>

        <g class="deck-grid">
          <path d="M40 26 H780 M40 248 H780"/>
          <path d="M90 26 V248 M730 26 V248" opacity="0.5"/>
        </g>

        <circle class="deck-screw" cx="18" cy="18" r="4"/>
        <circle class="deck-screw" cx="802" cy="18" r="4"/>
        <circle class="deck-screw" cx="18" cy="247" r="4"/>
        <circle class="deck-screw" cx="802" cy="247" r="4"/>

        <path class="tape-shadow" d="M211 161 L330 196 L490 196 L609 161"/>
        <path class="tape is-moving" id="tapePath" d="M211 159 L330 194 L490 194 L609 159"/>

        <g class="reel-g reel-supply" id="reelL" transform="translate(170,105) scale(1.25) translate(-170,-105)">
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

        <text class="deck-text" x="410" y="122" text-anchor="middle">PI9696</text>

        <circle class="device-guide" cx="221" cy="162" r="7"/>
        <circle class="device-guide" cx="599" cy="162" r="7"/>
        <circle class="device-guide" cx="330" cy="194" r="7"/>
        <circle class="device-guide" cx="490" cy="194" r="7"/>

        <g class="head">
          <polygon class="head-plate" points="272,252 272,198 288,184 532,184 548,198 548,252" fill="url(#headFace)"/>
          <path class="head-edge" d="M284 198 V246 M536 198 V246"/>
          <path class="head-gap" d="M402 190 L418 190"/>
          <rect class="head-window" x="300" y="198" width="220" height="48" rx="3"/>
          <path class="head-grid" d="M304 210 H516 M304 222 H516 M304 234 H516 M304 246 H516 M324 198 V246 M348 198 V246 M372 198 V246 M396 198 V246 M420 198 V246 M444 198 V246 M468 198 V246 M492 198 V246"/>
          <g id="seg7" transform="translate(312,202) skewX(-10) scale(2.12)"></g>
        </g>

        <text class="deck-text" x="146" y="24">SUPPLY</text>
        <text class="deck-text" x="614" y="24">TAKE-UP</text>

        <g class="hud">
          <path class="plate-bezel" d="M40 249 H780"/>
          <circle class="device-lamp" id="sysLamp" cx="54" cy="255" r="5"/>
          <text class="deck-text" x="66" y="258">SYS</text>
          <text class="deck-text" x="352" y="258">TRANSPORT</text>
          <text class="deck-text" x="697" y="258" text-anchor="end">INFERNO-LINK</text>
          <circle id="linkLamp" cx="706" cy="255" r="2.5"/>
        </g>

        <path class="deck-corner" d="M14 30 V14 H30"/>
        <path class="deck-corner" d="M806 14 H790 V30"/>
        <path class="deck-corner" d="M14 235 V251 H30"/>
        <path class="deck-corner" d="M806 251 V235 H790"/>
      </svg>
    </section>
    <div id="teleSock" hx-ext="ws" hx-ws:connect="/ws/telemetry" hx-target="#diskInfo" hx-swap="innerHTML" hidden></div>
    <div id="config" class="deck-status" hx-get="/api/config" hx-trigger="load" hx-swap="innerHTML">Loading...</div>
  </div>


<div class="panel recordings-section">
  <h2>Recordings <a class="dl-all" href="/download-all" title="Download every recording as one ZIP archive (with a manifest.txt listing each file)"><svg class="icon" aria-hidden="true"><use href="{{.IconSprite}}#icon-download"/></svg> Download ALL (.zip)</a></h2>
  <div id="recordings" class="scroll" hx-get="/api/recordings" hx-trigger="load" hx-swap="innerHTML">Loading...</div>
</div>
</div>
</main>

<section class="sys-pane" id="sysPane" aria-label="System" hidden>
  <h2>System</h2>
    <div class="sys-graphs">
      <div class="sys-graph"><h3>CPU %</h3><div id="cpuChart"><span class="sys-wait">collecting&hellip;</span></div></div>
      <div class="sys-graph"><h3 title="pi9696's own CPU, % of one core, by subsystem (thread names; see threadcpu.go)">App CPU by subsystem</h3><div id="subChart"><span class="sys-wait">collecting&hellip;</span></div></div>
      <div class="sys-graph"><h3>RAM MB</h3><div id="ramChart"><span class="sys-wait">collecting&hellip;</span></div></div>
      <div class="sys-graph"><h3>Temp &deg;C</h3><div id="tempChart"><span class="sys-wait">collecting&hellip;</span></div></div>
      <div class="sys-graph"><h3>Disk free GB</h3><div id="diskChart"><span class="sys-wait">collecting&hellip;</span></div></div>
      <pre id="teleHist" hidden></pre>
    </div>
</section>

<footer class="app-footer app-status" id="appFooter">
  <span id="diskInfo" class="footer-disk">Disk /rec &hellip;</span>
  <button class="sys-toggle btn btn-secondary" id="sysToggle" type="button" aria-controls="sysPane" aria-expanded="false" title="Show or hide the System graphs">
    <svg class="icon" aria-hidden="true"><use href="{{.IconSprite}}#icon-chevron-down"/></svg><span>System</span>
  </button>
</footer>

<div class="modal-backdrop modal-overlay" id="settingsModal">
  <div class="modal modal-lg modal--settings">
    <div class="modal-header">
      <h2>Unit Settings</h2>
      <button class="btn-close" id="settingsClose" type="button" aria-label="Close settings"></button>
    </div>
    <div class="modal-body">
      <div class="tabs settings-tabs" role="tablist" aria-orientation="vertical" aria-label="Settings groups">
        <button type="button" class="tab is-active" role="tab" aria-selected="true" data-pane="pane-device" id="tab-device">Device</button>
        <button type="button" class="tab" role="tab" aria-selected="false" data-pane="pane-audio" id="tab-audio">Audio</button>
        <button type="button" class="tab" role="tab" aria-selected="false" data-pane="pane-metering" id="tab-metering">Metering</button>
        <button type="button" class="tab" role="tab" aria-selected="false" data-pane="pane-metadata" id="tab-metadata">Metadata</button>
        <button type="button" class="tab" role="tab" aria-selected="false" data-pane="pane-transport" id="tab-transport">Transport</button>
        <button type="button" class="tab" role="tab" aria-selected="false" data-pane="pane-display" id="tab-display">Display</button>
        <button type="button" class="tab" role="tab" aria-selected="false" data-pane="pane-demo" id="tab-demo">Demo</button>
        <button type="button" class="tab" role="tab" aria-selected="false" data-pane="pane-logging" id="tab-logging">Logging</button>
        <button type="button" class="tab" role="tab" aria-selected="false" data-pane="pane-config" id="tab-config">Config</button>
        <button type="button" class="tab" role="tab" aria-selected="false" data-pane="pane-network" id="tab-network">Network</button>
      </div>
      {{template "settingsPanes" .}}
    </div>
  </div>
</div>

<div class="modal-backdrop modal-overlay" id="stopModal">
  <div class="modal modal--confirm">
    <button class="btn-close modal-close" id="stopModalClose" type="button" aria-label="Close"></button>
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
// Re-render the panes from the unit on every open (see handleSettingsPanes),
// keeping the selected tab. htmx swaps them so fragment scripts run and the
// new forms are wired.
settingsBtn.addEventListener('click', function() {
  settingsModal.classList.add('open');
  var cur = document.querySelector('.settings-tabs .tab.is-active');
  var pane = cur ? cur.dataset.pane : 'pane-device';
  htmx.ajax('GET', '/api/settings/panes', { target: '#settingsPanes', swap: 'outerHTML' })
    .then(function() { selectSettingsTab(pane, false); });
});
document.getElementById('settingsClose').addEventListener('click', function() { settingsModal.classList.remove('open'); });
settingsModal.addEventListener('click', function(e) { if (e.target === settingsModal) settingsModal.classList.remove('open'); });

// Settings sheet tabs: the vertical .tabs rail switches the group pane
// shown beside it (one group at a time instead of a ten-group scroll).
// Fragments keep their ids, so htmx posts re-render into hidden panes fine.
// The last open tab persists per browser like the meter collapse below.
var settingsTabs = Array.prototype.slice.call(document.querySelectorAll('.settings-tabs .tab'));
function selectSettingsTab(name, save) {
  if (!document.getElementById(name)) name = 'pane-device';
  settingsTabs.forEach(function(t) {
    var on = t.dataset.pane === name;
    t.classList.toggle('is-active', on);
    t.setAttribute('aria-selected', on ? 'true' : 'false');
    var pane = document.getElementById(t.dataset.pane);
    if (pane) pane.classList.toggle('is-active', on);
  });
  if (save !== false) { try { localStorage.setItem('pi9696_settingsTab', name); } catch (e) {} }
}
settingsTabs.forEach(function(t) { t.addEventListener('click', function() { selectSettingsTab(t.dataset.pane); }); });
try { selectSettingsTab(localStorage.getItem('pi9696_settingsTab') || 'pane-device', false); }
catch (e) { selectSettingsTab('pane-device', false); }

// Transport buttons are drawn by JS so they can switch between ICON and TEXT
// mode (Settings -> Transport Buttons) and, for the PLAY key, between a play
// triangle and a pause glyph while a track runs. ICON_MODE is seeded from the
// server (persisted setting); transportState is kept in sync by applyMeter.
var ICON_MODE = {{.TransportIcon}};
var transportState = { playing: false, paused: false, rec: false };
// SPRITE is the active theme's icon sprite (server-rendered, rewritten by
// the theme-swap OOB script); icons inherit currentColor, so the transport
// row's per-key colors apply to the stroke with no fill overrides.
var SPRITE = {{.IconSprite}};
function iconGlyph(name) { return '<svg class="icon" aria-hidden="true"><use href="' + SPRITE + '#' + name + '"/></svg>'; }
// transportKey renders one deck key as ftl-themes' .key (a backlit
// console key): lit via aria-pressed, which also tells assistive tech the
// lamp state. post is the hx-post target, or '' for the Stop key (handled
// by the stop-confirm listener via data-stop).
function transportKey(cls, post, title, label, lit) {
  return '<button class="key ' + cls + '"' + (post ? ' hx-post="' + post + '"' : ' data-stop') +
    ' title="' + title + '" aria-pressed="' + (lit ? 'true' : 'false') + '">' + label + '</button>';
}
function renderTransportRow() {
  var row = document.getElementById('transportRow');
  // The key shows its next action: pause while playing, play otherwise
  // (paused: resume). Its lamp shows the state (see .transport-row CSS).
  var pause = transportState.playing && !transportState.paused;
  var title = transportState.paused ? 'Resume' : (transportState.playing ? 'Pause' : 'Play');
  var stopped = !transportState.rec && !transportState.playing && !transportState.paused;
  var playCls = 'key-on play' + (pause ? ' pause' : '') + (transportState.paused ? ' is-flashing' : '');
  var html = '';
  if (ICON_MODE) {
    html += transportKey('key-mute record', '/api/input/button/record', 'Record', iconGlyph('icon-player-record'), transportState.rec);
    html += transportKey('key-sel stop', '', 'Stop', iconGlyph('icon-player-stop'), stopped);
    html += transportKey(playCls, '/api/input/button/play', title, iconGlyph(pause ? 'icon-player-pause' : 'icon-player-play'), transportState.playing || transportState.paused);
  } else {
    html += transportKey('key-mute record', '/api/input/button/record', 'Record', 'REC', transportState.rec);
    html += transportKey('key-sel stop', '', 'Stop', 'STOP', stopped);
    html += transportKey(playCls, '/api/input/button/play', title, pause ? 'II' : '>', transportState.playing || transportState.paused);
  }
  row.className = 'transport-row transport' + (ICON_MODE ? '' : ' text') + (transportState.rec ? ' is-rec' : '') +
    (transportState.paused ? ' is-pause' : (transportState.playing ? ' is-play' : '')) + (stopped ? ' is-stop' : '');
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
// transport key first opens a confirmation modal instead of tearing the
// recording down immediately. Desktop (non-touch) keeps the one-tap
// behaviour. (The status panel's own Stop button is gone: owner layout.)
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
document.addEventListener('click', function(e) {
  var stopBtn = e.target.closest ? e.target.closest('[data-stop]') : null;
  if (stopBtn) {
    e.preventDefault();
    e.stopPropagation();
    confirmStop(function(){ fetch('/api/input/button/stop', { method: 'POST' }); });
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
// changes (including on first load). Each tick is placed with vuPct, the
// same taper the bars use, so a tick sits exactly where that level's fill
// reaches. (They used to be placed linearly in dB, so a -12 dBFS peak drew
// level with the "-20" region of the scale and a -15 dBFS RMS with "-27".)
var DB_TICKS = [0, -6, -12, -18, -24, -36, -48];
var scaleFloor = null;
// rebuildDbScale fills every row's legend (.scale.is-meter); ensureChannels
// resets scaleFloor when it builds new, empty rows.
function rebuildDbScale(floor) {
  if (floor === scaleFloor) return;
  scaleFloor = floor;
  document.querySelectorAll('#chMeters .scale.is-meter').forEach(function(scale) {
    scale.innerHTML = '';
    DB_TICKS.filter(function(db) { return db > floor + 6; }).concat([floor]).forEach(function(db) {
      var span = document.createElement('span');
      span.style.setProperty('--at', vuPct(db) / 100); // .scale.is-meter places marks at --at (0-1)
      if (db === 0) span.className = 'is-unity';
      span.textContent = Math.round(db);
      scale.appendChild(span);
    });
  });
  // Position the green->yellow and yellow->red meter bands at the design's
  // absolute thresholds (-18 / -6 dBFS) mapped through the current floor.
  // The shared .meter reads these as --meter-warn-at/-peak-at; the
  // gradient spans the track by default (fixed upstream in ftl-themes#43).
  var root = document.documentElement;
  root.style.setProperty('--meter-warn-at', vuPct(-18) + '%');
  root.style.setProperty('--meter-peak-at', vuPct(-6) + '%');
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
  var collapsed = false;
  try { collapsed = localStorage.getItem('pi9696_meterCollapsed') === '1'; } catch (e) {}
  var meterOpen = document.getElementById('meterOpen');
  var meterOpenBtn = document.getElementById('meterOpenBtn');
  function applyCollapse() {
    meterFooter.classList.toggle('collapsed', collapsed);
    if (meterOpen) meterOpen.hidden = !collapsed;
    if (meterOpenBtn) meterOpenBtn.setAttribute('aria-expanded', !collapsed);
    meterCaret.style.transform = collapsed ? 'rotate(-90deg)' : '';
    meterToggle.setAttribute('aria-expanded', !collapsed);
  }
  applyCollapse();
  function toggleMeters() {
    collapsed = !collapsed;
    try { localStorage.setItem('pi9696_meterCollapsed', collapsed ? '1' : '0'); } catch (e) {}
    applyCollapse();
  }
  meterToggle.addEventListener('click', toggleMeters);
  if (meterOpenBtn) meterOpenBtn.addEventListener('click', toggleMeters);
}

// The meter band is as tall as the header (owner layout): measure the
// header and the footer and expose them as --deck-h / --footer-h.
(function() {
  var deck = document.querySelector('header.deck');
  var foot = document.getElementById('appFooter');
  function measure() {
    var root = document.documentElement.style;
    if (deck) root.setProperty('--deck-h', deck.offsetHeight + 'px');
    if (foot) root.setProperty('--footer-h', foot.offsetHeight + 'px');
  }
  measure();
  if (window.ResizeObserver) {
    var ro = new ResizeObserver(measure);
    if (deck) ro.observe(deck);
    if (foot) ro.observe(foot);
  } else {
    window.addEventListener('resize', measure);
  }
})();

// System pane: pops up above the footer. Hidden charts measure 0 wide, so
// opening it re-runs the charts' resize.
(function() {
  var btn = document.getElementById('sysToggle');
  var pane = document.getElementById('sysPane');
  if (!btn || !pane) return;
  var open = false;
  try { open = localStorage.getItem('pi9696_sysOpen') === '1'; } catch (e) {}
  function apply() {
    pane.hidden = !open;
    btn.setAttribute('aria-expanded', open);
    if (open) window.dispatchEvent(new Event('resize'));
  }
  apply();
  btn.addEventListener('click', function() {
    open = !open;
    try { localStorage.setItem('pi9696_sysOpen', open ? '1' : '0'); } catch (e) {}
    apply();
  });
})();

// Update meter badge (stereo/dual-mono indicator)
var meterBadge = document.getElementById('meterBadge');

var chMeters = document.getElementById('chMeters');
var chCount = -1, chNames = '';
// ensureChannels builds the meter banks: up to METERS_PER_BANK console
// strips per bank (ftl-themes .strip: channel number, segmented .meter.meter-v,
// .scribble with the name), each bank led by its own legend strip. Rebuilt
// only when the count or the names change.
var METERS_PER_BANK = 8;
var LEGEND_STRIP = '<section class="strip strip-legend" aria-hidden="true"><span class="strip-num">&nbsp;</span>' +
  '<div class="strip-fader"><div class="scale is-meter"></div></div><div class="scribble"><span class="scribble-name">dBFS</span></div></section>';
function ensureChannels(n, names) {
  var key = (names || []).join('\u0000');
  if (n === chCount && key === chNames) return;
  chMeters.innerHTML = '';
  var row = null;
  for (var i = 1; i <= n; i++) {
    if ((i - 1) % METERS_PER_BANK === 0) {
      row = document.createElement('div');
      row.className = 'mixer meter-group';
      row.innerHTML = LEGEND_STRIP;
      chMeters.appendChild(row);
    }
    var name = (names && names[i - 1]) || ('RX ' + i);
    var strip = document.createElement('section');
    strip.className = 'strip';
    strip.innerHTML = '<span class="strip-num"></span>' +
      '<div class="strip-fader"><div class="meter meter-v is-segmented" role="meter" aria-valuemin="' + FLOOR + '" aria-valuemax="0" aria-valuenow="' + FLOOR + '"><div class="meter-fill" data-i="' + i + '"></div><div class="meter-peak"></div></div></div>' +
      '<div class="scribble"><span class="scribble-name"></span></div>';
    strip.querySelector('.meter').setAttribute('aria-label', 'Channel ' + i + ' ' + name + ' level, dBFS');
    strip.querySelector('.strip-num').textContent = i;
    var nameEl = strip.querySelector('.scribble-name');
    nameEl.textContent = name;
    nameEl.title = name + ' - double-click to rename';
    nameEl.tabIndex = 0;
    nameEl.dataset.ch = i;
    row.appendChild(strip);
  }
  chCount = n;
  chNames = key;
  scaleFloor = null; // the new rows' legends are empty
}

// Double-click a channel's name to relabel it (Enter or F2 from the
// keyboard): Enter saves, Escape cancels, an empty name goes back to
// inferno's name. Owner rule: this is the unit's own label (POST
// /api/channels/label) - inferno's channel names are never changed.
function editChannelLabel(nameEl) {
  if (!nameEl || nameEl.parentNode.querySelector('input')) return;
  var ch = nameEl.dataset.ch;
  var old = nameEl.textContent;
  var input = document.createElement('input');
  input.className = 'input chan-label-input';
  input.value = old;
  input.maxLength = 32;
  input.setAttribute('aria-label', 'Name for channel ' + ch + ' (empty: the inferno name)');
  // On the .scribble, not in the name: the name clips its overflow.
  nameEl.style.visibility = 'hidden';
  nameEl.parentNode.appendChild(input);
  input.focus();
  input.select();
  var done = false;
  function finish(save) {
    if (done) return;
    done = true;
    var v = input.value.trim();
    input.remove();
    nameEl.style.visibility = '';
    nameEl.focus();
    if (!save || v === old) return;
    nameEl.textContent = v || old;
    fetch('/api/channels/label', { method: 'POST', body: new URLSearchParams({ channel: ch, name: v }) })
      .then(function(r) { return r.ok ? r.json() : null; })
      .then(function(j) {
        nameEl.textContent = j ? j.name : old;
        if (j) chNames = ''; // rebuild the strips with the new names on the next tick
      })
      .catch(function() { nameEl.textContent = old; });
  }
  input.addEventListener('keydown', function(e) {
    if (e.key === 'Enter') { e.preventDefault(); finish(true); }
    else if (e.key === 'Escape') { e.preventDefault(); finish(false); }
    e.stopPropagation();
  });
  input.addEventListener('blur', function() { finish(true); });
}
chMeters.addEventListener('dblclick', function(ev) {
  editChannelLabel(ev.target.closest('.strip:not(.strip-legend) .scribble-name'));
});
chMeters.addEventListener('keydown', function(ev) {
  if ((ev.key === 'Enter' || ev.key === 'F2') && ev.target.matches('.scribble-name')) {
    ev.preventDefault();
    editChannelLabel(ev.target);
  }
});

function applyMeter(m) {
  FLOOR = m.floorDB;

  // OLED mirror: reload only when the panel framebuffer actually changed.
  if (m.displaySeq !== oledSeq) {
    oledSeq = m.displaySeq;
    document.getElementById('oled').src = '/api/display.png?t=' + Date.now();
  }

  // inferno TX state rides the same tick: clock loss shows without refresh.
  var txEl = document.getElementById('txstatus');
  if (txEl && typeof m.txShort === 'string' && txEl.textContent !== m.txShort) {
    txEl.textContent = m.txShort;
  }

  var paused = !!m.paused;
  // Reels and the tape-path pulse stop moving while paused (frozen transport)
  // but the head display still shows the frozen elapsed time rather than
  // going blank.
  var moving = m.recording || m.playing;
  var showing = m.recording || m.playing || paused;
  setSeg7(showing ? m.elapsed : null);

  // The deck (ftl-themes .media-deck) takes one state class and draws the
  // rest: reels and tape move while recording or playing, the head gap and
  // transport lamp go red while recording, green while playing; paused
  // holds still.
  var deck = document.getElementById('deck');
  if (deck) {
    deck.classList.toggle('is-recording', !!m.recording);
    deck.classList.toggle('is-playing', !!m.playing && !m.recording);
    deck.classList.toggle('is-paused', paused);
  }
  var linkLamp = document.getElementById('linkLamp');
  if (linkLamp) linkLamp.classList.toggle('on', !!m.infernoUp);

  // Rebuild the transport row only when the play/pause state actually flips,
  // so the play triangle toggles to a pause glyph exactly when the state does.
  var next = { playing: !!m.playing, paused: paused, rec: !!m.recording };
  if (next.playing !== transportState.playing || next.paused !== transportState.paused || next.rec !== transportState.rec) {
    transportState = next;
    renderTransportRow();
  }

  var channels = Array.isArray(m.channels) ? m.channels : [];
  ensureChannels(channels.length, m.channelNames);
  rebuildDbScale(m.floorDB);
  channels.forEach(function(c, idx) {
    var i = idx + 1;
    // ftl-themes' .meter contract: level and peak are custom properties
    // on the .meter itself, read by its .meter-fill and .meter-peak.
    var fill = chMeters.querySelector('.meter-fill[data-i="' + i + '"]');
    var track = fill && fill.parentNode;
    if (track) {
      track.style.setProperty('--meter-level', vuPct(c.rmsDB) + '%');
      track.style.setProperty('--meter-peak', vuPct(c.peakDB) + '%');
      var now = String(Math.round(Math.max(c.rmsDB, FLOOR)));
      if (track.getAttribute('aria-valuenow') !== now) track.setAttribute('aria-valuenow', now);
    }
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
var teleCPU = null, teleRAM = null, teleTemp = null, teleDisk = null, teleSub = null;
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
    teleCSS('--chart-series-1', teleCSS('--glow', '#00d9ff')),
    teleCSS('--chart-series-2', teleCSS('--idle', '#2bffb0')),
    teleCSS('--chart-series-3', teleCSS('--orange', '#ff8c1a')),
    teleCSS('--chart-series-4', teleCSS('--rec', '#ff3355')),
    teleCSS('--chart-series-5', teleCSS('--dim', '#5b8aa8')),
    teleCSS('--chart-series-6', teleCSS('--text', '#cfeeff'))
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
// Graph heights, ~25% taller than the old side column (90/56 px) now that
// the System pane spans the page width.
var TELE_H = 112, TELE_H_SMALL = 70;
function teleOpts(extraSeries, ymin, ymax, h) {
  var dim = teleCSS('--dim', '#5b8aa8');
  var font = '9px ' + teleCSS('--font', 'Consolas,monospace');
  var o = {
    width: 300, height: h || TELE_H,
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
function initTeleCharts(ncores, subNames) {
  var cpuEl = document.getElementById('cpuChart');
  var subEl = document.getElementById('subChart');
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
  teleCPU = new uPlot(teleOpts(cpuSeries, 0, 100, TELE_H), dummy, cpuEl);
  if (subEl) {
    subEl.innerHTML = '';
    var subSeries = [], subDummy = [[0, 1]];
    (subNames || []).forEach(function(n, i) {
      subSeries.push({label: n, stroke: pal[i % pal.length], width: 1.5, dash: i >= pal.length ? [4, 3] : undefined});
      subDummy.push([0, 0]);
    });
    teleSub = new uPlot(teleOpts(subSeries, 0, null, TELE_H), subDummy, subEl);
    teleSize(teleSub, subEl, TELE_H);
  }
  teleRAM = new uPlot(teleOpts([
    {label: 'App MB', stroke: pal[0], width: 1.5, fill: 'rgba(0,217,255,0.10)'},
    {label: 'Sys MB', stroke: pal[2], width: 1.5}
  ], null, null, TELE_H), [[0, 1], [0, 0], [0, 0]], ramEl);
  teleTemp = new uPlot(teleOpts([{label: 'Temp C', stroke: pal[2], width: 1.5}], null, null, TELE_H_SMALL), [[0, 1], [0, 0]], tempEl);
  teleDisk = new uPlot(teleOpts([{label: 'Free GB', stroke: pal[1], width: 1.5}], 0, null, TELE_H_SMALL), [[0, 1], [0, 0]], diskEl);
  teleSize(teleCPU, cpuEl, TELE_H); teleSize(teleRAM, ramEl, TELE_H);
  teleSize(teleTemp, tempEl, TELE_H_SMALL); teleSize(teleDisk, diskEl, TELE_H_SMALL);
  // Armed once: every re-init (core-count change, theme switch) would
  // otherwise stack another resize listener and run setSize N times.
  if (!window.teleResizeArmed) {
    window.teleResizeArmed = true;
    window.addEventListener('resize', function() {
      teleSize(teleCPU, document.getElementById('cpuChart'), TELE_H);
      teleSize(teleSub, document.getElementById('subChart'), TELE_H);
      teleSize(teleRAM, document.getElementById('ramChart'), TELE_H);
      teleSize(teleTemp, document.getElementById('tempChart'), TELE_H_SMALL);
      teleSize(teleDisk, document.getElementById('diskChart'), TELE_H_SMALL);
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
  var subNames = h.subNames || [];
  if (!teleCPU && !initTeleCharts(ncores, subNames)) return;
  if (teleCPU && (teleCPU.series.length - 1 !== ncores || (teleSub && teleSub.series.length - 1 !== subNames.length))) {
    teleCPU = null; teleRAM = null; teleTemp = null; teleDisk = null; teleSub = null;
    if (!initTeleCharts(ncores, subNames)) return;
  }
  var cols = [h.t];
  for (var i = 0; i < ncores; i++) {
    cols.push(h.cores.map(function(row) { return (row && i < row.length) ? row[i] : null; }));
  }
  teleCPU.setData(cols);
  if (teleSub && h.sub) {
    var scols = [h.t];
    for (var j = 0; j < subNames.length; j++) {
      scols.push(h.sub.map(function(row) { return (row && j < row.length) ? Math.round(row[j] * 10) / 10 : null; }));
    }
    teleSub.setData(scols);
  }
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
  var bulb = lamp.querySelector('.lamp');
  if (bulb) { bulb.classList.toggle('is-on', !!on); bulb.classList.toggle('is-error', !on); }
  lamp.title = on ? 'Server connected' : 'Server disconnected';
  lamp.setAttribute('aria-label', lamp.title);
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
</body></html>
{{define "settingsPanes"}}      <div class="settings-panes scroll" id="settingsPanes">
      <section class="settings-group field-group settings-pane is-active" role="tabpanel" aria-labelledby="tab-device" id="pane-device">
        {{.DeviceNameFragment}}
      </section>

      <section class="settings-group field-group settings-pane" role="tabpanel" aria-labelledby="tab-audio" id="pane-audio">
        {{.SampleRateFragment}}
        {{.ChannelCountFragment}}
        {{.MonitorFragment}}
      </section>

      <section class="settings-group field-group settings-pane" role="tabpanel" aria-labelledby="tab-metering" id="pane-metering">
        {{.VURangeFragment}}
        {{.PeakHoldFragment}}
      </section>

      <section class="settings-group field-group settings-pane" role="tabpanel" aria-labelledby="tab-metadata" id="pane-metadata">
        {{.PrefixFragment}}
        {{.TagFragment}}
      </section>

      <section class="settings-group field-group settings-pane" role="tabpanel" aria-labelledby="tab-transport" id="pane-transport">
        {{.TransportFragment}}
        {{.HyperdeckFragment}}
      </section>

      <section class="settings-group field-group settings-pane" role="tabpanel" aria-labelledby="tab-display" id="pane-display">
        {{.ThemeFragment}}
        {{.TintFragment}}
        {{.MotionFragment}}
        {{.ContrastFragment}}
        {{.DensityFragment}}
        {{.BrightnessFragment}}
        {{.AutoDimFragment}}
      </section>

      <section class="settings-group field-group settings-pane" role="tabpanel" aria-labelledby="tab-demo" id="pane-demo">
        {{.DemoFragment}}
      </section>

      <section class="settings-group field-group settings-pane" role="tabpanel" aria-labelledby="tab-logging" id="pane-logging">
        {{.LogLevelFragment}}
      </section>

      <section class="settings-group field-group settings-pane" role="tabpanel" aria-labelledby="tab-config" id="pane-config">
        <div class="field-row field-row--pair">
          <button hx-post="/api/config/export" hx-target="#config-msg" class="btn btn-secondary">Export to USB</button>
          <button hx-post="/api/config/import" hx-target="#config-msg" class="btn btn-secondary">Import from USB</button>
        </div>
        <div class="field-row" id="config-msg"></div>
      </section>

      <section class="settings-group field-group settings-pane" role="tabpanel" aria-labelledby="tab-network" id="pane-network">
        <div id="wifi-settings">
          {{.WifiQRFragment}}
          <form hx-post="/api/settings/wifi" hx-target="#wifiqr" hx-swap="outerHTML" hx-status:400="target:#wifi-error">
            <div class="field-row field-row--pair">
              <span class="field">
                <label for="wifiSsid">SSID</label>
                <input id="wifiSsid" class="input" name="ssid" value="{{.WifiSSID}}" maxlength="32" required>
              </span>
              <span class="field">
                <label for="wifiPass">Password</label>
                <input id="wifiPass" class="input" name="password" type="password" value="{{.WifiPassword}}" minlength="8" maxlength="63" required>
              </span>
            </div>
            <div class="field-row">
              <label class="label" for="wifiEnabled">Access Point</label>
              <label class="switch" for="wifiEnabled">
                <input id="wifiEnabled" name="enabled" type="checkbox" {{if .WifiEnabled}}checked{{end}}>
                <span class="switch-track"><span class="switch-thumb"></span></span>
                <span class="switch-readout" data-on="ONLINE" data-off="OFFLINE"></span>
              </label>
            </div>
            <button type="submit" class="btn btn-secondary">Save WiFi</button>
          </form>
          <div id="wifi-error"></div>
        </div>
      </section>
      </div>{{end}}`))

func handleDashboard(w http.ResponseWriter, r *http.Request) {
	renderFragment(w, dashboardTmpl, buildDashboardData(r))
}

// handleSettingsPanes re-renders just the settings panes. The dashboard
// fetches it each time the settings sheet opens: the panes were otherwise
// rendered once at page load, so a change made on the front panel, another
// browser or a HyperDeck controller left an open dashboard showing the old
// value until a reload.
func handleSettingsPanes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTmpl.ExecuteTemplate(w, "settingsPanes", buildDashboardData(r)); err != nil {
		logErrorf("render settingsPanes: %v", err)
	}
}

// buildDashboardData snapshots everything the dashboard template renders.
func buildDashboardData(r *http.Request) dashboardData {
	mutex.Lock()
	name := deviceName
	wifiEn := wifiEnabled
	wifiS := wifiSSID
	wifiP := wifiPassword
	transportIcon := transportMode != "text"
	mutex.Unlock()

	activeTheme := currentTheme()
	activeVariant := currentThemeVariant()
	activeThemeCSS := themeCSSHref(activeTheme)
	// ?preview=<slug> renders a theme for this browser only, without
	// persisting it: try-before-apply on shared hardware, where selecting
	// rewrites the unit's look for every browser. Unknown slugs fall back
	// to the persisted theme, never to an error page.
	if pv := r.URL.Query().Get("preview"); isKnownTheme(pv) {
		activeTheme = pv
		activeThemeCSS = themeCSSHref(pv)
		// &variant=<id> previews one of its palettes (the library's demo
		// pages use the same parameter); anything else is its own palette.
		activeVariant = ""
		if pvv := r.URL.Query().Get("variant"); themeHasVariant(pv, pvv) {
			activeVariant = pvv
		}
	}
	// The tint follows the rendered theme: its stored colour, or for a
	// preview &tint=%23rrggbb (as on the library's demo pages).
	activeTint := currentThemeTint(activeTheme)
	if pt := normalizeTint(r.URL.Query().Get("tint")); pt != "" && r.URL.Query().Get("preview") != "" {
		activeTint = pt
	}

	var vuBuf, holdBuf, srBuf, chBuf, tagBuf, prefixBuf, transportBuf, hyperdeckBuf, logLevelBuf, brightnessBuf, autoDimBuf, monitorBuf, demoBuf, qrBuf, themeBuf, motionBuf, contrastBuf, densityBuf, nameBuf bytes.Buffer
	renderFragment(&vuBuf, selectFragmentTmpl, vuRangeSelect())
	renderFragment(&holdBuf, selectFragmentTmpl, peakHoldSelect())
	renderFragment(&srBuf, selectFragmentTmpl, sampleRateSelect())
	renderFragment(&chBuf, channelCountFragmentTmpl, currentChannelCountView())
	renderFragment(&tagBuf, selectFragmentTmpl, tagSelect())
	renderFragment(&prefixBuf, filePrefixFragmentTmpl, filePrefixView())
	renderFragment(&transportBuf, transportFragmentTmpl, transportOptionsView())
	renderFragment(&hyperdeckBuf, hyperdeckFragmentTmpl, hyperdeckViewData())
	renderFragment(&logLevelBuf, selectFragmentTmpl, logLevelSelect())
	renderFragment(&themeBuf, themeFragmentTmpl, themePicker())
	var tintBuf bytes.Buffer
	renderFragment(&tintBuf, tintFragmentTmpl, currentTintView())
	for _, opt := range displayOptions {
		buf := displayOptBuf(opt.id, &motionBuf, &contrastBuf, &densityBuf)
		if buf != nil {
			selectFragmentTmpl.Execute(buf, settingSelect(opt.id, opt.post, opt.label, "", func() optionsView { return optionsView{Options: opt.options, Idx: opt.get()} }))
		}
	}
	renderFragment(&brightnessBuf, brightnessFragmentTmpl, brightnessViewData())
	renderFragment(&autoDimBuf, autoDimFragmentTmpl, autoDimViewData())
	renderFragment(&demoBuf, demoFragmentTmpl, demoViewData())
	renderFragment(&nameBuf, deviceNameFragmentTmpl, deviceNameNow())
	renderFragment(&monitorBuf, monitorFragmentTmpl, monitorViewData())

	// Generate WiFi QR code as base64 PNG for the settings modal
	var qrBase64 string
	if wifiEn && wifiS != "" {
		if code, err := qrcode.New(wifiQRContent(), qrcode.Medium); err == nil {
			png, _ := code.PNG(256)
			qrBase64 = base64.StdEncoding.EncodeToString(png)
		}
	}
	renderFragment(&qrBuf, wifiQRFragmentTmpl, wifiQRView{wifiEn, wifiS, wifiP, qrBase64})

	return dashboardData{
		DeviceName:           name,
		Logo:                 template.HTML(pi9696LogoSVG),
		Theme:                activeTheme,
		ThemeCSS:             activeThemeCSS,
		CoreVersion:          themeBuildVersion(),
		IconSprite:           iconSpriteHref(activeTheme),
		HTMLTag:              displayHTMLTag(activeTheme, activeVariant, activeTint),
		VURangeFragment:      template.HTML(vuBuf.String()),
		PeakHoldFragment:     template.HTML(holdBuf.String()),
		SampleRateFragment:   template.HTML(srBuf.String()),
		ChannelCountFragment: template.HTML(chBuf.String()),
		TagFragment:          template.HTML(tagBuf.String()),
		PrefixFragment:       template.HTML(prefixBuf.String()),
		TransportFragment:    template.HTML(transportBuf.String()),
		HyperdeckFragment:    template.HTML(hyperdeckBuf.String()),
		LogLevelFragment:     template.HTML(logLevelBuf.String()),
		ThemeFragment:        template.HTML(themeBuf.String()),
		TintFragment:         template.HTML(tintBuf.String()),
		MotionFragment:       template.HTML(motionBuf.String()),
		ContrastFragment:     template.HTML(contrastBuf.String()),
		DensityFragment:      template.HTML(densityBuf.String()),
		BrightnessFragment:   template.HTML(brightnessBuf.String()),
		AutoDimFragment:      template.HTML(autoDimBuf.String()),
		MonitorFragment:      template.HTML(monitorBuf.String()),
		DemoFragment:         template.HTML(demoBuf.String()),
		DeviceNameFragment:   template.HTML(nameBuf.String()),
		WifiEnabled:          wifiEn,
		WifiSSID:             wifiS,
		WifiPassword:         wifiP,
		WifiQRFragment:       template.HTML(qrBuf.String()),
		TransportIcon:        transportIcon,
	}
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

// deviceNameNow reads the device name under the app mutex.
func deviceNameNow() string {
	mutex.Lock()
	defer mutex.Unlock()
	return deviceName
}

func handleAPIDeviceName(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if !isValidDeviceName(name) {
		// Same 400 pattern as prefix/WiFi: a silent 200 left the operator
		// thinking a rejected rename had saved.
		logWarnf("rejected invalid device name %q via remote", name)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `<span class="err">Letters, numbers, spaces, - and _ only (max 32)</span>`)
		return
	}
	mutex.Lock()
	deviceName = name
	persistConfig()
	mutex.Unlock()
	logInfof("Device name changed to %q via remote", name)
	// The advertised name lives on the paired device, so restart Inferno
	// (deferred while a take records or plays).
	restartInfernoServer()

	mutex.Lock()
	current := deviceName
	mutex.Unlock()
	// Wrap in the outer #devicename div: the form targets it with
	// outerHTML, and a bare <form> response would destroy the target so
	// the name is editable exactly once per page load. Markup mirrors the
	// dashboard row exactly (label/id/button), plus the error target and
	// an OOB clear of any stale validation error on success.
	renderFragment(w, deviceNameFragmentTmpl, current)
	fmt.Fprint(w, "\n<div id=\"devicename-error\" hx-swap-oob=\"innerHTML\"></div>")
}

// deviceNameFragmentTmpl is the Unit Name row, rendered by both the
// dashboard and the save response. The save used to return its own
// hand-written copy that had drifted (no label/input classes or input
// group), so the field lost its styling after every rename.
var deviceNameFragmentTmpl = template.Must(template.New("devicename").Parse(`<div id="devicename" class="setting-cell">
<div class="field-row">
<form hx-post="/api/device-name" hx-target="#devicename" hx-swap="outerHTML" hx-status:400="target:#devicename-error">
<label class="label" for="deviceNameInput">Unit Name</label>
<div class="input-group">
<input id="deviceNameInput" class="input" name="name" value="{{.}}" maxlength="32" pattern="[A-Za-z0-9 _\-]+" title="Letters, numbers, spaces, - and _ only">
<button type="submit" class="btn btn-secondary">Save</button>
</div>
</form>
</div>
<div id="devicename-error"></div>
</div>`))

// handleDisplayPNG mirrors the OLED - encoded from the supersampled canvas
// (see TTFDisplay.EncodePNG), not a separate HTML/CSS reimplementation of
// the layout that could drift from what render() actually draws. The served
// PNG can therefore show sub-nibble canvas detail the 4bpp panel quantizes
// away; noteDisplayFrame tracks both hashes so the mirror still refreshes.
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
<tr><td>Inferno TX</td><td id="txstatus">{{.TXShort}}</td></tr>
<tr><td>Clock</td><td>{{.Clock}}</td></tr>
<tr><td>Network</td><td>{{.Network}}</td></tr>
<tr><td>Uptime</td><td>{{.Uptime}}</td></tr>
<tr><td>Version</td><td>v{{.Version}}</td></tr>
</table>
`))

type configView struct {
	SampleRate int
	Channels   int
	Format     string
	Tag        string
	Inferno    string
	Clock      string
	Network    string
	TXShort    string // inferno TX state, kept live by the meter tick
	Uptime     string // to the minute, so the push only changes once a minute
	Version    string
}

// formatUptime renders an uptime to the minute ("3d 4h 12m", "7m"), so
// the Status table (pushed only when it changes) changes once a minute.
func formatUptime(d time.Duration) string {
	m := int(d / time.Minute)
	days, hours, mins := m/(24*60), m/60%24, m%60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh %dm", days, hours, mins)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
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
		Clock:      clockSyncTextLocked(time.Now()),
		Uptime:     formatUptime(time.Since(bootTime)),
		Version:    appVersion,
	}
	v.TXShort, _ = txStatusLocked()
	mutex.Unlock()

	_, v.Network = hwManager.Network.GetNetworkStatus()

	var buf bytes.Buffer
	if err := configTmpl.Execute(&buf, v); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// statusTmpl is the page footer's live line (#diskInfo), pushed over the
// telemetry socket: disk space and record time left, plus a short-lived
// dashboard notice (webNotice). Owner layout (2026-10-06): the Transport
// Status state line (#status) is gone - the deck, its keys and lamps show
// the transport state, the Status table carries Inferno, uptime and TX.
var statusTmpl = template.Must(template.New("status").Parse(`Disk /rec <progress class="progress" value="{{printf "%.0f" .DiskUsed}}" max="{{printf "%.0f" .DiskTotal}}" aria-label="Disk /rec used"></progress> <span class="readout readout-sm">{{printf "%.0f" .DiskFree}}<span class="readout-unit">GB</span></span> free of {{printf "%.0f" .DiskTotal}} GB &middot; record time left <span class="readout readout-sm">{{.RecordTime}}</span>{{if .Notice}} <span class="badge badge-warning footer-notice" role="alert">{{.Notice}}</span>{{end}}`))

// webNotice/webNoticeUntil is the dashboard counterpart of sysNotice: a
// one-shot notice rendered into the page footer. startPlayback uses it
// for the sample-rate/channel refusal, which previously only reached the log.
var webNotice string
var webNoticeUntil time.Time

func showWebNotice(msg string) {
	webNotice = msg
	webNoticeUntil = time.Now().Add(6 * time.Second)
}

type statusView struct {
	Recording   bool
	Playing     bool
	Paused      bool
	Monitoring  bool
	MonOutput   bool
	Elapsed     string
	Meter       string
	Notice      string
	Format      string
	SampleRate  int
	Channels    int
	InfernoUp   bool
	DemoMode    bool
	TXStatus    string
	Uptime      string
	AppVersion  string
	CPUPerCore  []float64
	RAMApp      float64
	RAMSysUsed  float64
	RAMSysTotal float64
	CPUTemp     float64
	DiskTotal   float64
	DiskFree    float64
	DiskUsed    float64 // DiskTotal - DiskFree, for the footer's <progress>
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
		Notice:     webNoticeIfLive(),
		Format:     "WAV",
		SampleRate: sampleRates[sampleRateIdx] / 1000,
		Channels:   channelCount,
		InfernoUp:  infernoUp(),
		DemoMode:   demoMode,
	}
	_, v.TXStatus = txStatusLocked()
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
	v.RAMApp, v.RAMSysUsed, v.RAMSysTotal = t.RAMApp, t.RAMSysUsed, t.RAMSysTotal
	v.CPUTemp, v.DiskTotal, v.DiskFree, v.RecordTime = t.CPUTemp, t.DiskTotal, t.DiskFree, t.RecordTime
	v.DiskUsed = max(0, v.DiskTotal-v.DiskFree)
	return v
}

// webNoticeIfLive returns the pending dashboard notice, if any, and clears
// it once expired. Must be called with the app mutex held, like the rest of
// currentStatusView.
func webNoticeIfLive() string {
	if webNotice == "" || !time.Now().Before(webNoticeUntil) {
		webNotice = ""
		return ""
	}
	return webNotice
}

func renderStatusHTML() (string, error) {
	var buf bytes.Buffer
	if err := statusTmpl.Execute(&buf, currentStatusView()); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	// Unchecked Execute hides truncated pages on client disconnect; debug,
	// not warn, since a gone client is not an app fault.
	if err := statusTmpl.Execute(w, currentStatusView()); err != nil {
		logDebugf("status render: %v", err)
	}
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
	// SubNames labels the columns of Sub, the app's CPU by subsystem in %
	// of one core (see threadcpu.go).
	SubNames []string    `json:"subNames"`
	Sub      [][]float64 `json:"sub"`
}

func currentTelemetryHist() telemetryHistView {
	mutex.Lock()
	defer mutex.Unlock()
	return telemetryHistView{
		T:        append([]int64(nil), teleHistT...),
		CPU:      append([]float64(nil), teleHistCPU...),
		Cores:    append([][]float64(nil), teleHistCores...),
		RAMApp:   append([]float64(nil), teleHistRAMApp...),
		RAMSys:   append([]float64(nil), teleHistRAMSys...),
		Temp:     append([]float64(nil), teleHistTemp...),
		Disk:     append([]float64(nil), teleHistDisk...),
		SubNames: cpuSubsystems,
		Sub:      append([][]float64(nil), teleHistSub...),
	}
}

func handleAPITelemetry(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(currentTelemetryHist()); err != nil {
		logDebugf("telemetry encode: %v", err)
	}
}

// teleWSHub tracks dashboard telemetry sockets; guarded by teleWSMu (never
// the app mutex - broadcast renders status HTML which takes it).
var teleWSHub = map[*websocket.Conn]bool{}
var teleWSMu sync.Mutex

// teleWSMessage is the hx-ws wire shape: target selects the swap element,
// content is HTML for #diskInfo (the footer) or the history JSON for #teleHist.
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
	// Snapshot the client set under lock, then send without it: holding
	// teleWSMu across blocking socket writes lets one slow/vanished
	// dashboard stall every connect/disconnect plus the panel render's wait
	// on the app mutex. A client that connects mid-tick simply joins the
	// next one 2s later; one that disconnects mid-tick gets a harmless
	// redundant Close+delete (both idempotent).
	teleWSMu.Lock()
	config, recs, configChanged, recsChanged := panelWSMessages()
	clients := make([]*websocket.Conn, 0, len(teleWSHub))
	for ws := range teleWSHub {
		clients = append(clients, ws)
	}
	teleWSMu.Unlock()
	for _, ws := range clients {
		alive := true
		if !wsSessionLive(ws) {
			alive = false // logged out or token rotated: stop streaming
		} else if !teleWSSend(ws, "#diskInfo", status) || !teleWSSend(ws, "#teleHist", hist) {
			alive = false
		} else if configChanged && !teleWSSend(ws, "#config", config) {
			alive = false
		} else if recsChanged && !teleWSSend(ws, "#recordings", recs) {
			alive = false
		}
		if !alive {
			teleWSMu.Lock()
			ws.Close()
			delete(teleWSHub, ws)
			teleWSMu.Unlock()
		}
	}
}

// lastPanelConfig/lastPanelRecs are the last pushed panel bodies; the
// Status and Recordings panels only go out over the socket when they
// actually changed, so an idle dashboard costs no re-renders. Guarded by
// teleWSMu - panelWSMessages renders (taking the app mutex inside), so
// never call it with the app mutex already held.
var lastPanelConfig, lastPanelRecs string

// lastPanelRecsKey is the take-set fingerprint the stored recordings body was
// rendered from. Empty until the first successful render.
var lastPanelRecsKey string

// panelWSMessages renders both panels, stores the new bodies, and reports
// whether each changed since the last push. Call with teleWSMu held.
func panelWSMessages() (config, recs string, configChanged, recsChanged bool) {
	if c, err := renderConfigHTML(); err == nil {
		config, configChanged = c, c != lastPanelConfig
		lastPanelConfig = c
	}
	// The row render opens and reads every WAV for its duration, so skip it
	// when the take set hasn't changed. recordingFilesKey fingerprints the
	// set from directory mtimes - the same key recordingFiles() itself
	// trusts for cache invalidation, so this is exactly as fresh as the
	// data source (and shares its documented blind spot: in-place content
	// edits without mtime change). An active take grows its file, which
	// bumps the day-dir mtime, so live durations keep flowing.
	// The playback selection is part of the key: it marks a row.
	mutex.Lock()
	sel := selectedPlayback
	mutex.Unlock()
	if recKey, ok := recordingFilesKey(); ok && recKey+"|"+sel == lastPanelRecsKey && lastPanelRecsKey != "" {
		return config, lastPanelRecs, configChanged, false
	}
	if r, err := renderRecordingsHTMLLimit(latestRecsPushCap); err == nil {
		recs, recsChanged = r, r != lastPanelRecs
		lastPanelRecs = r
		if key, ok := recordingFilesKey(); ok {
			lastPanelRecsKey = key + "|" + sel
		}
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
		if !teleWSSend(ws, "#diskInfo", status) || !teleWSSend(ws, "#teleHist", hist) {
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
	// TXStatus is the inferno transmit state (see txholder.go), pushed live
	// so the dashboard tracks clock loss without a status refresh.
	TXStatus string `json:"txStatus"`
	// TXShort is the same state in the Status table's short form.
	TXShort string `json:"txShort"`
	// ChannelNames label the meter strips: the unit's RX channel names as
	// a network controller named them (channelnames.go).
	ChannelNames []string `json:"channelNames"`
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
// currentMeterResponse is the meter snapshot plus the strips' channel
// names, which are read (cached) without the app mutex.
func currentMeterResponse() meterResponse {
	resp := buildMeterLevels()
	resp.ChannelNames = displayChannelNames(len(resp.Channels))
	return resp
}

// buildMeterLevels takes the app mutex for the levels and transport state.
func buildMeterLevels() meterResponse {
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
	resp.TXShort, resp.TXStatus = txStatusLocked()
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

// meterCache shares one marshaled snapshot across meter sockets: without it
// N dashboards each build (up to 128 channels) and marshal identical payloads
// at 10Hz. The payload is time-quantized to the tick anyway (Elapsed), so a
// 100ms TTL loses nothing. Guarded by its own small mutex, never the app
// mutex, so a slow snapshot build can't stall render/input. On marshal
// failure (unreachable for this struct, but cheap to honor) the last good
// frame keeps serving rather than pushing an empty one clients would choke on.
var meterCacheMu sync.Mutex
var meterCacheAt time.Time
var meterCachePayload []byte

const meterCacheTTL = 100 * time.Millisecond

func cachedMeterPayload() []byte {
	meterCacheMu.Lock()
	defer meterCacheMu.Unlock()
	if meterCachePayload != nil && time.Since(meterCacheAt) < meterCacheTTL {
		return meterCachePayload
	}
	data, err := json.Marshal(currentMeterResponse())
	if err != nil {
		logDebugf("meter marshal: %v", err)
		return meterCachePayload
	}
	meterCachePayload = data
	meterCacheAt = time.Now()
	return data
}

func handleAPIMeter(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(currentMeterResponse()); err != nil {
		logDebugf("meter encode: %v", err)
	}
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
	return websocket.JSON.Send(ws, json.RawMessage(cachedMeterPayload())) == nil
}

func handleWSMeter(ws *websocket.Conn) {
	defer ws.Close()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		if !wsSessionLive(ws) || !wsMeterSend(ws) {
			return
		}
	}
}

// wsSessionLive reports whether the login session that opened ws is still
// valid. requireAuth checks only the handshake, so a socket used to keep
// streaming after a logout, the session's expiry, or a token rotation -
// the response to a compromised token - had revoked it.
func wsSessionLive(ws *websocket.Conn) bool {
	r := ws.Request()
	return r != nil && validSession(r)
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
<div class="recordings-wrap scroll">
<table class="table is-sticky">
<thead><tr><th></th><th>File</th><th>Tracks</th><th>Channels</th><th>Format</th><th>Start</th><th>End</th><th>Duration</th><th></th></tr></thead>
<tbody>
{{if not .Rows}}<tr><td colspan="9"><div class="empty-state"><span class="empty-state-icon">&#8709;</span><span class="empty-state-title">None yet.</span><span class="empty-state-hint">Takes appear here as they finalize.</span></div></td></tr>{{else}}
{{range .Rows}}<tr{{if .Selected}} class="is-selected" aria-selected="true"{{end}}>
<td class="recs-play"><form hx-post="/api/playback/select" hx-target="#recordings" hx-swap="innerHTML"><input type="hidden" name="file" value="{{.RelPath}}"><button class="btn btn-sm btn-secondary" type="submit" title="Load this take: PLAY starts it" aria-label="Load {{.Name}} for playback"><svg class="icon" aria-hidden="true"><use href="{{$.Sprite}}#icon-play"/></svg></button></form></td>
<td>{{.Name}}{{if .Selected}} <span class="badge badge-accent">loaded</span>{{end}}</td>
<td>{{.Channels}}</td>
<td class="recs-chans"><details><summary title="Channel names of this take (from the unit's inferno channel names when it started; rename here - the unit itself is not changed)">{{.ChanSummary}}</summary>
<form hx-post="/api/recordings/channels" hx-target="#recordings" hx-swap="innerHTML" class="chan-form"><input type="hidden" name="file" value="{{.RelPath}}">
{{range .ChanNames}}<label class="chan-row"><span>{{.Number}}</span><input class="input" name="name_{{.Number}}" value="{{.Name}}" maxlength="32" required>{{if .Source}}<small title="Source when the take started">&larr; {{.Source}}</small>{{end}}</label>
{{end}}<button class="btn btn-sm" type="submit">Save names</button></form></details></td>
<td>{{.Format}} {{.SampleRate}}kHz</td>
<td>{{.StartStr}}</td>
<td>{{.EndStr}}</td>
<td>{{.DurationStr}}</td>
<td class="recs-dl"><a class="btn btn-sm btn-secondary" href="/download/{{.RelPath}}" download><svg class="icon" aria-hidden="true"><use href="{{$.Sprite}}#icon-download"/></svg> download</a></td>
</tr>{{end}}
{{end}}
</tbody>
</table>
{{if .Capped}}<p class="recs-note">Showing latest {{len .Rows}} of {{.Total}} takes.</p>{{end}}
</div>
`))

type recordingsView struct {
	Rows   []recordingRow
	Total  int
	Capped bool
	Sprite string
}

type recordingRow struct {
	Name        string
	RelPath     string
	Selected    bool // the take Play starts (see playselect.go)
	ChanNames   []recChannel
	ChanSummary string
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

// recordingDuration derives a take's length. The WAV data-chunk header is
// parsed when sane (ffmpeg writes LIST/INFO chunks before data, so the old
// fixed 44-byte header guess under-counted); a bogus size (0 = killed
// mid-take, or larger than the file) falls back to the file-size estimate.
func recordingDuration(path string, channels, sampleRate int) time.Duration {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}

	const bytesPerSample = 3 // pcm_s24le
	dataBytes := int64(0)
	if f, err := os.Open(path); err == nil {
		dataBytes = wavDataBytes(f)
		f.Close()
	}
	if dataBytes <= 0 || dataBytes > info.Size()-12 {
		dataBytes = info.Size() - 44
	}
	bytesPerSec := int64(channels) * int64(bytesPerSample) * int64(sampleRate)
	if dataBytes <= 0 || bytesPerSec <= 0 {
		return 0
	}
	// Divide before scaling to nanoseconds: time.Duration(dataBytes) *
	// time.Second overflows int64 past ~9.2 GiB (reached in ~2 min at
	// 128ch/192kHz), going negative and poisoning seeks, demo end timers
	// and the UI.
	secs := dataBytes / bytesPerSec
	// Saturate instead of overflowing: beyond ~136 years the exact value is
	// meaningless to every caller (UI, seeks, timers).
	const maxSecs = int64(1 << 32)
	if secs > maxSecs {
		return time.Duration(maxSecs) * time.Second
	}
	rem := dataBytes % bytesPerSec
	// rem < bytesPerSec, so rem*time.Second stays in int64 for any rate a
	// real stream can have; absurd rates skip the sub-second part they
	// cannot meaningfully have anyway.
	if bytesPerSec < int64(9e9) {
		return time.Duration(secs)*time.Second + time.Duration(rem)*time.Second/time.Duration(bytesPerSec)
	}
	return time.Duration(secs) * time.Second
}

// wavDataBytes returns the data-chunk size declared by a RIFF or RF64 WAV
// header, or 0 when there is none. Takes are written with -rf64 auto, so one
// past 4 GiB carries an RF64 header whose data chunk size is the 0xFFFFFFFF
// placeholder and whose real 64-bit size lives in the leading ds64 chunk.
func wavDataBytes(r io.ReadSeeker) int64 {
	var hdr [12]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil || string(hdr[8:12]) != "WAVE" {
		return 0
	}
	rf64 := string(hdr[0:4]) == "RF64"
	if !rf64 && string(hdr[0:4]) != "RIFF" {
		return 0
	}
	ds64Data := int64(0)
	for {
		var ch [8]byte
		if _, err := io.ReadFull(r, ch[:]); err != nil {
			return 0
		}
		size := int64(binary.LittleEndian.Uint32(ch[4:8]))
		switch string(ch[0:4]) {
		case "ds64":
			// riffSize(8) dataSize(8) sampleCount(8) [table...]
			var ds [16]byte
			if size < 16 {
				return 0
			}
			if _, err := io.ReadFull(r, ds[:]); err != nil {
				return 0
			}
			ds64Data = int64(binary.LittleEndian.Uint64(ds[8:16]))
			size -= 16
		case "data":
			if rf64 && size == 0xFFFFFFFF {
				return ds64Data
			}
			return size
		}
		if size%2 == 1 {
			size++ // chunks are word-aligned
		}
		if _, err := r.Seek(size, io.SeekCurrent); err != nil {
			return 0
		}
	}
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
	row.ChanNames = recordingChannels(path)
	row.ChanSummary = channelSummary(row.ChanNames)
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
// latestRecs caps the pushed render (0 = all): the list re-renders on every
// telemetry tick while recording (the in-progress take's duration grows),
// so hundreds of takes would resend a large table every 2s. The push shows
// the newest takes with a count note; the explicit GET renders everything.
const latestRecsPushCap = 50

func renderRecordingsHTML() (string, error) {
	return renderRecordingsHTMLLimit(0)
}

func renderRecordingsHTMLLimit(limit int) (string, error) {
	files := recordingFiles()
	view := recordingsView{Total: len(files), Sprite: iconSpriteHref(currentTheme())}
	if limit > 0 && len(files) > limit {
		files = files[len(files)-limit:]
		view.Capped = true
	}
	mutex.Lock()
	sel := selectedPlayback
	mutex.Unlock()
	for _, f := range files {
		row := buildRecordingRow(f)
		row.Selected = f == sel
		view.Rows = append(view.Rows, row)
	}
	var buf bytes.Buffer
	if err := recordingsTmpl.Execute(&buf, view); err != nil {
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
			// Never serve the take currently being written: it would be a
			// growing, half-finalized WAV (same rule as the USB copy loop).
			mutex.Lock()
			active := isRecording && f == recordingFile
			mutex.Unlock()
			if active {
				http.NotFound(w, r)
				return
			}
			// attachment: browsers otherwise open a WAV in their own
			// player instead of saving it.
			w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(f)}))
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
	// Snapshot the in-progress take under lock, then filter before any
	// headers go out: the bundle must never include the half-written WAV
	// (torn read, mid-write duration), and pre-statting keeps the manifest's
	// file count honest instead of skipping unreadables mid-stream.
	mutex.Lock()
	active := ""
	if isRecording {
		active = recordingFile
	}
	mutex.Unlock()
	var files []string
	for _, f := range recordingFiles() {
		if f == active {
			continue
		}
		if info, err := os.Stat(f); err != nil || info.IsDir() {
			continue
		}
		files = append(files, f)
	}
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
func writeRecordingZip(dst io.Writer, base string, files []string) (err error) {
	zw := zip.NewWriter(dst)
	// Close writes the central directory; without it the archive is
	// unreadable. Its error used to be dropped, so a client that went away
	// at the very end was never logged as a failed download.
	defer func() {
		if cerr := zw.Close(); err == nil {
			err = cerr
		}
	}()

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

		// Open before writing the entry header: opening after it left an
		// empty entry in the archive for a file that could not be read.
		in, err := os.Open(f)
		if err != nil {
			logErrorf("download-all: skipping %s: %v", entry, err)
			continue
		}
		// PCM WAV is incompressible noise to Deflate: Store skips the
		// CPU burn and streams multi-GB takes at disk speed instead.
		hdr := &zip.FileHeader{Name: entry, Method: zip.Store}
		hdr.SetModTime(info.ModTime())
		wc, err := zw.CreateHeader(hdr)
		if err != nil {
			in.Close()
			return err
		}
		_, copyErr := io.Copy(wc, in)
		in.Close()
		if copyErr != nil {
			return copyErr
		}

		row := buildRecordingRow(f)
		fmt.Fprintf(&manifest, "%-60s %12d %8d %6d %8s %10s  %s\n",
			entry, info.Size(), row.Channels, row.SampleRate, row.Format, row.DurationStr, row.StartStr)
		// The take's channel names: listed in the manifest, and the
		// sidecar itself goes in the bundle next to the take.
		chans := recordingChannels(f)
		for _, c := range chans {
			src := ""
			if c.Source != "" {
				src = "  <- " + c.Source
			}
			fmt.Fprintf(&manifest, "    ch%-3d %s%s\n", c.Number, c.Name, src)
		}
		if side, err := os.ReadFile(channelsSidecar(f)); err == nil {
			hdr := &zip.FileHeader{Name: strings.TrimSuffix(entry, filepath.Ext(entry)) + ".channels.json", Method: zip.Deflate}
			hdr.SetModTime(info.ModTime())
			if w, err := zw.CreateHeader(hdr); err == nil {
				w.Write(side)
			}
		}
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
//go:embed third_party/ftl-themes/dist/*.css third_party/ftl-themes/dist/themes.json third_party/ftl-themes/dist/icons/*.svg third_party/ftl-themes/assets/fonts/*.woff2
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
	mux.HandleFunc("GET /api/settings/panes", requireAuth(handleSettingsPanes))
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
	mux.HandleFunc("POST /api/settings/tint", requireAuth(handleAPISettingsTint))
	registerDisplayOptionRoutes(mux)
	mux.HandleFunc("POST /api/settings/brightness", requireAuth(handleAPISettingsBrightness))
	mux.HandleFunc("POST /api/settings/dim", requireAuth(handleAPISettingsAutoDim))
	mux.HandleFunc("POST /api/settings/demo", requireAuth(handleAPISettingsDemoMode))
	mux.HandleFunc("POST /api/settings/rotate-token", requireAuth(handleAPIRotateToken))
	mux.HandleFunc("POST /api/settings/hyperdeck", requireAuth(handleAPISettingsHyperdeck))
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
	mux.HandleFunc("POST /api/playback/select", requireAuth(handleAPIPlaybackSelect))
	mux.HandleFunc("GET /api/recordings/channels", requireAuth(handleAPIRecordingChannels))
	mux.HandleFunc("POST /api/recordings/channels", requireAuth(handleAPIRecordingChannels))
	mux.HandleFunc("POST /api/channels/label", requireAuth(handleAPIChannelLabel))
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
		// core.css is the always-linked reset+components file, not a
		// theme bundle; it serves alongside the themes it lacks.
		if !ok || (slug != "core" && !isKnownTheme(slug)) {
			http.NotFound(w, r)
			return
		}
		serveEmbeddedStatic(w, r, embeddedThemes, themeAssetPath("dist/"+slug+".css"), "text/css")
	})
	// Per-theme icon sprites (dist/icons/<slug>.svg, merged by the library's
	// build from generic + theme overrides) and the generic fallback.
	mux.HandleFunc("GET /static/themes/icons/{file}", func(w http.ResponseWriter, r *http.Request) {
		slug, ok := strings.CutSuffix(r.PathValue("file"), ".svg")
		if !ok || (slug != "generic" && !isKnownTheme(slug)) {
			http.NotFound(w, r)
			return
		}
		serveEmbeddedStatic(w, r, embeddedThemes, themeAssetPath("dist/icons/"+slug+".svg"), "image/svg+xml")
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

// remoteControlPort is the TCP port the control surface binds to. 8080 by
// default; override with PI9696_REMOTE_PORT for a host that fronts the UI on
// the standard HTTP port instead. Read once at startup, like the bind address.
func remoteControlPort() string {
	if p := os.Getenv("PI9696_REMOTE_PORT"); p != "" {
		return p
	}
	return "8080"
}

// startRemoteServer binds to the given address (normally "0.0.0.0" from
// remoteControlLoop, or a specific host via PI9696_REMOTE_BIND for dev
// testing). Binding 0.0.0.0 serves the control surface on every interface -
// the Round 3 design decision, replacing the old eth0-only constraint; access
// is still gated by the token/session auth.
func startRemoteServer(ip string) (*http.Server, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(ip, remoteControlPort()))
	if err != nil {
		return nil, err
	}

	srv := newRemoteHTTPServer()
	go func() {
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			logErrorf("Remote control server error: %v", err)
		}
	}()

	logInfof("Remote control server listening on http://%s", listener.Addr())
	return srv, nil
}

// remoteHeaderTimeout and remoteIdleTimeout bound what a client can hold
// open without doing anything: the server had no timeouts at all, so a
// client trickling request headers (or parking idle keep-alives) kept a
// connection and its goroutine forever. Vars so tests can shrink them.
// Deliberately no Read/WriteTimeout: Download-all streams multi-GB bodies
// and the WebSockets live for the session.
var (
	remoteHeaderTimeout = 10 * time.Second
	remoteIdleTimeout   = 2 * time.Minute
)

// newRemoteHTTPServer builds the WebUI server.
//
// ErrorLog bypasses slog: net/http reports recovered handler panics
// through it, and the default (log -> slog at Info) is dropped by the
// Error-only default level - a panic holding the app mutex would then
// wedge the whole WebUI with nothing in the journal.
func newRemoteHTTPServer() *http.Server {
	return &http.Server{
		Handler:           newRemoteMux(),
		ErrorLog:          log.New(os.Stderr, "http: ", 0),
		ReadHeaderTimeout: remoteHeaderTimeout,
		IdleTimeout:       remoteIdleTimeout,
	}
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
				setRemoteServer(nil)
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
					setRemoteServer(srv)
				}
			}
		}

		time.Sleep(5 * time.Second)
	}
}

// remoteServerMu guards remoteServer, the currently bound control server
// (if any). Published by remoteControlLoop so gracefulShutdown can stop
// accepting new requests before it drains transport.
var remoteServerMu sync.Mutex
var remoteServer *http.Server

func setRemoteServer(srv *http.Server) {
	remoteServerMu.Lock()
	remoteServer = srv
	remoteServerMu.Unlock()
}

// closeRemoteServer stops the control server if one is bound: no new
// connections, in-flight handlers drained briefly, then force-closed.
func closeRemoteServer() {
	remoteServerMu.Lock()
	srv := remoteServer
	remoteServer = nil
	remoteServerMu.Unlock()
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	err := srv.Shutdown(ctx)
	cancel()
	if err != nil {
		srv.Close()
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
		fmt.Sprintf("http://%s:%s", ip, remoteControlPort()),
		"Token: " + formatToken(remoteToken),
	}
}
