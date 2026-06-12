package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/kbelokon/readout/internal/config"
	"github.com/kbelokon/readout/internal/hooks"
	"golang.org/x/oauth2"
)

// SessionCookieName is the name of the sealed session cookie. It is also the
// AES-GCM associated data the session codec binds the envelope to, so a cookie
// resealed under a different name fails to open.
const SessionCookieName = "READOUT"

// StateCookieName is the name of the sealed OAuth state cookie (nonce +
// original URL), scoped to the callback path. Like the session cookie, the name
// doubles as the codec's associated data.
const StateCookieName = "READOUT_STATE"

// CallbackPath is the OAuth2 redirect/callback route. It is also a public path
// (auth bypasses it) so the unauthenticated callback can complete the handshake.
const CallbackPath = "/oauth2/callback"

// Session is the decoded sealed session: the OAuth tokens plus the
// user/email/groups identity. The JSON tags define the on-the-wire envelope
// payload, so they must stay stable for sealed cookies to survive across
// restarts and process versions.
type Session struct {
	AccessToken  string   `json:"access_token"`
	TokenType    string   `json:"token_type,omitempty"`
	RefreshToken string   `json:"refresh_token,omitempty"`
	IDToken      string   `json:"id_token,omitempty"`
	Expires      int64    `json:"expires"`
	User         string   `json:"user,omitempty"`
	Email        string   `json:"email,omitempty"`
	Groups       []string `json:"groups,omitempty"`
}

type oauthState struct {
	Nonce       string `json:"nonce"`
	OriginalURL string `json:"original_url"`
}

// Authenticator owns the security-sensitive surface: auth-mode dispatch, the
// OIDC/OAuth2 flows, the sealed-session codec, the OIDC provider cache, and the
// bearer-source policy. Web wires its handlers and middleware; it never reaches
// into these internals directly.
type Authenticator struct {
	cfg      config.Config
	sessions *sessionCodec
	hooks    *hooks.Client
	// now is the clock for session expiry comparisons. It defaults to time.Now
	// in New; tests inject a fixed instant.
	now func() time.Time

	// oidcMu guards the OIDC discovery cache below. oidcProvider is built once
	// per process from the configured issuer (go-oidc fetches the discovery
	// document and the remote key set) and reused for every login and callback.
	// Discovery is comparatively expensive and the issuer's metadata is stable,
	// so caching it avoids a network round trip on each OAuth handshake. A failed
	// construction is never cached: a transient issuer outage must not poison the
	// process, so the next request retries discovery from scratch.
	oidcMu       sync.Mutex
	oidcProvider *oidc.Provider
}

// New builds an Authenticator from the resolved config, the session secret
// (READOUT_SESSION_SECRET), an injectable clock, and the shared hooks client. A
// nil clock falls back to time.Now. The config is copied into the Authenticator
// so it owns an independent snapshot. The secret is hashed into the session
// codec's AES key; an empty secret yields an ephemeral per-process key (sessions
// will not survive restarts).
func New(cfg *config.Config, secret string, now func() time.Time, hooksClient *hooks.Client) (*Authenticator, error) {
	codec, err := newSessionCodec(secret)
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &Authenticator{
		cfg:      *cfg,
		sessions: codec,
		hooks:    hooksClient,
		now:      now,
	}, nil
}

// Middleware gates every non-public request by the effective auth mode: none
// passes through, headers requires a trusted identity header (and runs the
// authorization hook), and OIDC requires a valid session or starts the OAuth2
// handshake.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.IsPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		switch a.cfg.AuthMode {
		case "", config.AuthModeNone:
			next.ServeHTTP(w, r)
			return
		case config.AuthModeHeaders:
			// When trustedProxyCIDRs is configured, identity headers are only
			// honored from a TCP peer inside one of the CIDRs. The check gates on
			// r.RemoteAddr (the real peer) and NEVER on X-Forwarded-For or any
			// other client-settable header -- the whole point is to not trust
			// headers a direct client can forge. An unparseable/empty peer fails
			// closed (deny) since a CIDR was explicitly set. When the list is
			// empty, the gate is skipped and headers are trusted as before.
			if len(a.cfg.TrustedProxyCIDRs) > 0 && !peerInCIDRs(r.RemoteAddr, a.cfg.TrustedProxyCIDRs) {
				http.Error(w, "untrusted proxy", http.StatusForbidden)
				return
			}
			if r.Header.Get(a.cfg.TrustedHeaderUser) == "" && r.Header.Get(a.cfg.TrustedHeaderEmail) == "" {
				http.Error(w, "missing trusted identity header", http.StatusUnauthorized)
				return
			}
			session := a.trustedHeaderSession(r)
			allowed, err := a.authorizationHook(r.Context(), &oauth2.Token{}, &session)
			if err != nil {
				// The hook error can carry endpoint-internal (or attacker-chosen)
				// detail; log it server-side and return a fixed generic message so
				// nothing from the hook reaches the browser.
				slog.Error("authorization hook failed", "path", r.URL.Path, "error", err)
				http.Error(w, "authorization hook denied or failed", http.StatusForbidden)
				return
			}
			if !allowed {
				http.Error(w, "Access Denied", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
			return
		case config.AuthModeOIDC:
			if _, ok := a.Session(r); ok {
				next.ServeHTTP(w, r)
				return
			}
			a.startOAuth2(w, r, r.URL.RequestURI())
			return
		default:
			http.Error(w, "invalid auth mode", http.StatusInternalServerError)
			return
		}
	})
}

// authorizationHook consults the configured JSON authorization hook, updating
// session in place with any user/email/groups the hook returns. session is
// taken by pointer (it is a heavy value) and mutated directly; the caller reads
// the updated session after the call. Returns whether access is allowed. The
// hook IO lives in internal/hooks; this adapter maps the session to the hook's
// request DTO and applies the value result back onto the session struct.
func (a *Authenticator) authorizationHook(ctx context.Context, token *oauth2.Token, session *Session) (bool, error) {
	if a.cfg.AuthorizationHookURL == "" {
		return true, nil
	}
	sessionJSON, err := json.Marshal(*session)
	if err != nil {
		return false, err
	}
	// Token minimization: by default the hook receives only the token_type and
	// expiry metadata, never the bearer token set. The operator opts specific
	// tokens in via hooks.authorizationIncludeTokens (access|id|refresh); only the
	// listed tokens are added. A buggy or compromised hook thus cannot harvest the
	// full OAuth token set.
	tokenMap := map[string]any{
		"token_type": token.TokenType,
		"expiry":     token.Expiry.Format(time.RFC3339),
	}
	for _, want := range a.cfg.AuthorizationHookIncludeTokens {
		switch want {
		case "access":
			tokenMap["access_token"] = token.AccessToken
		case "refresh":
			tokenMap["refresh_token"] = token.RefreshToken
		case "id":
			if idToken, _ := token.Extra("id_token").(string); idToken != "" {
				tokenMap["id_token"] = idToken
			}
		}
	}
	result, err := a.hooks.Authorization(ctx, a.cfg.AuthorizationHookURL, hooks.AuthorizationRequest{
		Token:   tokenMap,
		Session: sessionJSON,
	})
	if err != nil {
		return false, err
	}
	if result.User != "" {
		session.User = result.User
	}
	if result.Email != "" {
		session.Email = result.Email
	}
	if result.Groups != nil {
		session.Groups = result.Groups
	}
	// Fail closed: a configured hook must return an explicit {"allowed": true} to
	// admit. A missing `allowed` field (nil) -- a hook bug, a malformed response,
	// or a compromised endpoint omitting the field -- denies. Only an explicit
	// true allows; an explicit false denies. (The no-hook-configured case returned
	// allow at the top of this function.)
	if result.Allowed != nil && *result.Allowed {
		return true, nil
	}
	return false, nil
}

func (a *Authenticator) trustedHeaderSession(r *http.Request) Session {
	return Session{
		User:   r.Header.Get(a.cfg.TrustedHeaderUser),
		Email:  r.Header.Get(a.cfg.TrustedHeaderEmail),
		Groups: splitHeaderGroups(r.Header.Get(a.cfg.TrustedHeaderGroups)),
	}
}

// peerInCIDRs reports whether the TCP peer in remoteAddr (host:port form, as
// set by net/http) is inside one of the trusted CIDRs. A remoteAddr that does
// not parse to an IP -- empty, a unix socket path, or otherwise malformed --
// returns false so the caller fails closed: when a CIDR is configured an
// unidentifiable peer is never trusted.
func peerInCIDRs(remoteAddr string, cidrs []netip.Prefix) bool {
	addr, ok := peerAddr(remoteAddr)
	if !ok {
		return false
	}
	for _, prefix := range cidrs {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// peerAddr extracts the bare IP from a net/http RemoteAddr. It accepts the
// usual host:port form and, defensively, a bare IP; an IPv6 peer's zone is
// stripped so it compares cleanly against an unzoned CIDR. ok is false on any
// value that does not yield an IP (empty, unix socket, garbage).
func peerAddr(remoteAddr string) (netip.Addr, bool) {
	if remoteAddr == "" {
		return netip.Addr{}, false
	}
	if ap, err := netip.ParseAddrPort(remoteAddr); err == nil {
		return ap.Addr().Unmap().WithZone(""), true
	}
	if addr, err := netip.ParseAddr(remoteAddr); err == nil {
		return addr.Unmap().WithZone(""), true
	}
	return netip.Addr{}, false
}

const (
	// maxHeaderGroups caps how many groups are read from the trusted groups
	// header; entries past the cap are dropped. maxHeaderGroupsLen caps the
	// total accepted header length so an enormous header cannot fan out into a
	// huge slice before the count cap applies.
	maxHeaderGroups    = 256
	maxHeaderGroupsLen = 16 * 1024
)

func splitHeaderGroups(value string) []string {
	if value == "" {
		return nil
	}
	if len(value) > maxHeaderGroupsLen {
		value = value[:maxHeaderGroupsLen]
	}
	parts := strings.Split(value, ",")
	groups := make([]string, 0, len(parts))
	for _, part := range parts {
		group := strings.TrimSpace(part)
		if group == "" {
			continue
		}
		groups = append(groups, group)
		if len(groups) >= maxHeaderGroups {
			break
		}
	}
	return groups
}

// Login starts the OAuth2 handshake for an explicit /oauth2/login hit, honoring
// a local-only `next` redirect target.
func (a *Authenticator) Login(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Query().Get("next")
	if !isLocalRedirect(next) {
		next = "/"
	}
	a.startOAuth2(w, r, next)
}

// Logout clears the session and OAuth state cookies and redirects home.
func (a *Authenticator) Logout(w http.ResponseWriter, r *http.Request) {
	clearCookie(w, r, SessionCookieName, "/")
	clearCookie(w, r, StateCookieName, CallbackPath)
	http.Redirect(w, r, "/", http.StatusFound)
}

// Callback completes the OAuth2/OIDC handshake: it validates the sealed state
// cookie and nonce, exchanges the code, verifies the ID token when OIDC, runs
// the authorization hook, and seals the resulting session cookie.
func (a *Authenticator) Callback(w http.ResponseWriter, r *http.Request) {
	stateCookie, err := r.Cookie(StateCookieName)
	if err != nil {
		http.Error(w, "missing OAuth state cookie", http.StatusBadRequest)
		return
	}
	var state oauthState
	if err := a.sessions.Open(StateCookieName, stateCookie.Value, &state); err != nil {
		http.Error(w, "invalid OAuth state", http.StatusBadRequest)
		return
	}
	clearCookie(w, r, StateCookieName, CallbackPath)
	if state.Nonce == "" || state.Nonce != r.URL.Query().Get("state") {
		http.Error(w, "OAuth state mismatch", http.StatusBadRequest)
		return
	}
	if msg := r.URL.Query().Get("error"); msg != "" {
		http.Error(w, "OAuth error: "+msg, http.StatusUnauthorized)
		return
	}
	oauthConfig, verifier, err := a.oauth2Config(r.Context(), r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	token, err := oauthConfig.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		http.Error(w, "OAuth token exchange failed: "+err.Error(), http.StatusUnauthorized)
		return
	}
	session := Session{
		AccessToken:  token.AccessToken,
		TokenType:    token.TokenType,
		RefreshToken: token.RefreshToken,
		Expires:      oauthExpiry(token).Unix(),
	}
	if idToken, _ := token.Extra("id_token").(string); idToken != "" {
		session.IDToken = idToken
		if verifier != nil {
			verified, err := verifier.Verify(r.Context(), idToken)
			if err != nil {
				http.Error(w, "OIDC ID token verification failed: "+err.Error(), http.StatusUnauthorized)
				return
			}
			var claims struct {
				Subject           string   `json:"sub"`
				Email             string   `json:"email"`
				PreferredUsername string   `json:"preferred_username"`
				Name              string   `json:"name"`
				Groups            []string `json:"groups"`
			}
			if err := verified.Claims(&claims); err != nil {
				http.Error(w, "OIDC claims parse failed: "+err.Error(), http.StatusUnauthorized)
				return
			}
			session.User = first(claims.PreferredUsername, claims.Name, claims.Subject)
			session.Email = claims.Email
			session.Groups = claims.Groups
		}
	}
	allowed, err := a.authorizationHook(r.Context(), token, &session)
	if err != nil {
		slog.Error("authorization hook failed", "path", r.URL.Path, "error", err)
		http.Error(w, "authorization hook denied or failed", http.StatusForbidden)
		return
	}
	if !allowed {
		http.Error(w, "Access Denied", http.StatusForbidden)
		return
	}
	ttl := time.Until(time.Unix(session.Expires, 0))
	if ttl <= 0 {
		http.Error(w, "OAuth token already expired", http.StatusUnauthorized)
		return
	}
	value, err := a.sessions.Seal(SessionCookieName, session, ttl)
	if err != nil {
		http.Error(w, "failed to create session", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   secureCookie(r),
		SameSite: http.SameSiteLaxMode,
	})
	target := state.OriginalURL
	if !isLocalRedirect(target) {
		target = "/"
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func (a *Authenticator) startOAuth2(w http.ResponseWriter, r *http.Request, originalURL string) {
	oauthConfig, _, err := a.oauth2Config(r.Context(), r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	nonce, err := randomToken(32)
	if err != nil {
		http.Error(w, "failed to generate OAuth state", http.StatusInternalServerError)
		return
	}
	state := oauthState{Nonce: nonce, OriginalURL: originalURL}
	cookieValue, err := a.sessions.Seal(StateCookieName, state, 10*time.Minute)
	if err != nil {
		http.Error(w, "failed to create OAuth state", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     StateCookieName,
		Value:    cookieValue,
		Path:     CallbackPath,
		MaxAge:   int((10 * time.Minute).Seconds()),
		HttpOnly: true,
		Secure:   secureCookie(r),
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, oauthConfig.AuthCodeURL(nonce), http.StatusFound)
}

func (a *Authenticator) oauth2Config(ctx context.Context, r *http.Request) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	if a.cfg.OIDCClientID == "" {
		return nil, nil, errors.New("missing OIDC/OAuth2 client ID")
	}
	// Redirect-URL precedence: an explicit oidc.redirectUrl wins; otherwise a
	// configured publicUrl (origin only) gives a stable callback that does not
	// depend on per-request headers; otherwise fall back to reconstructing the
	// external URL from the request (Host / X-Forwarded-*), a dev convenience.
	redirectURL := a.cfg.OIDCRedirectURL
	if redirectURL == "" && a.cfg.PublicURL != "" {
		redirectURL = a.cfg.PublicURL + CallbackPath
	}
	if redirectURL == "" {
		redirectURL = externalURL(r, CallbackPath)
	}
	scopes := strings.Fields(a.cfg.OAuth2Scope)
	var endpoint oauth2.Endpoint
	var verifier *oidc.IDTokenVerifier
	switch {
	case a.cfg.OIDCIssuerURL != "":
		provider, err := a.oidcDiscover()
		if err != nil {
			return nil, nil, fmt.Errorf("OIDC discovery failed: %w", err)
		}
		endpoint = provider.Endpoint()
		if len(scopes) == 0 {
			scopes = []string{oidc.ScopeOpenID, "email", "profile"}
		}
		verifier = provider.Verifier(&oidc.Config{ClientID: a.cfg.OIDCClientID})
	case a.cfg.OAuth2AuthorizeURL != "" && a.cfg.OAuth2TokenURL != "":
		endpoint = oauth2.Endpoint{
			AuthURL:  a.cfg.OAuth2AuthorizeURL,
			TokenURL: a.cfg.OAuth2TokenURL,
		}
	default:
		return nil, nil, errors.New("missing OIDC issuer URL or generic OAuth2 authorize/token URLs")
	}
	return &oauth2.Config{
		ClientID:     a.cfg.OIDCClientID,
		ClientSecret: a.cfg.OIDCClientSecret,
		Endpoint:     endpoint,
		RedirectURL:  redirectURL,
		Scopes:       scopes,
	}, verifier, nil
}

// oidcDiscover returns the process-wide cached OIDC provider, building it on
// first use and on every prior failure. Construction is pinned to
// context.Background() on purpose: go-oidc binds the remote key-set fetcher used
// by every later token verification to the context passed here, so a request
// context (which can be canceled when the client disconnects) would later break
// verification for unrelated requests. A failed construction is not stored, so
// the next caller retries discovery rather than inheriting a poisoned cache from
// a transient issuer outage.
func (a *Authenticator) oidcDiscover() (*oidc.Provider, error) {
	a.oidcMu.Lock()
	defer a.oidcMu.Unlock()
	if a.oidcProvider != nil {
		return a.oidcProvider, nil
	}
	provider, err := oidc.NewProvider(context.Background(), a.cfg.OIDCIssuerURL)
	if err != nil {
		return nil, err
	}
	a.oidcProvider = provider
	return provider, nil
}

// Session opens the sealed session cookie and returns it when present, decodable,
// non-empty (has an access token), and unexpired by its own Expires field.
func (a *Authenticator) Session(r *http.Request) (Session, bool) {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil {
		return Session{}, false
	}
	var session Session
	if err := a.sessions.Open(SessionCookieName, cookie.Value, &session); err != nil {
		return Session{}, false
	}
	if session.AccessToken == "" || session.Expires <= a.now().Unix() {
		return Session{}, false
	}
	return session, true
}

// RequestBearer resolves the viewer's bearer token: the Authorization header
// wins, otherwise the access token from a valid sealed session, otherwise empty
// (anonymous). The legacy access_token cookie is deliberately not a source.
func (a *Authenticator) RequestBearer(r *http.Request) string {
	authz := r.Header.Get("Authorization")
	if strings.HasPrefix(authz, "Bearer ") {
		return strings.TrimPrefix(authz, "Bearer ")
	}
	if session, ok := a.Session(r); ok {
		return session.AccessToken
	}
	return ""
}

// IsPublicPath reports whether a path bypasses auth (health/metrics probes, the
// OAuth routes, and static assets).
func (a *Authenticator) IsPublicPath(path string) bool {
	return path == "/health" ||
		path == "/healthz" ||
		path == "/readyz" ||
		path == "/metrics" ||
		path == CallbackPath ||
		path == "/oauth2/login" ||
		path == "/oauth2/logout" ||
		strings.HasPrefix(path, "/assets/")
}

// SealSession seals a session into a cookie value bound to the session cookie
// name, for callers that mint a session outside the callback flow. The session
// is taken by pointer (it is a heavy value).
func (a *Authenticator) SealSession(session *Session, ttl time.Duration) (string, error) {
	return a.sessions.Seal(SessionCookieName, session, ttl)
}

func oauthExpiry(token *oauth2.Token) time.Time {
	expires := token.Expiry
	if expires.IsZero() {
		expires = time.Now().Add(7 * 24 * time.Hour)
	}
	remaining := time.Until(expires)
	if remaining > 10*time.Minute {
		expires = expires.Add(-5 * time.Minute)
	}
	return expires
}

func externalURL(r *http.Request, path string) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(firstForwarded(r.Header.Get("X-Forwarded-Proto")), "https") {
		scheme = "https"
	}
	host := firstForwarded(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host + path
}

func firstForwarded(value string) string {
	part, _, _ := strings.Cut(value, ",")
	return strings.TrimSpace(part)
}

func clearCookie(w http.ResponseWriter, r *http.Request, name, path string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     path,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secureCookie(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func secureCookie(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(firstForwarded(r.Header.Get("X-Forwarded-Proto")), "https")
}

func randomToken(n int) (string, error) {
	data := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

// isLocalRedirect reports whether target is a safe same-origin redirect: a
// rooted path that is not protocol-relative or backslash-smuggled to an external
// host.
func isLocalRedirect(target string) bool {
	return strings.HasPrefix(target, "/") &&
		!strings.HasPrefix(target, "//") &&
		!strings.HasPrefix(target, `/\`)
}

// first returns the first non-empty value, or "" when all are empty.
func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
