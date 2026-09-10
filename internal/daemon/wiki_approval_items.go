package daemon

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/FelineStateMachine/tinyrelay/internal/event"
	"github.com/FelineStateMachine/tinyrelay/internal/storage"
)

// wikiApprovalItems adapts wiki proposals and merge requests to the common
// approvals shape. A proposal is addressed to the owner and moderators; a
// merge request is addressed only to its destination author.
func (t *Tenant) wikiApprovalItems(ctx context.Context, actor string, before *storage.EventCursor, limit int) ([]approvalItem, bool, error) {
	roles := t.wikiRoles()
	decides, err := roles.decides(ctx, actor)
	if err != nil {
		return nil, false, err
	}
	items, more, err := t.wikiProposalApprovalItems(ctx, actor, decides, roles, before, limit)
	if err != nil {
		return nil, false, err
	}
	merges, mergeMore, err := t.wikiMergeApprovalItems(ctx, actor, before, limit)
	if err != nil {
		return nil, false, err
	}
	items = append(items, merges...)
	return items, more || mergeMore, nil
}

func (t *Tenant) wikiProposalApprovalItems(ctx context.Context, actor string, decides bool, roles *wikiRoles, before *storage.EventCursor, limit int) ([]approvalItem, bool, error) {
	if !decides {
		return nil, false, nil
	}
	asked := t.wikiApprovalAudience(ctx)
	items := make([]approvalItem, 0, limit)
	more := false
	for {
		page, err := t.store.Query(ctx, event.Filter{Kinds: []int{kindWikiArticle}}, storage.QueryOptions{Now: nowUnix(), Access: storage.Access{All: true}, Limit: limit, Before: before})
		if err != nil {
			return nil, false, err
		}
		for i, row := range page.Events {
			before = &storage.EventCursor{CreatedAt: row.CreatedAt, ID: row.ID}
			proposer, err := roles.proposer(ctx, row.PubKey)
			if err != nil {
				return nil, false, err
			}
			if !proposer {
				continue
			}
			version := wikiVersionFrom(row, false)
			item := approvalItemFrom(row, approvalWikiProposal, strings.TrimRight(t.publicURL, "/"))
			item.Asked = asked
			item.Subject = version.Title
			item.About = &approvalAbout{Coordinate: version.Coordinate, Event: row.ID, Kind: "30818", URL: strings.TrimRight(t.publicURL, "/") + "/wiki/" + url.PathEscape(version.D) + "?version=" + url.QueryEscape(row.ID)}
			items = append(items, item)
			if len(items) == limit {
				more = page.More || i < len(page.Events)-1
				break
			}
		}
		if len(items) == limit || !page.More || len(page.Events) == 0 {
			break
		}
	}
	answers, err := t.approvalAnswers(ctx, items, nowUnix())
	if err != nil {
		return nil, false, err
	}
	for i := range items {
		approvalSettle(&items[i], answers[items[i].ID], nowUnix())
		if err := t.settleWikiProposal(ctx, &items[i], roles); err != nil {
			return nil, false, err
		}
	}
	return items, more, nil
}

func (t *Tenant) wikiMergeApprovalItems(ctx context.Context, actor string, before *storage.EventCursor, limit int) ([]approvalItem, bool, error) {
	page, err := t.store.Query(ctx, event.Filter{Kinds: []int{kindWikiMerge}, Tags: map[string][]string{"p": {actor}}}, storage.QueryOptions{Now: nowUnix(), Access: storage.Access{All: true}, Limit: limit, Before: before})
	if err != nil {
		return nil, false, err
	}
	items := make([]approvalItem, 0, len(page.Events))
	for _, row := range page.Events {
		merge := wikiMergeFrom(row)
		if merge.Destination != actor {
			continue
		}
		item := approvalItemFrom(row, approvalWikiMerge, strings.TrimRight(t.publicURL, "/"))
		item.Asked = []string{actor}
		item.Subject = "Wiki merge request"
		item.About = &approvalAbout{Coordinate: merge.Target, Event: row.ID, Kind: "818", URL: strings.TrimRight(t.publicURL, "/") + "/wiki/" + url.PathEscape(merge.TargetD) + "?merge=" + row.ID}
		items = append(items, item)
	}
	answers, err := t.approvalAnswers(ctx, items, nowUnix())
	if err != nil {
		return nil, false, err
	}
	for i := range items {
		approvalSettle(&items[i], answers[items[i].ID], nowUnix())
		status, err := t.wikiMergeStatus(ctx, items[i].ID, actor)
		if err != nil {
			return nil, false, err
		}
		if status.Status == wikiMergeOpen {
			items[i].State = "open"
			items[i].Answer = nil
		} else {
			items[i].State = "answered"
			answer, err := t.wikiReactionAnswer(ctx, status.Reaction)
			if err != nil {
				return nil, false, err
			}
			items[i].Answer = answer
		}
	}
	return items, page.More, nil
}

func (t *Tenant) settleWikiProposal(ctx context.Context, item *approvalItem, roles *wikiRoles) error {
	version := wikiVersionFrom(item.Event, false)
	versions := []wikiVersion{version}
	if err := t.wikiProposals(ctx, roles, versions); err != nil {
		return err
	}
	switch versions[0].Approval {
	case wikiProposalApproved, wikiProposalRejected:
		item.State = "answered"
		answer, err := t.wikiReactionAnswer(ctx, versions[0].ApprovalEvent)
		if err != nil {
			return err
		}
		item.Answer = answer
	default:
		item.State = "open"
		item.Answer = nil
	}
	return nil
}

func (t *Tenant) wikiReactionAnswer(ctx context.Context, id string) (*approvalAnswer, error) {
	if id == "" {
		return nil, nil
	}
	rows, err := t.store.Query(ctx, event.Filter{IDs: []string{id}, Kinds: []int{kindReaction}, Limit: intPtr(1)}, storage.QueryOptions{Now: nowUnix(), Access: storage.Access{All: true}})
	if err != nil || len(rows.Events) == 0 {
		return nil, err
	}
	reaction := rows.Events[0]
	decision := approvalDecision(reaction.Content)
	if decision == "" {
		return nil, nil
	}
	return &approvalAnswer{ID: reaction.ID, Kind: reaction.Kind, Author: reaction.PubKey, Decision: decision, Content: reaction.Content, CreatedAt: reaction.CreatedAt}, nil
}

// wikiApprovalByID returns a wiki request only when the caller is one of the
// people allowed to decide it, or the author of the proposed version. This
// keeps the detail endpoint consistent with the approvals listing while
// allowing a proposer to inspect their own pending request.
func (t *Tenant) wikiApprovalByID(ctx context.Context, actor, id string) (approvalItem, bool, error) {
	rows, err := t.store.Query(ctx, event.Filter{IDs: []string{id}, Kinds: []int{kindWikiArticle, kindWikiMerge}, Limit: intPtr(1)}, storage.QueryOptions{Now: nowUnix(), Access: storage.Access{All: true}})
	if err != nil || len(rows.Events) == 0 {
		return approvalItem{}, false, err
	}
	row := rows.Events[0]
	roles := t.wikiRoles()
	if row.Kind == kindWikiArticle {
		proposer, err := roles.proposer(ctx, row.PubKey)
		if err != nil {
			return approvalItem{}, false, err
		}
		decides, err := roles.decides(ctx, actor)
		if err != nil {
			return approvalItem{}, false, err
		}
		if !proposer || (!decides && row.PubKey != actor) {
			return approvalItem{}, false, nil
		}
		version := wikiVersionFrom(row, false)
		item := approvalItemFrom(row, approvalWikiProposal, strings.TrimRight(t.publicURL, "/"))
		item.Asked = t.wikiApprovalAudience(ctx)
		item.Subject = version.Title
		item.About = &approvalAbout{Coordinate: version.Coordinate, Event: row.ID, Kind: "30818", URL: strings.TrimRight(t.publicURL, "/") + "/wiki/" + url.PathEscape(version.D) + "?version=" + url.QueryEscape(row.ID)}
		return item, true, nil
	}
	merge := wikiMergeFrom(row)
	if merge.Destination == "" || merge.Destination != actor {
		return approvalItem{}, false, nil
	}
	item := approvalItemFrom(row, approvalWikiMerge, strings.TrimRight(t.publicURL, "/"))
	item.Asked = []string{actor}
	item.Subject = "Wiki merge request"
	item.About = &approvalAbout{Coordinate: merge.Target, Event: row.ID, Kind: "818", URL: strings.TrimRight(t.publicURL, "/") + "/wiki/" + url.PathEscape(merge.TargetD) + "?merge=" + url.QueryEscape(row.ID)}
	return item, true, nil
}

func (t *Tenant) wikiApprovalAudience(ctx context.Context) []string {
	moderators, err := t.community.Moderators(ctx)
	if err != nil {
		return []string{t.Policy().Owner}
	}
	return uniqueStrings(append([]string{t.Policy().Owner}, moderators...))
}

func nowUnix() int64 {
	return time.Now().Unix()
}
