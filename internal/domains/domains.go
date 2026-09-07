// Package domains implements operator-managed local host mappings. A mapping
// is explicit configuration; it never provisions DNS, certificates, or a
// wildcard route on the operator's behalf.
package domains

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/catalog"
	"github.com/FelineStateMachine/tinyrelay/internal/sites"
)

type ResolveDNS func(context.Context, string) ([]net.IP, error)

type Config struct {
	Catalog    *catalog.Catalog
	TenantID   string
	BaseHost   string
	RelayURL   string
	Owner      func(string) bool
	ResolveDNS ResolveDNS
}

type Service struct{ cfg Config }

type DNSRecord struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
	Note  string `json:"note"`
}

type View struct {
	Host      string      `json:"host"`
	Site      string      `json:"site,omitempty"`
	Status    string      `json:"status"`
	Ready     bool        `json:"ready"`
	CheckedAt int64       `json:"checkedAt"`
	Addresses []string    `json:"addresses,omitempty"`
	Records   []DNSRecord `json:"records"`
}

var labelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func New(cfg Config) (*Service, error) {
	if cfg.Catalog == nil {
		return nil, errors.New("domains: catalog is required")
	}
	if cfg.TenantID == "" {
		return nil, errors.New("domains: tenant id is required")
	}
	base, err := canonicalHost(cfg.BaseHost)
	if cfg.BaseHost != "" && err != nil {
		return nil, err
	}
	cfg.BaseHost = base
	return &Service{cfg: cfg}, nil
}

func (s *Service) Methods() []string {
	return []string{"adddomain", "setdomainsite", "checkdomain", "removedomain", "listdomains"}
}

func (s *Service) Execute(ctx context.Context, actor, method string, params []json.RawMessage) (any, error) {
	if method != "listdomains" && (s.cfg.Owner == nil || !s.cfg.Owner(actor)) {
		return nil, errors.New("restricted: only the owner manages domains")
	}
	str := func(i int) (string, error) {
		if i >= len(params) {
			return "", errors.New("invalid: missing hostname")
		}
		var v string
		if err := json.Unmarshal(params[i], &v); err != nil {
			return "", errors.New("invalid: hostname must be a string")
		}
		return v, nil
	}
	switch method {
	case "adddomain":
		host, err := str(0)
		if err != nil {
			return nil, err
		}
		site, err := optionalSite(params, 1)
		if err != nil {
			return nil, err
		}
		return s.Add(ctx, host, site)
	case "setdomainsite":
		host, err := str(0)
		if err != nil {
			return nil, err
		}
		site, err := optionalSite(params, 1)
		if err != nil {
			return nil, err
		}
		return s.SetSite(ctx, host, site)
	case "checkdomain":
		host, err := str(0)
		if err != nil {
			return nil, err
		}
		return s.Check(ctx, host)
	case "removedomain":
		host, err := str(0)
		if err != nil {
			return nil, err
		}
		return true, s.Remove(ctx, host)
	case "listdomains":
		return s.List(ctx)
	default:
		return nil, fmt.Errorf("unsupported: domain method %q", method)
	}
}

func optionalSite(params []json.RawMessage, index int) (string, error) {
	if len(params) <= index {
		return "", nil
	}
	var site string
	if string(params[index]) != "null" {
		if err := json.Unmarshal(params[index], &site); err != nil {
			return "", errors.New("invalid: site must be a string")
		}
	}
	if site != "" {
		if _, ok := sites.ParseSite(site); !ok {
			return "", errors.New("invalid: not a valid site hostname")
		}
	}
	return site, nil
}

func (s *Service) Add(ctx context.Context, rawHost, site string) (View, error) {
	host, err := s.validateHost(rawHost)
	if err != nil {
		return View{}, err
	}
	mapping, err := s.cfg.Catalog.AddHost(ctx, s.cfg.TenantID, host, site)
	if err != nil {
		return View{}, err
	}
	return s.view(mapping, nil), nil
}

func (s *Service) SetSite(ctx context.Context, rawHost, site string) (View, error) {
	host, err := s.validateHost(rawHost)
	if err != nil {
		return View{}, err
	}
	mapping, err := s.cfg.Catalog.SetHostSite(ctx, s.cfg.TenantID, host, site)
	if err != nil {
		return View{}, err
	}
	return s.view(mapping, nil), nil
}

func (s *Service) Check(ctx context.Context, rawHost string) (View, error) {
	host, err := s.validateHost(rawHost)
	if err != nil {
		return View{}, err
	}
	mapping, err := s.cfg.Catalog.ResolveHostMapping(ctx, host)
	if err != nil {
		return View{}, err
	}
	if mapping.TenantID != s.cfg.TenantID {
		return View{}, catalog.ErrHostMappingNotFound
	}
	var addresses []net.IP
	if s.cfg.ResolveDNS != nil {
		addresses, err = s.cfg.ResolveDNS(ctx, host)
		if err != nil {
			return s.view(mapping, err), nil
		}
	}
	return s.view(mapping, addresses), nil
}

func (s *Service) Remove(ctx context.Context, rawHost string) error {
	host, err := s.validateHost(rawHost)
	if err != nil {
		return err
	}
	return s.cfg.Catalog.RemoveHost(ctx, s.cfg.TenantID, host)
}

func (s *Service) List(ctx context.Context) ([]View, error) {
	mappings, err := s.cfg.Catalog.ListHosts(ctx, s.cfg.TenantID)
	if err != nil {
		return nil, err
	}
	result := make([]View, 0, len(mappings))
	for _, mapping := range mappings {
		result = append(result, s.view(mapping, nil))
	}
	return result, nil
}

func (s *Service) validateHost(raw string) (string, error) {
	host, err := canonicalHost(raw)
	if err != nil {
		return "", err
	}
	if net.ParseIP(host) != nil {
		return "", errors.New("invalid: an address is not a hostname")
	}
	if strings.Contains(host, "*") {
		return "", errors.New("invalid: wildcard hostnames are not allowed")
	}
	if strings.Count(host, ".") < 1 {
		return "", errors.New("invalid: hostname needs at least two labels")
	}
	if s.cfg.BaseHost != "" && (host == s.cfg.BaseHost || strings.HasSuffix(host, "."+s.cfg.BaseHost)) {
		return "", fmt.Errorf("invalid: names under %s are relays already", s.cfg.BaseHost)
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return "", errors.New("invalid: that name is local")
	}
	return host, nil
}

func canonicalHost(raw string) (string, error) {
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	if host == "" || len(host) > 253 || strings.ContainsAny(host, "/\\:@?#") {
		return "", errors.New("invalid: hostname length or syntax")
	}
	for _, label := range strings.Split(host, ".") {
		if !labelPattern.MatchString(label) {
			return "", fmt.Errorf("invalid: hostname label %q", label)
		}
	}
	return host, nil
}

func (s *Service) view(mapping catalog.HostMapping, value any) View {
	checked := time.Now().Unix()
	result := View{Host: mapping.Host, Site: mapping.Site, Status: mapping.Status, Ready: mapping.Status == "configured", CheckedAt: checked, Records: []DNSRecord{{Type: "CNAME", Name: mapping.Host, Value: s.cfg.RelayURL, Note: "route this hostname to the tiny listener"}}}
	if ips, ok := value.([]net.IP); ok {
		for _, ip := range ips {
			result.Addresses = append(result.Addresses, ip.String())
		}
		result.Ready = result.Ready && len(ips) > 0
		if len(ips) == 0 {
			result.Status = "dns_unresolved"
		}
	}
	if value != nil {
		if _, ok := value.(error); ok {
			result.Ready = false
			result.Status = "dns_error"
		}
	}
	return result
}
