package mcpconfig

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"
)

func TestOAuthEnvelopeBindsInstanceResourceAndOperation(t *testing.T) {
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const resource = "https://tools.example/mcp"
	update := OAuthUpdate{Action: "connect", Generation: "generation", Previous: "previous", Sequence: 5, Operation: "operation"}
	want := OAuthAccess{Token: "fixture-access-token", Expires: time.Now().UTC().Truncate(time.Second).Add(time.Hour)}
	if err := update.Encrypt(hex.EncodeToString(key.PublicKey().Bytes()), "instance", resource, want); err != nil {
		t.Fatal(err)
	}
	priv := hex.EncodeToString(key.Bytes())
	if got, err := update.Decrypt(priv, "instance", resource); err != nil || got != want {
		t.Fatal("original request cannot be decrypted")
	}
	if _, err := update.Decrypt(priv, "other-instance", resource); err == nil {
		t.Fatal("envelope moved to a different instance")
	}
	if _, err := update.Decrypt(priv, "instance", "https://other.example/mcp"); err == nil {
		t.Fatal("envelope moved to a different upstream")
	}
	update.Sequence++
	if _, err := update.Decrypt(priv, "instance", resource); err == nil {
		t.Fatal("envelope replayed at a different connection sequence")
	}
}
