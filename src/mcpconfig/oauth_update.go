package mcpconfig

import (
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

const OAuthProfileField = "agentgateway_mcp_oauth"

type OAuthProfile struct {
	Version      int      `json:"version"`
	Resource     string   `json:"resource"`
	Issuer       string   `json:"issuer"`
	ClientID     string   `json:"client_id"`
	Scopes       []string `json:"scopes"`
	CallbackPort int      `json:"callback_port,omitempty"`
}

func (p OAuthProfile) Validate() error {
	if p.Version != 1 || !OAuthURL(p.Resource) || !OAuthURL(p.Issuer) || p.ClientID == "" || len(p.Scopes) == 0 || p.CallbackPort < 0 || p.CallbackPort > 65535 {
		return errors.New("invalid team OAuth profile")
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
	Generation string     `json:"generation"`
	Operation  string     `json:"operation"`
	Sequence   uint64     `json:"sequence"`
	Connected  bool       `json:"connected"`
	Expires    *time.Time `json:"expires_at,omitempty"`
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
		(u.Access != nil && (u.Access.Token == "" || len(u.Access.Token) > 8192)) {
		return errors.New("invalid team OAuth update")
	}
	return nil
}

func (u OAuthUpdate) validOperation() bool {
	return ValidID(u.Generation) && ValidID(u.Operation) && u.Operation != u.Previous && (u.Previous == "" || ValidID(u.Previous)) && u.Sequence != ^uint64(0) && (u.Action == "connect" || u.Action == "disconnect")
}

type OAuthAccess struct {
	Token   string    `json:"token"`
	Expires time.Time `json:"expires"`
}
