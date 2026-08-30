import type { LevelsEvent } from "./types.js";

export type SSEEventHandler = (eventName: string, data: unknown) => void;

export class SSEClient {
  private url: string;
  private token: string | null = null;
  private abortController: AbortController | null = null;
  private isRunning: boolean = false;
  private reconnectDelayMs: number = 1000;
  private maxReconnectDelayMs: number = 10000;
  private heartbeatTimeoutMs: number = 30000;
  private heartbeatTimer: number | null = null;
  private handlers: Set<SSEEventHandler> = new Set();
  // generation invalidates an in-flight connect loop when stop()/start() race:
  // a loop keeps running only while its captured generation is still current.
  private generation = 0;

  constructor(url: string = "/api/v1/events") {
    this.url = url;
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

  private resetHeartbeat(): void {
    this.clearHeartbeat();
    this.heartbeatTimer = window.setTimeout(() => {
      console.warn("SSE heartbeat timeout exceeded (30s). Reconnecting...");
      if (this.abortController) {
        this.abortController.abort();
      }
    }, this.heartbeatTimeoutMs);
  }

  private clearHeartbeat(): void {
    if (this.heartbeatTimer !== null) {
      clearTimeout(this.heartbeatTimer);
      this.heartbeatTimer = null;
    }
  }

  private async connect(gen: number): Promise<void> {
    while (this.isRunning && gen === this.generation) {
      this.abortController = new AbortController();
      const headers = new Headers();
      headers.set("Accept", "text/event-stream");
      if (this.token) {
        headers.set("Authorization", `Bearer ${this.token}`);
      }

      try {
        const response = await fetch(this.url, {
          headers,
          signal: this.abortController.signal,
        });

        if (!response.ok || !response.body) {
          throw new Error(`SSE HTTP error: ${response.status} ${response.statusText}`);
        }

        // Successfully connected, reset backoff delay
        this.reconnectDelayMs = 1000;
        this.dispatch("connected", null);
        this.resetHeartbeat();

        const reader = response.body.getReader();
        const decoder = new TextDecoder();
        let buffer = "";

        while (this.isRunning) {
          const { done, value } = await reader.read();
          if (done) break;

          buffer += decoder.decode(value, { stream: true });
          const messages = buffer.split("\n\n");
          // Keep trailing incomplete chunk
          buffer = messages.pop() || "";

          for (const msg of messages) {
            this.parseMessage(msg);
          }
        }
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
        await new Promise((resolve) => setTimeout(resolve, this.reconnectDelayMs));
        this.reconnectDelayMs = Math.min(this.reconnectDelayMs * 2, this.maxReconnectDelayMs);
      }
    }
  }

  private parseMessage(raw: string): void {
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
      return;
    }

    if (eventName === "levels" && dataStr) {
      try {
        const payload = JSON.parse(dataStr) as LevelsEvent;
        this.resetHeartbeat();
        this.dispatch("levels", payload);
      } catch (err) {
        console.error("Failed to parse levels SSE payload:", err);
      }
    }
  }
}

export const sse = new SSEClient();
