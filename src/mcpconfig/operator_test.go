package mcpconfig

import "testing"

func TestSharedTargetIdentitiesRejectAmbiguousEffectiveNames(t *testing.T) {
	binding := Binding{Instance: Instance{Selections: []BackendSelection{
		{Name: "a-b", Identities: map[string]SelectedIdentity{"c": {ClientID: "principal-one"}}},
		{Name: "a", Identities: map[string]SelectedIdentity{"b-c": {ClientID: "principal-two"}}},
	}}}
	if _, err := binding.TargetIdentities(); err == nil {
		t.Fatal("ambiguous effective names must not overwrite selected identity")
	}
	binding.Instance.Selections[1].Identities = map[string]SelectedIdentity{"distinct": {ClientID: "principal-two"}}
	identities, err := binding.TargetIdentities()
	if err != nil || len(identities) != 2 {
		t.Fatalf("distinct targets: %v, %v", identities, err)
	}
	if identities[binding.TargetName()+"-a-b-c"].ClientID != "principal-one" || identities[binding.TargetName()+"-a-distinct"].ClientID != "principal-two" {
		t.Fatalf("identities not retained: %v", identities)
	}
}
