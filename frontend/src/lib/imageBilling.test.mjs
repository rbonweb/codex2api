import assert from "node:assert/strict";
import test from "node:test";

import { supportsImageBilling } from "./imageBilling.ts";

test("supportsImageBilling accepts gpt-image models only", () => {
  assert.equal(supportsImageBilling("gpt-image-2"), true);
  assert.equal(supportsImageBilling(" GPT-Image-1.5 "), true);
  assert.equal(supportsImageBilling("gpt-5.5"), false);
});
