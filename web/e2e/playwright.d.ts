// The slice of the Playwright API the sweep uses. Playwright is not a
// dependency of this repository (the sweep runs under `npx -p playwright@...`),
// so tsc cannot see its own types; this keeps web/e2e type-checked without a
// node_modules. Add members here as the sweep starts using them.

export interface CDPSession {
  send(method: string, params?: object): Promise<unknown>;
}

export interface Page {
  goto(url: string): Promise<unknown>;
  click(selector: string): Promise<void>;
  waitForSelector(selector: string, options?: { state?: "attached" | "visible"; timeout?: number }): Promise<unknown>;
  waitForTimeout(ms: number): Promise<void>;
  waitForFunction<A>(fn: (arg: A) => unknown, arg: A, options?: { timeout?: number }): Promise<unknown>;
  focus(selector: string): Promise<void>;
  reload(): Promise<unknown>;
  evaluate<R>(fn: () => R | Promise<R>): Promise<R>;
  evaluate<R, A>(fn: (arg: A) => R | Promise<R>, arg: A): Promise<R>;
  addInitScript<A>(fn: (arg: A) => void, arg?: A): Promise<void>;
  on(event: "pageerror", listener: (err: Error) => void): void;
  keyboard: { press(key: string): Promise<void> };
}

export interface BrowserContext {
  newPage(): Promise<Page>;
  newCDPSession(page: Page): Promise<CDPSession>;
  addInitScript<A>(fn: (arg: A) => void, arg?: A): Promise<void>;
  close(): Promise<void>;
}

export interface Browser {
  newContext(options?: {
    viewport?: { width: number; height: number };
    colorScheme?: "light" | "dark";
    reducedMotion?: "reduce" | "no-preference";
  }): Promise<BrowserContext>;
  close(): Promise<void>;
}

export interface Playwright {
  chromium: { launch(options?: { headless?: boolean }): Promise<Browser> };
}
