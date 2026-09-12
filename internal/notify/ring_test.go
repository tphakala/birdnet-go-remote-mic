package notify

import "testing"

// ids extracts the id sequence of a slice of notifications for compact asserts.
func ids(ns []Notification) []uint64 {
	out := make([]uint64, len(ns))
	for i := range ns {
		out[i] = ns[i].ID
	}
	return out
}

func equalIDs(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRingUnderCapacityKeepsOrder(t *testing.T) {
	r := newRing(5)
	for i := uint64(1); i <= 3; i++ {
		r.push(&Notification{ID: i})
	}
	if got := ids(r.all()); !equalIDs(got, []uint64{1, 2, 3}) {
		t.Fatalf("all() = %v, want [1 2 3]", got)
	}
}

func TestRingOverflowDropsOldest(t *testing.T) {
	r := newRing(5)
	for i := uint64(1); i <= 7; i++ {
		r.push(&Notification{ID: i})
	}
	// Capacity 5 keeps only the last five, oldest first.
	if got := ids(r.all()); !equalIDs(got, []uint64{3, 4, 5, 6, 7}) {
		t.Fatalf("all() = %v, want [3 4 5 6 7]", got)
	}
}

func TestRingCapacityBelowOneClampsToOne(t *testing.T) {
	r := newRing(0)
	r.push(&Notification{ID: 1})
	r.push(&Notification{ID: 2})
	if got := ids(r.all()); !equalIDs(got, []uint64{2}) {
		t.Fatalf("all() = %v, want [2] (capacity clamped to 1)", got)
	}
}
