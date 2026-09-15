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
