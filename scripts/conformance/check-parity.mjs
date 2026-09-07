#!/usr/bin/env node
import { readFileSync, readdirSync } from "node:fs";
import { resolve } from "node:path";

const root = resolve(import.meta.dirname, "../..");
const sourceRoot = resolve(process.env.BINDWS_ROOT ?? resolve(root, "../bindws"));
const ledger = JSON.parse(readFileSync(resolve(root, "docs/parity.json"), "utf8"));
const fail = (message) => { throw new Error(`parity ledger: ${message}`); };

if (!/^[0-9a-f]{7,40}$/.test(ledger.source.revision)) fail("source revision is missing");
for (const item of [...ledger.items, ...ledger.exclusions]) {
  if (!item.id || !item.source || !item.status || !item.check) fail(`incomplete item ${item.id ?? "<unnamed>"}`);
  if (!new Set(["pending", "verified", "intentional-change"]).has(item.status)) fail(`invalid status for ${item.id}`);
}

const manage = readFileSync(resolve(sourceRoot, "src/manage.ts"), "utf8").split("export const METHODS", 2)[1];
const methods = [...manage.matchAll(/^  ([A-Za-z][A-Za-z0-9_]*):/gm)].map((m) => m[1]);
const listedMethods = ledger.registryInventories.managementMethods.items;
const missingMethods = methods.filter((name) => name !== "claim" && !listedMethods.includes(name));
const extraMethods = listedMethods.filter((name) => !methods.includes(name));
if (missingMethods.length || extraMethods.length) fail(`management registry drift missing=${missingMethods} extra=${extraMethods}`);

const compareFiles = (key, directory, suffix) => {
  const actual = readdirSync(resolve(sourceRoot, directory)).filter((name) => name.endsWith(suffix)).sort();
  const listed = [...ledger.registryInventories[key].items].sort().map((name) => key === "conformanceTests" ? name : `${name}${suffix}`);
  if (actual.join("\n") !== listed.join("\n")) fail(`${key} drift`);
};
compareFiles("relayTemplates", "relay-templates", ".jsonc");
compareFiles("connectionTemplates", "connection-templates", ".jsonc");
compareFiles("conformanceTests", "test/conformance", ".test.ts");
console.log(`parity ledger OK: ${ledger.items.length} retained families, ${listedMethods.length} retained methods`);
