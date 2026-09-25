package notify

// ring is a fixed-capacity FIFO of notifications in insertion order, which is
// ascending ID order because Center assigns IDs monotonically under the same
// mutex that pushes here. At capacity the oldest entry is overwritten, so the
// ring is discrete history bounded by count, never by memory. It is not safe for
// concurrent use; Center holds its mutex around every call.
type ring struct {
	buf  []Notification
	head int // index of the oldest entry
	size int // number of entries currently held (0..len(buf))
}

// newRing returns an empty ring holding at most capacity entries (at least one).
func newRing(capacity int) *ring {
	if capacity < 1 {
		capacity = 1
	}
	return &ring{buf: make([]Notification, capacity)}
}

// push stores *n, dropping the oldest entry when the ring is already at
// capacity. It takes a pointer to avoid copying the notification into the call;
// the ring keeps its own value copy.
func (r *ring) push(n *Notification) {
	if r.size < len(r.buf) {
		r.buf[(r.head+r.size)%len(r.buf)] = *n
		r.size++
		return
	}
	// Full: overwrite the oldest and advance head so it points at the new oldest.
	r.buf[r.head] = *n
	r.head = (r.head + 1) % len(r.buf)
}

// replace overwrites the held entry whose ID is n.ID with *n, reporting whether
// one was held; an entry the ring has already trimmed is left trimmed.
func (r *ring) replace(n *Notification) bool {
	for i := range r.size {
		if j := (r.head + i) % len(r.buf); r.buf[j].ID == n.ID {
			r.buf[j] = *n
			return true
		}
	}
	return false
}

// all returns the entries in ascending ID order (oldest first) as a fresh slice.
func (r *ring) all() []Notification {
	out := make([]Notification, r.size)
	for i := 0; i < r.size; i++ {
		out[i] = r.buf[(r.head+i)%len(r.buf)]
	}
	return out
}
