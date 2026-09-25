// I/O-free logic for the Management Certificate card. No DOM, no fetch: the
// extra-SAN parser and the managed-state label live here so they are unit-tested
// with node:test and no browser. The view (views/system.ts) owns the DOM and
// calls into here.

// ParsedSans is parseExtraSans's result: the cleaned list, or the first
// offending token's message with an empty list.
export interface ParsedSans {
  sans: string[];
  error: string | null;
}

// MAX_DNS_NAME_LENGTH is the RFC 1035 limit on a full domain name.
const MAX_DNS_NAME_LENGTH = 253;

// DNS_LABEL is one hostname label: letters, digits and hyphens, no leading or
// trailing hyphen, 1 to 63 characters. A wildcard "*" fails it by construction.
const DNS_LABEL = /^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$/;

function isIPv4(s: string): boolean {
  const parts = s.split(".");
  return parts.length === 4 && parts.every((p) => /^\d{1,3}$/.test(p) && Number(p) <= 255);
}

// isIPv6 accepts the plain textual form: up to eight hex groups separated by
// colons, at most one "::" compression, optionally ending in a dotted IPv4
// quad (which counts as two groups). A zone suffix (%eth0) is not accepted.
function isIPv6(s: string): boolean {
  if (!s.includes(":") || !/^[0-9A-Fa-f:.]+$/.test(s)) return false;
  const halves = s.split("::");
  if (halves.length > 2) return false;
  const groups: string[] = [];
  for (const half of halves) {
    if (half !== "") groups.push(...half.split(":"));
  }
  let count = 0;
  for (let i = 0; i < groups.length; i++) {
    const g = groups[i];
    if (g.includes(".")) {
      // The dotted quad must be the last group AND end the input: "::" splitting
      // drops an empty trailing half, so "192.0.2.1::" would otherwise pass.
      if (i !== groups.length - 1 || !s.endsWith(g) || !isIPv4(g)) return false;
      count += 2;
    } else {
      if (!/^[0-9A-Fa-f]{1,4}$/.test(g)) return false;
      count += 1;
    }
  }
  return halves.length === 2 ? count < 8 : count === 8;
}

function isDnsName(s: string): boolean {
  if (s.length === 0 || s.length > MAX_DNS_NAME_LENGTH) return false;
  return s.split(".").every((label) => DNS_LABEL.test(label));
}

// parseExtraSans turns the free-text extra-SANs box into the list the
// regenerate request sends. Tokens are split on commas and whitespace, trimmed,
// and empties dropped; DNS names are lower-cased while IP addresses are kept as
// typed; duplicates are removed case-insensitively. Each token must be an IPv4
// or IPv6 address or a plain DNS hostname (no wildcard). The first invalid
// token stops the parse and is named in the error. This is a client-side
// pre-check so an obvious typo is caught before the round trip; the server
// stays authoritative and may still reject a token this accepts.
export function parseExtraSans(raw: string): ParsedSans {
  const sans: string[] = [];
  const seen = new Set<string>();
  for (const token of raw.split(/[\s,]+/)) {
    if (token === "") continue;
    const key = token.toLowerCase();
    if (seen.has(key)) continue;
    if (isIPv4(token) || isIPv6(token)) {
      seen.add(key);
      sans.push(token);
      continue;
    }
    if (!isDnsName(key)) {
      return { sans: [], error: `${token} is not a valid DNS name or IP address` };
    }
    seen.add(key);
    sans.push(key);
  }
  return { sans, error: null };
}

// CERT_TOO_LARGE_FALLBACK is the 413 reason when the response carries no usable
// detail (an older appliance, or a proxy's own error page).
export const CERT_TOO_LARGE_FALLBACK = "the request is larger than the appliance accepts";

// MAX_ECHOED_DETAIL_LEN is the longest problem detail certTooLargeReason
// echoes into the toast. The appliance's own 413 detail is one short sentence;
// anything longer is treated as a proxy's error text and replaced.
export const MAX_ECHOED_DETAIL_LEN = 200;

// certTooLargeReason is the reason clause of the certificate-install 413 toast:
// the problem detail the appliance sent (which names its body limit), or a
// generic clause when there is none. A detail that looks like markup or runs
// long is a proxy's error page rather than an appliance problem, and is not
// echoed. A trailing period is dropped because the toast continues the
// sentence.
export function certTooLargeReason(detail: string | undefined): string {
  const d = (detail ?? "").trim().replace(/\.+$/, "");
  if (d === "" || d.length > MAX_ECHOED_DETAIL_LEN || /[<>\n]/.test(d)) return CERT_TOO_LARGE_FALLBACK;
  return d;
}

// describeManaged renders the CertificateInfo.managed flag as the operator-facing
// "Management" row of the certificate panel.
export function describeManaged(managed: boolean): string {
  return managed ? "Appliance-managed (self-signed)" : "Operator-installed (custom)";
}
