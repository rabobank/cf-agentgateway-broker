package main

import (
	"bytes"
	mcpconfig "cf-mcp-config"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
)

func registerClient(ctx context.Context, as authorizationServer, registration mcpconfig.OAuthRegistration, redirect, scope string, refresh bool) (oauthClient, error) {
	var client oauthClient
	if !mcpconfig.OAuthURL(as.Registration) {
		return client, errors.New("dynamic OAuth registration requires an HTTPS registration endpoint")
	}
	method, err := registrationAuthMethod(as.TokenMethods)
	if err != nil {
		return client, err
	}
	metadata := map[string]any{
		"client_name": registration.ClientName, "application_type": "native",
		"redirect_uris": []string{redirect}, "response_types": []string{"code"},
		"grant_types": []string{"authorization_code"}, "token_endpoint_auth_method": method, "scope": scope,
	}
	if refresh {
		metadata["grant_types"] = []string{"authorization_code", "refresh_token"}
	}
	if registration.ClientURI != "" {
		metadata["client_uri"] = registration.ClientURI
	}
	body, _ := json.Marshal(metadata)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, as.Registration, bytes.NewReader(body))
	if err != nil {
		return client, errors.New("invalid OAuth registration endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	if err := readOAuthJSON(req, http.StatusCreated, &client); err != nil {
		return oauthClient{}, fmt.Errorf("dynamic OAuth registration failed: %w", err)
	}
	if !mcpconfig.ValidOAuthToken(client.ID) || (client.Secret != "" && !mcpconfig.ValidOAuthToken(client.Secret)) {
		return oauthClient{}, errors.New("OAuth registration returned invalid client credentials")
	}
	if client.Method == "" {
		// RFC 7591 §2: omission defaults to Basic, even if we requested none.
		client.Method = "client_secret_basic"
	}
	return client, nil
}

func registrationAuthMethod(methods []string) (string, error) {
	if len(methods) == 0 {
		return "client_secret_basic", nil // RFC 8414 default.
	}
	for _, method := range []string{"none", "client_secret_basic", "client_secret_post"} {
		if slices.Contains(methods, method) {
			return method, nil
		}
	}
	return "", errors.New("OAuth server does not advertise a supported registration authentication method")
}

func clientAuthMethod(c oauthClient, methods []string) (string, error) {
	if c.Method != "" && c.Method != "none" && c.Method != "client_secret_basic" && c.Method != "client_secret_post" {
		return "", errors.New("OAuth registration returned an unsupported client authentication method")
	}
	usable := func(method string) bool {
		return method == "none" || (c.Secret != "" && (method == "client_secret_basic" || method == "client_secret_post"))
	}
	if usable(c.Method) && (len(methods) == 0 || slices.Contains(methods, c.Method)) {
		return c.Method, nil
	}
	if len(methods) == 0 {
		if c.Secret != "" {
			return "client_secret_basic", nil
		}
		return "none", nil
	}
	for _, method := range []string{"client_secret_basic", "client_secret_post", "none"} {
		if usable(method) && slices.Contains(methods, method) {
			return method, nil
		}
	}
	return "", errors.New("OAuth server does not advertise a usable client authentication method")
}
