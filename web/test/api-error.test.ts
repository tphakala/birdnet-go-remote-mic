// Unit tests for the error helpers in lib/ui.ts that turn a failed request
// into text: apiErrorMessage for a toast, firstProblem for a field error.
// Run with node:test (see web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { ApiError } from "../src/lib/api.ts";
import { apiErrorMessage, firstProblem } from "../src/lib/ui.ts";

test("firstProblem returns the first validation problem with its field and reason", () => {
  const err = new ApiError(422, "Invalid configuration", undefined, [
    { field: "network.hostname", reason: "must be a valid hostname" },
    { field: "network.port", reason: "out of range" },
  ]);
  assert.deepEqual(firstProblem(err), { field: "network.hostname", reason: "must be a valid hostname" });
});

test("firstProblem falls back to the problem title when the item has no reason", () => {
  const err = new ApiError(422, "Invalid configuration", undefined, [{ field: "auth.token" }]);
  assert.deepEqual(firstProblem(err), { field: "auth.token", reason: "Invalid configuration" });
});

test("firstProblem is null for a failure with no validation problem", () => {
  assert.equal(firstProblem(new ApiError(500, "Internal error")), null);
  assert.equal(firstProblem(new ApiError(422, "Invalid", undefined, [])), null);
  assert.equal(firstProblem(new Error("offline")), null);
  assert.equal(firstProblem("offline"), null);
});

test("apiErrorMessage shows a problem title, an error message, or the value", () => {
  assert.equal(apiErrorMessage(new ApiError(500, "Internal error", "disk full")), "Internal error");
  assert.equal(apiErrorMessage(new Error("offline")), "offline");
  assert.equal(apiErrorMessage("offline"), "offline");
});
