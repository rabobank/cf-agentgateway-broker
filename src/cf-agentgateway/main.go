package main

import (
	mcpconfig "cf-mcp-config"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"code.cloudfoundry.org/cli/v8/plugin"
)

type command struct{}

func (command) GetMetadata() plugin.PluginMetadata {
	return plugin.PluginMetadata{Name: "agentgateway", Version: plugin.VersionType{Major: 1}, Commands: []plugin.Command{{Name: "agentgateway", Alias: "ag", HelpText: "Manage team MCP OAuth connections", UsageDetails: plugin.Usage{Usage: "cf ag list | status INSTANCE | connect INSTANCE [--replace] | disconnect INSTANCE"}}}}
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
		fmt.Println("Usage: cf ag list | status INSTANCE | connect INSTANCE [--replace] | disconnect INSTANCE")
		return nil
	}
	action := args[0]
	replace := action == "connect" && len(args) == 3 && args[2] == "--replace"
	if (action == "list" && len(args) != 1) || (action != "list" && len(args) != 2 && !replace) || (action != "list" && action != "status" && action != "connect" && action != "disconnect") {
		return errors.New("usage: cf ag list | status INSTANCE | connect INSTANCE [--replace] | disconnect INSTANCE")
	}
	space, err := cf.GetCurrentSpace()
	if err != nil || !mcpconfig.ValidID(space.Guid) {
		return errors.New("target a Cloud Foundry space first")
	}
	q := url.Values{"space_guids": {space.Guid}, "type": {"managed"}}
	if action != "list" {
		q.Set("names", args[1])
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
	if action != "list" && len(instances) != 1 {
		return errors.New("expected one managed instance in the targeted space")
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
		if action == "connect" && status.Connected && !replace {
			return errors.New("already connected; use --replace to authorize again")
		}
		update := mcpconfig.OAuthUpdate{Action: action, Generation: status.Generation, Previous: status.Operation, Sequence: status.Sequence, Operation: hex.EncodeToString(randomBytes(16))}
		if action == "connect" {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			access, err := login(ctx, profile)
			cancel()
			if err != nil {
				return err
			}
			if update.Encrypt(profile.PublicKey, i.GUID, profile.Resource, access) != nil {
				return errors.New("cannot encrypt upstream authorization")
			}
		}
		body, _ := json.Marshal(map[string]any{"parameters": map[string]any{mcpconfig.OAuthProfileField: update}})
		if err := api(cf, path, nil, "-X", "PATCH", "-d", string(body)); err != nil {
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
