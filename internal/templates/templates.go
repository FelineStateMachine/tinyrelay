package templates

import (
	"encoding/json"
	"fmt"

	"github.com/FelineStateMachine/tinyrelay/internal/policy"
)

// Template describes a built-in relay policy and its recommended connections.
type Template struct {
	Name          string
	Title         string
	About         string
	Source        string
	EveryHours    int
	Policy        map[string]any
	AllowKinds    []int
	BlockKinds    []int
	RetentionDays int
	Connections   []string
}

// Connection describes one client integration displayed for a relay.
type Connection struct {
	Name       string
	Title      string
	About      string
	App        string
	Where      string
	Icon       string
	Feature    string
	Visibility string
	QR         string
	Inputs     []Input
	Links      []Link
}

// Input describes a value a connection asks the user to supply.
type Input struct{ Name, Label, Placeholder, Default, Pattern string }

// Link describes an external URL or copyable value for a connection.
type Link struct{ Label, Href, Copy string }

var templateCatalog = []Template{
	{Name: "default", Title: "Default", About: "Anyone writes, anyone reads, every kind, kept forever. Feature settings stay as they are.", Policy: map[string]any{"writes": "open", "reads": "open", "directoryPublic": true}},
	{Name: "outbox", Title: "Outbox", About: "Only you write, anyone reads. Your public notes and articles, kept forever. Private kinds are refused.", Policy: map[string]any{"writes": "owner", "reads": "open", "directoryPublic": true, "delivery": map[string]any{"enabled": true}}, BlockKinds: []int{4, 14, 1059}, Connections: []string{"notes", "find-me"}},
	{Name: "inbox", Title: "Inbox", About: "Anyone writes notes, replies, reactions and zaps meant for you; anyone reads. Everything is kept 90 days.", Policy: map[string]any{"writes": "open", "reads": "open", "directoryPublic": true, "inbox": map[string]any{"targeted": true}}, AllowKinds: []int{0, 1, 6, 7, 16, 1111, 1984, 9735, 9802}, RetentionDays: 90, Connections: []string{"find-me", "notes"}},
	{Name: "private", Title: "Private", About: "Only you write, only members read. Drafts, wallets and app data, every kind, kept forever.", Policy: map[string]any{"writes": "owner", "reads": "members", "directoryPublic": false}},
	{Name: "chat", Title: "Chat", About: "Members write, members read. Private messages and the group's chat, kept forever. The directory is hidden.", Policy: map[string]any{"writes": "allowlist", "reads": "members", "directoryPublic": false}, AllowKinds: []int{0, 7, 9, 11, 1059, 1111}, Connections: []string{"group"}},
	{Name: "media", Title: "Media", About: "A media host. Members upload files; the only events accepted are profiles and Blossom server lists. Anyone reads.", Policy: map[string]any{"writes": "allowlist", "reads": "open", "directoryPublic": true}, AllowKinds: []int{0, 10063}, Connections: []string{"files", "photos"}},
	{Name: "search", Title: "Search replica", About: "A read-only copy of another relay's notes, threads, comments, highlights, articles and wiki pages, refreshed every six hours. Anyone reads.", Source: "required", EveryHours: 6, Policy: map[string]any{"writes": "owner", "reads": "open", "directoryPublic": true}, AllowKinds: []int{0, 1, 11, 1111, 9802, 30023, 30818}, Connections: []string{"notes"}},
	{Name: "articles", Title: "Articles", About: "Long-form articles and the profiles behind them, nothing else. Only you write, anyone reads. Give a source relay to mirror its articles daily.", Source: "optional", EveryHours: 24, Policy: map[string]any{"writes": "owner", "reads": "open", "directoryPublic": true}, AllowKinds: []int{0, 30023}, Connections: []string{"notes"}},
	{Name: "dm", Title: "DM inbox", About: "An inbox for private messages. Anyone drops gift wraps, only members read them. Nothing else is accepted.", Policy: map[string]any{"writes": "open", "reads": "members", "directoryPublic": false}, AllowKinds: []int{0, 1059}, Connections: []string{"dm"}},
	{Name: "quiet", Title: "Quiet", About: "A small private relay with nothing that costs or announces. Members write and read; optional features are off.", Policy: map[string]any{"writes": "allowlist", "reads": "members", "directoryPublic": false, "features": map[string]any{"search": "off", "sync": false, "count": false, "discovery": false, "names": false, "files": false, "pages": false, "signer": false, "sites": false, "marmot": false, "grasp": false}}, Connections: []string{}},
	{Name: "site", Title: "Sites", About: "A static website host. Members publish manifests and files; the relay mirrors missing files into its own store.", Policy: map[string]any{"writes": "allowlist", "reads": "open", "directoryPublic": true, "features": map[string]any{"files": true, "sites": map[string]any{"enabled": true, "mirror": true}}}, Connections: []string{"notes"}},
	{Name: "marmot", Title: "Marmot", About: "Public Marmot transport. Anyone publishes KeyPackages and opaque group messages; gift wraps keep their recipient checks.", Policy: map[string]any{"writes": "open", "reads": "open", "directoryPublic": true, "features": map[string]any{"marmot": true}}, AllowKinds: []int{10002, 10050, 1059, 30443, 445}, Connections: []string{"marmot"}},
	{Name: "grasp", Title: "Git repositories", About: "A GRASP Git host. Admitted members publish repository state and related events.", Policy: map[string]any{"writes": "allowlist", "reads": "open", "directoryPublic": true, "features": map[string]any{"grasp": true}}, Connections: []string{"repos", "notes"}},
	{Name: "marmot-members", Title: "Marmot members", About: "Members publish Marmot KeyPackages and authenticate group message writes. Encrypted transport stays publicly readable.", Policy: map[string]any{"writes": "allowlist", "reads": "open", "directoryPublic": true, "features": map[string]any{"marmot": true}}, AllowKinds: []int{10002, 10050, 1059, 30443, 445}, Connections: []string{"marmot"}},
	{Name: "home", Title: "Home", About: "Your relay as your home on nostr. Members write, anyone reads; sites, files and Git hosting on.", Policy: map[string]any{"writes": "allowlist", "reads": "open", "directoryPublic": true, "features": map[string]any{"files": true, "sites": map[string]any{"enabled": true, "mirror": true}, "grasp": true}}, Connections: []string{"notes", "repos", "photos", "find-me", "group"}},
}

var connectionCatalog = []Connection{
	{Name: "notes", Title: "Notes", About: "Everything posted on this relay as one feed.", App: "Jumble", Where: "web", Icon: "notes", Visibility: "public", Links: []Link{{Label: "Open", Href: "https://jumble.social/?r={relay:url|enc}"}}},
	{Name: "find-me", Title: "Find me here", About: "The owner's profile with this relay attached, for feed apps.", App: "Primal", Where: "web, phone", Icon: "person", Visibility: "public", QR: "nostr:{owner:nprofile}", Links: []Link{{Label: "Open", Href: "https://primal.net/p/{owner:nprofile}"}, {Label: "Open in app", Href: "nostr:{owner:nprofile}"}}},
	{Name: "group", Title: "Group", About: "The relay as a space, with its group as a room.", App: "Flotilla", Where: "web, phone", Icon: "chat", Visibility: "public", QR: "nostr:{relay:naddr}", Links: []Link{{Label: "Open", Href: "https://app.flotilla.social/spaces/{relay:host|enc}"}, {Label: "Open group in app", Href: "nostr:{relay:naddr}"}, {Label: "Copy naddr", Copy: "{relay:naddr}"}}},
	{Name: "blog", Title: "Blog", About: "The owner's articles, with this relay attached so a reader finds the rest of them.", App: "YakiHonne", Where: "web, phone", Icon: "blog", Visibility: "public", QR: "nostr:{owner:nprofile}", Links: []Link{{Label: "Open", Href: "https://yakihonne.com/profile/{owner:nprofile}"}, {Label: "Open in app", Href: "nostr:{owner:nprofile}"}}},
	{Name: "repos", Title: "Repos", About: "This relay's Git repositories, and the clone command for yours.", App: "GitWorkshop", Where: "web, terminal", Icon: "git", Feature: "grasp", Visibility: "public", Inputs: []Input{{Name: "repo", Label: "Repository name", Placeholder: "my-project", Default: "<repo>", Pattern: "^[A-Za-z0-9][A-Za-z0-9._\\-]{0,63}$"}}, Links: []Link{{Label: "Open", Href: "https://gitworkshop.dev/relay/{relay:host|enc}"}, {Label: "Copy clone command", Copy: "git clone '{relay:web}/{user:npub}/{input:repo}.git'"}}},
	{Name: "sites", Title: "Sites", About: "Static sites this relay hosts, and the command that publishes one here.", App: "nsyte", Where: "web, terminal", Icon: "site", Feature: "sites", Visibility: "public", Links: []Link{{Label: "Open the owner's site", Href: "https://{owner:npub}.{relay:domain}"}, {Label: "Open your site", Href: "https://{user:npub}.{relay:domain}"}, {Label: "Copy publish command", Copy: "nsyte deploy ./dist --relays {relay:url} --servers {relay:web}"}}},
	{Name: "bookmarks", Title: "Bookmarks", About: "Your bookmarks and lists in a list manager; add this relay in its settings so they land here.", App: "Listr", Where: "web", Icon: "bookmark", Visibility: "auth", Links: []Link{{Label: "Open", Href: "https://listr.lol/{user:npub}"}, {Label: "Open in noStrudel", Href: "https://nostrudel.ninja/bookmarks/{user:nprofile}"}}},
	{Name: "photos", Title: "Photo library", About: "The owner's photos on this relay's file store.", App: "bouquet", Where: "web", Icon: "photo", Feature: "files", Visibility: "owner", Links: []Link{{Label: "Open", Href: "https://bouquet.slidestr.net/"}, {Label: "Copy Blossom server URL", Copy: "{relay:web}"}}},
	{Name: "files", Title: "Files", About: "This relay's file store, for members, in a media manager.", App: "bouquet", Where: "web", Icon: "files", Feature: "files", Visibility: "members", Links: []Link{{Label: "Open", Href: "https://bouquet.slidestr.net/"}, {Label: "Copy Blossom server URL", Copy: "{relay:web}"}, {Label: "Copy NIP-96 URL", Copy: "{relay:web}/.well-known/nostr/nip96.json"}}},
	{Name: "dm", Title: "Messages", About: "Message the owner privately; put this relay in your DM inbox list.", App: "0xchat", Where: "phone, desktop", Icon: "chat", Visibility: "public", QR: "nostr:{owner:nprofile}", Links: []Link{{Label: "Open in app", Href: "nostr:{owner:nprofile}"}}},
	{Name: "marmot", Title: "Encrypted groups", About: "End-to-end encrypted group chat over this relay.", App: "White Noise", Where: "phone", Icon: "lock", Feature: "marmot", Visibility: "public", Links: []Link{{Label: "Open", Href: "https://whitenoise.chat/"}, {Label: "Get the app", Href: "https://whitenoise.chat/download"}}},
	{Name: "relay-page", Title: "Relay page", About: "This relay's page in Coracle: its feed and its people.", App: "Coracle", Where: "web", Icon: "feed", Visibility: "public", Links: []Link{{Label: "Open", Href: "https://coracle.social/relays/{relay:host|enc}"}}},
}

func Names() []string {
	out := make([]string, 0, len(templateCatalog))
	for _, t := range templateCatalog {
		out = append(out, t.Name)
	}
	return out
}

// Connections returns the built-in connection catalog in display order.
func Connections() []Connection {
	out := make([]Connection, len(connectionCatalog))
	for i, c := range connectionCatalog {
		out[i] = cloneConnection(c)
	}
	return out
}

// Find returns a named template and a boolean indicating whether it exists.
func Find(name string) (Template, bool) {
	for _, t := range templateCatalog {
		if t.Name == name {
			return cloneTemplate(t), true
		}
	}
	return Template{}, false
}

func cloneTemplate(t Template) Template {
	t.Policy = cloneMap(t.Policy)
	t.AllowKinds = append([]int(nil), t.AllowKinds...)
	t.BlockKinds = append([]int(nil), t.BlockKinds...)
	t.Connections = append([]string(nil), t.Connections...)
	return t
}

func cloneConnection(c Connection) Connection {
	c.Inputs = append([]Input(nil), c.Inputs...)
	c.Links = append([]Link(nil), c.Links...)
	return c
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		if nested, ok := v.(map[string]any); ok {
			out[k] = cloneMap(nested)
		} else {
			out[k] = v
		}
	}
	return out
}

func ApplyTemplate(name, owner string) (policy.Policy, error) {
	t, ok := Find(name)
	if !ok {
		return policy.Policy{}, fmt.Errorf("template %q not found", name)
	}
	p := policy.Defaults(owner)
	b, err := jsonMarshal(t.Policy)
	if err != nil {
		return policy.Policy{}, err
	}
	p, err = policy.Patch(p, b)
	if err != nil {
		return policy.Policy{}, fmt.Errorf("template %q policy: %w", name, err)
	}
	if len(t.AllowKinds) > 0 {
		p.AllowedKinds = append([]int(nil), t.AllowKinds...)
	}
	if len(t.BlockKinds) > 0 {
		p.BlockedKinds = append([]int(nil), t.BlockKinds...)
	}
	if t.RetentionDays > 0 {
		p.Retention = []policy.RetentionRule{{Kind: -1, Days: t.RetentionDays}}
	}
	return p, nil
}

func jsonMarshal(v map[string]any) (map[string]json.RawMessage, error) {
	result := make(map[string]json.RawMessage, len(v))
	for k, value := range v {
		b, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		result[k] = b
	}
	return result, nil
}
