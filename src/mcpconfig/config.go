// Package mcpconfig defines provider-neutral operator configuration and persisted
// MCP grants. No credential acquisition or cloud-specific defaults live here.
package mcpconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
)

type Backend struct {
	Name        string            `json:"name"`
	PlanID      string            `json:"plan_id,omitempty"`
	Description string            `json:"description,omitempty"`
	URL         string            `json:"url"`
	Headers     map[string]string `json:"headers,omitempty"`
	Auth        Auth              `json:"auth,omitempty"`
}
type Auth struct {
	Method            string            `json:"method,omitempty"`
	Issuer            string            `json:"issuer,omitempty"`
	JWKSURL           string            `json:"jwks_url,omitempty"`
	Audience          string            `json:"audience,omitempty"`
	TokenEndpoint     string            `json:"token_endpoint,omitempty"`
	Scopes            []string          `json:"scopes,omitempty"`
	Parameters        map[string]string `json:"parameters,omitempty"`
	IdentityReference string            `json:"identity_reference,omitempty"`
}
type SelectedIdentity struct {
	BackendID         string `json:"backend_id"`
	AuthMethod        string `json:"auth_method"`
	IdentityReference string `json:"identity_reference,omitempty"`
	ClientID          string `json:"client_id,omitempty"`
	Authority         string `json:"authority,omitempty"`
}
type Account struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}
type Instance struct {
	Generation   string            `json:"generation"`
	ID           string            `json:"id"`
	Installation string            `json:"installation"`
	ServiceID    string            `json:"service_id"`
	PlanID       string            `json:"plan_id"`
	Org          string            `json:"org"`
	Space        string            `json:"space"`
	Fingerprint  string            `json:"fingerprint"`
	Parameters   map[string]string `json:"parameters,omitempty"`
	Backend      Backend           `json:"backend"`
	Revoked      bool              `json:"revoked,omitempty"`
}
type Binding struct {
	Generation string   `json:"generation"`
	ID         string   `json:"id"`
	Instance   Instance `json:"instance"`
	Account    Account  `json:"account"`
	Revoked    bool     `json:"revoked,omitempty"`
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func ValidID(s string) bool { return identifier.MatchString(s) }

func validHeader(name, value string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			continue
		}
		return false
	}
	for i := 0; i < len(value); i++ {
		if c := value[i]; c < 32 && c != '\t' || c == 127 {
			return false
		}
	}
	return true
}

// ManagedID is the stable gateway resource key shared by lifecycle translation
// and authorization. An empty binding identifies the parent instance resource.
func ManagedID(installation, instance, binding string) string {
	data, _ := json.Marshal([]string{installation, instance, binding})
	hash := sha256.Sum256(data)
	return "cf-mcp-" + hex.EncodeToString(hash[:])
}
func secureURL(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Fragment == "" && u.RawQuery == ""
}
func Parse(raw string) ([]Backend, error) {
	if raw == "" {
		raw = "[]"
	}
	var result []Backend
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&result); err != nil {
		return nil, JSONError("MCP_SERVERS", err)
	}
	if result == nil {
		return nil, errors.New("MCP_SERVERS must be an array of backend definitions, not null")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, errors.New("unexpected trailing backend configuration")
	}
	if err := Validate(result); err != nil {
		return nil, err
	}
	return result, nil
}
func Validate(backends []Backend) (result error) {
	index := 0
	defer func() {
		if result != nil {
			result = fmt.Errorf("MCP_SERVERS[%d]: %w", index, result)
		}
	}()
	names, plans := map[string]bool{}, map[string]bool{}
	for i, b := range backends {
		index = i
		u, e := url.Parse(b.URL)
		if !ValidID(b.Name) {
			return errors.New("name must be a 1..128 character identifier using letters, digits, underscores or hyphens, starting with a letter or digit")
		}
		if names[b.Name] {
			return fmt.Errorf("duplicate backend name %q", b.Name)
		}
		if e != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Fragment != "" {
			return errors.New("url must be an HTTP(S) URL with a host and without credentials or fragment")
		}
		names[b.Name] = true
		for k, v := range b.Headers {
			if !validHeader(k, v) {
				return errors.New("invalid upstream header")
			}
		}
		if b.PlanID == "" {
			if b.Auth.Method != "" {
				return errors.New("auth method requires a plan_id")
			}
			continue
		}
		if u.Scheme != "https" {
			return errors.New("plan MCP endpoints must use HTTPS")
		}
		if !ValidID(b.PlanID) {
			return errors.New("plan_id must be a 1..128 character identifier using letters, digits, underscores or hyphens, starting with a letter or digit")
		}
		if plans[b.PlanID] {
			return fmt.Errorf("duplicate plan_id %q", b.PlanID)
		}
		plans[b.PlanID] = true
		for _, field := range []struct{ name, value string }{{"auth.issuer", b.Auth.Issuer}, {"auth.jwks_url", b.Auth.JWKSURL}} {
			if !secureURL(field.value) {
				return fmt.Errorf("%s must be an HTTPS URL with a host and without credentials, query or fragment", field.name)
			}
		}
		if b.Auth.Audience == "" {
			return errors.New("auth.audience is required")
		}
		switch b.Auth.Method {
		case "none", "static", "passthrough":
			if b.Auth.TokenEndpoint != "" || len(b.Auth.Parameters) > 0 || len(b.Auth.Scopes) > 0 {
				return errors.New("exchange parameters require federation")
			}
			if b.Auth.Method == "static" {
				found := false
				for k := range b.Headers {
					if strings.EqualFold(k, "authorization") {
						found = true
					}
				}
				if !found {
					return errors.New("static auth requires an explicit authorization header")
				}
			}
		case "federated_client_credentials":
			if len(b.Auth.Scopes) == 0 {
				return errors.New("federation requires scopes")
			}
			probe := b.Auth.TokenEndpoint
			for k, pattern := range b.Auth.Parameters {
				if !ValidID(k) || k == "client_id" {
					return errors.New("invalid exchange parameter name")
				}
				if _, e := regexp.Compile(pattern); e != nil {
					return errors.New("invalid parameter validation pattern")
				}
				probe = strings.ReplaceAll(probe, "{"+k+"}", "example")
			}
			u, e := url.Parse(b.Auth.TokenEndpoint)
			// Parameters can substitute only path segments, never authority or scheme.
			if e != nil || strings.ContainsAny(u.Host, "{}") || !secureURL(probe) || strings.ContainsAny(probe, "{}") {
				return errors.New("token endpoint must have a fixed HTTPS authority and declared path parameters")
			}
		default:
			return errors.New("unsupported MCP auth method")
		}
		for k := range b.Headers {
			if strings.EqualFold(k, "authorization") && b.Auth.Method != "static" {
				return errors.New("authorization header requires static auth")
			}
		}
	}
	return nil
}
func (b Backend) Resolve(params map[string]any) (map[string]string, error) {
	out := map[string]string{}
	if b.Auth.Method != "federated_client_credentials" {
		if len(params) > 0 {
			return nil, errors.New("this plan accepts no instance parameters")
		}
		return out, nil
	}
	for k, v := range params {
		s, ok := v.(string)
		if !ok || s == "" {
			return nil, fmt.Errorf("instance parameter %q must be a nonempty string", k)
		}
		out[k] = s
	}
	if !ValidID(out["client_id"]) {
		return nil, errors.New("client_id is required and must be an identifier")
	}
	for k, pattern := range b.Auth.Parameters {
		s := out[k]
		ok, e := regexp.MatchString("^(?:"+pattern+")$", s)
		if e != nil || !ok || s == "" || strings.ContainsAny(s, "/\\?#%{}\r\n") || s == "." || s == ".." {
			return nil, fmt.Errorf("invalid parameter %s", k)
		}
	}
	for k := range out {
		if k != "client_id" {
			if _, ok := b.Auth.Parameters[k]; !ok {
				return nil, fmt.Errorf("unknown instance parameter %q; use client_id and operator-declared identity parameters only", k)
			}
		}
	}
	return out, nil
}
func (b Backend) Endpoint(params map[string]string) string {
	endpoint := b.Auth.TokenEndpoint
	for k := range b.Auth.Parameters {
		endpoint = strings.ReplaceAll(endpoint, "{"+k+"}", url.PathEscape(params[k]))
	}
	return endpoint
}
func (b Binding) RouteName() string {
	data, _ := json.Marshal([]string{b.Instance.Installation, b.Instance.ID, b.ID})
	digest := sha256.Sum256(data)
	return "cf-mcp-route-" + hex.EncodeToString(digest[:])
}

// TargetName remains generation-specific so replacing a stable route cannot
// authorize an old route generation or resume an earlier binding's MCP session.
func (b Binding) TargetName() string {
	data, _ := json.Marshal([]string{b.Instance.Installation, b.Instance.ID, b.ID, b.Instance.Generation, b.Generation})
	digest := sha256.Sum256(data)
	return "cf-mcp-" + hex.EncodeToString(digest[:])
}
func (b Binding) Path() string { return "/mcp/" + b.Instance.ID + "/" + b.ID }
func (b Binding) Selected() SelectedIdentity {
	backend := b.Instance.Backend
	return SelectedIdentity{BackendID: backend.Name, AuthMethod: backend.Auth.Method, IdentityReference: backend.Auth.IdentityReference, ClientID: b.Instance.Parameters["client_id"], Authority: backend.Endpoint(b.Instance.Parameters)}
}
func (b Binding) Route() map[string]any {
	backend := b.Instance.Backend
	headers := map[string]string{}
	for k, v := range backend.Headers {
		headers[k] = v
	}
	modifier := map[string]any{"set": headers}
	targetPolicies := map[string]any{"requestHeaderModifier": modifier}
	switch backend.Auth.Method {
	case "none":
		modifier["remove"] = []string{"authorization"}
	case "passthrough": // The frontend validated and preserved the original token.
	case "federated_client_credentials":
		u, _ := url.Parse(backend.Endpoint(b.Instance.Parameters))
		targetPolicies["backendAuth"] = map[string]any{"oauthTokenExchange": map[string]any{
			"host": u.Scheme + "://" + u.Host, "path": u.EscapedPath(),
			"grantType": "federatedClientCredentials", "subjectToken": map[string]any{"tokenType": "urn:ietf:params:oauth:token-type:jwt"},
			"scopes": backend.Auth.Scopes, "clientAuth": map[string]any{"method": "clientSecretPost", "clientId": b.Instance.Parameters["client_id"]},
		}}
	}
	quote := func(s string) string { v, _ := json.Marshal(s); return string(v) }
	body := `{"binding_id":` + quote(b.ID) + `,"instance_id":` + quote(b.Instance.ID) + `,"target":` + quote(b.TargetName()) + `,"issuer":jwt.iss,"subject":jwt.sub}`
	policies := map[string]any{
		"jwtAuth":       map[string]any{"mode": "strict", "preserveToken": true, "issuer": b.Account.Issuer, "audiences": []string{backend.Auth.Audience}, "jwks": map[string]any{"url": backend.Auth.JWKSURL}, "jwtValidationOptions": map[string]any{"requiredClaims": []string{"exp", "sub", "iss", "aud"}}},
		"authorization": map[string]any{"rules": []string{"jwt.sub == " + quote(b.Account.Subject)}},
		"extAuthz":      map[string]any{"host": "127.0.0.1:50052", "failureMode": "deny", "protocol": map[string]any{"http": map[string]any{"path": "'/authorize-binding'", "body": body, "metadata": map[string]string{"binding": "json(response.body)"}}}},
		"mcpGuardrails": map[string]any{"processors": []any{map[string]any{"kind": "remote", "host": "127.0.0.1:50051", "failureMode": "failClosed", "methods": map[string]string{"tools/call": "full"}, "metadata": map[string]string{"binding": "extauthz.binding", "capsule_plan": "has(mcpGuardrails.capsule_plan) ? mcpGuardrails.capsule_plan : ''"}}}},
	}
	return map[string]any{"name": b.RouteName(), "gateways": []string{"inference"}, "matches": []any{map[string]any{"path": map[string]any{"exact": b.Path()}}}, "policies": policies, "backends": []any{map[string]any{"mcp": map[string]any{"targets": []any{map[string]any{"name": b.TargetName(), "mcp": map[string]string{"host": backend.URL}, "policies": targetPolicies}}}}}}
}
