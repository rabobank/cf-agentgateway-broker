package main

import (
	mcpconfig "cf-mcp-config"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"golang.org/x/term"
)

func randomBytes(n int) []byte { b := make([]byte, n); rand.Read(b); return b }

var oauthHTTP = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func oauthJSON(ctx context.Context, endpoint string, form url.Values, out any) error {
	method := http.MethodGet
	var body io.Reader
	if form != nil {
		method = http.MethodPost
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return errors.New("invalid OAuth endpoint")
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return readOAuthJSON(req, http.StatusOK, out)
}

func readOAuthJSON(req *http.Request, status int, out any) error {
	resp, err := oauthHTTP.Do(req)
	if err != nil {
		return errors.New("OAuth endpoint unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != status || json.NewDecoder(io.LimitReader(resp.Body, 128<<10)).Decode(out) != nil {
		return fmt.Errorf("OAuth endpoint rejected the request or returned invalid JSON (HTTP %d)", resp.StatusCode)
	}
	return nil
}

func metadataURL(resource, wellKnown string) string {
	u, _ := url.Parse(resource)
	u.Path = wellKnown + strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String()
}

var challengePart = regexp.MustCompile(`(?:[^,"]|"(?:[^"\\]|\\.)*")+`)
var challengeParam = regexp.MustCompile(`(?i)^resource_metadata\s*=\s*"([^"\\]+)"$`)

func advertisedMetadata(ctx context.Context, resource string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"cf-agentgateway","version":"1"}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, resource, strings.NewReader(body))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := oauthHTTP.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		return ""
	}
	for _, header := range resp.Header.Values("WWW-Authenticate") {
		bearer := false
		for _, part := range challengePart.FindAllString(header, -1) {
			part = strings.TrimSpace(part)
			if i := strings.IndexAny(part, " \t"); i >= 0 && !strings.Contains(part[:i], "=") && !strings.HasPrefix(strings.TrimSpace(part[i:]), "=") {
				bearer = strings.EqualFold(part[:i], "Bearer")
				part = strings.TrimSpace(part[i:])
			} else if !strings.Contains(part, "=") {
				bearer = strings.EqualFold(part, "Bearer")
			}
			if match := challengeParam.FindStringSubmatch(part); bearer && match != nil && mcpconfig.OAuthURL(match[1]) {
				return match[1]
			}
		}
	}
	return ""
}

func discoverProtectedResource(ctx context.Context, p mcpconfig.OAuthProfile) error {
	const wellKnown = "/.well-known/oauth-protected-resource"
	u, _ := url.Parse(p.Resource)
	u.Path, u.RawPath = wellKnown, ""
	seen := map[string]bool{"": true}
	for _, endpoint := range []string{advertisedMetadata(ctx, p.Resource), metadataURL(p.Resource, wellKnown), u.String()} {
		if seen[endpoint] {
			continue
		}
		seen[endpoint] = true
		var protected struct {
			Resource string
			Servers  []string `json:"authorization_servers"`
		}
		if err := oauthJSON(ctx, endpoint, nil, &protected); err != nil {
			continue
		}
		if protected.Resource != p.Resource {
			return errors.New("MCP discovery does not match the operator OAuth profile")
		}
		for _, issuer := range protected.Servers {
			if issuer == p.Issuer {
				return nil
			}
		}
		return errors.New("MCP discovery does not match the operator OAuth profile")
	}
	return errors.New("MCP protected resource metadata is unavailable")
}

type authorizationServer struct {
	Issuer       string   `json:"issuer"`
	Auth         string   `json:"authorization_endpoint"`
	Token        string   `json:"token_endpoint"`
	Registration string   `json:"registration_endpoint"`
	TokenMethods []string `json:"token_endpoint_auth_methods_supported"`
	PKCE         []string `json:"code_challenge_methods_supported"`
	IssRequired  bool     `json:"authorization_response_iss_parameter_supported"`
}

type oauthClient struct {
	ID     string `json:"client_id"`
	Secret string `json:"client_secret"`
	Method string `json:"token_endpoint_auth_method"`
}

func login(ctx context.Context, p mcpconfig.OAuthProfile, paste bool) (mcpconfig.OAuthAccess, error) {
	var none mcpconfig.OAuthAccess
	if paste && !term.IsTerminal(int(os.Stdin.Fd())) {
		return none, errors.New("--paste requires an interactive terminal for hidden input")
	}
	if err := discoverProtectedResource(ctx, p); err != nil {
		return none, err
	}
	var as authorizationServer
	if err := oauthJSON(ctx, metadataURL(p.Issuer, "/.well-known/oauth-authorization-server"), nil, &as); err != nil {
		return none, err
	}
	s256 := false
	for _, method := range as.PKCE {
		s256 = s256 || method == "S256"
	}
	if as.Issuer != p.Issuer || !mcpconfig.OAuthURL(as.Auth) || !mcpconfig.OAuthURL(as.Token) || !s256 {
		return none, errors.New("unsupported authorization server metadata")
	}
	if p.RefreshTokenURL != "" && as.Token != p.RefreshTokenURL {
		return none, errors.New("discovered token endpoint does not match the configured refresh endpoint")
	}
	port := p.CallbackPort
	var listener net.Listener
	if paste {
		if port == 0 {
			port = 19876
		}
	} else {
		var err error
		listener, err = net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return none, errors.New("cannot open the OAuth loopback callback")
		}
		defer listener.Close()
		port = listener.Addr().(*net.TCPAddr).Port
	}
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", port)
	client := oauthClient{ID: p.ClientID, Method: "none"}
	if p.Registration != nil {
		var err error
		client, err = registerClient(ctx, as.Registration, *p.Registration, redirect, strings.Join(p.Scopes, " "), p.RefreshTokenURL != "")
		if err != nil {
			return none, err
		}
	}
	// A static profile explicitly configures a pre-registered public client.
	method := "none"
	if p.Registration != nil {
		var err error
		method, err = clientAuthMethod(client, as.TokenMethods)
		if err != nil {
			return none, err
		}
	}
	enc := base64.RawURLEncoding.EncodeToString
	state, verifier := enc(randomBytes(32)), enc(randomBytes(32))
	hash := sha256.Sum256([]byte(verifier))
	q := url.Values{"response_type": {"code"}, "client_id": {client.ID}, "redirect_uri": {redirect}, "scope": {strings.Join(p.Scopes, " ")}, "resource": {p.Resource}, "state": {state}, "code_challenge": {enc(hash[:])}, "code_challenge_method": {"S256"}}
	authorize := as.Auth + "?" + q.Encode()
	callback := oauthCallback{redirect: redirect, state: state, issuer: p.Issuer, requireIssuer: as.IssRequired}
	var code string
	var err error
	if paste {
		code, err = callback.paste(ctx, authorize)
	} else {
		code, err = callback.listen(ctx, listener, authorize)
	}
	if err != nil {
		return none, err
	}
	if code == "" {
		return none, errors.New("upstream authorization denied")
	}
	return exchangeCode(ctx, as.Token, client, method, p.Resource, redirect, verifier, code, p.RefreshTokenURL != "")
}

type oauthCallback struct {
	redirect, state, issuer string
	requireIssuer           bool
}

func (c oauthCallback) parse(address string) (string, bool) {
	u, err := url.Parse(address)
	if err != nil {
		return "", false
	}
	q, err := url.ParseQuery(u.RawQuery)
	u.RawQuery, u.ForceQuery = "", false
	if err != nil || u.String() != c.redirect || len(q["state"]) != 1 || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(c.state)) != 1 {
		return "", false
	}
	if (c.requireIssuer || q.Has("iss")) && (len(q["iss"]) != 1 || q.Get("iss") != c.issuer) {
		return "", false
	}
	if len(q["error"]) == 1 && q.Get("error") != "" && !q.Has("code") {
		return "", true
	}
	return q.Get("code"), !q.Has("error") && len(q["code"]) == 1 && q.Get("code") != ""
}

func (c oauthCallback) paste(ctx context.Context, authorize string) (string, error) {
	fmt.Printf("Open this authorization URL in your browser:\n%s\n", authorize)
	for ctx.Err() == nil {
		fmt.Fprint(os.Stderr, "Paste the full callback URL (input hidden): ")
		line, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", errors.New("could not read the OAuth callback from the terminal")
		}
		if ctx.Err() == nil {
			if code, ok := c.parse(strings.TrimSpace(string(line))); ok {
				return code, nil
			}
			fmt.Fprintln(os.Stderr, "Invalid OAuth callback; paste the redirect from this login.")
		}
	}
	return "", errors.New("OAuth login timed out")
}

func (c oauthCallback) listen(ctx context.Context, listener net.Listener, authorize string) (string, error) {
	result := make(chan string, 1)
	expected, _ := url.Parse(c.redirect)
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		code, valid := c.parse("http://" + r.Host + r.URL.RequestURI())
		if r.Method != "GET" || r.Host != expected.Host || !valid {
			http.Error(w, "Invalid OAuth callback", 400)
			return
		}
		fmt.Fprintln(w, "Authorization response received. Return to the terminal.")
		select {
		case result <- code:
		default:
		}
	})}
	defer server.Close()
	go server.Serve(listener)
	openBrowser(ctx, authorize)
	select {
	case code := <-result:
		return code, nil
	case <-ctx.Done():
		return "", errors.New("OAuth login timed out")
	}
}

func exchangeCode(ctx context.Context, endpoint string, client oauthClient, method, resource, redirect, verifier, code string, refreshEnabled bool) (mcpconfig.OAuthAccess, error) {
	var none mcpconfig.OAuthAccess
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect}, "resource": {resource}, "code_verifier": {verifier}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return none, errors.New("invalid OAuth token endpoint")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mcpconfig.ApplyOAuthClientAuth(req, form, method, client.ID, client.Secret)
	encoded := form.Encode()
	req.Body, req.ContentLength = io.NopCloser(strings.NewReader(encoded)), int64(len(encoded))
	var token struct {
		Access  string  `json:"access_token"`
		Type    *string `json:"token_type"`
		Expiry  *int64  `json:"expires_in"`
		Refresh string  `json:"refresh_token"`
	}
	if err := readOAuthJSON(req, http.StatusOK, &token); err != nil {
		return none, err
	}
	if token.Access == "" || len(token.Access) > 8192 || strings.ContainsFunc(token.Access, func(r rune) bool { return r < 33 || r > 126 }) {
		return none, errors.New("OAuth token response has a missing or invalid access_token")
	}
	// Some MCP providers omit token_type. MCP uses the Bearer header; only
	// default an omitted type, never a different explicitly declared type.
	if token.Type != nil && !strings.EqualFold(*token.Type, "bearer") {
		return none, errors.New("OAuth token response declares a non-Bearer token_type")
	}
	lifetime, err := accessLifetime(token.Expiry)
	if err != nil {
		return none, err
	}
	access := mcpconfig.OAuthAccess{Token: token.Access, Expires: time.Now().UTC().Add(lifetime)}
	if refreshEnabled && token.Refresh != "" {
		refresh := mcpconfig.OAuthRefresh{Token: token.Refresh, Endpoint: endpoint, ClientID: client.ID, Method: method}
		if method != "none" {
			refresh.ClientSecret = client.Secret
		}
		if !refresh.Valid() {
			return none, errors.New("OAuth token response has unusable refresh credentials")
		}
		access.Refresh = &refresh
	}
	return access, nil
}

func accessLifetime(expiresIn *int64) (time.Duration, error) {
	return mcpconfig.OAuthAccessLifetime(expiresIn)
}

func openBrowser(ctx context.Context, address string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", address)
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", address)
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", address)
	}
	if err := cmd.Start(); err != nil {
		fmt.Printf("Open this authorization URL in your browser:\n%s\n", address)
		return
	}
	go func() {
		if err := cmd.Wait(); err != nil && ctx.Err() == nil {
			fmt.Printf("Open this authorization URL in your browser:\n%s\n", address)
		}
	}()
}
