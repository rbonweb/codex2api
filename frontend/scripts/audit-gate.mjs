#!/usr/bin/env node
// Frontend dependency vulnerability gate: equivalent to `npm audit --omit=dev --audit-level=high`,
// but it supports a per-advisory exception allowlist, which npm audit itself does not have,
// it can only lower --audit-level as a whole, which would also let real high findings through.
//
// Every allowlist entry must state "why this project is not affected" and a review date.
// The allowlist only applies to high/critical; other severities are not blocked anyway.
// If the audit result cannot be obtained (network or parse failure), always fail, so the gate cannot silently stop working.

import { spawnSync } from "node:child_process";

/**
 * Reviewed exceptions: GHSA id → reason (including the review date).
 * Before adding one, confirm that the vulnerability's trigger path does not exist in this project, rather than "nobody is exploiting it for now".
 */
const ALLOWLIST = new Map([
  // GHSA-qwww-vcr4-c8h2 (React Router RSC CSRF) was fixed in react-router 7.18.2,
  // the entry was removed on 2026-08-08. Before adding a new exception, confirm the trigger path does not exist in this project.
]);

const BLOCKING_SEVERITIES = new Set(["high", "critical"]);

const npmCommand = process.platform === "win32" ? "npm.cmd" : "npm";
const result = spawnSync(npmCommand, ["audit", "--omit=dev", "--json"], {
  encoding: "utf8",
  maxBuffer: 32 * 1024 * 1024,
  // On Windows, npm is a .cmd file and must be started through cmd.exe.
  shell: process.platform === "win32",
});

// npm audit exits non-zero when it finds vulnerabilities. That is normal output, not an execution failure; only a missing stdout is a real failure.
if (result.error || !result.stdout) {
  console.error("audit-gate: could not get npm audit results");
  if (result.error) console.error(result.error.message);
  if (result.stderr) console.error(result.stderr);
  process.exit(1);
}

let report;
try {
  report = JSON.parse(result.stdout);
} catch (err) {
  console.error(`audit-gate: failed to parse npm audit JSON:${err.message}`);
  process.exit(1);
}

/** Gets the GHSA id from the advisory url, and falls back to npm's numeric source id if there is none. */
function advisoryID(via) {
  const matched = /GHSA-[0-9a-z-]+/i.exec(via.url ?? "");
  return matched ? matched[0] : String(via.source ?? "unknown");
}

// vulnerabilities[pkg].via contains both advisory objects and strings that point to other package names (transitive dependencies),
// the advisories of the latter appear on their root package, so this only collects the object form and deduplicates by id.
const advisories = new Map();
for (const vuln of Object.values(report.vulnerabilities ?? {})) {
  for (const via of vuln.via ?? []) {
    if (typeof via !== "object" || !BLOCKING_SEVERITIES.has(via.severity)) continue;
    const id = advisoryID(via);
    if (!advisories.has(id)) {
      advisories.set(id, {
        id,
        title: via.title ?? "(no title)",
        pkg: via.name ?? vuln.name ?? "(unknown)",
        range: via.range ?? "(unknown)",
        url: via.url ?? "",
      });
    }
  }
}

const blocking = [];
const allowed = [];
for (const advisory of advisories.values()) {
  (ALLOWLIST.has(advisory.id) ? allowed : blocking).push(advisory);
}

for (const advisory of allowed) {
  console.log(`allowed ${advisory.id} [${advisory.pkg} ${advisory.range}] ${advisory.title}`);
  console.log(`         reason:${ALLOWLIST.get(advisory.id)}`);
}

// The vulnerability for an allowlist entry is gone (the dependency was upgraded or the advisory was withdrawn) → remind to remove it, so the allowlist does not rot.
const stale = [...ALLOWLIST.keys()].filter((id) => !advisories.has(id));
for (const id of stale) {
  console.log(`allowlist entry ${id} no longer matches, and can be removed from audit-gate.mjs`);
}

if (blocking.length === 0) {
  console.log(`audit-gate: passed (0 pending high/critical, ${allowed.length} allowed)`);
  process.exit(0);
}

console.error(`\naudit-gate: found ${blocking.length} high/critical vulnerabilities that are not allowlisted:`);
for (const advisory of blocking) {
  console.error(`  ${advisory.id} [${advisory.pkg} ${advisory.range}] ${advisory.title}`);
  if (advisory.url) console.error(`    ${advisory.url}`);
}
console.error(
  "\nFirst try upgrading with `npm audit fix`; only when you confirm this project is not affected, add the GHSA id and reason to ALLOWLIST in audit-gate.mjs.",
);
process.exit(1);
