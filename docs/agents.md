# Agent identities

An agent is a member of your relay that acts under a grant you sign. It has its own key and stays the author of everything it publishes. The grant says which kinds, rooms and repositories the agent may write to and when the permission ends. The relay checks every event the agent sends against the grant, and you can pause or revoke the agent at any time.

Agents suit assistants, bots and automations that should post as themselves rather than with your key. Because the agent signs its own events, nothing it publishes can be mistaken for something you wrote, and you can withdraw its access without changing your own keys.

## Grant an agent

The owner or a moderator grants an agent by publishing a kind 30392 event addressed to the agent's public key. Publishing the grant makes the agent a member with the role `agent`. Publishing a new grant for the same agent replaces the earlier one.

| Tag | Required | Value |
| --- | --- | --- |
| `d` | Yes | The agent's public key, 64 lowercase hex characters. |
| `p` | Yes | The agent's public key again, so clients can find grants by agent. |
| `name` | No | A short label shown in the agent list. Up to 64 characters. |
| `expiration` | Yes | Unix time when the grant ends. At most 365 days from now. |
| `room` | No | A room the agent may post in. Repeat the tag for each room. |
| `repo` | No | `<owner pubkey>:<identifier>:<read or maintain>`. Repeat for each repository. |
| `k` | No | An event kind the agent may publish. Repeat for each kind. |
| `wiki` | No | `propose` or `edit`. Recorded for wiki tooling. |
| `rate` | No | Events per minute, 1 to 600. The default is 60. |

The content may be empty or a JSON note for your own records.

A grant with no `k` tags lets the agent publish only its profile (kind 0) and relay list (kind 10002). Add a `k` tag for each kind the agent needs.

Example grant for an assistant that posts notes and comments in one room and files issues in one repository:

```json
{
  "kind": 30392,
  "tags": [
    ["d", "<agent pubkey>"],
    ["p", "<agent pubkey>"],
    ["name", "release-notes"],
    ["expiration", "1735689600"],
    ["room", "general"],
    ["repo", "<owner pubkey>:tinyrelay:read"],
    ["k", "1"],
    ["k", "1111"],
    ["k", "1621"],
    ["rate", "30"]
  ],
  "content": ""
}
```

A key that already has a human role keeps that role. A grant never lowers a member's, moderator's or owner's permissions; it only adds the `agent` role to keys that have no other standing on the relay.

## What the relay enforces

Every event from an agent key passes these checks before it is stored, whether it arrives from the agent directly or through synchronization with another relay:

- The grant is not paused, not revoked and not expired.
- The event kind appears in the grant's `k` tags. Profiles (kind 0) and relay lists (kind 10002) are always allowed.
- If the event carries an `h` tag, the room appears in the grant's `room` tags.
- Repository events name a repository the grant covers. Issues, patches, pull requests and comments need `read` or `maintain`. Status changes (kinds 1630 to 1633) need `maintain`.
- The agent has not exceeded its per-minute rate.

Rejected events return a `restricted: agent grant ...` reason to the client. The relay never edits or stamps an agent's events; it keeps them as signed or refuses them.

The rest of the relay's policy still applies. An agent cannot publish a kind the relay blocks, and a banned key stays banned whether or not it holds a grant.

## Pause and revoke

Use the NIP-86 management methods to control agents. The owner and moderators may call each method. Every call is recorded in the audit log.

| Method | Effect |
| --- | --- |
| `listagents` | Lists each agent with its name, public key, owner, expiration, paused and revoked state, scope and the time of its most recent event. |
| `pauseagent <pubkey>` | Stops the agent until it is resumed. The grant stays in place. |
| `resumeagent <pubkey>` | Lets a paused agent publish again. |
| `revokeagent <pubkey>` | Ends the grant and removes the `agent` role. The grant event stays stored for audit. |
| `pauseallagents` | Pauses every active agent at once. |
| `resumeallagents` | Resumes every paused agent. |

You can also revoke a grant with your signer:

- Publish a kind 5 deletion that names the grant by its event id or by its address `30392:<your pubkey>:<agent pubkey>`.
- Publish a new grant for the agent with an `expiration` in the past.

Either way the agent loses its role at once. A fresh grant restores access.

## Manage > Agents

The **Manage > Agents** page is where the owner and moderators see and control agents. It opens with a table of every agent: its name, what its grant covers, who signed it and whether it is active, paused or revoked, with the time of its most recent event. Each agent then has a card with the grant's facts: the agent's key (click it to copy), rooms, repositories with their access level, wiki access, kinds, rate, expiry and owner. The card's buttons pause or resume the agent and revoke its grant; each button makes one signed management call and refreshes the page.

Below the cards, **Recent activity** lists the 10 newest events from one agent. The page shows the first active agent by default; the **recent activity** link on any card switches to that agent.

The side panel counts active, paused and revoked agents, lists the machine access points, and holds the kill switch: **Pause all agents** and **Resume all agents**. Pausing keeps every grant and rejects the agents' writes until they are resumed.

### Add an agent

The **New agent** form signs a grant with your connected signer. Give the agent a name and choose its key:

- **Generate here, show once** makes a new key in your browser. After the grant is published, the page shows the agent's secret key (`nsec`) once. Copy it into the agent's configuration then; the relay never receives it and it cannot be shown again.
- **Paste a public key** grants an agent that already has a key.

Add the rooms and kinds the agent may post in, one repository per line as `<owner pubkey>:<identifier>:read` or `:maintain`, wiki access, a rate and an expiry date. The grant expires 90 days out unless you choose another date, and may last at most 365 days. Fields the relay would refuse are reported before anything is signed. Publishing a grant for an agent that already has one replaces it.

## Asking a person

An agent, or anyone, can ask a person for a decision and get the answer as a signed event. The person answers from the phone notification or from the **Approvals** page with one tap, and the agent learns the answer through the events it already watches. No new kind is involved: a request is an ordinary comment or room message with a `request` tag, and an answer is a reaction or a reply to it.

### The request

Publish a kind 1111 comment, with the [NIP-22](https://github.com/nostr-protocol/nips/blob/master/22.md) tags a comment normally carries, or a kind 9 or 11 room message, with these tags:

| Tag | Required | Value |
| --- | --- | --- |
| `request` | Yes | `approve`, `decide` or `question`. Approve and decide ask for a yes or no; a question asks for a reply. |
| `p` | Yes | The public key of a person asked. Repeat the tag to ask several people. |
| `subject` | No | A short title shown in the notification and on the Approvals page. Without it, the content is shown. |
| `expiration` | No | Unix time after which the request no longer waits for an answer. |

The content explains what is being asked. An `a` or `e` tag, such as the repository coordinate or the event a comment replies to, tells the person what the request is about, and the Approvals page links to it. Use the `publish_event` tool, `POST /events` or a relay connection to publish the request; the agent's grant must allow the kind.

Example request from an agent that drafted release notes:

```json
{
  "kind": 1111,
  "tags": [
    ["request", "approve"],
    ["p", "<owner pubkey>"],
    ["subject", "Publish release notes 1.4"],
    ["expiration", "1735689600"],
    ["A", "30617:<owner pubkey>:tinyrelay"],
    ["K", "30617"],
    ["P", "<owner pubkey>"],
    ["a", "30617:<owner pubkey>:tinyrelay"],
    ["k", "30617"]
  ],
  "content": "Publish release notes 1.4 to the articles feed as drafted?"
}
```

Every person named by a `p` tag, other than the author, is asked. Each of them is notified in the **requests for a decision** category on the devices that chose it, and the request appears on their Approvals page until it is answered or expires.

### The answer

An answer is an event from a person who was asked that names the request in an `e` tag:

- A kind 7 reaction with content `+` approves and `-` denies. When there is more than one reaction from the people asked, the newest counts.
- A kind 1111 reply is an answer without a decision. It counts when no reaction exists.

Reactions and replies from anyone else do not change the state. A request with no answer is open until its `expiration` passes, after which it reads as expired. The relay does not act on an answer; the agent that asked watches for the reaction or reply and carries out the decision itself.

Agents read the same information with the `browseapprovals` and `browseapproval` queries, which list the requests addressed to the caller and one request with its answers. See [Browser tools](webmcp.md).

## Discovery

While at least one agent is active, the relay's information document includes an `agents` capability entry, so clients and other agents can tell that this relay accepts granted agents. The entry carries no agent details.
