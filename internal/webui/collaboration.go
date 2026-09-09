package webui

import "encoding/json"

// The in-process backend can return typed slices inside a map. Normalize the
// slice before handing it to templates, just as a JSON client would receive it.
func collaborationSlice(value any, field string) []any {
	items := valueMap(value)[field]
	if rows, ok := items.([]any); ok {
		return rows
	}
	var rows []any
	encoded, err := json.Marshal(items)
	if err == nil {
		_ = json.Unmarshal(encoded, &rows)
	}
	return rows
}

func collaborationItems(value any) []any   { return collaborationSlice(value, "items") }
func collaborationReplies(value any) []any { return collaborationSlice(value, "replies") }

// collaborationProposalView is how an issue, pull request or comment's
// proposal state reads for one viewer, with what the decision buttons need.
// Shown, Decide, State, At and By follow wikiProposalView.
type collaborationProposalView struct {
	wikiProposalView
	ID, Author, Kind string
}

// collaborationProposal reads an item's proposal state for the viewer. The
// item is a list or detail item, whose author is under author, or a reply,
// whose author is under pubkey. canApprove is the browse result's
// can_approve flag.
func collaborationProposal(item any, actor string, canApprove any) collaborationProposalView {
	m := valueMap(item)
	if m == nil || m["proposal"] != true {
		return collaborationProposalView{}
	}
	author := plainString(m["author"])
	if author == "" {
		author = plainString(m["pubkey"])
	}
	view := collaborationProposalView{ID: plainString(m["id"]), Author: author, Kind: plainString(m["kind"])}
	view.wikiProposalView = wikiProposal(map[string]any{"proposal": true, "author": author, "approval": m["approval"], "approval_at": m["approval_at"], "approval_by": m["approval_by"]}, actor, canApprove)
	return view
}

func collaborationLabels(value any) []string {
	var labels []string
	encoded, err := json.Marshal(valueMap(value)["labels"])
	if err == nil {
		_ = json.Unmarshal(encoded, &labels)
	}
	return labels
}
