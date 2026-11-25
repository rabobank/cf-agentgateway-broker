package main

import (
	"net/url"
	"testing"
	"time"
)

func TestAccessLifetime(t *testing.T) {
	if lifetime, err := accessLifetime(nil); err != nil || lifetime != 24*time.Hour {
		t.Fatalf("omitted expiry: got %v, %v", lifetime, err)
	}
	for _, tc := range []struct {
		seconds int64
		want    time.Duration
	}{
		{3600, time.Hour},
		{1<<63 - 1, 24 * time.Hour},
		{0, 0},
		{-1, 0},
	} {
		if lifetime, err := accessLifetime(&tc.seconds); lifetime != tc.want || (err != nil) != (tc.want == 0) {
			t.Errorf("expires_in %d: got %v, %v; want %v", tc.seconds, lifetime, err, tc.want)
		}
	}
}

func TestRegistrationAuthenticationSelection(t *testing.T) {
	for _, tc := range []struct {
		methods []string
		want    string
	}{
		{[]string{"client_secret_basic", "none"}, "none"},
		{[]string{"client_secret_post", "client_secret_basic"}, "client_secret_basic"},
		{[]string{"client_secret_post"}, "client_secret_post"},
		{nil, "client_secret_basic"},
		{[]string{"private_key_jwt"}, ""},
	} {
		got, err := registrationAuthMethod(tc.methods)
		if got != tc.want || (err != nil) != (tc.want == "") {
			t.Errorf("methods %v: got %q, %v; want %q", tc.methods, got, err, tc.want)
		}
	}
}

func TestClientAuthenticationSelection(t *testing.T) {
	for _, tc := range []struct {
		name, hint, secret, want string
		advertised               []string
	}{
		{"conflicting public hint", "none", "secret", "client_secret_basic", []string{"client_secret_basic", "client_secret_post"}},
		{"honor post hint", "client_secret_post", "secret", "client_secret_post", []string{"client_secret_basic", "client_secret_post"}},
		{"public", "none", "", "none", []string{"none"}},
		{"legacy static", "none", "", "none", nil},
		{"missing secret", "client_secret_basic", "", "", []string{"client_secret_basic"}},
		{"unsupported registration result", "private_key_jwt", "secret", "", []string{"client_secret_basic"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := clientAuthMethod(oauthClient{Secret: tc.secret, Method: tc.hint}, tc.advertised)
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("method = %q, error = %v, want %q", got, err, tc.want)
			}
		})
	}
}

func TestCallbackRequiresMatchingRedirectStateAndIssuer(t *testing.T) {
	c := oauthCallback{redirect: "http://127.0.0.1:19876/callback", state: "expected", issuer: "https://issuer.example", requireIssuer: true}
	valid := url.Values{"code": {"fixture-code"}, "state": {c.state}, "iss": {c.issuer}}
	if code, ok := c.parse(c.redirect + "?" + valid.Encode()); !ok || code != "fixture-code" {
		t.Fatal("valid callback rejected")
	}
	for _, address := range []string{
		"http://127.0.0.1:19877/callback?" + valid.Encode(),
		c.redirect + "?" + valid.Encode() + "&state=duplicate",
		c.redirect + "?" + valid.Encode() + "&code=duplicate",
		c.redirect + "?" + valid.Encode() + "#fragment",
		c.redirect + "?code=fixture-code&state=expected",
		c.redirect + "?code=fixture-code&state=expected&iss=https://other.example",
	} {
		if _, ok := c.parse(address); ok {
			t.Fatal("mismatched callback was accepted")
		}
	}
}
