// Minimal ambient declarations for the Node built-in test modules, covering
// only the surface the notification-core tests use. This keeps the test build
// dependency-free (no @types/node): the types live under web/test, so they are
// compiled only by tsconfig.test.json, never by the dist build or the linter,
// both of which are scoped to web/src.

declare module "node:test" {
  type TestFn = () => void | Promise<void>;
  function test(name: string, fn: TestFn): void;
  export default test;
  export { test };
}

declare module "node:assert/strict" {
  interface StrictAssert {
    (value: unknown, message?: string): asserts value;
    equal(actual: unknown, expected: unknown, message?: string): void;
    deepEqual(actual: unknown, expected: unknown, message?: string): void;
    ok(value: unknown, message?: string): asserts value;
  }
  const assert: StrictAssert;
  export default assert;
}
