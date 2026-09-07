package replication

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
	"github.com/FelineStateMachine/tinyrelay/internal/work"
)

type Resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

type CallbackClient struct {
	HTTP     *http.Client
	Resolver Resolver
	Timeout  time.Duration
}

func NewCallbackHandler(store *storage.Store, client CallbackClient) work.Handler {
	return NewCallbackHandlerWithPolicy(store, client, nil, nil, nil)
}

func NewCallbackHandlerWithPolicy(store *storage.Store, client CallbackClient, policy func() CallbackPolicy, enabled func() bool, visible func(context.Context, string) bool) work.Handler {
	return newCallbackHandler(store, client, policy, enabled, visible, nil)
}

// NewCallbackHandlerWithEventVisibility is the event-aware variant used by
// tenant adapters. It rechecks visibility against the exact event immediately
// before delivery, after registration revocation and filter matching.
func NewCallbackHandlerWithEventVisibility(store *storage.Store, client CallbackClient, policy func() CallbackPolicy, enabled func() bool, visible func(context.Context, string) bool, visibleEvent func(context.Context, string, event.Event) bool) work.Handler {
	return newCallbackHandler(store, client, policy, enabled, visible, visibleEvent)
}

func newCallbackHandler(store *storage.Store, client CallbackClient, policy func() CallbackPolicy, enabled func() bool, visible func(context.Context, string) bool, visibleEvent func(context.Context, string, event.Event) bool) work.Handler {
	return func(ctx context.Context, intent work.Intent) error {
		var envelope struct {
			Registration PushRegistration `json:"registration"`
		}
		if err := json.Unmarshal([]byte(intent.Payload), &envelope); err != nil {
			return err
		}
		if enabled != nil && !enabled() {
			return nil
		}
		registration, err := currentRegistration(ctx, store, envelope.Registration, policy)
		if err != nil || registration.ID == "" {
			return nil
		}
		envelope.Registration = registration
		result, err := store.Query(ctx, event.Filter{IDs: []string{intent.EventID}, Tags: map[string][]string{}}, storage.QueryOptions{Now: time.Now().Unix(), Access: storage.Access{All: true}, Limit: 1})
		if err != nil {
			return err
		}
		if len(result.Events) == 0 {
			return nil
		}
		delivered := result.Events[0]
		if visible != nil && !visible(ctx, envelope.Registration.Owner) {
			return nil
		}
		if visibleEvent != nil && !visibleEvent(ctx, envelope.Registration.Owner, delivered) {
			return nil
		}
		if !PushMatches(envelope.Registration, delivered) {
			return nil
		}
		return client.Post(ctx, envelope.Registration, delivered)
	}
}

func currentRegistration(ctx context.Context, store *storage.Store, expected PushRegistration, policy func() CallbackPolicy) (PushRegistration, error) {
	var raw string
	if err := store.DB().QueryRowContext(ctx, `SELECT raw FROM events WHERE id=? AND kind=?`, expected.ID, event.KIND_PUSH_REGISTRATION).Scan(&raw); err != nil {
		return PushRegistration{}, err
	}
	var item event.Event
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		return PushRegistration{}, err
	}
	approved := CallbackPolicy{}
	if policy != nil {
		approved = policy()
	} else {
		approved = CallbackPolicy{HostOrigins: []string{expected.Callback}, OwnerOrigins: []string{expected.Callback}}
	}
	parsed, err := ParsePushRegistration(item, approved, expected.Relay)
	if err != nil || parsed.Owner != expected.Owner {
		return PushRegistration{}, errors.New("replication: callback registration revoked")
	}
	return parsed, nil
}

func (c CallbackClient) Post(ctx context.Context, registration PushRegistration, e event.Event) error {
	body, err := CallbackPayload(registration, e)
	if err != nil {
		return err
	}
	parsed, err := url.Parse(registration.Callback)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return errors.New("replication: callback URL must be HTTPS")
	}
	client := c.HTTP
	if client == nil {
		client = pinnedClient(c.Resolver, c.Timeout)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, parsed.String(), strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	request.Header.Set("content-type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("replication: callback request: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("replication: callback returned HTTP %d", response.StatusCode)
	}
	return nil
}

func pinnedClient(resolver Resolver, timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			if resolver == nil {
				resolver = net.DefaultResolver
			}
			ips, err := resolver.LookupNetIP(ctx, "ip", host)
			if err != nil || len(ips) == 0 {
				return nil, errors.New("replication: callback host did not resolve")
			}
			for _, ip := range ips {
				if isPrivateIP(ip) {
					continue
				}
				conn, dialErr := (&net.Dialer{Timeout: timeout}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if dialErr == nil {
					return conn, nil
				}
			}
			return nil, errors.New("replication: callback host resolved only to private addresses")
		},
	}
	return &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func isPrivateIP(addr netip.Addr) bool {
	ip := net.IP(addr.AsSlice())
	private := []string{"10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.168.0.0/16", "::1/128", "fc00::/7", "fe80::/10"}
	for _, raw := range private {
		_, network, err := net.ParseCIDR(raw)
		if err == nil && network.Contains(ip) {
			return true
		}
	}
	return false
}
