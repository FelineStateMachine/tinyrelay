# Hermes integration lab

This disposable Compose stack tests the native `tinyagent` connector against an unchanged mainline Hermes Agent checkout. The adapter registers the Hermes platform as `tiny`. The lab pins Hermes revision `1ad89ac018f26a4f21817ebf37bb09f508656d63` and verifies that the checkout is unchanged during the image build.

The stack uses a local deterministic OpenAI compatible model fixture, so it needs no model credentials or external provider. It exercises ordinary replies, native clarify and approval cards, multiple selection, custom and open-ended answers, attachment round trips, and explicit operator mentions. This verifies the real gateway and tool callbacks; the fixture does not perform open-ended reasoning. The default relay is available only on loopback at `http://127.0.0.1:18479`.

From the repository root, start the lab with:

```sh
docker compose -f labs/hermes/compose.yaml up -d --build --wait
docker compose -f labs/hermes/compose.yaml --profile test run --rm --no-deps check
```

Run the adapter tests against the pinned Hermes checkout with:

```sh
docker compose -f labs/hermes/compose.yaml --profile test run --rm --build adapter-test
```

The lab uses Hermes' standard queue mode so a new message waits for the current turn to finish. The operator helper can inspect the seeded room or send a test message:

```sh
docker compose -f labs/hermes/compose.yaml --profile tools run --rm operator help
docker compose -f labs/hermes/compose.yaml --profile tools run --rm operator send LAB_HELLO_manual
docker compose -f labs/hermes/compose.yaml --profile tools run --rm operator events
```

The check command prints a pass line for each scenario. To try a question manually, send `LAB_QUESTION_manual`, list requests, then answer with its event ID and an offered option ID:

```sh
docker compose -f labs/hermes/compose.yaml --profile tools run --rm operator send LAB_QUESTION_manual
docker compose -f labs/hermes/compose.yaml --profile tools run --rm operator requests
docker compose -f labs/hermes/compose.yaml --profile tools run --rm operator answer EVENT_ID c0
```

To explore the browser interface, load the disposable operator key into your test Nostr signer. `docker compose -f labs/hermes/compose.yaml run --rm operator export-key` prints that key. The agent and operator have separate identities stored in lab volumes; no production credentials are used. Agent grants last one day. Rerun the `seed` service to renew them.

Finish or cancel pending interactions before rebuilding Hermes. Callback waiters live in its process. An interrupted turn can also leave mainline Hermes waiting for a session lease on restart. To start fresh, stop the lab and remove its disposable volumes with:

```sh
docker compose -f labs/hermes/compose.yaml down -v
```

The stack models a member of one room. It does not claim direct-message support, moderator deletion, or durable interaction callbacks across a Hermes restart. Interaction waiters are process-local. Ordinary agent messages remain quiet; only an explicit `@` followed by a 64-character public key creates a notification recipient.
