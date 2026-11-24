package main

import (
	"bytes"
	mcpconfig "cf-mcp-config"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"code.cloudfoundry.org/cli/v8/plugin"
)

type command struct{}

const usage = "cf ag list | status INSTANCE | connect INSTANCE [--replace] [--paste] | disconnect INSTANCE"

func (command) GetMetadata() plugin.PluginMetadata {
	return plugin.PluginMetadata{Name: "agentgateway", Version: plugin.VersionType{Major: 1}, Commands: []plugin.Command{{Name: "agentgateway", Alias: "ag", HelpText: "Manage team MCP OAuth connections", UsageDetails: plugin.Usage{Usage: usage}}}}
}

func (command) Run(cf plugin.CliConnection, args []string) {
	if len(args) == 0 || args[0] == "CLI-MESSAGE-UNINSTALL" {
		return
	}
	if err := run(cf, args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func main() { plugin.Start(command{}) }

type instance struct {
	GUID          string
	Name          string
	Relationships struct {
		ServicePlan struct{ Data struct{ GUID string } } `json:"service_plan"`
	}
	LastOperation struct{ State string } `json:"last_operation"`
}

func api(cf plugin.CliConnection, path string, out any, args ...string) error {
	lines, err := cf.CliCommandWithoutTerminalOutput(append([]string{"curl", path, "--fail"}, args...)...)
	if err != nil {
		return errors.New("Cloud Foundry API request failed; check the target and your access")
	}
	if out != nil && json.Unmarshal([]byte(strings.Join(lines, "\n")), out) != nil {
		return errors.New("invalid Cloud Foundry API response")
	}
	return nil
}

func run(cf plugin.CliConnection, args []string) error {
	if len(args) == 0 || args[0] == "help" {
		fmt.Println("Usage:", usage)
		return nil
	}
	action := args[0]
	if (action == "list" && len(args) != 1) || (action != "list" && len(args) < 2) || (action != "list" && action != "status" && action != "connect" && action != "disconnect") {
		return errors.New("usage: " + usage)
	}
	flags := map[string]bool{}
	if len(args) > 2 {
		for _, flag := range args[2:] {
			if action != "connect" || (flag != "--replace" && flag != "--paste") || flags[flag] {
				return errors.New("usage: " + usage)
			}
			flags[flag] = true
		}
	}
	space, err := cf.GetCurrentSpace()
	if err != nil || !mcpconfig.ValidID(space.Guid) {
		return errors.New("target a Cloud Foundry space first")
	}
	q := url.Values{"space_guids": {space.Guid}, "type": {"managed"}}
	if action != "list" {
		// CAPI splits array filters on commas before URI-unescaping each item.
		q.Set("names", strings.ReplaceAll(url.QueryEscape(args[1]), "+", "%20"))
	}
	var instances []instance
	for path := "/v3/service_instances?" + q.Encode(); path != ""; {
		var result struct {
			Resources  []instance
			Pagination struct{ Next *struct{ Href string } }
		}
		if err := api(cf, path, &result); err != nil {
			return err
		}
		instances = append(instances, result.Resources...)
		path = ""
		if next := result.Pagination.Next; next != nil {
			u, err := url.Parse(next.Href)
			if err != nil || u.Path != "/v3/service_instances" {
				return errors.New("invalid service instance pagination")
			}
			path = u.RequestURI()
		}
	}
	if action != "list" && (len(instances) != 1 || instances[0].Name != args[1]) {
		return errors.New("expected one exactly matching managed instance in the targeted space")
	}
	for _, i := range instances {
		plan := i.Relationships.ServicePlan.Data.GUID
		if !mcpconfig.ValidID(plan) || !mcpconfig.ValidID(i.GUID) {
			return errors.New("invalid CF instance or plan GUID")
		}
		var details struct {
			Catalog struct{ Metadata map[string]json.RawMessage } `json:"broker_catalog"`
		}
		if err := api(cf, "/v3/service_plans/"+plan, &details); err != nil {
			return err
		}
		meta := details.Catalog.Metadata[mcpconfig.OAuthProfileField]
		if meta == nil && action == "list" {
			continue
		}
		var profile mcpconfig.OAuthProfile
		if json.Unmarshal(meta, &profile) != nil || profile.Validate() != nil {
			return errors.New("service plan has no valid Agentgateway OAuth profile")
		}
		path := "/v3/service_instances/" + i.GUID
		status, err := getStatus(cf, path)
		if err != nil {
			return err
		}
		if action == "list" || action == "status" {
			printStatus(i.Name, status)
			continue
		}
		if action == "connect" && status.Connected && !flags["--replace"] {
			return errors.New("already connected; use --replace to authorize again")
		}
		update := mcpconfig.OAuthUpdate{Resource: profile.Resource, Action: action, Generation: status.Generation, Previous: status.Operation, Sequence: status.Sequence, Operation: hex.EncodeToString(randomBytes(16))}
		if action == "connect" {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			access, err := login(ctx, profile, flags["--paste"])
			cancel()
			if err != nil {
				return err
			}
			update.Access = &access
		}
		body, _ := json.Marshal(map[string]any{"parameters": map[string]any{mcpconfig.OAuthProfileField: update}})
		if err := patchInstance(cf, path, body); err != nil {
			return err
		}
		deadline := time.Now().Add(time.Minute)
		for {
			var state instance
			if err := api(cf, path, &state); err != nil {
				return err
			}
			if state.LastOperation.State == "failed" {
				return errors.New("service instance update failed")
			}
			if state.LastOperation.State == "succeeded" {
				observed, err := getStatus(cf, path)
				if err == nil && observed.Operation == update.Operation {
					printStatus(i.Name, observed)
					break
				}
			}
			if time.Now().After(deadline) {
				return errors.New("service instance update not confirmed; run cf ag status")
			}
			time.Sleep(time.Second)
		}
	}
	return nil
}

// Send secret-bearing updates directly so the token does not pass through
// cf curl arguments, output, or CF_TRACE.
func patchInstance(cf plugin.CliConnection, path string, body []byte) error {
	target, err := cf.ApiEndpoint()
	if err != nil {
		return errors.New("cannot read the targeted CF API")
	}
	u, err := url.Parse(target)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") ||
		(u.Scheme != "https" && !(u.Scheme == "http" && net.ParseIP(u.Hostname()).IsLoopback())) {
		return errors.New("OAuth updates require a trusted HTTPS CF API or local loopback")
	}
	token, err := cf.AccessToken()
	if err != nil || token == "" {
		return errors.New("cannot read the CF access token; log in again")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	u.Path = path
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u.String(), bytes.NewReader(body))
	if err != nil {
		return errors.New("cannot construct CF service instance update")
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := oauthHTTP.Do(req)
	if err != nil {
		return errors.New("Cloud Foundry instance update failed; check the target and your access")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Cloud Foundry instance update rejected (HTTP %d)", resp.StatusCode)
	}
	return nil
}

func getStatus(cf plugin.CliConnection, path string) (mcpconfig.OAuthStatus, error) {
	var result map[string]mcpconfig.OAuthStatus
	if err := api(cf, path+"/parameters", &result); err != nil {
		return mcpconfig.OAuthStatus{}, err
	}
	s := result[mcpconfig.OAuthProfileField]
	if !mcpconfig.ValidID(s.Generation) {
		return s, errors.New("instance does not report Agentgateway OAuth status")
	}
	return s, nil
}

func printStatus(name string, s mcpconfig.OAuthStatus) {
	if s.Connected && s.Expires != nil {
		fmt.Printf("%s: connected (expires %s)\n", name, s.Expires.UTC().Format(time.RFC3339))
		return
	}
	fmt.Printf("%s: not connected\n", name)
}
