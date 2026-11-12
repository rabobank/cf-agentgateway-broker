package plugin

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	emit "github.com/action-state-group/capsule-emit/go"
)

type SignedProducer struct{ identity emit.SigningIdentity }

func NewProducer(key ed25519.PrivateKey) (*SignedProducer, error) {
	identity, err := emit.NewEd25519SigningIdentity(key)
	if err != nil {
		return nil, err
	}
	return &SignedProducer{identity: identity}, nil
}

func (p *SignedProducer) Seal(record Capsule, body json.RawMessage) (emit.Result, error) {
	if !json.Valid(body) {
		return emit.Result{}, errors.New("MCP body must be valid JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return emit.Result{}, err
	}
	canonical, err := emit.CanonicalizeForDigest(decoded)
	if err != nil {
		return emit.Result{}, err
	}
	// RawMessage also keeps a JSON null body distinct from an absent Seal payload.
	encodedBody, err := json.Marshal(canonical)
	if err != nil {
		return emit.Result{}, err
	}
	metadata, err := json.Marshal(struct {
		Identity
		Phase        string   `json:"phase"`
		Tool         string   `json:"tool"`
		Backends     []string `json:"backends"`
		BodyEncoding string   `json:"body_encoding"`
	}{record.Identity, record.Phase, record.Tool, record.Backends, "mcp-json/canonicalize-for-digest/v1"})
	if err != nil {
		return emit.Result{}, err
	}
	input := emit.Input{ActionID: record.ID, ActionType: emit.ActionTypeFYI, Operator: record.Identity.Installation, Developer: "cf-agentgateway-capsule@v1", Timestamp: record.ObservedAt, Provenance: emit.ProvenanceCollector, Extensions: []emit.Extension{{Name: "gateway", Value: metadata}}}
	input.Effect = &emit.Effect{Type: "mcp.tools.call", Status: emit.EffectStatus(record.Phase), IrreversibilityClass: emit.IrreversibilityTwoWay}
	if record.Phase != "planned" {
		input.Effect.EffectAttestation = emit.AttestationRuntimeClaimed
	}
	sealed := emit.SealInput{Capsule: input, Identity: p.identity, Runtime: "agentgateway"}
	digest, err := emit.DigestJSON(json.RawMessage(encodedBody))
	if err != nil {
		return emit.Result{}, err
	}
	if record.Parent != "" {
		sealed.Capsule.Effect.ResponseDigest = digest
	}
	if record.Parent == "" {
		sealed.Payload = json.RawMessage(encodedBody)
	} else {
		sealed.AgentOutput = json.RawMessage(encodedBody)
		sealed.Capsule.Chain = &emit.Chain{ParentCapsuleID: record.Parent, Relation: emit.ChainConfirms}
	}
	return emit.Seal(sealed)
}
