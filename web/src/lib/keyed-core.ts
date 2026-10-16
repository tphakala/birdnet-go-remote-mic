// The pure decision behind KeyedList (lib/keyed-list.ts): given the keys now
// shown, the keys held out of the DOM, and the keys wanted next, which to
// create, rebuild, drop or hold. No DOM here, so node:test covers it (see
// web/test/keyed-core.test.ts).

export interface KeyedPlan<K> {
  // The wanted keys in order; for a repeated key the first occurrence wins.
  order: K[];
  // Wanted, in order, and neither shown nor held.
  create: K[];
  // Wanted, in order, shown or held, and stale.
  rebuild: K[];
  // Shown (in shown order) and then held keys that are not wanted and not
  // retained.
  drop: K[];
  // Shown keys that are not wanted but retained: they leave the DOM and stay
  // held. A held key that stays retained is in no list.
  hold: K[];
}

// keyedPlan decides one sync. is.stale is asked only for wanted keys that are
// shown or held, and is.retain only for shown or held keys that are not
// wanted. A held key that is wanted and not stale is revived: it appears in
// order and in no other list.
export function keyedPlan<K>(
  shown: Iterable<K>,
  held: Iterable<K>,
  next: Iterable<K>,
  is: { stale(key: K): boolean; retain(key: K): boolean },
): KeyedPlan<K> {
  const shownKeys = new Set(shown);
  const heldKeys = new Set(held);
  const order = [...new Set(next)];
  const wanted = new Set(order);
  const plan: KeyedPlan<K> = { order, create: [], rebuild: [], drop: [], hold: [] };
  for (const k of order) {
    if (!shownKeys.has(k) && !heldKeys.has(k)) plan.create.push(k);
    else if (is.stale(k)) plan.rebuild.push(k);
  }
  for (const k of shownKeys) {
    if (wanted.has(k)) continue;
    (is.retain(k) ? plan.hold : plan.drop).push(k);
  }
  for (const k of heldKeys) {
    if (!wanted.has(k) && !is.retain(k)) plan.drop.push(k);
  }
  return plan;
}
