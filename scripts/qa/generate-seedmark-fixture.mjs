#!/usr/bin/env node

// Regenerate the Seedmark v1 parity fixture from a local upstream checkout.
// Usage: node scripts/qa/generate-seedmark-fixture.mjs ../seedmark [output]
import {execFileSync} from 'node:child_process';
import {createHash} from 'node:crypto';
import {writeFile} from 'node:fs/promises';
import {pathToFileURL} from 'node:url';
import path from 'node:path';

const upstream = path.resolve(process.argv[2] || '../seedmark');
const output = path.resolve(process.argv[3] || 'internal/seedmark/testdata/v1.json');
const expectedCommit = 'c808a940bc193f97512c02d58da33ecc28ba1f53';
const commit = execFileSync('git', ['-C', upstream, 'rev-parse', 'HEAD'], {encoding: 'utf8'}).trim();
if (commit !== expectedCommit) {
  throw new Error(`Seedmark checkout is ${commit}; expected ${expectedCommit}`);
}

const {avatar, recipe} = await import(pathToFileURL(path.join(upstream, 'src/index.js')).href);
const seeds = [
  '',
  'seedmark',
  'a'.repeat(64),
  'A'.repeat(64),
  '0'.repeat(64),
  'seed/\u0000part',
  'é',
  'e\u0301',
  '猫🌱',
  '👩‍💻',
  ...Array.from({length: 1000}, (_, index) => `sample/${index}`),
];
const fixtures = seeds.map(seed => {
  const svg = avatar(seed);
  return {
    seed,
    family: recipe(seed).family,
    sha256: createHash('sha256').update(svg).digest('hex'),
  };
});
await writeFile(output, `${JSON.stringify(fixtures, null, 2)}\n`);
process.stderr.write(`wrote ${fixtures.length} cases from Seedmark ${commit} to ${output}\n`);
