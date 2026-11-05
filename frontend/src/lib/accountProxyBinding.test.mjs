import assert from "node:assert/strict";
import test from "node:test";

import {
  buildProxyBindingContext,
  formatProxyDisplayURL,
  formatProxyEgressSummary,
  formatProxyShortName,
  isManagedProxyUnavailable,
  proxyIsUsable,
  resolveAccountProxyBinding,
} from "./accountProxyBinding.ts";

const healthy = {
  id: 1,
  url: "http://hk-01:8080",
  label: "HK-01",
  enabled: true,
  test_status: "success",
  test_ip: "1.2.3.4",
  test_location: "Hong Kong",
  test_latency_ms: 82,
};

const disabled = {
  id: 2,
  url: "http://jp-02:8080",
  label: "JP-02",
  enabled: false,
  test_status: "success",
};

const failed = {
  id: 3,
  url: "http://sg-03:8080",
  label: "SG-03",
  enabled: true,
  test_status: "error",
};

const groups = [
  { id: 10, name: "primary", proxy_urls: ["http://hk-01:8080"] },
  { id: 11, name: "no-proxy group", proxy_urls: [] },
  { id: 12, name: "all-down group", proxy_urls: ["http://sg-03:8080"] },
];

function ctxWith(overrides = {}) {
  return buildProxyBindingContext({
    proxies: [healthy, disabled, failed],
    groups,
    poolEnabled: true,
    globalProxy: "",
    ...overrides,
  });
}

test("pool membership mirrors ListEnabledProxies: enabled and not test-failed", () => {
  assert.equal(proxyIsUsable(healthy), true);
  assert.equal(proxyIsUsable(disabled), false);
  assert.equal(proxyIsUsable(failed), false);
  assert.equal(proxyIsUsable({ enabled: true }), true);

  const ctx = ctxWith();
  assert.equal(ctx.poolSize, 1);
  assert.deepEqual([...ctx.usable], ["http://hk-01:8080"]);
  assert.equal(ctx.managed.size, 3);
});

test("account pinned to a healthy pool entry is bound", () => {
  const binding = resolveAccountProxyBinding(
    { proxy_url: "http://hk-01:8080" },
    ctxWith(),
  );
  assert.equal(binding.kind, "bound");
  assert.equal(binding.url, "http://hk-01:8080");
  assert.equal(binding.proxy?.label, "HK-01");
  assert.equal(binding.usable, true);
});

test("account pinned to a disabled or test-failed managed proxy is fail-closed", () => {
  for (const url of ["http://jp-02:8080", "http://sg-03:8080"]) {
    const binding = resolveAccountProxyBinding({ proxy_url: url }, ctxWith());
    assert.equal(binding.kind, "bound_unusable");
    assert.equal(binding.usable, false);
    // The badge must still get that row, so the tooltip can say clearly which proxy is down.
    assert.equal(binding.proxy?.url, url);
  }
});

test("pool disabled keeps a pinned unavailable proxy working — no false alarm", () => {
  const ctx = ctxWith({ poolEnabled: false });
  const binding = resolveAccountProxyBinding(
    { proxy_url: "http://jp-02:8080" },
    ctx,
  );
  assert.equal(binding.kind, "bound");
  assert.equal(binding.usable, true);
  assert.equal(isManagedProxyUnavailable("http://jp-02:8080", ctx), false);
});

test("a pinned URL outside the proxies table is custom and never fail-closed", () => {
  const binding = resolveAccountProxyBinding(
    { proxy_url: "socks5://127.0.0.1:1080" },
    ctxWith(),
  );
  assert.equal(binding.kind, "bound_custom");
  assert.equal(binding.proxy, null);
  assert.equal(binding.usable, true);
});

test("account proxy is matched by trim only, never normalized", () => {
  const ctx = ctxWith();
  assert.equal(
    resolveAccountProxyBinding({ proxy_url: "  http://hk-01:8080  " }, ctx).kind,
    "bound",
  );
  // A trailing slash is a different key on the backend, so this must resolve to custom and must not be "fixed".
  assert.equal(
    resolveAccountProxyBinding({ proxy_url: "http://hk-01:8080/" }, ctx).kind,
    "bound_custom",
  );
});

test("unpinned account inherits the first group that still has a usable proxy", () => {
  const binding = resolveAccountProxyBinding(
    { proxy_url: "", group_ids: [11, 10] },
    ctxWith(),
  );
  assert.equal(binding.kind, "group");
  assert.equal(binding.groupName, "primary");
  // The inherited state does not report a specific URL: selection within the pool or group is sticky hashing, and a frontend copy would inevitably drift.
  assert.equal(binding.url, "");
});

test("a group whose proxies are all unavailable is skipped, not reported", () => {
  const binding = resolveAccountProxyBinding(
    { group_ids: [12, 10] },
    ctxWith(),
  );
  assert.equal(binding.kind, "group");
  assert.equal(binding.groupName, "primary");

  // Only when the group is entirely down does it continue falling back to the proxy pool.
  const fallback = resolveAccountProxyBinding({ group_ids: [12] }, ctxWith());
  assert.equal(fallback.kind, "pool");
});

test("unknown group ids and missing group data fall through", () => {
  const binding = resolveAccountProxyBinding({ group_ids: [999] }, ctxWith());
  assert.equal(binding.kind, "pool");
});

test("fallback order is pool then global then direct", () => {
  assert.equal(resolveAccountProxyBinding({}, ctxWith()).kind, "pool");

  const noPool = ctxWith({ proxies: [], globalProxy: "http://global:9090" });
  assert.equal(resolveAccountProxyBinding({}, noPool).kind, "global");

  const poolOff = ctxWith({ poolEnabled: false, proxies: [healthy] });
  assert.equal(resolveAccountProxyBinding({}, poolOff).kind, "direct");
  assert.equal(resolveAccountProxyBinding({}, poolOff).usable, true);
});

test("pool enabled with nothing usable and no global proxy blocks direct egress", () => {
  const ctx = ctxWith({ proxies: [disabled, failed], globalProxy: "" });
  const binding = resolveAccountProxyBinding({}, ctx);
  assert.equal(binding.kind, "direct_blocked");
  assert.equal(binding.usable, false);
});

test("null account resolves without throwing", () => {
  assert.equal(resolveAccountProxyBinding(null, ctxWith()).kind, "pool");
  assert.equal(resolveAccountProxyBinding(undefined, ctxWith()).kind, "pool");
});

test("short name prefers label, then host, then the raw string", () => {
  assert.equal(formatProxyShortName(healthy, healthy.url), "HK-01");
  assert.equal(
    formatProxyShortName(null, "http://user:pass@hk-01:8080"),
    "hk-01:8080",
  );
  assert.equal(formatProxyShortName(null, "not a url"), "not a url");
  assert.equal(formatProxyShortName(null, ""), "");
});

test("tooltip URLs keep scheme/host but never leak credentials", () => {
  assert.equal(
    formatProxyDisplayURL("socks5h://user:pass@66.253.7.45:10000"),
    "socks5h://***:***@66.253.7.45:10000",
  );
  // URLs without credentials are returned as-is; do not blur a URL that is already recognizable.
  assert.equal(
    formatProxyDisplayURL("http://hk-01:8080"),
    "http://hk-01:8080",
  );
  assert.equal(formatProxyDisplayURL("  http://hk-01:8080  "), "http://hk-01:8080");
  assert.equal(formatProxyDisplayURL("not a url"), "not a url");
  assert.equal(formatProxyDisplayURL(""), "");
});

test("egress summary only shows for a successful test", () => {
  assert.equal(formatProxyEgressSummary(healthy), "1.2.3.4 · Hong Kong · 82ms");
  assert.equal(formatProxyEgressSummary(failed), "");
  assert.equal(formatProxyEgressSummary(null), "");
});

test("resin overrides every layer of the proxy chain for codex accounts", () => {
  const ctx = buildProxyBindingContext({
    proxies: [healthy, disabled],
    groups: [{ id: 5, name: "hk", proxy_urls: ["http://hk-01:8080"] }],
    poolEnabled: true,
    globalProxy: "",
    resinEnabled: true,
  });

  // Unbound + pool on but only one entry: normally reports pool; under Resin it always reports resin and is schedulable.
  const unbound = resolveAccountProxyBinding({}, ctx);
  assert.equal(unbound.kind, "resin");
  assert.equal(unbound.url, "");
  assert.equal(unbound.usable, true);

  // Bound to a disabled managed proxy: normally it fails closed; once Resin takes over egress, it is no longer skipped.
  const pinned = resolveAccountProxyBinding({ proxy_url: disabled.url }, ctx);
  assert.equal(pinned.kind, "resin");
  assert.equal(pinned.url, disabled.url, "keeps the overridden URL for the tooltip");
  assert.equal(pinned.proxy, disabled);
  assert.equal(pinned.usable, true);

  // Group proxies are overridden in the same way.
  assert.equal(resolveAccountProxyBinding({ group_ids: [5] }, ctx).kind, "resin");

  // Pages that do not pass resinEnabled (relay channels) behave as before.
  const plain = buildProxyBindingContext({ proxies: [healthy], poolEnabled: true });
  assert.equal(plain.resinEnabled, false);
  assert.equal(resolveAccountProxyBinding({}, plain).kind, "pool");
});
