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

// Emitter announces named events whose payloads are fixed by the event map M
// (event name to payload type; undefined for an event with none). A listener
// gets its payload typed, and a misspelled name or a payload of the wrong type
// fails tsc at the on or emit call instead of at runtime. on and emit wrap a
// private EventTarget, so every listener goes through on.
export class Emitter<M extends object> {
  private readonly bus = new EventTarget();

  public on<K extends keyof M & string>(name: K, listener: (payload: M[K]) => void): void {
    // The cast is sound: bus is private, emit is its only dispatcher, and
    // emit's signature ties each name's payload to M[K].
    this.bus.addEventListener(name, (e) => listener((e as PayloadEvent<M[K]>).payload));
  }

  protected emit<K extends keyof M & string>(name: K, ...payload: M[K] extends undefined ? [] : [payload: M[K]]): void {
    this.bus.dispatchEvent(new PayloadEvent(name, payload[0]));
  }
}
