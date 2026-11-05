import assert from "node:assert/strict";
import test from "node:test";

import {
  mergeConfirmedSettingsValues,
  mergeConfirmedSettingsWithVersions,
  resolveAutoSaveRollbackValues,
} from "./settingsAutoSave.ts";

test("auto-save rollback restores the last server-confirmed value instead of the typed draft", () => {
  const confirmed = {
    output_guard_max_channel_bytes: 2 * 1024 * 1024,
    site_name: "CodexProxy",
  };
  // The draft value typed before the blur commit is already synced into settingsFormRef, but the server has never confirmed it.
  const typedDraft = {
    output_guard_max_channel_bytes: 3 * 1024 * 1024,
    site_name: "CodexProxy",
  };

  assert.deepEqual(
    resolveAutoSaveRollbackValues(confirmed, typedDraft, [
      "output_guard_max_channel_bytes",
    ]),
    { output_guard_max_channel_bytes: 2 * 1024 * 1024 },
  );
});

test("auto-save rollback only touches the failing request's fields", () => {
  const confirmed = {
    output_guard_max_channel_bytes: 2 * 1024 * 1024,
    site_name: "CodexProxy",
    max_concurrency: 16,
  };
  const submitted = {
    output_guard_max_channel_bytes: 3 * 1024 * 1024,
    site_name: "Renamed",
    max_concurrency: 16,
  };

  assert.deepEqual(
    resolveAutoSaveRollbackValues(confirmed, submitted, [
      "output_guard_max_channel_bytes",
    ]),
    { output_guard_max_channel_bytes: 2 * 1024 * 1024 },
    "unrelated form edits must not leak into the rollback patch",
  );
});

test("auto-save rollback falls back to the submitted form before the first confirmed snapshot", () => {
  const submitted = {
    output_guard_max_channel_bytes: 3 * 1024 * 1024,
  };

  assert.deepEqual(
    resolveAutoSaveRollbackValues(null, submitted, [
      "output_guard_max_channel_bytes",
    ]),
    { output_guard_max_channel_bytes: 3 * 1024 * 1024 },
  );
});

test("confirmed snapshot takes over only the acknowledged request's fields", () => {
  const confirmed = {
    output_guard_max_channel_bytes: 2 * 1024 * 1024,
    site_name: "CodexProxy",
  };

  const merged = mergeConfirmedSettingsValues(confirmed, {
    output_guard_max_channel_bytes: 4 * 1024 * 1024,
  });

  assert.deepEqual(merged, {
    output_guard_max_channel_bytes: 4 * 1024 * 1024,
    site_name: "CodexProxy",
  });
  assert.deepEqual(
    confirmed,
    {
      output_guard_max_channel_bytes: 2 * 1024 * 1024,
      site_name: "CodexProxy",
    },
    "the previous snapshot must not be mutated",
  );
  assert.equal(
    mergeConfirmedSettingsValues(null, {
      output_guard_max_channel_bytes: 4 * 1024 * 1024,
    }),
    null,
    "no snapshot is created before the initial load confirms one",
  );
});

test("confirmed settings merge with field versions ignores stale out-of-order responses", () => {
  const initial = {
    output_guard_max_channel_bytes: 2 * 1024 * 1024,
    output_guard_max_request_bytes: 4 * 1024 * 1024,
    site_name: "InitialSite",
  };
  let confirmedState = {
    confirmed: initial,
    confirmedVersions: {
      output_guard_max_channel_bytes: 1,
      output_guard_max_request_bytes: 1,
      site_name: 1,
    },
  };

  // Request 2 arrives first and is confirmed (for example channel_bytes becomes 8MB)
  confirmedState = mergeConfirmedSettingsWithVersions(
    confirmedState.confirmed,
    confirmedState.confirmedVersions,
    { output_guard_max_channel_bytes: 8 * 1024 * 1024 },
    { output_guard_max_channel_bytes: 3 }, // version 3
  );

  assert.equal(
    confirmedState.confirmed?.output_guard_max_channel_bytes,
    8 * 1024 * 1024,
  );
  assert.equal(
    confirmedState.confirmedVersions.output_guard_max_channel_bytes,
    3,
  );

  // Request 1 (the old response, for example version 2, with the old value 4MB) arrives later
  const afterStaleResponse = mergeConfirmedSettingsWithVersions(
    confirmedState.confirmed,
    confirmedState.confirmedVersions,
    { output_guard_max_channel_bytes: 4 * 1024 * 1024 },
    { output_guard_max_channel_bytes: 2 }, // version 2 < 3
  );

  // It must stay at the new value 8MB and must not be overwritten by the old response's 4MB
  assert.equal(
    afterStaleResponse.confirmed?.output_guard_max_channel_bytes,
    8 * 1024 * 1024,
    "stale response must not overwrite newer confirmed baseline",
  );
  assert.equal(
    afterStaleResponse.confirmedVersions.output_guard_max_channel_bytes,
    3,
  );

  // Another field (such as request_bytes, version 2 > 1) merges normally
  const multiFieldResponse = mergeConfirmedSettingsWithVersions(
    afterStaleResponse.confirmed,
    afterStaleResponse.confirmedVersions,
    {
      output_guard_max_channel_bytes: 4 * 1024 * 1024, // old version 2, still ignored
      output_guard_max_request_bytes: 16 * 1024 * 1024, // new version 2 > 1, accepted
    },
    {
      output_guard_max_channel_bytes: 2,
      output_guard_max_request_bytes: 2,
    },
  );

  assert.equal(
    multiFieldResponse.confirmed?.output_guard_max_channel_bytes,
    8 * 1024 * 1024,
  );
  assert.equal(
    multiFieldResponse.confirmed?.output_guard_max_request_bytes,
    16 * 1024 * 1024,
  );
  assert.equal(
    multiFieldResponse.confirmedVersions.output_guard_max_request_bytes,
    2,
  );
});

test("confirmed settings merge with field versions handles null initial confirmed snapshot safely", () => {
  const result = mergeConfirmedSettingsWithVersions(
    null,
    {},
    { output_guard_max_channel_bytes: 2 * 1024 * 1024 },
    { output_guard_max_channel_bytes: 1 },
  );

  assert.equal(result.confirmed, null);
  assert.equal(result.confirmedVersions.output_guard_max_channel_bytes, 1);
});

test("confirmed settings merge uses output_guard_config_generation to resolve out-of-order responses", () => {
  const initial = {
    output_guard_max_channel_bytes: 2 * 1024 * 1024,
    output_guard_config_generation: 1,
  };
  let confirmedState = {
    confirmed: initial,
    confirmedVersions: {
      output_guard_max_channel_bytes: 1,
    },
  };

  // Scenario: two channel-bytes saves overlap.
  // Request A (sent first, client version 2) is written last; after the server commits it, it produces version 2 (4MiB, generation 3)
  // Request B (sent later, client version 3) is written first; after the server commits it, it produces version 1 (8MiB, generation 2)
  // The client receives request B's response first (8MiB, generation 2)
  confirmedState = mergeConfirmedSettingsWithVersions(
    confirmedState.confirmed,
    confirmedState.confirmedVersions,
    {
      output_guard_max_channel_bytes: 8 * 1024 * 1024,
      output_guard_config_generation: 2,
    },
    { output_guard_max_channel_bytes: 3 }, // client requestedVersion 3
  );

  assert.equal(confirmedState.confirmed?.output_guard_max_channel_bytes, 8 * 1024 * 1024);
  assert.equal(confirmedState.confirmed?.output_guard_config_generation, 2);

  // The client receives request A's response later (4MiB, generation 3, client requestedVersion 2 < 3)
  // Although client version 2 < 3, the server's output_guard_config_generation 3 > 2 shows that request A is the value the server finally committed
  confirmedState = mergeConfirmedSettingsWithVersions(
    confirmedState.confirmed,
    confirmedState.confirmedVersions,
    {
      output_guard_max_channel_bytes: 4 * 1024 * 1024,
      output_guard_config_generation: 3,
    },
    { output_guard_max_channel_bytes: 2 }, // client requestedVersion 2
  );

  // The client baseline must adopt the server's final committed value 4MiB, not keep 8MiB by discarding it based on request order
  assert.equal(
    confirmedState.confirmed?.output_guard_max_channel_bytes,
    4 * 1024 * 1024,
    "client baseline must equal server final committed value when server generation is newer",
  );
  assert.equal(confirmedState.confirmed?.output_guard_config_generation, 3);

  // Reverse check: if another response with an old generation arrives (for example generation 1), it must be discarded
  const afterOlderGen = mergeConfirmedSettingsWithVersions(
    confirmedState.confirmed,
    confirmedState.confirmedVersions,
    {
      output_guard_max_channel_bytes: 2 * 1024 * 1024,
      output_guard_config_generation: 1,
    },
    { output_guard_max_channel_bytes: 4 }, // not accepted even if the client version is larger
  );

  assert.equal(afterOlderGen.confirmed?.output_guard_max_channel_bytes, 4 * 1024 * 1024);
  assert.equal(afterOlderGen.confirmed?.output_guard_config_generation, 3);
});


