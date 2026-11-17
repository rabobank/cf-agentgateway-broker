package main

import "testing"

func TestCFManagementRequiresHTTPSAndInstanceIdentity(t *testing.T) {
	for _, name := range []string{"CF_INSTANCE_CERT", "CF_INSTANCE_KEY", "CF_SYSTEM_CERT_PATH", "MANAGEMENT_TRUSTED_CA"} {
		t.Setenv(name, "")
	}
	t.Setenv("VCAP_APPLICATION", "{}")
	t.Setenv("MANAGEMENT_URL", "https://management.apps.identity")
	if _, err := managementConfig(); err == nil {
		t.Fatal("CF launch accepted missing identity")
	}
	t.Setenv("CF_INSTANCE_CERT", "/identity/cert")
	if _, err := managementConfig(); err == nil {
		t.Fatal("CF launch accepted missing key")
	}
	t.Setenv("CF_INSTANCE_KEY", "/identity/key")
	t.Setenv("CF_SYSTEM_CERT_PATH", "/platform/trust")
	config, err := managementConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config["management_certificate"] != "/identity/cert" || config["management_private_key"] != "/identity/key" || config["ca_directory"] != "/platform/trust" {
		t.Fatalf("identity/trust not mapped: %v", config)
	}
	t.Setenv("MANAGEMENT_URL", "http://management.apps.identity")
	if _, err := managementConfig(); err == nil {
		t.Fatal("CF launch accepted plaintext management")
	}
}
