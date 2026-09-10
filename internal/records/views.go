package records

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/policy"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

func effectiveViewTrigger(p policy.Policy, name string) string {
	if mode, ok := p.Views[name]; ok && mode != "" {
		return mode
	}
	return defaultViewTrigger(name)
}

func defaultViewTrigger(name string) string {
	return map[string]string{"profiles": "daily", "relays": "daily", "calendar": "hourly", "moderation": "daily", "articles": "write", "zaps": "hourly", "presence": "live"}[name]
}

// viewAudience applies directory and relay read policy to a view.
func viewAudience(p policy.Policy, name string) string {
	if name == "profiles" || name == "relays" {
		if !p.DirectoryPublic {
			return "members"
		}
	}
	if p.Reads == "members" {
		return "members"
	}
	return "public"
}

func viewStored(p policy.Policy, name string) bool {
	return name != "presence" && viewAudience(p, name) == "public"
}

func (s *Service) Views(ctx context.Context, caller policy.Access, now int64) ([]View, error) {
	p := s.policy()
	if p.Reads == "members" && !caller.Member && !caller.Owner {
		return nil, errors.New("restricted: views")
	}
	if p.Reads == "auth" && len(caller.PubKeys) == 0 && !caller.Owner {
		return nil, errors.New("auth-required: views")
	}
	var out []View
	for _, name := range viewNames {
		if name == "presence" {
			continue
		}
		if effectiveViewTrigger(p, name) == "off" {
			continue
		}
		if viewAudience(p, name) == "members" && !caller.Member && !caller.Owner {
			return nil, errors.New("auth-required: view is for members")
		}
		v, err := s.View(ctx, name, caller, now)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if p.Views["presence"] != "off" {
		e, err := s.Presence(ctx, caller, now)
		if err != nil {
			return nil, err
		}
		out = append(out, View{Name: "presence", Event: e, Rows: maxInt(0, len(e.Tags)-1)})
	}
	return out, nil
}

// View folds and signs one named view after applying audience and read policy.
func (s *Service) View(ctx context.Context, name string, caller policy.Access, now int64) (View, error) {
	p := s.policy()
	if !containsView(name) || effectiveViewTrigger(p, name) == "off" {
		return View{}, errors.New("not found: view")
	}
	if p.Reads == "members" && !caller.Member && !caller.Owner {
		return View{}, errors.New("restricted: views")
	}
	if p.Reads == "auth" && len(caller.PubKeys) == 0 && !caller.Owner {
		return View{}, errors.New("auth-required: views")
	}
	if viewAudience(p, name) == "members" && !caller.Member && !caller.Owner {
		return View{}, errors.New("auth-required: view is for members")
	}
	if name == "presence" {
		e, err := s.Presence(ctx, caller, now)
		if err != nil {
			return View{}, err
		}
		return View{Name: name, Event: e, Rows: maxInt(0, len(e.Tags)-1)}, nil
	}
	e, err := s.view(ctx, name, caller, now)
	if err != nil {
		return View{}, err
	}
	rows := len(e.Tags) - 3
	if name == "presence" {
		rows = len(e.Tags) - 1
	}
	return View{Name: name, Event: e, Rows: maxInt(0, rows)}, nil
}

func containsView(name string) bool {
	for _, n := range viewNames {
		if n == name {
			return true
		}
	}
	return false
}

func (s *Service) view(ctx context.Context, name string, caller policy.Access, now int64) (event.Event, error) {
	rows, err := s.store.Query(ctx, event.Filter{}, storage.QueryOptions{Now: now, Access: storage.Access{All: true}, Limit: 0})
	if err != nil {
		return event.Event{}, err
	}
	var tags [][]string
	var content string
	if name == "moderation" {
		tags, content = s.moderationFold(ctx, now)
	} else if name == "zaps" {
		tags, content = s.zapsFold(ctx, rows.Events, now)
	} else if name == "profiles" {
		people, names := s.viewPeople(ctx)
		rows.Events = onlyAuthors(rows.Events, people)
		tags, content = profileFold(rows.Events, s.relayURL, people, names)
	} else if name == "relays" {
		people, _ := s.viewPeople(ctx)
		tags, content = fold(name, onlyAuthors(rows.Events, people), s.relayURL, now)
	} else {
		tags, content = fold(name, rows.Events, s.relayURL, now)
	}
	trigger := s.policy().Views[name]
	if trigger == "" {
		trigger = map[string]string{"profiles": "daily", "relays": "daily", "calendar": "hourly", "moderation": "daily", "articles": "write", "zaps": "hourly"}[name]
	}
	tags = append([][]string{{"-"}, {"d", "bind.ws/view/" + name}, {"trigger", trigger}}, tags...)
	if viewStored(s.policy(), name) {
		return s.signed(ctx, event.KIND_VIEW, tags, content, now)
	}
	return s.signedOnly(ctx, event.KIND_VIEW, tags, content, now)
}

func onlyAuthors(events []event.Event, allowed map[string]string) []event.Event {
	out := make([]event.Event, 0, len(events))
	for _, e := range events {
		if _, ok := allowed[e.PubKey]; ok {
			out = append(out, e)
		}
	}
	return out
}

func (s *Service) viewPeople(ctx context.Context) (map[string]string, []string) {
	p := s.policy()
	people := make(map[string]string)
	order := make([]string, 0, 8)
	if p.Owner != "" {
		people[p.Owner] = ""
		order = append(order, p.Owner)
	}
	members, err := s.members(ctx)
	if err != nil {
		return people, order
	}
	for _, member := range members {
		if member.PubKey == "" {
			continue
		}
		if _, exists := people[member.PubKey]; !exists {
			order = append(order, member.PubKey)
		}
		people[member.PubKey] = member.Name
	}
	return people, order
}

func profileFold(events []event.Event, relayURL string, people map[string]string, order []string) ([][]string, string) {
	latest := make(map[string]event.Event)
	for _, e := range events {
		if e.Kind != event.KIND_PROFILE {
			continue
		}
		if old, ok := latest[e.PubKey]; !ok || e.CreatedAt > old.CreatedAt {
			latest[e.PubKey] = e
		}
	}
	host := relayURL
	if u, err := url.Parse(relayURL); err == nil && u.Host != "" {
		host = u.Host
	}
	host = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(host, "wss://"), "ws://"), "/")
	if host == "" {
		host = "relay"
	}
	out := make([][]string, 0, len(order))
	for _, pk := range order {
		m := map[string]any{}
		if e, ok := latest[pk]; ok {
			_ = json.Unmarshal([]byte(e.Content), &m)
		}
		nip05 := trimString(m["nip05"], 200)
		if people[pk] != "" {
			nip05 = people[pk] + "@" + host
		}
		out = append(out, []string{"p", pk, trimString(m["name"], 200), trimString(m["picture"], 2000), nip05})
	}
	return out, ""
}

func (s *Service) zapsFold(ctx context.Context, events []event.Event, now int64) ([][]string, string) {
	allowed := map[string]bool{}
	if owner := s.policy().Owner; owner != "" {
		allowed[owner] = true
	}
	if members, err := s.members(ctx); err == nil {
		for _, member := range members {
			allowed[member.PubKey] = true
		}
	}
	byEvent, byAuthor := map[string]int64{}, map[string]int64{}
	for _, receipt := range events {
		if receipt.Kind != 9735 {
			continue
		}
		var req event.Event
		if json.Unmarshal([]byte(event.Tag(receipt, "description")), &req) != nil || req.Kind != 9734 {
			continue
		}
		msats := bolt11MSats(event.Tag(receipt, "bolt11"))
		if msats <= 0 {
			continue
		}
		if pk := event.Tag(req, "p"); allowed[pk] {
			byAuthor[pk] += msats
		}
		if id := event.Tag(req, "e"); len(id) == 64 {
			var exists int
			if s.store.DB().QueryRowContext(ctx, `SELECT 1 FROM events WHERE id=?`, id).Scan(&exists) == nil {
				byEvent[id] += msats
			}
		}
	}
	out := make([][]string, 0, len(byEvent)+len(byAuthor))
	for id, n := range byEvent {
		out = append(out, []string{"e", id, strconv.FormatInt(n, 10)})
	}
	for pk, n := range byAuthor {
		out = append(out, []string{"p", pk, strconv.FormatInt(n, 10)})
	}
	sort.Slice(out, func(i, j int) bool {
		ni, _ := strconv.ParseInt(out[i][2], 10, 64)
		nj, _ := strconv.ParseInt(out[j][2], 10, 64)
		if ni != nj {
			return ni > nj
		}
		return out[i][1] < out[j][1]
	})
	return out, ""
}

func (s *Service) moderationFold(ctx context.Context, now int64) ([][]string, string) {
	month := time.Unix(now, 0).UTC().Format("2006-01")
	start := time.Unix(now, 0).UTC()
	start = time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC)
	// Preserve available counts and the wire format during a partial failure.
	// The community reader leaves each unavailable counter at zero.
	moderation, _ := s.moderationCounts(ctx, start.Unix())
	counts := map[string]any{"month": month, "bans": moderation.Bans, "reports": moderation.Reports, "resolved": moderation.Resolved, "hidden": moderation.Hidden, "deleted": 0, "blocked_addresses": moderation.BlockedAddresses}
	b, _ := json.Marshal(counts)
	return [][]string{{"month", month}}, string(b)
}

func fold(name string, events []event.Event, relayURL string, now int64) ([][]string, string) {
	profiles := map[string]event.Event{}
	relays := map[string]event.Event{}
	for _, e := range events {
		if e.Kind == event.KIND_PROFILE {
			if _, ok := profiles[e.PubKey]; !ok {
				profiles[e.PubKey] = e
			}
		}
		if e.Kind == 10002 {
			if _, ok := relays[e.PubKey]; !ok {
				relays[e.PubKey] = e
			}
		}
	}
	switch name {
	case "profiles":
		out := [][]string{}
		for pk, e := range profiles {
			if e.Kind != event.KIND_PROFILE {
				continue
			}
			var m map[string]any
			_ = json.Unmarshal([]byte(e.Content), &m)
			out = append(out, []string{"p", pk, trimString(m["name"], 200), trimString(m["picture"], 200), trimString(m["nip05"], 200)})
		}
		sort.Slice(out, func(i, j int) bool { return out[i][1] < out[j][1] })
		return out, ""
	case "relays":
		counts := map[string]int{}
		for _, e := range relays {
			seen := map[string]bool{}
			for _, t := range e.Tags {
				if len(t) < 2 || t[0] != "r" {
					continue
				}
				x, err := url.Parse(strings.TrimSpace(t[1]))
				if err != nil || (x.Scheme != "ws" && x.Scheme != "wss") || x.Host == "" {
					continue
				}
				u := strings.ToLower(x.Scheme + "://" + x.Host + strings.TrimRight(x.EscapedPath(), "/"))
				if x.RawQuery != "" {
					u += "?" + x.RawQuery
				}
				if seen[u] {
					continue
				}
				seen[u] = true
				counts[u]++
			}
		}
		keys := make([]string, 0, len(counts))
		for u := range counts {
			keys = append(keys, u)
		}
		sort.Slice(keys, func(i, j int) bool {
			if counts[keys[i]] != counts[keys[j]] {
				return counts[keys[i]] > counts[keys[j]]
			}
			return keys[i] < keys[j]
		})
		out := [][]string{}
		for _, u := range keys[:minInt(len(keys), 100)] {
			out = append(out, []string{"r", u, strconv.Itoa(counts[u])})
		}
		return out, ""
	case "calendar":
		type cal struct {
			e  event.Event
			at int64
		}
		var upcoming []cal
		for _, e := range events {
			if e.Kind != 31922 && e.Kind != 31923 {
				continue
			}
			at := calendarStart(e)
			if at < now-86400 || at > now+30*86400 {
				continue
			}
			upcoming = append(upcoming, cal{e, at})
		}
		sort.Slice(upcoming, func(i, j int) bool { return upcoming[i].at < upcoming[j].at })
		rsvp := map[string]map[string]bool{}
		for _, e := range events {
			if e.Kind != 31925 || event.Tag(e, "status") != "accepted" {
				continue
			}
			for _, a := range event.TagValues(e, "a") {
				if rsvp[a] == nil {
					rsvp[a] = map[string]bool{}
				}
				rsvp[a][e.PubKey] = true
			}
		}
		out := [][]string{}
		for _, x := range upcoming[:minInt(len(upcoming), 50)] {
			a := strconv.Itoa(x.e.Kind) + ":" + x.e.PubKey + ":" + event.Tag(x.e, "d")
			row := []string{"a", a, strconv.FormatInt(x.at, 10), event.Tag(x.e, "title")}
			if n := len(rsvp[a]); n > 0 {
				row = append(row, strconv.Itoa(n))
			}
			out = append(out, row)
		}
		return out, ""
	case "moderation":
		counts := map[string]int{"reports": 0, "deleted": 0}
		for _, e := range events {
			if e.Kind == event.KIND_REPORT {
				counts["reports"]++
			}
			if e.Kind == event.KIND_DELETION {
				counts["deleted"]++
			}
		}
		b, _ := json.Marshal(map[string]any{"month": time.Unix(now, 0).UTC().Format("2006-01"), "reports": counts["reports"], "deleted": counts["deleted"]})
		return [][]string{{"month", time.Unix(now, 0).UTC().Format("2006-01")}}, string(b)
	case "articles":
		articles := make([]event.Event, 0)
		for _, e := range events {
			if e.Kind == 30023 {
				articles = append(articles, e)
			}
		}
		sort.SliceStable(articles, func(i, j int) bool { return articlePublished(articles[i]) > articlePublished(articles[j]) })
		out := [][]string{}
		for _, e := range articles[:minInt(len(articles), 100)] {
			published := event.Tag(e, "published_at")
			if published == "" {
				published = strconv.FormatInt(e.CreatedAt, 10)
			}
			out = append(out, []string{"a", strconv.Itoa(e.Kind) + ":" + e.PubKey + ":" + event.Tag(e, "d"), event.Tag(e, "title"), published})
		}
		return out, ""
	case "zaps":
		byEvent, byAuthor := map[string]int64{}, map[string]int64{}
		for _, receipt := range events {
			if receipt.Kind != 9735 {
				continue
			}
			var req event.Event
			if json.Unmarshal([]byte(event.Tag(receipt, "description")), &req) != nil || req.Kind != 9734 {
				continue
			}
			msats := bolt11MSats(event.Tag(receipt, "bolt11"))
			if msats <= 0 {
				continue
			}
			if target := event.Tag(req, "p"); target != "" {
				byAuthor[target] += msats
			}
			if target := event.Tag(req, "e"); len(target) == 64 {
				byEvent[target] += msats
			}
		}
		out := make([][]string, 0, len(byEvent)+len(byAuthor))
		for id, n := range byEvent {
			out = append(out, []string{"e", id, strconv.FormatInt(n, 10)})
		}
		for pk, n := range byAuthor {
			out = append(out, []string{"p", pk, strconv.FormatInt(n, 10)})
		}
		sort.Slice(out, func(i, j int) bool {
			ni, _ := strconv.ParseInt(out[i][2], 10, 64)
			nj, _ := strconv.ParseInt(out[j][2], 10, 64)
			if ni != nj {
				return ni > nj
			}
			return out[i][1] < out[j][1]
		})
		return out, ""
	case "presence":
		return nil, ""
	default:
		return nil, ""
	}
}

// bolt11MSats parses the amount prefix of a BOLT11 invoice.
func bolt11MSats(invoice string) int64 {
	if len(invoice) < 6 || !strings.HasPrefix(strings.ToLower(invoice), "lnbc") {
		return 0
	}
	s := strings.ToLower(invoice[4:])
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0
	}
	amount := s[:i]
	suffix := ""
	if i < len(s) {
		suffix = s[i : i+1]
	}
	var factor float64
	switch suffix {
	case "m":
		factor = 1e8
	case "u":
		factor = 1e5
	case "n":
		factor = 1e2
	case "p":
		factor = 0.1
	case "":
		factor = 1e11
	default:
		return 0
	}
	var value float64
	if _, err := fmt.Sscanf(amount, "%f", &value); err != nil {
		return 0
	}
	value *= factor
	if value <= 0 || value > math.MaxInt64 {
		return 0
	}
	return int64(value + 0.5)
}

// MarkView marks a write-triggered view dirty.
func (s *Service) MarkView(ctx context.Context, name string, now int64) error {
	p := s.policy()
	if name == "presence" || effectiveViewTrigger(p, name) != "write" || !viewStored(p, name) {
		return nil
	}
	var dirty int64
	if err := s.store.GetSetting(ctx, "records.view."+name+".dirty", &dirty); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if dirty == 0 {
		return s.store.PutSetting(ctx, "records.view."+name+".dirty", now)
	}
	return nil
}

// NextDue returns the earliest scheduled view or pending write deadline.
func (s *Service) NextDue(ctx context.Context, now int64) (int64, error) {
	p := s.policy()
	next := int64(0)
	periods := map[string]int64{"profiles": 86400, "relays": 86400, "calendar": 3600, "moderation": 86400, "articles": 86400, "zaps": 3600}
	for name, period := range periods {
		mode := effectiveViewTrigger(p, name)
		if mode == "off" || !viewStored(p, name) {
			continue
		}
		if mode == "write" {
			period = 86400
		}
		if mode == "hourly" {
			period = 3600
		}
		if mode == "daily" {
			period = 86400
		}
		if mode == "write" {
			var dirty int64
			if err := s.store.GetSetting(ctx, "records.view."+name+".dirty", &dirty); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return 0, err
			}
			if dirty != 0 {
				due := dirty + 10
				if due <= now {
					return now, nil
				}
				if next == 0 || due < next {
					next = due
				}
				continue
			}
		}
		var last int64
		if err := s.store.GetSetting(ctx, "records.view."+name+".at", &last); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		due := last + period
		if last == 0 || due <= now {
			return now, nil
		}
		if next == 0 || due < next {
			next = due
		}
	}
	return next, nil
}

func (s *Service) viewFingerprint(ctx context.Context, name string, now int64) (string, error) {
	var n, max sql.NullInt64
	var q string
	switch name {
	case "calendar":
		q = `SELECT count(*),max(created_at) FROM events WHERE kind IN (31922,31923,31925)`
	case "zaps":
		q = `SELECT count(*),max(created_at) FROM events WHERE kind=9735`
	default:
		return "", nil
	}
	if err := s.store.DB().QueryRowContext(ctx, q).Scan(&n, &max); err != nil {
		return "", err
	}
	day := int64(0)
	if name == "calendar" {
		day = now / 86400
	}
	return fmt.Sprintf("%d:%d:%d", n.Int64, max.Int64, day), nil
}

func (s *Service) recordViewRun(ctx context.Context, name string, at int64, rows int) error {
	if _, err := s.store.DB().ExecContext(ctx, `INSERT INTO records_view_runs(name,at,rows) VALUES(?,?,?)`, name, at, rows); err != nil {
		return err
	}
	_, err := s.store.DB().ExecContext(ctx, `DELETE FROM records_view_runs WHERE name=? AND rowid NOT IN (SELECT rowid FROM records_view_runs WHERE name=? ORDER BY at DESC,rowid DESC LIMIT 60)`, name, name)
	return err
}

// ViewRuns returns bounded view run history.
func (s *Service) ViewRuns(ctx context.Context, name string) ([]ViewRun, error) {
	rows, err := s.store.DB().QueryContext(ctx, `SELECT at,rows FROM records_view_runs WHERE name=? ORDER BY at,rowid`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ViewRun, 0, 60)
	for rows.Next() {
		var r ViewRun
		if err := rows.Scan(&r.At, &r.Rows); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Service) ViewRowsSince(ctx context.Context, name string, since int64) (int64, error) {
	var total sql.NullInt64
	err := s.store.DB().QueryRowContext(ctx, `SELECT sum(rows) FROM records_view_runs WHERE name=? AND at>=?`, name, since).Scan(&total)
	if err != nil {
		return 0, err
	}
	return total.Int64, nil
}

func (s *Service) ViewSummaries(ctx context.Context) ([]map[string]any, error) {
	p := s.policy()
	out := make([]map[string]any, 0, len(viewNames))
	for _, name := range viewNames {
		trigger := effectiveViewTrigger(p, name)
		runs, err := s.ViewRuns(ctx, name)
		if err != nil {
			return nil, err
		}
		var last any
		if len(runs) > 0 {
			last = runs[len(runs)-1]
		}
		choices := []string{"off", "write", "hourly", "daily"}
		if name == "presence" {
			choices = []string{"off"}
		}
		about := map[string]string{"profiles": "every member's newest profile in one record", "relays": "where the members are: the union of their relay lists", "calendar": "what is on: calendar events starting in the next 30 days, with RSVPs counted", "moderation": "this month's moderation counts, no ids", "articles": "the newest hundred articles, by address", "zaps": "zap totals for the top notes and authors here", "presence": "who is connected now and who wrote in the last 15 minutes"}[name]
		out = append(out, map[string]any{
			"name": name, "about": about, "trigger": trigger, "default": defaultViewTrigger(name), "choices": choices,
			"audience": viewAudience(p, name), "on": trigger != "off", "stored": viewStored(p, name),
			"last": last, "path": "/view/" + name,
		})
	}
	return out, nil
}

func (s *Service) ViewNames() []string { return append([]string(nil), viewNames...) }
