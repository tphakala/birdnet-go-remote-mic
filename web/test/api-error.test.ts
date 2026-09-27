// Unit tests for lib/api.ts's error handling: how request() maps a failed or
// unreadable response (ApiError, UnreadableResponseError), and the helpers
// that turn one into text (apiErrorMessage, firstProblem, problemFor).
// Run with node:test (see web:test).

import test from "node:test";
import assert from "node:assert/strict";

import { ApiClient, ApiError, apiErrorMessage, firstProblem, isRefusal, problemFor, problemReason, unconfirmedText, UnreadableResponseError } from "../src/lib/api.ts";

test("firstProblem returns the first validation problem with its field and reason", () => {
  const err = new ApiError(422, "Invalid configuration", { errors: [
    { field: "network.hostname", reason: "must be a valid hostname" },
    { field: "network.port", reason: "out of range" },
  ], problem: true });
  assert.deepEqual(firstProblem(err), { field: "network.hostname", reason: "must be a valid hostname" });
});

test("firstProblem falls back to the problem title when the item has no reason", () => {
  const err = new ApiError(422, "Invalid configuration", { errors: [{ field: "auth.token" }], problem: true });
  assert.deepEqual(firstProblem(err), { field: "auth.token", reason: "Invalid configuration" });
});

test("firstProblem is null for a failure with no validation problem", () => {
  assert.equal(firstProblem(new ApiError(500, "Internal error")), null);
  assert.equal(firstProblem(new ApiError(422, "Invalid", { errors: [], problem: true })), null);
  assert.equal(firstProblem(new Error("offline")), null);
  assert.equal(firstProblem("offline"), null);
});

test("apiErrorMessage shows a problem's detail, which says what went wrong", () => {
  assert.equal(apiErrorMessage(new ApiError(500, "internal error", { detail: "persist config: disk full", problem: true })), "persist config: disk full");
  // A problem with no detail falls back to its title.
  assert.equal(apiErrorMessage(new ApiError(500, "internal error", { detail: "", problem: true })), "internal error");
});

test("apiErrorMessage never shows a body that was not a problem", () => {
  // An ApiError that is not a problem shows its status text, whatever its
  // detail holds (request() keeps none, see the client tests below).
  assert.equal(apiErrorMessage(new ApiError(502, "Bad Gateway", { detail: "<html><body>502</body></html>" })), "Bad Gateway");
  assert.equal(apiErrorMessage(new ApiError(502, "", { detail: "<html></html>" })), "HTTP 502");
});

test("apiErrorMessage shows an error message, or the value", () => {
  assert.equal(apiErrorMessage(new Error("offline")), "offline");
  assert.equal(apiErrorMessage("offline"), "offline");
});

// failWith makes the client's next request answer with res.
async function failWith(res: Response): Promise<unknown> {
  const g = globalThis as unknown as { fetch: typeof fetch };
  const saved = g.fetch;
  g.fetch = () => Promise.resolve(res);
  try {
    await new ApiClient().getHealth();
    return null;
  } catch (err) {
    return err;
  } finally {
    g.fetch = saved;
  }
}

test("a problem+json error body is a problem whose detail is shown", async () => {
  const err = await failWith(
    new Response(JSON.stringify({ status: 400, title: "bad request", detail: "name must not be empty" }), {
      status: 400,
      headers: { "Content-Type": "application/problem+json" },
    }),
  );
  assert.ok(err instanceof ApiError);
  assert.equal(err.problem, true);
  assert.equal(apiErrorMessage(err), "name must not be empty");
});

test("any other error body is not shown, and an empty status text falls back to the code", async () => {
  // HTTP/2 carries no status text.
  const err = await failWith(new Response("<html>bad gateway</html>", { status: 502, statusText: "", headers: { "Content-Type": "text/html" } }));
  assert.ok(err instanceof ApiError);
  assert.equal(err.problem, false);
  assert.equal(err.title, "HTTP 502");
  assert.equal(apiErrorMessage(err), "HTTP 502");
  assert.equal(err.message, "HTTP 502", "the error keeps no copy of the body");
});

test("an error body labelled JSON that does not parse keeps only its status", async () => {
  // A proxy's HTML page sent as JSON: the parser's message would quote it.
  for (const type of ["application/json", "application/problem+json"]) {
    const err = await failWith(new Response("<html>secret</html>", { status: 502, statusText: "Bad Gateway", headers: { "Content-Type": type } }));
    assert.ok(err instanceof ApiError, `${type}: a parse failure must still be an ApiError`);
    assert.equal(apiErrorMessage(err), "Bad Gateway", type);
    assert.equal(err.message.includes("secret"), false, type);
  }
  const unauthorized = await failWith(new Response("<html></html>", { status: 401, headers: { "Content-Type": "application/problem+json" } }));
  assert.equal(apiErrorMessage(unauthorized), "the access token was not accepted", "a 401 reads the same whatever its body");
});

test("a problem body that is not an object keeps only its status", async () => {
  const err = await failWith(new Response("null", { status: 500, statusText: "", headers: { "Content-Type": "application/problem+json" } }));
  assert.ok(err instanceof ApiError);
  assert.equal(apiErrorMessage(err), "HTTP 500");
});

test("a success body labelled JSON that does not parse is an unknown outcome that quotes nothing", async () => {
  const err = await failWith(new Response("<html>secret</html>", { status: 200, headers: { "Content-Type": "application/json" } }));
  // Not an ApiError: the request was accepted, so every caller that tells a
  // refusal from an unknown outcome (isRefusal) takes it as unknown.
  assert.ok(err instanceof UnreadableResponseError);
  assert.equal(err instanceof ApiError, false);
  assert.equal(apiErrorMessage(err), "the response could not be read");
});

test("an ApiError's message never holds a detail that is not a problem's", () => {
  assert.equal(new ApiError(502, "Bad Gateway", { detail: "<html>secret</html>" }).message, "Bad Gateway");
  assert.equal(new ApiError(500, "internal error", { detail: "disk full", problem: true }).message, "disk full");
});

test("the failure's status is the response's, whatever the problem body says", async () => {
  const err = await failWith(
    new Response(JSON.stringify({ status: 202, title: "accepted?" }), { status: 409, headers: { "Content-Type": "application/problem+json" } }),
  );
  assert.ok(err instanceof ApiError);
  assert.equal(err.status, 409);
});

test("a plain JSON error body is not a problem, so its fields are not shown", async () => {
  // A proxy answering with its own JSON error.
  const err = await failWith(
    new Response(JSON.stringify({ title: "proxy says no", detail: "upstream timed out", errors: [{ field: "x", reason: "y" }] }), {
      status: 504,
      statusText: "Gateway Timeout",
      headers: { "Content-Type": "application/json" },
    }),
  );
  assert.ok(err instanceof ApiError);
  assert.equal(err.problem, false);
  assert.equal(err.problemDetail, undefined);
  assert.equal(apiErrorMessage(err), "Gateway Timeout");
  assert.equal(firstProblem(err), null, "a non-problem body carries no validation problems");
});

test("a problem whose detail is not a string shows its title", async () => {
  const err = await failWith(
    new Response(JSON.stringify({ status: 500, title: "internal error", detail: { nested: true } }), {
      status: 500,
      headers: { "Content-Type": "application/problem+json" },
    }),
  );
  assert.ok(err instanceof ApiError);
  assert.equal(err.problemDetail, undefined);
  assert.equal(apiErrorMessage(err), "internal error");
});

test("a 401 says the token was not accepted instead of the problem detail", () => {
  const err = new ApiError(401, "unauthorized", { detail: "a valid access token is required (Authorization: Bearer <token>)", problem: true });
  assert.equal(apiErrorMessage(err), "the access token was not accepted");
});

test("problemFor finds the first matching problem with the shared fallback", () => {
  const err = new ApiError(422, "invalid request", { errors: [
    { field: "certPem", reason: "not a certificate" },
    { field: "extraSans[1]" },
  ], problem: true });
  assert.deepEqual(problemFor(err, (e) => e.field?.startsWith("extraSans") ?? false), { field: "extraSans[1]", reason: "invalid request" });
  assert.equal(problemFor(err, (e) => e.field === "keyPem"), null);
  assert.equal(problemReason(err, { field: "keyPem" }), "invalid request");
  assert.equal(problemReason(err, { reason: "too long" }), "too long");
});

test("a malformed entry in a problem's errors list is dropped", async () => {
  const err = await failWith(
    new Response(JSON.stringify({ status: 422, title: "invalid request", errors: [null, "x", { field: 5 }, { field: "keyPem", reason: 7 }, { field: "certPem", reason: "bad" }] }), {
      status: 422,
      headers: { "Content-Type": "application/problem+json" },
    }),
  );
  assert.ok(err instanceof ApiError);
  assert.deepEqual(err.errors, [{ field: "certPem", reason: "bad" }]);
  assert.deepEqual(problemFor(err, (e) => e.field === "certPem"), { field: "certPem", reason: "bad" });
});

test("only an ApiError is a refusal; an unknown outcome is said without error text", () => {
  assert.equal(isRefusal(new ApiError(409, "conflict", { problem: true })), true);
  assert.equal(isRefusal(new ApiError(504, "Gateway Timeout")), false, "a proxy's error says nothing about the appliance");
  assert.equal(isRefusal(new UnreadableResponseError(200)), false);
  assert.equal(isRefusal(new TypeError("Failed to fetch")), false);
  assert.equal(unconfirmedText("the save", "refreshing"), "Could not confirm the save; refreshing.");
});
