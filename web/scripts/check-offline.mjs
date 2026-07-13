// Air-gap guard: the embedded console must not FETCH anything at runtime
// (no CDN scripts, remote fonts, or API hosts). The bundle legitimately
// contains external URLs in strings that never cause a request — React/Router
// error-doc links, third-party license/credit text, SVG namespaces. Those are
// not violations. What WOULD fetch is a remote URL in a network-loading
// position: a CSS `url(...)` / `@import`, or an HTML `src=`/`href=` attribute.
// Scan only those positions.
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";

const DIST = new URL("../../pkg/webui/dist", import.meta.url).pathname;

// Loading positions whose target must stay same-origin/relative.
const PATTERNS = [
  { re: /url\(\s*['"]?(https?:\/\/[^)'"]+)/gi, what: "css url()" },
  { re: /@import\s+['"](https?:\/\/[^'"]+)/gi, what: "css @import" },
  { re: /\b(?:src|href)\s*=\s*['"](https?:\/\/[^'"]+)/gi, what: "html attribute" },
];

// data: and same-origin are fine; only remote hosts fetch. (All PATTERNS
// already require http(s), so any match is remote by construction.)

function walk(dir) {
  const out = [];
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) out.push(...walk(p));
    else out.push(p);
  }
  return out;
}

let bad = 0;
for (const file of walk(DIST)) {
  if (!/\.(js|css|html)$/.test(file)) continue;
  const text = readFileSync(file, "utf8").replace(/\/\/# sourceMappingURL=.*$/gm, "");
  for (const { re, what } of PATTERNS) {
    for (const m of text.matchAll(re)) {
      console.error(`offline check: ${file.replace(DIST, "dist")}: ${what} → ${m[1]}`);
      bad++;
    }
  }
}

if (bad > 0) {
  console.error(`\n✗ ${bad} network-loading external URL(s) in the built console — the embed must be air-gapped.`);
  process.exit(1);
}
console.log("✓ offline check: no external fetches in the built console.");
