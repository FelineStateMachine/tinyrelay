package daemon

import (
	"context"
	"net/url"
	"strings"
)

// personSegment is how a person is written in a page path: the member name
// when the key has one, else the hex pubkey.
func (t *Tenant) personSegment(ctx context.Context, pubkey string) string {
	if t.community == nil {
		return pubkey
	}
	name, err := t.community.MemberName(ctx, pubkey)
	if err != nil || name == "" {
		return pubkey
	}
	return url.PathEscape(name)
}

// repoPageURL is the repository home page under the public URL.
func (t *Tenant) repoPageURL(ctx context.Context, owner, identifier string) string {
	return strings.TrimRight(t.publicURL, "/") + "/repos/" + t.personSegment(ctx, owner) + "/" + url.PathEscape(identifier)
}

// repoItemURL is the page of an issue ("issues") or pull request ("prs")
// in the repository.
func (t *Tenant) repoItemURL(ctx context.Context, owner, identifier, section, id string) string {
	return t.repoPageURL(ctx, owner, identifier) + "/" + section + "/" + id
}

// wikiProposalURL is the merge request compare page of a wiki page.
func (t *Tenant) wikiProposalURL(d, id string) string {
	return strings.TrimRight(t.publicURL, "/") + "/wiki/" + url.PathEscape(d) + "/proposals/" + id
}
