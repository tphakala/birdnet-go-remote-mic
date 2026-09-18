// Unit tests for the pure Management Certificate card logic. Run with Node's
// built-in test runner over the compiled output (see the web:test task): no
// browser, no DOM, no dependencies.

import test from "node:test";
import assert from "node:assert/strict";

import { describeManaged, parseExtraSans } from "../src/lib/certificate-core.js";

test("parseExtraSans splits on commas and whitespace, trims, and drops empties", () => {
  const r = parseExtraSans("  mic.lan,  192.168.1.20 \n\tsensor.local ,, ");
  assert.equal(r.error, null);
  assert.deepEqual(r.sans, ["mic.lan", "192.168.1.20", "sensor.local"]);
});

test("parseExtraSans returns the empty-list shape on blank input", () => {
  assert.deepEqual(parseExtraSans(""), { sans: [], error: null });
  assert.deepEqual(parseExtraSans("   \n "), { sans: [], error: null });
});

test("parseExtraSans lower-cases DNS names and de-duplicates case-insensitively", () => {
  const r = parseExtraSans("Mic.LAN mic.lan MIC.lan other.lan");
  assert.equal(r.error, null);
  assert.deepEqual(r.sans, ["mic.lan", "other.lan"]);
});

test("parseExtraSans keeps IP addresses as typed and de-duplicates them", () => {
  const r = parseExtraSans("10.0.0.5, 10.0.0.5, FE80::1, fe80::1");
  assert.equal(r.error, null);
  assert.deepEqual(r.sans, ["10.0.0.5", "FE80::1"]);
});

test("parseExtraSans accepts IPv4, IPv6, hostnames and .local names", () => {
  const ok = ["192.168.0.1", "::1", "2001:db8::8a2e:370:7334", "::ffff:192.0.2.128", "mic", "birdmic.local", "a-b.example.com"];
  for (const s of ok) {
    const r = parseExtraSans(s);
    assert.equal(r.error, null, `${s} should be accepted`);
    assert.equal(r.sans.length, 1, `${s} yields one entry`);
  }
});

test("parseExtraSans rejects a wildcard, an empty label, a bad IP and an overlong name", () => {
  const long = Array(4).fill("a".repeat(63)).join(".");
  assert.equal(long.length, 255, "test fixture exceeds the 253-character limit");
  const bad: Array<[string, string]> = [
    ["*.example.com", "*.example.com"],
    ["a..b", "a..b"],
    ["-bad.lan", "-bad.lan"],
    ["mic.lan.", "mic.lan."],
    ["1:::2", "1:::2"],
    ["1:2:3:4:5:6:7:8:9", "1:2:3:4:5:6:7:8:9"],
    ["192.0.2.1::", "192.0.2.1::"],
    [long, long],
  ];
  for (const [input, token] of bad) {
    const r = parseExtraSans(`good.lan ${input}`);
    assert.deepEqual(r.sans, [], `${input}: list is empty on error`);
    assert.equal(r.error, `${token} is not a valid DNS name or IP address`);
  }
});

test("parseExtraSans keeps an IPv4-mapped IPv6 address whose dotted quad ends the input", () => {
  const r = parseExtraSans("::ffff:192.0.2.1");
  assert.equal(r.error, null);
  assert.deepEqual(r.sans, ["::ffff:192.0.2.1"]);
});

test("parseExtraSans names the first invalid token and stops there", () => {
  const r = parseExtraSans("ok.lan, bad_name, *.also.bad");
  assert.deepEqual(r.sans, []);
  assert.equal(r.error, "bad_name is not a valid DNS name or IP address");
});

test("describeManaged distinguishes appliance-managed from operator-installed", () => {
  assert.equal(describeManaged(true), "Appliance-managed (self-signed)");
  assert.equal(describeManaged(false), "Operator-installed (custom)");
});
