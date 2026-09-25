// Shared test fixtures. This file holds no tests of its own.

import type { Notification } from "../src/lib/types.js";

// notif builds a well-formed notification with every field defaulted, so a test
// sets only what it is about.
export function notif(over: Partial<Notification> & { id: number }): Notification {
  return {
    id: over.id,
    bootId: over.bootId ?? "boot-a",
    time: over.time ?? "2026-09-12T14:00:00Z",
    uptimeMs: over.uptimeMs ?? 0,
    severity: over.severity ?? "info",
    category: over.category ?? "system",
    kind: over.kind ?? "event",
    key: over.key,
    source: over.source,
    title: over.title ?? "Title",
    message: over.message ?? "Message",
  };
}
