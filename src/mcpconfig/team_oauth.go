package mcpconfig

import (
	"encoding/hex"
	"errors"
	"net/url"
	"regexp"
	"strings"
)

const TeamOAuthMethod = "team_oauth"

type OAuthClient struct {
	AuthorizationURL string   `json:"authorization_url"`
	TokenURL         string   `json:"token_url"`
	ClientID         string   `json:"client_id"`
	ClientSecret     string   `json:"client_secret"`
	ClientAuth       string   `json:"client_auth"`
	Scopes           []string `json:"scopes"`
	Issuer           string   `json:"issuer,omitempty"`
}

type TeamOAuth struct {
	AllowedTools []string `json:"allowed_tools"`
}

type OAuthDashboard struct {
	PublicURL string      `json:"public_url"`
	CFAPI     string      `json:"cf_api"`
	Key       string      `json:"encryption_key"`
	Login     OAuthClient `json:"login"`
}

func OAuthURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == ""
}

func (c OAuthClient) Validate() error {
	if !OAuthURL(c.AuthorizationURL) || !OAuthURL(c.TokenURL) || c.ClientID == "" || c.ClientSecret == "" ||
		(c.ClientAuth != "client_secret_basic" && c.ClientAuth != "client_secret_post") || len(c.Scopes) == 0 ||
		(c.Issuer != "" && !OAuthURL(c.Issuer)) {
		return errors.New("OAuth requires HTTPS endpoints, client credentials, an explicit client auth method and scopes")
	}
	for _, scope := range c.Scopes {
		if scope == "" || strings.ContainsAny(scope, " \r\n\t") {
			return errors.New("invalid OAuth scope")
		}
	}
	return nil
}

func (d OAuthDashboard) Validate() error {
	key, err := hex.DecodeString(d.Key)
	if err != nil || len(key) != 32 || !OAuthOrigin(d.PublicURL) || !OAuthOrigin(d.CFAPI) {
		return errors.New("OAuth dashboard requires HTTPS origins and a 32-byte hexadecimal encryption key")
	}
	if err := d.Login.Validate(); err != nil {
		return err
	}
	for _, scope := range d.Login.Scopes {
		if scope == "cloud_controller_service_permissions.read" {
			return nil
		}
	}
	return errors.New("dashboard OAuth client requires cloud_controller_service_permissions.read")
}

func OAuthOrigin(raw string) bool {
	if !OAuthURL(raw) {
		return false
	}
	u, _ := url.Parse(raw)
	return u.Path == "" || u.Path == "/"
}

var toolName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

func (c TeamOAuth) Validate() error {
	if len(c.AllowedTools) == 0 {
		return errors.New("team OAuth requires an explicit tool allowlist")
	}
	seen := map[string]bool{}
	for _, tool := range c.AllowedTools {
		if !toolName.MatchString(tool) || seen[tool] {
			return errors.New("invalid or duplicate allowed MCP tool")
		}
		seen[tool] = true
	}
	return nil
}
