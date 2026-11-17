package main

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

type Connection struct {
	URL       string `json:"url"`
	TrustedCA string `json:"trusted_ca,omitempty"`
}
type Offering struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Protocol    string         `json:"protocol"`
	Connection  Connection     `json:"connection"`
	Plans       []OfferingPlan `json:"plans"`
}
type OfferingPlan struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Free        bool   `json:"free"`
	Access      struct {
		Models   []string `json:"models,omitempty"`
		Backends []string `json:"backends,omitempty"`
	} `json:"access"`
	Budget *struct {
		USD    json.Number `json:"usd"`
		Window string      `json:"window"`
	} `json:"budget,omitempty"`
	Schemas map[string]any `json:"schemas,omitempty"`
}

func (c Config) offering(service, plan string) (*Offering, *OfferingPlan) {
	for i := range c.Services {
		s := &c.Services[i]
		if s.ID == service {
			for j := range s.Plans {
				if s.Plans[j].ID == plan {
					return s, &s.Plans[j]
				}
			}
		}
	}
	return nil, nil
}
func (c Config) inference(service, plan string) *InferencePlan {
	if len(c.Services) == 0 {
		if service == c.ServiceID {
			return (&Broker{cfg: c}).inferencePlan(plan)
		}
		return nil
	}
	s, p := c.offering(service, plan)
	if s == nil || s.Protocol != "llm" || p.Budget == nil {
		return nil
	}
	return &InferencePlan{ID: p.ID, Name: p.Name, Description: p.Description, Free: p.Free, Models: p.Access.Models, Budget: p.Budget.USD, Window: p.Budget.Window}
}
func (c Config) validateOfferings() error {
	ids, names, plans := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, s := range c.Services {
		if !resourceID.MatchString(s.ID) || !resourceID.MatchString(s.Name) || ids[s.ID] || names[s.Name] {
			return errors.New("invalid or duplicate service ID/name")
		}
		ids[s.ID], names[s.Name] = true, true
		if strings.TrimSpace(s.Description) == "" || len(s.Plans) == 0 {
			return errors.New("service description and plans required")
		}
		if err := validConnection(s.Connection.URL); err != nil {
			return err
		}
		if s.Connection.TrustedCA != "" && !x509.NewCertPool().AppendCertsFromPEM([]byte(s.Connection.TrustedCA)) {
			return errors.New("connection.trusted_ca must contain PEM certificates")
		}
		local := map[string]bool{}
		for _, p := range s.Plans {
			if !resourceID.MatchString(p.ID) || !resourceID.MatchString(p.Name) || plans[p.ID] || local[p.Name] {
				return errors.New("invalid or duplicate plan ID/name")
			}
			plans[p.ID], local[p.Name] = true, true
			if strings.TrimSpace(p.Description) == "" {
				return errors.New("plan description required")
			}
			switch s.Protocol {
			case "llm":
				if len(p.Access.Models) == 0 || len(p.Access.Backends) > 0 || p.Budget == nil || len(p.Schemas) > 0 {
					return errors.New("LLM plan requires models and budget only")
				}
				v, err := p.Budget.USD.Float64()
				if err != nil || v <= 0 || math.IsInf(v, 0) || math.IsNaN(v) || p.Budget.Window == "" {
					return errors.New("invalid LLM allowance")
				}
				seen := map[string]bool{}
				for _, m := range p.Access.Models {
					if m == "" || strings.Contains(m, "*") || seen[m] {
						return errors.New("explicit model aliases required")
					}
					seen[m] = true
				}
			case "mcp":
				seen := map[string]bool{}
				for _, name := range p.Access.Backends {
					if !resourceID.MatchString(name) || seen[name] {
						return errors.New("invalid or duplicate backend access")
					}
					seen[name] = true
				}
				if len(p.Access.Backends) == 0 || len(p.Access.Models) > 0 || p.Budget != nil {
					return errors.New("MCP plan requires backend access only")
				}
				if _, err := parameterSchema(p.Schemas); err != nil {
					return err
				}
			default:
				return fmt.Errorf("unsupported service protocol %q", s.Protocol)
			}
		}
	}
	return nil
}
