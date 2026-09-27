import type { OneShotTimers } from "./timers.ts";

export type SSEEventHandler = (eventName: string, data: unknown) => void;

// The wire event types the UI consumes (the appliance's names: eventName in
// internal/levels/levels.go, notificationEvent in internal/notify/notify.go).
// Every consumer compares against these, so a filter built from them cannot
// drift from what the consumers read.
export const LEVELS_EVENT = "levels";
export const NOTIFICATION_EVENT = "notification";

// Event names the client synthesizes internally (from the connect loop) and the
// store routes on. A server event reusing one would be misrouted into the
// connection/auth state machine, so the generic wire dispatch refuses them.
const RESERVED_EVENTS = new Set(["connected", "disconnected", "unauthorized", "heartbeat"]);

// SSEDeps is what the client needs from the browser, injected so node:test can
// drive the connect loop with a fake fetch, no real timers and a set clock.
// timers is the same seam the stores use (lib/timers.ts); now is a monotonic
// clock in milliseconds.
export interface SSEDeps {
  fetch(url: string, init: RequestInit): Promise<Response>;
  timers: OneShotTimers;
  now(): number;
}

const browserDeps: SSEDeps = {
  fetch: (url, init) => fetch(url, init),
  timers: globalThis,
  now: () => performance.now(),
};

// HEARTBEAT_TIMEOUT_MS is how long a stream may stay silent before the client
// reconnects; the server sends a heartbeat every 15 s (defaultHeartbeat in
// internal/sse/sse.go).
export const HEARTBEAT_TIMEOUT_MS = 30_000;
// RECONNECT_DELAY_MS is the first backoff after a dropped stream, doubled up
// to RECONNECT_MAX_MS.
export const RECONNECT_DELAY_MS = 1_000;
export const RECONNECT_MAX_MS = 10_000;

export class SSEClient {
  private url: string;
  private readonly deps: SSEDeps;
  // The ?events= value, each name URL-encoded and joined by literal commas
  // (the spec's form style, explode false), or null for every event type.
  private events: string | null = null;
  private token: string | null = null;
  private abortController: AbortController | null = null;
  private isRunning: boolean = false;
  private reconnectDelayMs: number = RECONNECT_DELAY_MS;
  private heartbeatTimer: ReturnType<typeof setTimeout> | null = null;
  // When the stream last showed it is alive (deps.now). The watchdog checks
  // it when it fires, so a message costs no timer calls.
  private aliveAt = 0;
  private handlers: Set<SSEEventHandler> = new Set();
  // generation invalidates an in-flight connect loop when stop()/start() race:
  // a loop keeps running only while its captured generation is still current.
  private generation = 0;

  constructor(url: string = "/api/v1/events", deps: SSEDeps = browserDeps) {
    this.url = url;
    this.deps = deps;
  }

  // setEvents sets the event types to stream; null or an empty list means
  // every type, as the server reads an empty filter (parseEventFilter in
  // internal/sse/sse.go). A running stream reconnects under the new filter;
  // a stopped one only records it, so this never starts a stream the store
  // stopped (a 401, a hidden page).
  public setEvents(names: readonly string[] | null): void {
    const events = names === null || names.length === 0 ? null : names.map(encodeURIComponent).join(",");
    if (events === this.events) return;
    this.events = events;
    if (!this.isRunning) return;
    this.stop();
    this.start();
  }

  public setToken(token: string | null): void {
    this.token = token;
  }

  public subscribe(handler: SSEEventHandler): () => void {
    this.handlers.add(handler);
    return () => this.handlers.delete(handler);
  }

  private dispatch(eventName: string, data: unknown): void {
    for (const handler of this.handlers) {
      try {
        handler(eventName, data);
      } catch (err) {
        console.error("Error in SSE event handler:", err);
      }
    }
  }

  public start(): void {
    if (this.isRunning) return;
    this.isRunning = true;
    this.connect(++this.generation);
  }

  public stop(): void {
    this.isRunning = false;
    // Bump the generation so any connect loop still winding down (parked in a
    // reconnect delay or a read) exits instead of resurrecting on the next start.
    this.generation++;
    this.clearHeartbeat();
    if (this.abortController) {
      this.abortController.abort();
      this.abortController = null;
    }
  }

  // resetHeartbeat marks the stream alive now and makes sure the watchdog is
  // armed. The watchdog fires at most HEARTBEAT_TIMEOUT_MS after it was set
  // and re-arms for what is left of the window when data came since, so a
  // stream carrying levels at 10 Hz does not clear and set a timer per message.
  private resetHeartbeat(): void {
    this.aliveAt = this.deps.now();
    if (this.heartbeatTimer === null) this.armHeartbeat(HEARTBEAT_TIMEOUT_MS);
  }

  private armHeartbeat(ms: number): void {
    this.heartbeatTimer = this.deps.timers.setTimeout(() => {
      this.heartbeatTimer = null;
      const silent = this.deps.now() - this.aliveAt;
      if (silent < HEARTBEAT_TIMEOUT_MS) {
        this.armHeartbeat(HEARTBEAT_TIMEOUT_MS - silent);
        return;
      }
      console.warn(`SSE heartbeat timeout exceeded (${HEARTBEAT_TIMEOUT_MS / 1000}s). Reconnecting...`);
      if (this.abortController) {
        this.abortController.abort();
      }
    }, ms);
  }

  private clearHeartbeat(): void {
    if (this.heartbeatTimer !== null) {
      this.deps.timers.clearTimeout(this.heartbeatTimer);
      this.heartbeatTimer = null;
    }
  }

  private async connect(gen: number): Promise<void> {
    while (this.isRunning && gen === this.generation) {
      this.abortController = new AbortController();
      const headers = new Headers();
      headers.set("Accept", "text/event-stream");
      // Capture the token this request is sent under so a 401 can be classified:
      // a rejection of the token still in force is genuine, while a rejection of
      // a token that has since been rotated is stale and must reconnect instead.
      const used = this.token;
      if (used) {
        headers.set("Authorization", `Bearer ${used}`);
      }

      // Every connect runs under the watchdog: a restart is silent (a filter
      // change, the end of a token swap), so the store may still say
      // connected, and a connect that hangs on a dead link must not leave
      // the page looking live, or parked, until the browser gives up.
      this.resetHeartbeat();

      try {
        const url = this.events === null ? this.url : `${this.url}?events=${this.events}`;
        const response = await this.deps.fetch(url, {
          headers,
          signal: this.abortController.signal,
        });
        // A stop or restart that ran while the response was on its way owns
        // the stream now: this loop must not act on a stale answer (a 401
        // here would stop the new stream).
        if (gen !== this.generation) {
          void response.body?.cancel().catch(() => {});
          return;
        }

        if (response.status === 401) {
          // Release the unread body so the rejected connection is not left open.
          // cancel() rejects when the stream is already errored; swallow that so
          // it does not surface as an unhandled rejection.
          void response.body?.cancel().catch(() => {});
          if (used === this.token) {
            // The appliance rejects the token in force. Reconnecting on a timer
            // would hammer it with the same rejected credential every 1..10 s,
            // so stop; the store restarts the stream once a token is accepted.
            this.isRunning = false;
            this.generation++;
            // A watchdog armed for this connect (setEvents arms one) must not
            // fire into the stream a later start opens.
            this.clearHeartbeat();
            this.abortController = null;
            this.dispatch("unauthorized", null);
            return;
          }
          // The token was rotated while this request was in flight, so the 401
          // is against the OLD credential. Do not prompt; fall through to the
          // reconnect path so the stream comes back under the current token.
          throw new Error("SSE token rotated mid-connect; reconnecting");
        }

        if (!response.ok || !response.body) {
          throw new Error(`SSE HTTP error: ${response.status} ${response.statusText}`);
        }

        const reader = response.body.getReader();
        const decoder = new TextDecoder();
        let buffer = "";
        let announced = false;

        while (this.isRunning && gen === this.generation) {
          const { done, value } = await reader.read();
          if (done) break;
          // The stream counts as connected at its first bytes, not at the
          // 200: the appliance writes its open comment once its sources are
          // subscribed (openComment in internal/sse/sse.go), so a re-sync on
          // connect cannot miss what is raised in between. An older
          // appliance's first bytes are its first event.
          if (!announced) {
            announced = true;
            this.resetHeartbeat();
            this.dispatch("connected", null);
            if (gen !== this.generation) break;
          }

          // SSE allows CRLF and CR line endings as well as LF.
          buffer += decoder.decode(value, { stream: true }).replace(/\r\n?/g, "\n");
          const messages = buffer.split("\n\n");
          // Keep trailing incomplete chunk
          buffer = messages.pop() || "";

          for (const msg of messages) {
            // A handler may have stopped or restarted the stream.
            if (gen !== this.generation) break;
            // The backoff resets on the first event, not on the 200 or the
            // open comment: a server or proxy that answers and closes at once
            // must still back off.
            if (this.parseMessage(msg)) this.reconnectDelayMs = RECONNECT_DELAY_MS;
          }
        }
        // The server ended the stream (a proxy timeout, say): the reconnect
        // below runs as after an error, and listeners learn the stream is down
        // until it comes back. A stop or restart that ran during a read also
        // ends the loop here; the catch drops it, as it drops the aborted read
        // a restart usually ends in.
        throw new Error("SSE stream closed by the server");
      } catch (err: unknown) {
        if (!this.isRunning || gen !== this.generation) return;
        this.dispatch("disconnected", err);
      } finally {
        // Only clear the heartbeat if this loop is still the current generation.
        // A stale loop winding down after a stop()+start() race must not clear
        // the live loop's heartbeat timer and leave it unmonitored.
        if (gen === this.generation) this.clearHeartbeat();
      }

      if (this.isRunning && gen === this.generation) {
        await new Promise<void>((resolve) => this.deps.timers.setTimeout(resolve, this.reconnectDelayMs));
        // Re-check after the delay: a stop()+start() during it must not let this
        // stale loop double the new generation's shared backoff.
        if (!this.isRunning || gen !== this.generation) return;
        this.reconnectDelayMs = Math.min(this.reconnectDelayMs * 2, RECONNECT_MAX_MS);
      }
    }
  }

  // parseMessage dispatches one message and reports whether it was an event
  // (a heartbeat or a data frame) rather than a comment.
  private parseMessage(raw: string): boolean {
    let eventName = "message";
    let dataStr = "";

    const lines = raw.split("\n");
    for (const line of lines) {
      if (line.startsWith("event:")) {
        eventName = line.slice(6).trim();
      } else if (line.startsWith("data:")) {
        dataStr += (dataStr ? "\n" : "") + line.slice(5).trim();
      }
    }

    if (eventName === "heartbeat") {
      this.resetHeartbeat();
      this.dispatch("heartbeat", {});
      return true;
    }

    // Any other named event that carries a JSON data payload is dispatched under
    // its own name. Listeners subscribe to the names they know ("levels",
    // "notification") and ignore the rest, which is what the SSE contract asks
    // of clients; an unknown event is harmless.
    if (dataStr) {
      // A received frame means the stream is alive, so reset the heartbeat
      // before anything below can return; a malformed or reserved-name frame
      // must not let an active connection look idle and get torn down.
      this.resetHeartbeat();
      if (RESERVED_EVENTS.has(eventName)) {
        console.warn(`Ignoring SSE event with reserved name "${eventName}"`);
        return true;
      }
      try {
        const payload: unknown = JSON.parse(dataStr);
        this.dispatch(eventName, payload);
      } catch (err) {
        console.error(`Failed to parse SSE payload for event "${eventName}":`, err);
      }
      return true;
    }
    return false;
  }
}

export const sse = new SSEClient();
