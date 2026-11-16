// I/O-free logic for the Access Token card. No DOM: the card
// (views/system/access-card.ts) owns the elements and calls into here, so the
// keyboard rule is unit-tested with node:test and no browser.

// TokenKey is the part of a keydown event tokenKeydown reads.
export interface TokenKey {
  key: string;
  isComposing: boolean;
  keyCode: number;
  preventDefault(): void;
}

// tokenKeydown handles a keydown in the token field. The card is not a <form>,
// so Enter would do nothing; it saves instead, matching the muscle memory of a
// single-field form. An Enter that confirms an IME composition is not a save,
// so it is left to the IME. Safari reports isComposing false on that keydown;
// keyCode 229 covers it.
export function tokenKeydown(e: TokenKey, save: () => void): void {
  if (e.key !== "Enter" || e.isComposing || e.keyCode === 229) return;
  e.preventDefault();
  save();
}
