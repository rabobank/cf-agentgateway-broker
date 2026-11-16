package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManagementMTLSRotationAndTrust(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "client CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("management request sent Authorization")
		}
		w.Header().Set("Connection", "close") // Force a new handshake after rotation.
		write(w, 200, map[string]any{"resources": []any{}})
	}))
	server.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots}
	server.StartTLS()
	t.Cleanup(server.Close)
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "client.pem"), filepath.Join(dir, "client.key")
	put := func(path string, data []byte) {
		t.Helper()
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	rotate := func(serial int64) {
		t.Helper()
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, pub, key)
		if err != nil {
			t.Fatal(err)
		}
		private, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		put(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		put(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}))
	}
	rotate(2)
	trust := filepath.Join(t.TempDir(), "server.pem")
	put(trust, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	base, _ := fixture(t)
	cfg := base.cfg
	cfg.ManagementURL, cfg.CAFile = server.URL, trust
	cfg.ManagementCertificate, cfg.ManagementPrivateKey = certFile, keyFile
	b, err := newBroker(cfg)
	if err != nil {
		t.Fatal(err)
	}
	request := func() error { return b.gateway(context.Background(), "GET", "", nil, nil) }
	if err := request(); err != nil {
		t.Fatal(err)
	}
	rotate(3)
	if err := request(); err != nil {
		t.Fatalf("rotated identity failed: %v", err)
	}
	put(keyFile, []byte("invalid rotated key"))
	if err := request(); err == nil {
		t.Fatal("cached identity used after invalid rotation")
	}
	rotate(4)
	if err := request(); err != nil {
		t.Fatalf("rotation did not recover: %v", err)
	}
	cfg.ManagementCertificate, cfg.ManagementPrivateKey = "", ""
	b, err = newBroker(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := request(); err == nil {
		t.Fatal("missing client identity accepted")
	}
	cfg.ManagementCertificate, cfg.ManagementPrivateKey = certFile, keyFile
	cfg.CAFile = ""
	b, err = newBroker(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := request(); err == nil {
		t.Fatal("untrusted management server accepted")
	}
	cfg.CADirectory = filepath.Dir(trust)
	b, err = newBroker(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := request(); err != nil {
		t.Fatalf("mounted CA directory not trusted: %v", err)
	}
	cfg.ManagementURL = "http://management.test"
	if _, err := newBroker(cfg); err == nil {
		t.Fatal("client identity accepted over HTTP")
	}
}
