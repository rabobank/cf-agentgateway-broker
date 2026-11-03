package plugin

import (
	"crypto/ed25519"
	"encoding/json"
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
	metadata, err := json.Marshal(struct {
		Identity
		Phase    string   `json:"phase"`
		Tool     string   `json:"tool"`
		Backends []string `json:"backends"`
	}{record.Identity, record.Phase, record.Tool, record.Backends})
	if err != nil {
		return emit.Result{}, err
	}
	input := emit.Input{ActionID: record.ID, ActionType: emit.ActionTypeFYI, Operator: record.Identity.Installation, Developer: "cf-agentgateway-capsule@v1", Timestamp: record.ObservedAt, Provenance: emit.ProvenanceCollector, Extensions: []emit.Extension{{Name: "gateway", Value: metadata}}}
	input.Effect = &emit.Effect{Type: "mcp.tools.call", Status: emit.EffectStatus(record.Phase), IrreversibilityClass: emit.IrreversibilityTwoWay}
	if record.Phase != "planned" {
		input.Effect.EffectAttestation = emit.AttestationRuntimeClaimed
	}
	sealed := emit.SealInput{Capsule: input, Identity: p.identity, Runtime: "agentgateway"}
	digest, err := emit.DigestJSON(body)
	if err != nil {
		return emit.Result{}, err
	}
	if record.Parent != "" {
		sealed.Capsule.Effect.ResponseDigest = digest
	}
	if record.Parent == "" {
		sealed.Payload = body
	} else {
		sealed.AgentOutput = body
		sealed.Capsule.Chain = &emit.Chain{ParentCapsuleID: record.Parent, Relation: emit.ChainConfirms}
	}
	return emit.Seal(sealed)
}
