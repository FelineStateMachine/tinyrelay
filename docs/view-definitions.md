# Reusable view definitions

Status: Proposal. These definitions are not accepted by the current management API. [Custom views](views.md) describes the block transforms available today.

A view selects relay data and presents a result. It can list events, summarize a set of events or render one event differently. A diagram renderer is one application of the third mode.

## Three modes

| Mode | Input and result | Examples |
| --- | --- | --- |
| `list` | A bounded event query becomes an ordered collection. | Articles by an author, open repository issues, a room feed. |
| `aggregate` | A bounded set of events becomes one derived result. | Counts by kind, a calendar with RSVP totals, a weekly activity report. |
| `render` | One event or an extracted part becomes a replacement or companion result. | Diagram blocks, a formatted event card, a rendered README. |

The modes share selection, access, refresh and presentation concepts. They have different result identities and update rules. A scheduled render remains a set of independent renders; running it hourly does not make it an aggregate.

## Shared fields

| Field | Meaning |
| --- | --- |
| `version` | Definition format version. |
| `name` | Stable owner-selected name. |
| `mode` | `list`, `aggregate` or `render`. |
| `source` | A Nostr filter and a bounded selection policy. |
| `refresh` | Refresh on matching writes or on an interval. Manual refresh is always available. |
| `audience` | Who can read the view: `public` or `members`. |
| `presentation` | How the result appears: events, a table, structured data or an inline rendering. |

`source.filter` uses the relay's existing Nostr filter semantics, including kinds, authors, IDs and tag filters, through its access-controlled query path. A source limit and deterministic ordering are explicit; equal timestamps use the relay's event ID ordering. A time window is separate from the refresh interval. `refresh.on` and `refresh.every_seconds` are mutually exclusive.

Mode-specific fields are validated for that mode. A list does not require a transform URL. An aggregate does not require a language. A render does not have to produce an image.

## Event list

An article view needs a query and a presentation:

```json
{
  "version": 1,
  "name": "articles",
  "mode": "list",
  "source": {
    "filter": { "kinds": [30023] },
    "limit": 100,
    "order": "newest"
  },
  "refresh": { "on": "write" },
  "audience": "public",
  "presentation": { "type": "events" }
}
```

The result retains event references. Existing event components can render them without an external service or generated HTML. Event replacement, deletion and expiration update membership in the list.

## Aggregate

A report counts notes and articles written in the last seven days:

```json
{
  "version": 1,
  "name": "weekly-activity",
  "mode": "aggregate",
  "source": {
    "filter": { "kinds": [1, 30023] },
    "window_seconds": 604800,
    "limit": 10000,
    "order": "newest"
  },
  "transform": {
    "type": "builtin",
    "name": "count",
    "group_by": ["kind"]
  },
  "refresh": { "every_seconds": 3600 },
  "audience": "members",
  "presentation": { "type": "table" }
}
```

This produces one result for the selected set. Its response records the time range, source count and whether the selection was truncated. A partial selection must not be presented as a complete total. A table and a chart can present the same structured result without rerunning the aggregation.

Built-in reducers cover common reports. An HTTP reducer can use the same mode only with an explicit, bounded event projection defining the fields sent to the service. It must not inherit the block endpoint's request contract.

## Event rendering

A diagram view selects events, extracts their fenced blocks and renders those blocks:

```json
{
  "version": 1,
  "name": "diagrams",
  "mode": "render",
  "source": {
    "filter": { "kinds": [1, 30023, 30818] },
    "limit": 500,
    "order": "newest"
  },
  "input": {
    "type": "fenced-blocks",
    "from": "content",
    "languages": ["mermaid", "dot", "d2"]
  },
  "transform": {
    "type": "http",
    "url": "https://diagram.zip/transform/blocks",
    "connection": "diagram-renderer",
    "options": { "appearance": "auto-transparent" }
  },
  "output": {
    "types": ["image/svg+xml", "image/png"],
    "max_bytes": 1048576
  },
  "refresh": { "on": "write" },
  "audience": "public",
  "presentation": {
    "type": "inline",
    "target": "input",
    "fallback": "source"
  }
}
```

Languages belong to the fenced-block extractor. Image types belong to this renderer's output contract. Neither is a requirement for every view.

Whole-event rendering uses an explicit field projection as its input and can return structured data or text for a registered presenter. Repository rendering uses an explicit `repo-readme` source adapter. Kind 30618 alone should not silently change event content into a Git file.

Transform options belong to the chosen adapter. Diagram appearance and frame settings do not become relay-wide view fields. Credentials live in the named connection and are excluded from exported definitions.

## Results and access

Each mode has a distinct result identity:

- A list identifies a query result and the referenced events.
- An aggregate identifies the definition revision and the selected source snapshot or window.
- A render identifies the definition revision, the input and the renderer revision. Reuse across events is allowed only when the renderer depends solely on the extracted input.

Changing a filter, transform option, output type or renderer revision invalidates the affected results. Refresh replaces a result atomically only after the new result passes validation. A completed run cannot overwrite a newer definition or source snapshot. Readers can see when a result was produced and whether it is stale or failed.

The view's audience cannot grant access to source data a reader could not otherwise read. Selection, presentation and HTTP serving enforce that rule. Public summaries of restricted data need a separate, explicit policy; choosing an aggregate mode must not publish private inputs. The source fields exported to an external transform are declared independently from the result's audience.

Each presentation has a server-rendered baseline. Lists use event components, tables use defined columns, structured results use the JSON viewer, and inline renders retain source access. Arbitrary HTML, scripts and executable templates are outside this proposal.

## Migration

The existing flat custom-view definition maps to `mode: "render"`: `kinds` becomes the source filter, `languages` moves into the fenced-block input, and the URL becomes an HTTP transform. Existing API calls and artifact URLs remain valid during migration. An old definition that includes repository state needs an explicit README adapter when exported to the new format.

The built-in article view is a candidate for `list`; calendar, zap and moderation summaries are candidates for `aggregate`. Their existing audience rules must remain intact. Presence is live state and needs a live source adapter rather than being forced into a stored event query.

Use one catalog and one definition validator for the three modes. Keep execution and result handling specific to each mode. Validate the shared design with an event list, an aggregate report and a block render before replacing the current APIs.
