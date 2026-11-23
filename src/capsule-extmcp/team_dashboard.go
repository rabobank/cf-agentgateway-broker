package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const loginCookie = "__Secure-mcp-login"
const dashboardCookie = "__Secure-mcp-dashboard"
const dashboardPath = "/mcp-connections/"

type dashboardLogin struct {
	Instance string
	State    string
	Verifier string
	Expires  time.Time
}

type dashboardUser struct {
	Token   string
	CSRF    string
	Expires time.Time
}

func dashboardHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func dashboardSetCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: dashboardPath, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: maxAge})
}

func (d *teamDashboard) user(r *http.Request) (dashboardUser, bool) {
	var u dashboardUser
	c, err := r.Cookie(dashboardCookie)
	ok := err == nil && d.open(dashboardCookie, c.Value, &u) && u.Expires.After(time.Now()) && validToken(u.Token)
	return u, ok
}

func (d *teamDashboard) canManage(ctx context.Context, instance, token string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.config.CFAPI+"/v3/service_instances/"+url.PathEscape(instance)+"/permissions", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := d.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var permissions struct{ Manage bool }
	return resp.StatusCode == 200 && readJSON(resp.Body, 8192, &permissions) == nil && permissions.Manage
}

func (d *teamDashboard) startLogin(w http.ResponseWriter, r *http.Request, instance string) {
	state, err1 := nonce()
	verifier, err2 := nonce()
	if err1 != nil || err2 != nil {
		http.Error(w, "dashboard login unavailable", 503)
		return
	}
	sealed, err := d.seal(loginCookie, dashboardLogin{instance, state, verifier, time.Now().Add(5 * time.Minute)})
	if err != nil {
		http.Error(w, "dashboard login unavailable", 503)
		return
	}
	dashboardSetCookie(w, loginCookie, sealed, 300)
	c := d.config.Login
	challenge := sha256.Sum256([]byte(verifier))
	query := url.Values{"response_type": {"code"}, "client_id": {c.ClientID}, "redirect_uri": {d.config.PublicURL + dashboardPath + "callback"},
		"state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}, "scope": {strings.Join(c.Scopes, " ")}}
	http.Redirect(w, r, c.AuthorizationURL+"?"+query.Encode(), http.StatusSeeOther)
}

func (d *teamDashboard) loginCallback(w http.ResponseWriter, r *http.Request) {
	dashboardHeaders(w)
	var s dashboardLogin
	cookie, err := r.Cookie(loginCookie)
	c := d.config.Login
	q := r.URL.Query()
	dashboardSetCookie(w, loginCookie, "", -1)
	if err != nil || !d.open(loginCookie, cookie.Value, &s) || !s.Expires.After(time.Now()) || !validToken(q.Get("code")) ||
		subtle.ConstantTimeCompare([]byte(s.State), []byte(q.Get("state"))) != 1 || (c.Issuer != "" && q.Get("iss") != c.Issuer) {
		http.Error(w, "dashboard login rejected", 403)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	form := url.Values{"grant_type": {"authorization_code"}, "code": {q.Get("code")}, "code_verifier": {s.Verifier}, "redirect_uri": {d.config.PublicURL + dashboardPath + "callback"}}
	if c.ClientAuth == "client_secret_post" {
		form.Set("client_id", c.ClientID)
		form.Set("client_secret", c.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		http.Error(w, "dashboard login unavailable", 503)
		return
	}
	if c.ClientAuth == "client_secret_basic" {
		req.SetBasicAuth(url.QueryEscape(c.ClientID), url.QueryEscape(c.ClientSecret))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := d.client.Do(req)
	if err != nil {
		http.Error(w, "dashboard login unavailable", 503)
		return
	}
	defer resp.Body.Close()
	var token struct {
		AccessToken string `json:"access_token"`
		Type        string `json:"token_type"`
		Expires     int    `json:"expires_in"`
	}
	if resp.StatusCode != 200 || readJSON(resp.Body, 64<<10, &token) != nil || !strings.EqualFold(token.Type, "bearer") || !validToken(token.AccessToken) || token.Expires <= 0 {
		http.Error(w, "dashboard login rejected", 403)
		return
	}
	i, ok := d.instance(ctx, s.Instance)
	if !ok || !d.canManage(ctx, i.ID, token.AccessToken) {
		http.Error(w, "instance access denied", 403)
		return
	}
	csrf, err := nonce()
	if err != nil {
		http.Error(w, "dashboard login unavailable", 503)
		return
	}
	lifetime := min(token.Expires, 900)
	sealed, err := d.seal(dashboardCookie, dashboardUser{token.AccessToken, csrf, time.Now().Add(time.Duration(lifetime) * time.Second)})
	if err != nil || len(sealed) > 3700 {
		http.Error(w, "dashboard session unavailable", 503)
		return
	}
	dashboardSetCookie(w, dashboardCookie, sealed, lifetime)
	http.Redirect(w, r, dashboardPath+s.Instance, http.StatusSeeOther)
}

var connectionPage = template.Must(template.New("connections").Parse(`<!doctype html>
<html lang="en"><meta charset="utf-8"><title>MCP connection</title>
<h1>MCP connection: {{.Backend}}</h1>
<p>{{if .Expiry}}Connected until {{.Expiry}}.{{else}}Not connected.{{end}}</p>
<p>Import an access token issued for <code>{{.Endpoint}}</code> using an approved client. The connection is shared by this service instance's bindings.</p>
<form method="post" autocomplete="off">
<input type="hidden" name="csrf" value="{{.CSRF}}">
<label>Access token <input required type="password" name="token" maxlength="8192" autocomplete="off"></label>
<label>Expiry (RFC 3339, UTC) <input required name="expires" placeholder="2026-11-20T14:00:00Z"></label>
<button name="action" value="connect">Import token</button></form>
<form method="post"><input type="hidden" name="csrf" value="{{.CSRF}}"><button name="action" value="disconnect">Disconnect</button></form>
</html>`))

func (d *teamDashboard) dashboard(w http.ResponseWriter, r *http.Request) {
	dashboardHeaders(w)
	id := r.PathValue("instance")
	u, ok := d.user(r)
	if !ok {
		d.startLogin(w, r, id)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	i, ok := d.instance(ctx, id)
	if !ok || !d.canManage(ctx, i.ID, u.Token) {
		http.Error(w, "instance access denied", 403)
		return
	}
	s, err := d.snapshot(ctx, oauthKind)
	if err != nil {
		http.Error(w, "connection status unavailable", 503)
		return
	}
	expiry := ""
	if token, ok := d.usable(s, id, i); ok {
		expiry = token.Expires.UTC().Format(time.RFC3339)
	}
	backend := i.Selections[0].Name
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = connectionPage.Execute(w, map[string]string{"Backend": backend, "Endpoint": d.backends[backend].endpoint, "CSRF": u.CSRF, "Expiry": expiry})
}

func (d *teamDashboard) changeConnection(w http.ResponseWriter, r *http.Request) {
	dashboardHeaders(w)
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	u, ok := d.user(r)
	if !ok || r.Header.Get("Origin") != d.config.PublicURL || r.ParseForm() != nil || subtle.ConstantTimeCompare([]byte(u.CSRF), []byte(r.PostForm.Get("csrf"))) != 1 {
		http.Error(w, "dashboard request denied", 403)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	// Read the generation before checking the parent. A concurrent OSB delete
	// or another dashboard write invalidates our conditional write.
	s, err := d.snapshot(ctx, oauthKind)
	if err != nil {
		http.Error(w, "connection unavailable", 503)
		return
	}
	id := r.PathValue("instance")
	i, ok := d.instance(ctx, id)
	if !ok || !d.canManage(ctx, i.ID, u.Token) {
		http.Error(w, "instance access denied", 403)
		return
	}
	c := oauthConnection{Name: id, Generation: i.Generation, Backend: i.Selections[0].Name, Revision: i.Selections[0].Revision}
	switch r.PostForm.Get("action") {
	case "connect":
		token := strings.TrimSpace(r.PostForm.Get("token"))
		expires, err := time.Parse(time.RFC3339, r.PostForm.Get("expires"))
		now := time.Now()
		if !validToken(token) || err != nil || !expires.After(now) || expires.After(now.Add(24*time.Hour)) {
			http.Error(w, "an access token and expiry within 24 hours are required", 400)
			return
		}
		epoch, err := nonce()
		if err == nil {
			c.Sealed, err = d.seal(connectionPurpose(c), upstreamSecret{token, expires, epoch})
		}
		if err != nil {
			http.Error(w, "connection unavailable", 503)
			return
		}
	case "disconnect":
	default:
		http.Error(w, "unknown dashboard action", 400)
		return
	}
	if !d.writeConnection(ctx, s, c) {
		http.Error(w, "connection changed or is unavailable; reload and retry", 409)
		return
	}
	http.Redirect(w, r, dashboardPath+id, http.StatusSeeOther)
}
