package main

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"code.cloudfoundry.org/cli/v8/plugin"
	models "code.cloudfoundry.org/cli/v8/plugin/models"
)

type instanceLookupCLI struct {
	plugin.CliConnection
	name, result string
	wrongName    bool
}

func (c instanceLookupCLI) GetCurrentSpace() (models.Space, error) {
	var space models.Space
	space.Guid = "space"
	return space, nil
}

func (c *instanceLookupCLI) CliCommandWithoutTerminalOutput(args ...string) ([]string, error) {
	if c.result != "" {
		panic("lookup must not proceed to plan discovery")
	}
	c.result = args[1]
	u, _ := url.Parse(args[1])
	var instances []instance
	// Mirror CAPI: split after query decoding, then URI-unescape each element.
	for _, value := range strings.Split(u.Query().Get("names"), ",") {
		name, _ := url.PathUnescape(value)
		if name == c.name || name == "design" {
			if c.wrongName {
				name = "design"
			}
			instances = append(instances, instance{Name: name, GUID: "instance"})
		}
	}
	data, _ := json.Marshal(map[string]any{"resources": instances})
	return []string{string(data)}, nil
}

func TestInstanceLookupPreservesLiteralFilterElements(t *testing.T) {
	const name = "design, team%2C+"
	cf := &instanceLookupCLI{name: name}
	// The matching fixture has no plan GUID, so execution stops just after name selection.
	if err := run(cf, []string{"disconnect", name}); err == nil || err.Error() != "invalid CF instance or plan GUID" {
		t.Fatalf("expected the exact instance to be selected; got %v", err)
	}
	cf = &instanceLookupCLI{name: name, wrongName: true}
	if err := run(cf, []string{"disconnect", name}); err == nil || !strings.Contains(err.Error(), "exactly matching") {
		t.Fatalf("expected a mismatched response to be rejected before plan discovery; got %v", err)
	}
}
