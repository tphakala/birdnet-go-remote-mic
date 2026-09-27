// withDeadline runs a request under an abort signal that fires after ms, so a
// stalled request (headers or body) ends in an error rather than hanging. A
// timer and an AbortController rather than AbortSignal.timeout, which is
// missing before Safari 16.
export async function withDeadline<T>(ms: number, run: (signal: AbortSignal) => Promise<T>): Promise<T> {
  const abort = new AbortController();
  const timer = setTimeout(() => abort.abort(), ms);
  try {
    return await run(abort.signal);
  } finally {
    clearTimeout(timer);
  }
}
