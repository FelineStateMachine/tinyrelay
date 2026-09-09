// Package policy contains the durable, host-independent policy for a relay.
package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

type Succession struct {
	Heir      string `json:"heir"`
	AfterDays int    `json:"afterDays"`
}

type Notify struct {
	Reports    bool `json:"reports"`
	Jobs       bool `json:"jobs"`
	Succession bool `json:"succession"`
	Digest     bool `json:"digest"`
}

type MemberInvites struct {
	Depth int `json:"depth"`
	Quota int `json:"quota"`
}

type Sites struct {
	Enabled bool `json:"enabled"`
	Mirror  bool `json:"mirror"`
}

type Features struct {
	Search    string `json:"search"`
	Sync      bool   `json:"sync"`
	Count     bool   `json:"count"`
	Discovery bool   `json:"discovery"`
	Names     bool   `json:"names"`
	Files     bool   `json:"files"`
	Pages     bool   `json:"pages"`
	Sites     Sites  `json:"sites"`
	Signer    bool   `json:"signer"`
	Marmot    bool   `json:"marmot"`
	Grasp     bool   `json:"grasp"`
	Grasp02   bool   `json:"grasp02"`
	Grasp03   bool   `json:"grasp03"`
	Grasp05   bool   `json:"grasp05"`
	Grasp06   bool   `json:"grasp06"`
	// Grasp08 enables the private-service profile. It is a tenant boundary,
	// not a repository visibility hint: all reads require current membership.
	Grasp08 bool `json:"grasp08"`
	Push    bool `json:"push"`
}

type Delivery struct {
	Enabled bool `json:"enabled"`
}

type Inbox struct {
	Targeted bool `json:"targeted"`
}

// FileLimits controls a tenant's file storage allowances. Zero is unlimited.
type FileLimits struct {
	MaxFileBytes     int64 `json:"maxFileBytes"`
	UserStorageBytes int64 `json:"userStorageBytes"`
}

type CustomHost struct {
	Host      string `json:"host"`
	ID        string `json:"id"`
	Site      string `json:"site,omitempty"`
	At        int64  `json:"at"`
	Status    string `json:"status"`
	SSLStatus string `json:"sslStatus"`
}

type RetentionRule struct {
	Kind int `json:"kind"`
	Days int `json:"days"`
}

// Policy is the tenant policy. Host capacity and scheduling controls are
// intentionally outside this type; they are not tenant entitlements.
type Policy struct {
	Owner              string            `json:"owner"`
	CustomHosts        []CustomHost      `json:"customHosts"`
	Succession         *Succession       `json:"succession"`
	Name               string            `json:"name"`
	Description        string            `json:"description"`
	Icon               string            `json:"icon"`
	Banner             string            `json:"banner"`
	Contact            string            `json:"contact"`
	PostingPolicy      string            `json:"postingPolicy"`
	PrivacyPolicy      string            `json:"privacyPolicy"`
	Tags               []string          `json:"tags"`
	LanguageTags       []string          `json:"languageTags"`
	RelayCountries     []string          `json:"relayCountries"`
	Notify             Notify            `json:"notify"`
	Writes             string            `json:"writes"`
	OpenKinds          []int             `json:"openKinds"`
	GuestReplies       bool              `json:"guestReplies"`
	BlockedWords       []string          `json:"blockedWords"`
	BlockedWordsInTags bool              `json:"blockedWordsInTags"`
	ReportThreshold    int               `json:"reportThreshold"`
	Reads              string            `json:"reads"`
	JoinTerms          string            `json:"joinTerms"`
	DirectoryPublic    bool              `json:"directoryPublic"`
	MinPow             int               `json:"minPow"`
	MaxFuture          int64             `json:"maxFuture"`
	MemberInvites      MemberInvites     `json:"memberInvites"`
	Views              map[string]string `json:"views"`
	Features           Features          `json:"features"`
	PushCallbacks      []string          `json:"pushCallbacks"`
	PrivatePeers       []string          `json:"privatePeers,omitempty"`
	FileLimits         FileLimits        `json:"fileLimits"`
	LetteredNips       bool              `json:"letteredNips"`
	Delivery           Delivery          `json:"delivery"`
	Inbox              Inbox             `json:"inbox"`
	Dumps              string            `json:"dumps"`
	DumpsKeep          int               `json:"dumpsKeep"`
	// Rooms caps the chat rooms a tenant may create beside its main group.
	// Zero switches room creation off.
	Rooms        int             `json:"rooms"`
	BlockedKinds []int           `json:"-"`
	AllowedKinds []int           `json:"-"`
	Retention    []RetentionRule `json:"-"`
}

// DefaultRooms is the room allowance applied when a policy does not name one.
const DefaultRooms = 64

// UnmarshalJSON keeps the room allowance at its default when a stored policy
// predates the field, so existing tenants do not lose room creation.
func (p *Policy) UnmarshalJSON(data []byte) error {
	type plain Policy
	decoded := plain{Rooms: DefaultRooms}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*p = Policy(decoded)
	return nil
}

func Defaults(owner string) Policy {
	return Policy{Owner: owner, CustomHosts: []CustomHost{}, Writes: "open", Reads: "open", DirectoryPublic: true, MaxFuture: 900, Dumps: "off", DumpsKeep: 7, Rooms: DefaultRooms,
		Features: Features{Search: "prose", Sync: true, Count: true, Discovery: true, Names: true, Files: true, Pages: true, Signer: true, Sites: Sites{Enabled: true, Mirror: true}},
		Notify:   Notify{}, Views: map[string]string{}, Tags: []string{}, LanguageTags: []string{}, RelayCountries: []string{}, OpenKinds: []int{}, BlockedWords: []string{}, PushCallbacks: []string{}, Delivery: Delivery{}}
}

func Validate(p Policy) error {
	if p.FileLimits.MaxFileBytes < 0 || p.FileLimits.UserStorageBytes < 0 {
		return errors.New("fileLimits: byte allowances cannot be negative")
	}
	if p.Writes != "open" && p.Writes != "allowlist" && p.Writes != "wot" && p.Writes != "owner" {
		return fmt.Errorf("writes: invalid rule %q", p.Writes)
	}
	if p.Reads != "open" && p.Reads != "auth" && p.Reads != "members" {
		return fmt.Errorf("reads: invalid rule %q", p.Reads)
	}
	if p.Features.Search != "full" && p.Features.Search != "prose" && p.Features.Search != "off" {
		return fmt.Errorf("features.search: invalid mode %q", p.Features.Search)
	}
	if p.Dumps != "off" && p.Dumps != "daily" && p.Dumps != "weekly" {
		return fmt.Errorf("dumps: invalid mode %q", p.Dumps)
	}
	if p.DumpsKeep < 0 {
		return errors.New("dumpsKeep: cannot be negative")
	}
	for name, value := range map[string]string{"postingPolicy": p.PostingPolicy, "privacyPolicy": p.PrivacyPolicy} {
		if value != "" {
			u, err := url.Parse(value)
			if err != nil || u.Scheme != "https" || u.Host == "" {
				return fmt.Errorf("%s: must be an https URL", name)
			}
		}
	}
	for _, peer := range p.PrivatePeers {
		u, err := url.Parse(strings.TrimRight(strings.TrimSpace(peer), "/"))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("privatePeers: invalid peer URL %q", peer)
		}
	}
	if p.Features.Sites.Mirror && !p.Features.Sites.Enabled {
		return errors.New("features.sites.mirror requires sites.enabled")
	}
	if (p.Features.Grasp02 || p.Features.Grasp03 || p.Features.Grasp05 || p.Features.Grasp06 || p.Features.Grasp08) && !p.Features.Grasp {
		return errors.New("grasp subfeatures require grasp")
	}
	if (p.Features.Grasp03 || p.Features.Grasp05) && !p.Features.Grasp02 {
		return errors.New("features.grasp03 and grasp05 require grasp02")
	}
	if p.Features.Grasp08 && p.Reads != "members" {
		return errors.New("features.grasp08 requires reads=members")
	}
	for name, value := range p.Views {
		if value != "off" && value != "write" && value != "hourly" && value != "daily" {
			return fmt.Errorf("views.%s: invalid setting", name)
		}
	}
	for _, word := range p.BlockedWords {
		if strings.HasPrefix(word, "/") && strings.HasSuffix(word, "/") {
			if _, err := regexp.Compile(word[1 : len(word)-1]); err != nil {
				return fmt.Errorf("blockedWords: %w", err)
			}
		}
	}
	if p.ReportThreshold < 0 || p.MinPow < 0 || p.MaxFuture < 0 || p.MemberInvites.Depth < 0 || p.MemberInvites.Quota < 0 || p.Rooms < 0 {
		return errors.New("policy: numeric values cannot be negative")
	}
	if p.Succession != nil && (p.Succession.Heir == "" || p.Succession.AfterDays <= 0) {
		return errors.New("succession: heir and positive afterDays required")
	}
	return nil
}

// Patch applies only recognized fields. A missing field is unchanged; an
// explicit null resets pointer fields and an explicit empty array clears it.
func Patch(cur Policy, raw map[string]json.RawMessage) (Policy, error) {
	b, err := json.Marshal(cur)
	if err != nil {
		return Policy{}, fmt.Errorf("marshal policy: %w", err)
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(b, &merged); err != nil {
		return Policy{}, fmt.Errorf("decode policy: %w", err)
	}
	for key, value := range raw {
		if _, ok := merged[key]; !ok {
			continue
		}
		if key == "features" || key == "notify" || key == "memberInvites" || key == "delivery" || key == "inbox" || key == "fileLimits" {
			var patchMap, currentMap map[string]json.RawMessage
			if json.Unmarshal(value, &patchMap) == nil && json.Unmarshal(merged[key], &currentMap) == nil {
				if key == "features" {
					if sites, ok := patchMap["sites"]; ok && string(sites) == "false" {
						patchMap["sites"] = json.RawMessage(`{"enabled":false}`)
					}
				}
				for nestedKey, nestedValue := range patchMap {
					currentMap[nestedKey] = nestedValue
				}
				value, err = json.Marshal(currentMap)
				if err != nil {
					return Policy{}, fmt.Errorf("marshal %s patch: %w", key, err)
				}
			}
		}
		merged[key] = value
	}
	b, err = json.Marshal(merged)
	if err != nil {
		return Policy{}, fmt.Errorf("marshal patch: %w", err)
	}
	var out Policy
	if err := json.Unmarshal(b, &out); err != nil {
		return Policy{}, fmt.Errorf("decode patch: %w", err)
	}
	if err := Validate(out); err != nil {
		return Policy{}, err
	}
	return out, nil
}
