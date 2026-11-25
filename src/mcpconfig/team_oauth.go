package mcpconfig

import (
	"errors"
	"net/url"
	"regexp"
)

const TeamOAuthMethod = "team_oauth"

type TeamOAuth struct {
	AllowedTools    []string `json:"allowed_tools"`
	RefreshTokenURL string   `json:"refresh_token_url,omitempty"`
}

func OAuthURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == ""
}

var toolName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

func (c TeamOAuth) Validate() error {
	if c.RefreshTokenURL != "" && !OAuthURL(c.RefreshTokenURL) {
		return errors.New("OAuth refresh token endpoint must be HTTPS")
	}
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
