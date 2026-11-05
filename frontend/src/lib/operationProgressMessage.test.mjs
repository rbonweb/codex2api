import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { operationProgressMessage } from "./operationProgressMessage.ts";

describe("operationProgressMessage", () => {
  it("shows the current account while a clean is still running", () => {
    assert.equal(
      operationProgressMessage({
        type: "progress",
        action: "clean",
        message: "Account cleared",
        account_email: "a@example.com",
      }),
      "a@example.com",
    );
  });

  it("keeps the summary only after the clean completes", () => {
    assert.equal(
      operationProgressMessage(
        {
          type: "complete",
          action: "clean",
          message: "Cleaned up 532 accounts",
        },
        "a@example.com",
      ),
      "Cleaned up 532 accounts",
    );
  });

  it("prefers a clean failure over the current account", () => {
    assert.equal(
      operationProgressMessage({
        type: "progress",
        action: "clean",
        error: "Account does not exist",
        account_email: "a@example.com",
      }),
      "Account does not exist",
    );
  });
});
