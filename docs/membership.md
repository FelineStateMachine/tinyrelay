# Membership

A relay's members are the keys the owner admits. The owner and moderators add members on **Manage > People**, members bring in others with invites, and clients that speak [NIP-43](https://github.com/nostr-protocol/nips/blob/master/43.md) join and leave with signed requests. Every change is recorded in the audit log and published as the relay's signed member list.

## Invites

An invite is a code with a lifetime and a use limit. The owner and moderators create one on **Manage > People** or with the `createinvite` management method, which takes the lifetime in seconds, the maximum number of uses and a note. Members may create invites too, within the depth and quota set on **Manage > Rules**; removing a member with `removesubtree` also removes the people they invited.

Share the code as a link: `/invite/<code>` on the relay shows the join terms, if any, and joins the visitor with one signature from their Nostr signer. A claim is a named code created with `createclaim` for the same purpose. `listinvites`, `listclaims`, `revokeinvite` and `deleteclaim` manage them.

## Joining with NIP-43

A NIP-43 client asks a member's relay for an invite by requesting kind 28935 over an authenticated connection; the relay answers with a signed event that carries a `claim` tag. The invitee publishes a kind 28934 join carrying that claim and becomes a member. A kind 28936 leave removes the key.

The relay publishes its member list as kind 13534 and each admission as a kind 8000 add-user record, both signed by the relay's own key, so clients learn who belongs without a management call.

## Access requests

A kind 28934 join without a `claim` tag is an access request. Clients such as Flotilla send one when a person presses **Request Access**, with the reason in the event's content. The relay accepts it with `OK` and the reason `info: access request received, the relay owner will review it`, keeps the key, the reason and the time, and stores no event. The reason is kept up to 500 characters. A second request from the same key updates the reason and time; it does not add a second entry. A member who asks again is told `duplicate: already a member`, and a banned key is refused with a `restricted:` reason. A join that carries a claim is checked against the invite as before.

Review requests on **Manage > People** under **Access requests**. Each pending request shows the person, the reason and when they asked, with **Approve** and **Deny**. Approving adds the key as a member the same way **Save member** does, so the member list and add-user record follow; denying leaves the key outside the relay, and it may ask again. The last decided requests are listed below the pending ones with their state, and the panel counts the pending ones.

A new request wakes the owner and moderators on their devices in the **approvals** category, once per request, with the asker's short key and reason. Enable device notifications under **Inbox**. See [Installed app](app.md).

The same review is available to agents:

| Method | Tool | Browser tool |
| --- | --- | --- |
| `listjoinrequests` | `list_join_requests` | `tiny.list_join_requests` |
| `approvejoin <pubkey>` | `approve_join` | `tiny.approve_join` |
| `denyjoin <pubkey>` | `deny_join` | `tiny.deny_join` |

The management methods and tools follow the relay's roles: the owner and moderators may list and decide, and each decision is recorded in the audit log. See [MCP](mcp.md) and [Browser tools](webmcp.md).
