package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
)

type diagnosticTransport func(*http.Request) (*http.Response, error)

func (f diagnosticTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestManagementFailureIsObservableWithoutSecrets(t *testing.T) {
	b, _ := fixture(t)
	b.cfg.ManagementURL = "https://management.example/secret-path"
	b.cfg.ManagementToken = "secret-token"
	b.client.Transport = diagnosticTransport(func(*http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Get", URL: "https://user:secret-password@management.example/?token=secret-query", Err: syscall.ECONNREFUSED}
	})
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	w := call(b, "PUT", "/v2/service_instances/instance", provision)
	if w.Code != 503 || strings.Contains(w.Body.String(), "connection refused") {
		t.Fatalf("unsafe or unexpected public response: %d %s", w.Code, w.Body)
	}
	for _, expected := range []string{"method=GET", "kind=llm.apiKey", `osb_request="instance"`, "connection refused"} {
		if !strings.Contains(logs.String(), expected) {
			t.Errorf("missing diagnostic %q: %s", expected, &logs)
		}
	}
	if strings.Contains(logs.String(), "secret-") || strings.Contains(logs.String(), "management.example") {
		t.Fatalf("credential-bearing URL or token leaked: %s", &logs)
	}
	if strings.Count(logs.String(), "gateway management method=") != 1 {
		t.Fatalf("expected one diagnostic: %s", &logs)
	}
}

func TestManagementStatusAndDecodeDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"unauthorized", 401, "secret-response", "HTTP 401 Unauthorized"},
		{"missing", 404, "secret-response", "HTTP 404 Not Found"},
		{"malformed JSON", 200, `{"resources":secret-response}`, "invalid JSON syntax at byte"},
		{"wrong type", 200, `{"resources":"secret-response"}`, "incorrect JSON type at byte"},
	} {
		t.Run(test.name, func(t *testing.T) {
			b, _ := fixture(t)
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer s.Close()
			b.cfg.ManagementURL = s.URL
			var logs bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(previous)
			_, err := b.find(context.Background(), "instance")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("want %q, got %v", test.want, err)
			}
			if strings.Contains(logs.String(), "secret-response") {
				t.Fatalf("response body leaked: %s", &logs)
			}
			if test.status == 404 {
				if err := b.deleteManaged(context.Background(), "mcp.grant", "grant"); err != nil {
					t.Fatalf("idempotent missing grant deletion failed: %v", err)
				}
			}
		})
	}
}

func TestTransportCausePreservesCancellation(t *testing.T) {
	err := transportCause(&url.Error{Op: "Get", URL: "https://secret", Err: context.Canceled})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation classification lost: %v", err)
	}
}

func TestURLValidationDiagnosticsExcludeSecrets(t *testing.T) {
	b, _ := fixture(t)
	for _, field := range []string{"mcp_url", "gateway_url", "management_url"} {
		for _, invalid := range []struct{ name, value string }{
			{"invalid port", "https://secret-user:secret-password@host:invalid/?token=secret-query#secret-fragment"},
			{"invalid escape", "https://host/%zz?token=secret-query"},
			{"credentials", "https://secret-user:secret-password@host/path"},
			{"query", "https://host/path?token=secret-query"},
			{"fragment", "https://host/path#secret-fragment"},
			{"missing host", "https:///secret-path"},
			{"unsupported scheme", "file://host/secret-path"},
		} {
			t.Run(field+"/"+invalid.name, func(t *testing.T) {
				cfg := b.cfg
				label := field
				switch field {
				case "mcp_url":
					cfg.MCPURL = invalid.value
					label = "MCP URL"
				case "gateway_url":
					cfg.GatewayURL = invalid.value
				case "management_url":
					cfg.ManagementURL = invalid.value
				}
				_, err := newBroker(cfg)
				if err == nil {
					t.Fatal("invalid URL accepted")
				}
				message := err.Error()
				for _, expected := range []string{label, "HTTP(S)", "host", "credentials, query, or fragment"} {
					if !strings.Contains(message, expected) {
						t.Errorf("missing requirement %q: %s", expected, message)
					}
				}
				if strings.Contains(message, "secret-") || strings.Contains(message, "<nil>") || strings.Contains(message, invalid.value) {
					t.Fatalf("unsafe or misleading URL diagnostic: %s", message)
				}
			})
		}
	}
}
