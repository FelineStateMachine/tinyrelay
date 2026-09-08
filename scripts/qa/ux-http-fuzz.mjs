#!/usr/bin/env node
// Read-only HTTP fuzzing for the browser-facing hosted-content viewers.
import { readFile, mkdir, writeFile } from "node:fs/promises";
import { dirname, resolve } from "node:path";

const args = process.argv.slice(2);
const option = (key, fallback) => args.includes(key) ? args[args.indexOf(key) + 1] : fallback;
const manifestPath = resolve(option("--manifest", "output/playwright/ux/fixture.json"));
const manifest = JSON.parse(await readFile(manifestPath, "utf8"));
const base = (option("--relay", manifest.relay)).replace(/\/$/, "");
const out = resolve(option("--out", "output/playwright/ux/http-fuzz.json"));
const timeoutMs = Number(option("--timeout-ms", "5000"));
const unusual = " café/路径 #hash ?query %percent &amp; <script>alert(1)</script>\u0000";
const prefixes = ["", "/r/main"];
const cases = [];
const add = (name, path, query = "") => { for (const prefix of prefixes) cases.push({ name: `${prefix || "root"} ${name}`, url: `${base}${prefix}${path}${query}` }); };

for (const q of ["", "?limit=-1&cursor=%00", `?q=${encodeURIComponent(unusual)}&limit=999999`, "?owner=%00&ref=refs%2Fheads%2Fmain&path=%2F..%2F"])
  add("search", "/search" + q);
for (const q of ["", `?owner=${encodeURIComponent(unusual)}&repo=${encodeURIComponent(unusual)}&ref=%00&path=${encodeURIComponent(unusual)}`, "?owner=%00&repo=%00&limit=NaN&cursor=%00"])
  add("repo", "/repo" + q);
for (const q of ["", `?hash=${encodeURIComponent(unusual)}&limit=-1&cursor=%00`, "?sha=%00&hash=not-a-sha"])
  add("file", "/file" + q);
for (const id of ["", "not-an-event", "0".repeat(64), unusual, manifest.notes[0].id]) add("event", "/e/" + encodeURIComponent(id));

const results = [];
const duplicateIDs = html => [...html.matchAll(/\bid=["']([^"']+)["']/gi)].map(m => m[1]).filter((id, i, all) => all.indexOf(id) !== i);
async function request(item) {
  const controller = new AbortController(); const timer = setTimeout(() => controller.abort(), timeoutMs);
  const started = performance.now();
  try {
    const response = await fetch(item.url, { method: "GET", redirect: "manual", signal: controller.signal });
    const body = await response.text();
    const duplicate = duplicateIDs(body);
    const injected = /<script\s*>\s*alert\s*\(/i.test(body) || body.includes("<script>alert(1)</script>");
    return { ...item, status: response.status, ms: Math.round(performance.now() - started), bytes: body.length, ok: response.status < 500 && !injected && duplicate.length === 0, checks: { no5xx: response.status < 500, noScriptInjection: !injected, noDuplicateIDs: duplicate.length === 0 }, ...(duplicate.length ? { duplicateIDs: duplicate } : {}) };
  } catch (error) { return { ...item, status: 0, ms: Math.round(performance.now() - started), ok: false, error: error.name === "AbortError" ? "timeout" : error.message }; }
  finally { clearTimeout(timer); }
}
for (const item of cases) results.push(await request(item));

const valid = [];
const validGet = async (name, path, expected) => {
  const item = { name: `valid ${name}`, url: base + path }; const row = await request(item);
  const body = row.status ? await (await fetch(item.url)).text() : "";
  row.checks = { ...row.checks, expected: body.includes(expected), noClassAttributes: !/\bclass\s*=/i.test(body) };
  row.ok = row.ok && row.checks.expected && row.checks.noClassAttributes;
  valid.push(row);
};
const note = manifest.notes[0], article = manifest.articles[0], file = manifest.files.find(f => f.label === "markdown");
await validGet("note", `/e/${note.id}`, note.id);
await validGet("article", "/a/ux-long-article", "A long article");
await validGet("articles", "/articles", "A long article");
await validGet("files", "/files", "Files");
await validGet("file", `/file?hash=${file.sha256}`, file.sha256);
await validGet("repos", "/repos", manifest.repository.id);
await validGet("repo home", `/repo?owner=${manifest.owner}&repo=${manifest.repository.id}`, manifest.repository.id);
await validGet("repo tree", `/repo?owner=${manifest.owner}&repo=${manifest.repository.id}&view=tree&ref=refs%2Fheads%2Fmain`, "README UX fixture.md");
await validGet("repo file", `/repo?owner=${manifest.owner}&repo=${manifest.repository.id}&view=file&ref=refs%2Fheads%2Fmain&path=${encodeURIComponent("nested dir/Unicode café/emoji 🧪.md")}`, "unicode path content");

const report = { schema: 1, fixture: manifest.fixture, relay: base, generatedAt: new Date().toISOString(), cases: cases.length, fuzz: results.length, validCases: valid.length, passed: [...results, ...valid].filter(r => r.ok).length, failed: [...results, ...valid].filter(r => !r.ok).length, results, valid };
await mkdir(dirname(out), { recursive: true }); await writeFile(out, JSON.stringify(report, null, 2) + "\n");
console.log(JSON.stringify({ ok: report.failed === 0, cases: report.cases, valid: report.validCases, passed: report.passed, failed: report.failed, report: out }));
process.exitCode = report.failed ? 1 : 0;
