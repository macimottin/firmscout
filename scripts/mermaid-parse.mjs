// Validate every fenced ```mermaid block in the repository with the real Mermaid
// parser.
//
// This runs Mermaid's own grammar in a jsdom document rather than rendering to an
// image, so it needs no headless browser and no Chromium download. That matters: the
// usual approach, @mermaid-js/mermaid-cli, pulls Puppeteer, which fails in many CI and
// container environments and leaves diagram validation silently skipped. A skipped
// check that reports success is worse than no check.
//
// Usage: node scripts/mermaid-parse.mjs <root>
import fs from 'node:fs';
import path from 'node:path';
import { JSDOM } from 'jsdom';

const dom = new JSDOM('<!DOCTYPE html><body></body>', { pretendToBeVisual: true });
global.window = dom.window;
global.document = dom.window.document;
Object.defineProperty(global, 'navigator', { value: dom.window.navigator, configurable: true });

const mermaid = (await import('mermaid')).default;
mermaid.initialize({ startOnLoad: false, securityLevel: 'strict' });

// GitHub renders these natively. Anything else may look fine locally and break in the
// repository, which is where the diagrams actually have to work.
const ALLOWED = /^(flowchart|graph|sequenceDiagram|stateDiagram-v2|erDiagram|classDiagram|journey|pie)\b/;

const IGNORED_DIRS = new Set(['node_modules', '.git', '.next', 'dist', 'vendor']);

function markdownFiles(dir, out = []) {
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      if (!IGNORED_DIRS.has(entry.name)) markdownFiles(full, out);
    } else if (entry.name.endsWith('.md')) {
      out.push(full);
    }
  }
  return out;
}

const root = process.argv[2] ?? '.';
const failures = [];
let total = 0;
let files = 0;

for (const file of markdownFiles(root)) {
  const text = fs.readFileSync(file, 'utf8');
  const blocks = [...text.matchAll(/```mermaid\r?\n([\s\S]*?)```/g)].map((m) => m[1]);
  if (blocks.length === 0) continue;
  files++;

  for (let i = 0; i < blocks.length; i++) {
    total++;
    const rel = path.relative(root, file);
    const source = blocks[i];
    const firstLine = source.split('\n').map((l) => l.trim()).find((l) => l && !l.startsWith('%%')) ?? '';

    if (!ALLOWED.test(firstLine)) {
      failures.push(`${rel} block ${i + 1}: diagram type "${firstLine.split(/\s/)[0]}" is not rendered by GitHub`);
      continue;
    }
    try {
      await mermaid.parse(source);
    } catch (err) {
      const detail = String(err?.message ?? err).split('\n').slice(0, 3).join(' | ');
      failures.push(`${rel} block ${i + 1}: ${detail}`);
    }
  }
}

console.log(`Mermaid: parsed ${total} block(s) across ${files} file(s).`);
if (failures.length > 0) {
  console.log('');
  for (const f of failures) console.log(`  FAIL ${f}`);
  console.log('');
  console.log(`RESULT: FAILED (${failures.length} of ${total} blocks)`);
  process.exit(1);
}
console.log('RESULT: PASSED');
