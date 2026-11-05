import assert from "node:assert/strict";
import test from "node:test";

import { getUsageTokenBreakdown } from "./usageTokenDisplay.ts";

test("input stays the stored total and cache buckets are reported separately", () => {
  assert.deepEqual(
    getUsageTokenBreakdown({
      channel: "codex",
      input_tokens: 1_000,
      output_tokens: 25,
      cached_tokens: 700,
      cache_write_5m_tokens: 100,
      cache_write_1h_tokens: 150,
    }),
    {
      totalInputTokens: 1_000,
      inputTokens: 1_000,
      outputTokens: 25,
      cacheReadTokens: 700,
      cacheWriteTokens: 250,
      cacheWrite5mTokens: 100,
      cacheWrite1hTokens: 150,
    },
  );
});

test("the channel and model name never change the displayed input", () => {
  for (const channel of [undefined, "", " ", null, "codex", "claude"]) {
    const result = getUsageTokenBreakdown({
      channel,
      model: "claude-sonnet-4-6",
      input_tokens: 1_000,
      output_tokens: 25,
      cached_tokens: 700,
      cache_write_5m_tokens: 100,
    });
    assert.equal(result.inputTokens, 1_000);
    assert.equal(result.cacheReadTokens, 700);
    assert.equal(result.cacheWriteTokens, 100);
  }
});

test("missing, negative, and non-finite token values normalize to zero without coercion", () => {
  for (const invalid of [undefined, null, -1, -Infinity, Infinity, NaN, "42"]) {
    assert.deepEqual(
      getUsageTokenBreakdown({
        input_tokens: invalid,
        output_tokens: invalid,
        cached_tokens: invalid,
        cache_write_5m_tokens: invalid,
        cache_write_1h_tokens: invalid,
      }),
      {
        totalInputTokens: 0,
        inputTokens: 0,
        outputTokens: 0,
        cacheReadTokens: 0,
        cacheWriteTokens: 0,
        cacheWrite5mTokens: 0,
        cacheWrite1hTokens: 0,
      },
    );
  }
});

test("display conversion never mutates the source usage log", () => {
  const log = Object.freeze({
    channel: "codex",
    input_tokens: 46_601,
    output_tokens: 65,
    cached_tokens: 46_334,
    cache_write_5m_tokens: 173,
    cache_write_1h_tokens: 0,
  });
  const original = { ...log };
  const first = getUsageTokenBreakdown(log);
  const second = getUsageTokenBreakdown(log);
  assert.deepEqual(log, original);
  assert.deepEqual(first, second);
  assert.notEqual(first, second);
});
