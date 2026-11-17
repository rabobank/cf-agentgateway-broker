package mcpconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// DecodeOperator validates the integration envelope without interpreting native
// provider schemas. Do not include values (which may contain secrets) in errors.
func DecodeOperator(data []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return JSONError("AGENTGATEWAY_CONFIG", err)
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("AGENTGATEWAY_CONFIG: unexpected trailing data")
	}
	return nil
}

type CallerAuth struct {
	Issuer    string   `json:"issuer"`
	Audiences []string `json:"audiences"`
	JWKS      struct {
		URL string `json:"url"`
	} `json:"jwks"`
}
type SharedBackend struct {
	Name     string `json:"name"`
	Revision string `json:"revision"`
	MCP      struct {
		Targets []map[string]any `json:"targets"`
	} `json:"mcp"`
}
type BackendSelection struct {
	Name         string                      `json:"name"`
	Revision     string                      `json:"revision"`
	Parameters   map[string]string           `json:"parameters"`
	TargetPrefix string                      `json:"targetPrefix"`
	Identities   map[string]SelectedIdentity `json:"identities,omitempty"`
}
type GatewayOperator struct {
	Database struct {
		ServiceName string `json:"service_name"`
	} `json:"database"`
	LLM struct {
		Providers []json.RawMessage `json:"providers"`
		Models    []json.RawMessage `json:"models"`
	} `json:"llm"`
	ModelCatalog json.RawMessage `json:"model_catalog"`
	MCP          struct {
		SessionKey string          `json:"session_key"`
		CallerAuth CallerAuth      `json:"caller_auth"`
		Backends   []SharedBackend `json:"backends"`
	} `json:"mcp"`
	Capsules CapsuleOperator `json:"capsules"`
}
type CapsuleOperator struct {
	LogID       string `json:"log_id"`
	SigningSeed string `json:"signing_seed"`
	Witness     struct {
		URL       string `json:"url"`
		PublicKey string `json:"public_key"`
	} `json:"witness"`
}
