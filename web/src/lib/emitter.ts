// PayloadEvent carries an emitted payload as given. CustomEvent would not do:
// its detail turns undefined into null, which an event map that says undefined
// would not describe.
class PayloadEvent<T> extends Event {
  readonly payload: T;

  constructor(type: string, payload: T) {
    super(type);
    this.payload = payload;
  }
}

// EmitArgs is one [name, payload] tuple per event in M (just [name] for an
// event with no payload), so a name typed as a union of event names still has
// to come with that event's own payload.
type EmitArgs<M> = {
  [K in keyof M & string]: M[K] extends undefined ? [name: K] : [name: K, payload: M[K]];
}[keyof M & string];

// Emitter announces named events whose payloads are fixed by the event map M
// (event name to payload type; undefined for an event with none). A listener
// gets its payload typed, and a misspelled name or a payload of the wrong type
// fails tsc at the on or emit call, where a listener added to an EventTarget
// under a misspelled name would just never fire. on and emit wrap a private
// EventTarget, so every listener goes through on.
export class Emitter<M extends object> {
  private readonly bus = new EventTarget();

  public on<K extends keyof M & string>(name: K, listener: (payload: M[K]) => void): void {
    // The cast holds because bus is private and emit, its only dispatcher,
    // takes each name together with that event's payload (EmitArgs).
    this.bus.addEventListener(name, (e) => listener((e as PayloadEvent<M[K]>).payload));
  }

  protected emit(...args: EmitArgs<M>): void {
    const [name, payload] = args;
    this.bus.dispatchEvent(new PayloadEvent(name, payload));
  }
}
