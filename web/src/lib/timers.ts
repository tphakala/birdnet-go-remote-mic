// Timers is the timer API the stores, the SSE client and the meters' frame
// loop schedule with: the globals in the app, a fake a test fires by hand. A
// leaf module, so a DOM-free core can depend on it without the store.
export interface Timers {
  setTimeout(fn: () => void, ms: number): ReturnType<typeof setTimeout>;
  clearTimeout(handle: ReturnType<typeof setTimeout>): void;
  setInterval(fn: () => void, ms: number): ReturnType<typeof setInterval>;
  clearInterval(handle: ReturnType<typeof setInterval>): void;
}

// OneShotTimers is the part of Timers for code that only sets timeouts.
export type OneShotTimers = Pick<Timers, "setTimeout" | "clearTimeout">;
