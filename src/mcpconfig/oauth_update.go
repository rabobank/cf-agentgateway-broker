package mcpconfig

import (
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const OAuthProfileField = "agentgateway_mcp_oauth"

type OAuthProfile struct {
	Version         int                `json:"version"`
	Resource        string             `json:"resource"`
	Issuer          string             `json:"issuer"`
	ClientID        string             `json:"client_id,omitempty"`
	Registration    *OAuthRegistration `json:"registration,omitempty"`
	Scopes          []string           `json:"scopes"`
	CallbackPort    int                `json:"callback_port,omitempty"`
	RefreshTokenURL string             `json:"refresh_token_url,omitempty"`
}

// OAuthRegistration opts the CLI into RFC 7591 dynamic registration.
type OAuthRegistration struct {
	ClientName string `json:"client_name"`
	ClientURI  string `json:"client_uri,omitempty"`
}

func (p OAuthProfile) Validate() error {
	if p.Version != 1 || !OAuthURL(p.Resource) || !OAuthURL(p.Issuer) || len(p.Scopes) == 0 || p.CallbackPort < 0 || p.CallbackPort > 65535 || (p.ClientID != "") == (p.Registration != nil) {
		return errors.New("invalid team OAuth profile")
	}
	if p.RefreshTokenURL != "" && !OAuthURL(p.RefreshTokenURL) {
		return errors.New("OAuth refresh token endpoint must be HTTPS")
	}
	if r := p.Registration; r != nil && (strings.TrimSpace(r.ClientName) == "" || (r.ClientURI != "" && !OAuthURL(r.ClientURI))) {
		return errors.New("dynamic OAuth registration requires a client name and an HTTPS client URI if supplied")
	}
	for _, scope := range p.Scopes {
		if scope == "" || strings.ContainsAny(scope, " \r\n\t") {
			return errors.New("invalid team OAuth scope")
		}
	}
	return nil
}

type OAuthUpdates struct {
	StorageKey string `json:"encryption_key"`
}

func (c OAuthUpdates) Validate() error {
	key, err := hex.DecodeString(c.StorageKey)
	if err != nil || len(key) != 32 {
		return errors.New("team OAuth requires a 32-byte hexadecimal storage key")
	}
	return nil
}

type OAuthStatus struct {
	Generation  string     `json:"generation"`
	Operation   string     `json:"operation"`
	Sequence    uint64     `json:"sequence"`
	Connected   bool       `json:"connected"`
	Expires     *time.Time `json:"expires_at,omitempty"`
	Refreshable bool       `json:"refreshable,omitempty"`
}

type OAuthUpdate struct {
	Resource   string       `json:"resource"`
	Action     string       `json:"action"`
	Generation string       `json:"generation"`
	Previous   string       `json:"previous"`
	Sequence   uint64       `json:"sequence"`
	Operation  string       `json:"operation"`
	Access     *OAuthAccess `json:"access,omitempty"`
}

func (u OAuthUpdate) Validate() error {
	if !u.validOperation() || !OAuthURL(u.Resource) || (u.Action == "connect") != (u.Access != nil) ||
		(u.Access != nil && (!ValidOAuthToken(u.Access.Token) || (u.Access.Refresh != nil && !u.Access.Refresh.Valid()))) {
		return errors.New("invalid team OAuth update")
	}
	return nil
}

func (u OAuthUpdate) validOperation() bool {
	return ValidID(u.Generation) && ValidID(u.Operation) && u.Operation != u.Previous && (u.Previous == "" || ValidID(u.Previous)) && u.Sequence != ^uint64(0) && (u.Action == "connect" || u.Action == "disconnect")
}

type OAuthAccess struct {
	Token   string        `json:"token"`
	Expires time.Time     `json:"expires"`
	Refresh *OAuthRefresh `json:"refresh,omitempty"`
}

type OAuthRefresh struct {
	Token        string `json:"token"`
	Endpoint     string `json:"endpoint"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret,omitempty"`
	Method       string `json:"method"`
}

func (r OAuthRefresh) Valid() bool {
	return OAuthURL(r.Endpoint) && ValidOAuthToken(r.Token) && ValidOAuthToken(r.ClientID) &&
		((r.Method == "none" && r.ClientSecret == "") || ((r.Method == "client_secret_basic" || r.Method == "client_secret_post") && ValidOAuthToken(r.ClientSecret)))
}

func ValidOAuthToken(s string) bool {
	return s != "" && len(s) <= 8192 && !strings.ContainsFunc(s, func(r rune) bool { return r < 33 || r > 126 })
}

func OAuthAccessLifetime(expiresIn *int64) (time.Duration, error) {
	// RFC 6749 recommends expires_in but permits providers to omit it.
	const maxSeconds = int64(24 * time.Hour / time.Second)
	if expiresIn == nil {
		return 24 * time.Hour, nil
	}
	if *expiresIn <= 0 {
		return 0, errors.New("OAuth token response declares a non-positive expires_in")
	}
	return time.Duration(min(*expiresIn, maxSeconds)) * time.Second, nil
}

func ApplyOAuthClientAuth(r *http.Request, form url.Values, method, id, secret string) {
	if method == "client_secret_basic" {
		r.SetBasicAuth(url.QueryEscape(id), url.QueryEscape(secret))
	} else {
		form.Set("client_id", id)
		if method == "client_secret_post" {
			form.Set("client_secret", secret)
		}
	}
}
