// deploy installs the CF apps and their broker-only management identity route.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

type cfCommand func(...string) ([]byte, error)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	args, cleanup, err := prepareVars(args)
	if err != nil {
		return err
	}
	defer cleanup()
	return deploy(args, executeCF)
}

// Go closes inherited descriptors when starting cf. Materialize process
// substitutions before launching any child process, then remove private copies.
func prepareVars(args []string) ([]string, func(), error) {
	args = append([]string(nil), args...)
	var paths []string
	cleanup := func() {
		for _, path := range paths {
			_ = os.Remove(path)
		}
	}
	for i := 4; i < len(args); i++ {
		index, prefix := i, ""
		if args[i] == "--vars-file" {
			i++
			if i >= len(args) {
				cleanup()
				return nil, func() {}, errors.New("--vars-file requires a path")
			}
			index = i
		} else if strings.HasPrefix(args[i], "--vars-file=") {
			prefix = "--vars-file="
		} else {
			continue
		}
		path := strings.TrimPrefix(args[index], prefix)
		if !strings.HasPrefix(path, "/dev/fd/") && !strings.HasPrefix(path, "/proc/self/fd/") {
			continue
		}
		input, err := os.Open(path)
		if err != nil {
			cleanup()
			return nil, func() {}, errors.New("cannot read process-substitution vars file")
		}
		temp, err := os.CreateTemp("", "agentgateway-vars-*.json")
		if err != nil {
			input.Close()
			cleanup()
			return nil, func() {}, errors.New("cannot create private vars file")
		}
		paths = append(paths, temp.Name())
		_, copyErr := io.Copy(temp, input)
		input.Close()
		closeErr := temp.Close()
		if copyErr != nil || closeErr != nil {
			cleanup()
			return nil, func() {}, errors.New("cannot prepare process-substitution vars file")
		}
		args[index] = prefix + temp.Name()
	}
	return args, cleanup, nil
}

func executeCF(args ...string) ([]byte, error) {
	command := exec.Command("cf", args...)
	command.Stdin = os.Stdin
	// Captured API output can contain credentials. Never print it in errors.
	if args[0] == "curl" || args[0] == "app" {
		return command.Output()
	}
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	return nil, command.Run()
}

func deploy(args []string, cf cfCommand) error {
	if len(args) < 4 {
		return errors.New("Usage: deploy GATEWAY BROKER DOMAIN HOST [cf push options]")
	}
	gateway, broker, domain, host := args[0], args[1], args[2], args[3]
	for _, arg := range args[4:] {
		flag := strings.SplitN(arg, "=", 2)[0]
		if flag == "--no-start" || flag == "--no-route" || flag == "--random-route" {
			return errors.New("deployment helper controls app startup and routes; supply only configuration options")
		}
	}
	call := func(args ...string) ([]byte, error) {
		out, err := cf(args...)
		if err != nil {
			return nil, fmt.Errorf("cf %s failed", args[0])
		}
		return out, nil
	}
	api := func(path string, value any) error {
		out, err := call("curl", path)
		if err != nil {
			return err
		}
		if json.Unmarshal(out, value) != nil {
			return errors.New("CF API returned an invalid response")
		}
		return nil
	}
	guid := func(app string) (string, error) {
		out, err := call("app", app, "--guid")
		if err != nil {
			return "", err
		}
		value := strings.TrimSpace(string(out))
		if value == "" || strings.ContainsAny(value, "/?# \t\r\n") {
			return "", errors.New("CF returned an invalid app GUID")
		}
		return value, nil
	}
	var domains struct {
		Resources []struct {
			GUID    string `json:"guid"`
			Enforce bool   `json:"enforce_route_policies"`
		} `json:"resources"`
	}
	if err := api("/v3/domains?names="+url.QueryEscape(domain), &domains); err != nil {
		return err
	}
	if len(domains.Resources) != 1 || !domains.Resources[0].Enforce {
		return errors.New("management domain must enforce route policies")
	}
	push := append([]string{"push", "-f", "manifest.yml"}, args[4:]...)
	if _, err := call(append(push, "--no-start", "--redact-env")...); err != nil {
		return err
	}
	brokerGUID, err := guid(broker)
	if err != nil {
		return err
	}
	environment := func(id string) (map[string]json.RawMessage, error) {
		var response struct {
			Variables map[string]json.RawMessage `json:"var"`
		}
		if err := api("/v3/apps/"+id+"/environment_variables", &response); err != nil {
			return nil, err
		}
		if response.Variables == nil {
			return nil, errors.New("CF environment response is missing variables")
		}
		return response.Variables, nil
	}
	env, err := environment(brokerGUID)
	if err != nil {
		return err
	}
	var raw string
	var config struct {
		Gateway struct {
			ManagementURL string `json:"management_url"`
		} `json:"gateway"`
	}
	if json.Unmarshal(env["AGENTGATEWAY_CONFIG"], &raw) != nil || json.Unmarshal([]byte(raw), &config) != nil {
		return errors.New("broker AGENTGATEWAY_CONFIG must contain gateway.management_url")
	}
	u, err := url.Parse(config.Gateway.ManagementURL)
	if err != nil || u.Scheme != "https" || u.Hostname() != host+"."+domain || (u.Port() != "" && u.Port() != "443") || (u.Path != "" && u.Path != "/") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("gateway.management_url must match the HTTPS identity route on port 443")
	}
	if _, err := call("map-route", gateway, domain, "--hostname", host, "--app-port", "9090"); err != nil {
		return err
	}
	var routes struct {
		Resources []struct {
			GUID         string `json:"guid"`
			Destinations []struct {
				Port int `json:"port"`
				App  struct {
					GUID string `json:"guid"`
				} `json:"app"`
			} `json:"destinations"`
		} `json:"resources"`
	}
	if err := api("/v3/routes?hosts="+url.QueryEscape(host)+"&domain_guids="+url.QueryEscape(domains.Resources[0].GUID), &routes); err != nil {
		return err
	}
	if len(routes.Resources) != 1 {
		return errors.New("expected one management route")
	}
	route := routes.Resources[0]
	gatewayGUID, err := guid(gateway)
	if err != nil {
		return err
	}
	if len(route.Destinations) == 0 {
		return errors.New("management route has no destinations")
	}
	for _, destination := range route.Destinations {
		if destination.App.GUID != gatewayGUID || destination.Port != 9090 {
			return errors.New("management route has unexpected destinations")
		}
	}
	var policies struct {
		Resources []struct {
			Source string `json:"source"`
		} `json:"resources"`
		Pagination struct {
			Next *json.RawMessage `json:"next"`
		} `json:"pagination"`
	}
	if err := api("/v3/route_policies?route_guids="+url.QueryEscape(route.GUID)+"&per_page=5000", &policies); err != nil {
		return err
	}
	if policies.Pagination.Next != nil {
		return errors.New("management route policy response is paginated; reconcile policies first")
	}
	for _, policy := range policies.Resources {
		if policy.Source != "cf:app:"+brokerGUID {
			return errors.New("management route has unexpected callers; reconcile its route policies first")
		}
	}
	if len(policies.Resources) == 0 {
		if _, err := call("add-route-policy", domain, "--hostname", host, "--source-app", broker); err != nil {
			return err
		}
	}
	// CF push merges environment; remove known retired settings before startup.
	for _, app := range []struct{ name, id string }{{gateway, gatewayGUID}, {broker, brokerGUID}} {
		present, err := environment(app.id)
		if err != nil {
			return err
		}
		for _, key := range retiredVariables {
			if _, exists := present[key]; exists {
				if _, err := call("unset-env", app.name, key); err != nil {
					return err
				}
			}
		}
	}
	for _, app := range []string{gateway, broker} {
		if _, err := call("restart", app, "--strategy", "rolling"); err != nil {
			return err
		}
	}
	return nil
}

var retiredVariables = []string{
	"MANAGEMENT_TOKEN", "DATABASE_SERVICE_NAME", "GATEWAY_MODELS", "MODEL_CATALOG",
	"MCP_SERVERS", "SESSION_KEY", "CAPSULE_LOG_ID", "CAPSULE_SIGNING_SEED",
	"CAPSULE_WITNESS_URL", "CAPSULE_WITNESS_PUBLIC_KEY", "BROKER_PASSWORD",
	"INSTALLATION", "MANAGEMENT_URL", "MANAGEMENT_TRUSTED_CA", "GATEWAY_URL",
	"GATEWAY_TRUSTED_CA", "INFERENCE_PLANS", "MCP_URL", "MODEL_ALIASES",
	"USD_ALLOWANCE", "BUDGET_WINDOW",
}
