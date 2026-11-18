package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
)

const parameterSchemaDialect = "http://json-schema.org/draft-04/schema#"

func validConnection(raw string) error {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("connection URL must be HTTP(S), without credentials/query/fragment")
	}
	return nil
}

type stringParameter struct {
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
	Pattern     string `json:"pattern,omitempty"`
}
type instanceSchema struct {
	Dialect              json.RawMessage            `json:"$schema"`
	Type                 string                     `json:"type"`
	AdditionalProperties *bool                      `json:"additionalProperties"`
	Required             []string                   `json:"required"`
	Properties           map[string]stringParameter `json:"properties"`
}

func parameterSchema(schemas map[string]any) (*instanceSchema, error) {
	if len(schemas) == 0 {
		return nil, nil
	}
	data, _ := json.Marshal(schemas)
	var envelope struct {
		Instance struct {
			Create struct {
				Parameters json.RawMessage `json:"parameters"`
			} `json:"create"`
		} `json:"service_instance"`
	}
	// Restrict supported OSB schema paths rather than silently ignoring constraints.
	if err := strictJSON(data, &envelope); err != nil {
		return nil, errors.New("only service_instance.create.parameters schema supported")
	}
	var s instanceSchema
	if err := strictJSON(envelope.Instance.Create.Parameters, &s); err != nil {
		return nil, errors.New("unsupported instance schema")
	}
	if s.Dialect != nil {
		var dialect string
		if json.Unmarshal(s.Dialect, &dialect) != nil || dialect != parameterSchemaDialect {
			return nil, errors.New("instance schema $schema must declare JSON Schema Draft 4")
		}
	}
	if s.Type != "object" || s.AdditionalProperties == nil || *s.AdditionalProperties {
		return nil, errors.New("instance schema must be object with additionalProperties false")
	}
	for k, p := range s.Properties {
		if !resourceID.MatchString(k) || p.Type != "string" {
			return nil, errors.New("instance schema supports named string parameters only")
		}
		if _, err := regexp.Compile(p.Pattern); err != nil {
			return nil, errors.New("invalid parameter pattern")
		}
	}
	for _, k := range s.Required {
		if _, ok := s.Properties[k]; !ok {
			return nil, errors.New("required parameter missing from properties")
		}
	}
	return &s, nil
}

// catalogParameterSchemas adds the required OSB declaration to a validated
// schema without modifying the broker's shared configuration.
func catalogParameterSchemas(schemas map[string]any) map[string]any {
	instance := schemas["service_instance"].(map[string]any)
	create := instance["create"].(map[string]any)
	parameters := maps.Clone(create["parameters"].(map[string]any))
	parameters["$schema"] = parameterSchemaDialect
	return map[string]any{"service_instance": map[string]any{
		"create": map[string]any{"parameters": parameters},
	}}
}

func validateParameters(schemas map[string]any, params map[string]any) (map[string]string, error) {
	s, err := parameterSchema(schemas)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if s == nil {
		if len(params) > 0 {
			return nil, errors.New("plan accepts no instance parameters")
		}
		return out, nil
	}
	for k, v := range params {
		p, ok := s.Properties[k]
		if !ok {
			return nil, fmt.Errorf("unknown instance parameter %q", k)
		}
		str, ok := v.(string)
		if !ok || str == "" {
			return nil, fmt.Errorf("parameter %q must be a nonempty string", k)
		}
		if ok, _ := regexp.MatchString(p.Pattern, str); !ok {
			return nil, fmt.Errorf("invalid parameter %q", k)
		}
		out[k] = str
	}
	for _, k := range s.Required {
		if out[k] == "" {
			return nil, fmt.Errorf("instance parameter %q is required", k)
		}
	}
	return out, nil
}
