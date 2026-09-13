import { describe, expect, it } from "vitest";
import { hexToBytes } from "@noble/hashes/utils.js";
import { Client, item, newEvent, newKey, now, RELAY_URL } from "./helpers.ts";

const secondary = process.env.RELAY_SECONDARY_URL;

describe.skipIf(!secondary)("cross-instance reconciliation", () => {
  it("reconciles overlapping actor histories and converges both relays", async () => {
    if (!process.env.CLAIM_SK || !secondary)
      throw new Error(
        "explicit disposable relay URLs and CLAIM_SK are required",
      );
    const keys = [
      hexToBytes(process.env.CLAIM_SK),
      newKey(),
      newKey(),
      newKey(),
    ];
    const tag = `cross-relay-${Date.now()}-${Math.random().toString(16).slice(2)}`;
    const events = keys.flatMap((key, actor) =>
      Array.from({ length: 48 }, (_, n) =>
        newEvent(
          key,
          1,
          `actor ${actor} event ${n}`,
          [["t", tag]],
          now() - 100 + n,
        ),
      ),
    );
    const local = await Client.connect(RELAY_URL);
    const remote = await Client.connect(secondary);
    try {
      for (const key of keys) {
        await local.auth(key, RELAY_URL);
        await remote.auth(key, secondary);
      }
      const left = events.filter((_, n) => n % 3 !== 0);
      const right = events.filter((_, n) => n % 3 !== 1);
      await Promise.all([
        (async () => {
          for (const event of left) await local.publish(event);
        })(),
        (async () => {
          for (const event of right) await remote.publish(event);
        })(),
      ]);
      const filter = { kinds: [1], "#t": [tag] };
      const diff = await remote.sync(left.map(item), filter);
      const leftIDs = new Set(left.map((event) => event.id));
      const rightIDs = new Set(right.map((event) => event.id));
      expect(diff.have).toEqual(
        [...leftIDs].filter((id) => !rightIDs.has(id)).sort(),
      );
      expect(diff.need).toEqual(
        [...rightIDs].filter((id) => !leftIDs.has(id)).sort(),
      );
      const missingHere = await remote.req({ ids: diff.need });
      const missingThere = await local.req({ ids: diff.have });
      for (const event of missingHere.events) await local.publish(event);
      for (const event of missingThere.events) await remote.publish(event);
      const expected = events.map((event) => event.id).sort();
      for (const client of [local, remote]) {
        const result = await client.req(filter);
        expect(result.events.map((event) => event.id).sort()).toEqual(expected);
      }
      const converged = await remote.sync(
        events.map(item),
        filter,
        "converged",
      );
      expect(converged.have).toEqual([]);
      expect(converged.need).toEqual([]);
      console.log(
        JSON.stringify({
          actors: keys.length,
          events: events.length,
          transferredEachWay: diff.have.length,
          converged: true,
        }),
      );
    } finally {
      local.close();
      remote.close();
    }
  }, 90_000);
});
