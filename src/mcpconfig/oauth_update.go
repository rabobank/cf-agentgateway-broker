package mcpconfig

import (
	"crypto/ecdh"
	"crypto/hpke"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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
	PublicKey    string   `json:"public_key"`
}

func (p OAuthProfile) Validate() error {
	key, err := hex.DecodeString(p.PublicKey)
	if p.Version != 1 || !OAuthURL(p.Resource) || !OAuthURL(p.Issuer) || p.ClientID == "" || len(p.Scopes) == 0 || p.CallbackPort < 0 || p.CallbackPort > 65535 || err != nil || len(key) != 32 {
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
	PrivateKey string `json:"private_key"`
	StorageKey string `json:"encryption_key"`
}

func (c OAuthUpdates) Validate() error {
	priv, e := hex.DecodeString(c.PrivateKey)
	key, err := hex.DecodeString(c.StorageKey)
	if e != nil || err != nil || len(priv) != 32 || len(key) != 32 || strings.EqualFold(c.PrivateKey, c.StorageKey) {
		return errors.New("team OAuth requires distinct 32-byte hexadecimal private and storage keys")
	}
	return nil
}

func (c OAuthUpdates) PublicKey() string {
	key, _ := hex.DecodeString(c.PrivateKey)
	if private, err := ecdh.X25519().NewPrivateKey(key); err == nil {
		return hex.EncodeToString(private.PublicKey().Bytes())
	}
	return ""
}

type OAuthStatus struct {
	Generation string     `json:"generation"`
	Operation  string     `json:"operation"`
	Sequence   uint64     `json:"sequence"`
	Connected  bool       `json:"connected"`
	Expires    *time.Time `json:"expires_at,omitempty"`
}

type OAuthUpdate struct {
	Action     string `json:"action"`
	Generation string `json:"generation"`
	Previous   string `json:"previous"`
	Sequence   uint64 `json:"sequence"`
	Operation  string `json:"operation"`
	Envelope   string `json:"envelope,omitempty"`
}

func (u OAuthUpdate) Validate() error {
	if !ValidID(u.Generation) || !ValidID(u.Operation) || u.Operation == u.Previous || (u.Previous != "" && !ValidID(u.Previous)) || u.Sequence == ^uint64(0) ||
		!(u.Action == "connect" && len(u.Envelope) > 0 && len(u.Envelope) <= 16<<10 || u.Action == "disconnect" && u.Envelope == "") {
		return errors.New("invalid team OAuth update")
	}
	return nil
}

type OAuthAccess struct {
	Token   string    `json:"token"`
	Expires time.Time `json:"expires"`
}

func (u OAuthUpdate) info(instance, resource string) []byte {
	info, _ := json.Marshal([]any{"cf-agentgateway/mcp-oauth/v1", instance, resource, u.Generation, u.Previous, u.Sequence, u.Operation, u.Action})
	return info
}

func (u *OAuthUpdate) Encrypt(publicKey, instance, resource string, access OAuthAccess) error {
	key, err := hex.DecodeString(publicKey)
	if err != nil {
		return err
	}
	pub, err := ecdh.X25519().NewPublicKey(key)
	if err != nil {
		return err
	}
	pk, err := hpke.NewDHKEMPublicKey(pub)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(access)
	sealed, err := hpke.Seal(pk, hpke.HKDFSHA256(), hpke.AES256GCM(), u.info(instance, resource), body)
	if err != nil {
		return err
	}
	u.Envelope = base64.RawURLEncoding.EncodeToString(sealed)
	return nil
}

func (u OAuthUpdate) Decrypt(privateKey, instance, resource string) (OAuthAccess, error) {
	var access OAuthAccess
	key, err := hex.DecodeString(privateKey)
	if err != nil {
		return access, err
	}
	priv, err := ecdh.X25519().NewPrivateKey(key)
	if err != nil {
		return access, err
	}
	sk, err := hpke.NewDHKEMPrivateKey(priv)
	if err != nil {
		return access, err
	}
	sealed, err := base64.RawURLEncoding.DecodeString(u.Envelope)
	if err != nil {
		return access, err
	}
	body, err := hpke.Open(sk, hpke.HKDFSHA256(), hpke.AES256GCM(), u.info(instance, resource), sealed)
	if err != nil {
		return access, err
	}
	err = json.Unmarshal(body, &access)
	return access, err
}
