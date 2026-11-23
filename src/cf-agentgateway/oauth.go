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
	"os/exec"
	"runtime"
	"strings"
	"time"
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
	resp, err := oauthHTTP.Do(req)
	if err != nil {
		return errors.New("OAuth endpoint unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 128<<10)).Decode(out) != nil {
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

func login(ctx context.Context, p mcpconfig.OAuthProfile) (mcpconfig.OAuthAccess, error) {
	var none mcpconfig.OAuthAccess
	var protected struct {
		Resource string
		Servers  []string `json:"authorization_servers"`
	}
	if err := oauthJSON(ctx, metadataURL(p.Resource, "/.well-known/oauth-protected-resource"), nil, &protected); err != nil {
		return none, err
	}
	trusted := false
	for _, issuer := range protected.Servers {
		trusted = trusted || issuer == p.Issuer
	}
	if protected.Resource != p.Resource || !trusted {
		return none, errors.New("MCP discovery does not match the operator OAuth profile")
	}
	var as struct {
		Issuer      string
		Auth        string   `json:"authorization_endpoint"`
		Token       string   `json:"token_endpoint"`
		PKCE        []string `json:"code_challenge_methods_supported"`
		IssRequired bool     `json:"authorization_response_iss_parameter_supported"`
	}
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
	listener, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", p.CallbackPort))
	if err != nil {
		return none, errors.New("cannot open the OAuth loopback callback")
	}
	redirect := "http://" + listener.Addr().String() + "/callback"
	enc := base64.RawURLEncoding.EncodeToString
	state, verifier := enc(randomBytes(32)), enc(randomBytes(32))
	hash := sha256.Sum256([]byte(verifier))
	result := make(chan string, 1)
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		q := r.URL.Query()
		if r.Method != "GET" || r.URL.Path != "/callback" || len(q["state"]) != 1 || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 || (as.IssRequired || q.Get("iss") != "") && q.Get("iss") != p.Issuer {
			http.Error(w, "Invalid OAuth callback", 400)
			return
		}
		code := q.Get("code")
		if q.Get("error") != "" || len(q["code"]) != 1 {
			code = ""
		}
		fmt.Fprintln(w, "Authorization response received. Return to the terminal.")
		select {
		case result <- code:
		default:
		}
	})}
	defer server.Close()
	go server.Serve(listener)
	q := url.Values{"response_type": {"code"}, "client_id": {p.ClientID}, "redirect_uri": {redirect}, "scope": {strings.Join(p.Scopes, " ")}, "resource": {p.Resource}, "state": {state}, "code_challenge": {enc(hash[:])}, "code_challenge_method": {"S256"}}
	openBrowser(ctx, as.Auth+"?"+q.Encode())
	var code string
	select {
	case code = <-result:
	case <-ctx.Done():
		return none, errors.New("OAuth login timed out")
	}
	if code == "" {
		return none, errors.New("upstream authorization denied")
	}
	var token struct {
		Access string `json:"access_token"`
		Type   string `json:"token_type"`
		Expiry int    `json:"expires_in"`
	}
	if err := oauthJSON(ctx, as.Token, url.Values{"grant_type": {"authorization_code"}, "client_id": {p.ClientID}, "code": {code}, "redirect_uri": {redirect}, "resource": {p.Resource}, "code_verifier": {verifier}}, &token); err != nil {
		return none, err
	}
	if token.Access == "" || len(token.Access) > 8192 || !strings.EqualFold(token.Type, "bearer") || token.Expiry <= 0 || token.Expiry > 86400 {
		return none, errors.New("server did not return a usable short-lived bearer")
	}
	return mcpconfig.OAuthAccess{Token: token.Access, Expires: time.Now().UTC().Add(time.Duration(token.Expiry) * time.Second)}, nil
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
