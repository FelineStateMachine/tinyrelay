# Tiny agent interactions

Tiny agent interactions preserve a small set of structured choices in an ordinary room request. They are intended for an agent platform adapter that needs an inline answer while keeping the relay compatible with standard Nostr clients.

A native request remains a kind 9 room message and carries the usual `h` and `p` tags, plus:

```json
["request", "question"],
["tinyagent", "1"],
["interaction", "question"],
["selection", "single"],
["option", "continue", "Continue playing"],
["option", "stop", "Stop here"]
```

Use `selection=multiple` for checkboxes. Use `selection=text` for an open-ended question with no options. Add `freeform=true` to an option question to offer a text answer alongside its choices. Include `subject` and `expiration` when useful. The relay accepts at most 12 options. An option id is 1 to 64 characters and its label is 1 to 200 characters.

The `p` tags identify the people who may answer. Native notifications are sent only to asked people also named by a `mention` tag. This keeps an agent's pending card visible without waking a device unless the agent explicitly mentions that person.

Answers are kind 1111 NIP-22 replies naming the request. For a single selection, the content is the selected option id. For multiple selections, the content is a JSON array of option ids. A text answer uses the plain text content for `selection=text`, or `{"text":"..."}` when `freeform=true` accompanies options. When `freeform=true` accompanies options, an answer may include both a selection and an explanation as `{"choices":["stop"],"text":"The run needs review."}`. The `choices` array must contain one valid id for `selection=single`, or one or more unique valid ids for `selection=multiple`; `text` must be nonempty and at most 8,000 characters. The complete answer content is limited to 8,000 characters. The combined form accepts only the `choices` and `text` fields. The relay settles only answers from asked keys, before expiration, whose ids match the request. Older requests without the native tags retain the existing approve, deny and freeform reply behavior.

## Long task cards

A long task is a kind 43001 request that names the agent key in a `p` tag and carries the room in `h`. The Hermes adapter accepts a request from an allowed user in a room it watches, runs it as one turn in a thread anchored at the request (or at the root the request names in a marked `e` tag) and answers with the task kinds. Every answer carries `e` with the request id, `p` with the requester's key and `h` with the room, so clients derive the card state from the newest events.

| Kind | When the adapter publishes it | Content |
| --- | --- | --- |
| 43002 accepted | Once, when Hermes starts processing the request, not on receipt. A queued request is accepted when its turn begins. | Empty. |
| 43003 progress | While the turn runs, at most once every 15 seconds per task. Tool calls are never published one by one. | The newest line of intermediate assistant text, then the newest tool line, each clipped to 200 characters. Nothing is published when nothing changed. |
| 43004 result | Once, when the turn ends successfully. | The first line of the final reply, clipped to 200 characters, with an extra `e` tag naming the thread message that holds the full text. Posted files add a `Posted name, name` line and one `e` tag per file message. A turn that posted nothing carries the text Hermes returned, else `Done`. |
| 43006 error | Once, when the turn fails or is stopped without a requester cancel. | The error text Hermes exposes, `The task failed` when there is none, `Cancelled` when Hermes stopped the turn, or `Cancelled by <pubkey>` when an allowed user sent `!cancel`. |

Ordinary replies during the task stay kind 12 messages under the request root, so the room timeline keeps the conversation and the card summarises it. The final text is never repeated in the result; the card links it.

A task gets exactly one terminal event from the adapter. A kind 43005 cancel from the requester ends the task on its own: the adapter stops the turn and publishes nothing further. A `!cancel` message mentioning the agent in the thread, or in the room while the task runs, stops the turn and ends the task with a 43006 naming the sender. On start, the adapter checks the relay for an existing result, error or cancel before running a request, so a restart does not run a finished task again.

Hermes handoffs open a kind 11 thread in the home room whose content is the handoff name; the handed-off session continues there as kind 12 replies.
