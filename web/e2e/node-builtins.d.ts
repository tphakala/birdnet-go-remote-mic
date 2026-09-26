// Minimal ambient declarations for the Node built-ins web/e2e uses, in the same
// spirit as web/test/node-builtins.d.ts: the sweep stays dependency-free (no
// @types/node) and still type-checks under web/e2e/tsconfig.json. Only the
// surface these files call is declared.

interface ImportMeta {
  // True when this module is the entry point Node was started with.
  main: boolean;
}

declare const process: {
  argv: string[];
  env: Record<string, string | undefined>;
  exitCode: number | undefined;
  exit(code?: number): never;
  stderr: { write(s: string): boolean };
};

declare const Buffer: {
  concat(chunks: Uint8Array[]): { toString(encoding: "utf8"): string };
};

declare module "node:fs" {
  export function existsSync(path: string): boolean;
}

declare module "node:fs/promises" {
  export function readFile(path: string): Promise<Uint8Array>;
  export function stat(path: string): Promise<{ isFile(): boolean }>;
}

declare module "node:path" {
  export const delimiter: string;
  export const sep: string;
  export function dirname(p: string): string;
  export function extname(p: string): string;
  export function join(...parts: string[]): string;
  export function normalize(p: string): string;
  export function resolve(...parts: string[]): string;
}

declare module "node:url" {
  export function pathToFileURL(path: string): URL;
}

declare module "node:http" {
  export interface IncomingMessage extends AsyncIterable<unknown> {
    url?: string;
    method?: string;
    on(event: "close", listener: () => void): this;
  }
  export interface ServerResponse {
    headersSent: boolean;
    writeHead(status: number, headers?: Record<string, string>): this;
    write(chunk: string): boolean;
    end(chunk?: string | Uint8Array): this;
  }
  export interface Server {
    listen(port: number, host: string, cb: () => void): this;
    once(event: "error", listener: (err: Error) => void): this;
    close(cb: () => void): this;
    address(): { port: number } | string | null;
  }
  export function createServer(handler: (req: IncomingMessage, res: ServerResponse) => void): Server;
}
