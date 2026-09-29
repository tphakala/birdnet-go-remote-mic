package monitor

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// The Done and Wait seams are reached through interface assertions, so a
// missing method fails an assertion here rather than the build.

type doneMonitor interface{ Done() <-chan struct{} }

func doneOf(t *testing.T, m any) <-chan struct{} {
	t.Helper()
	d, ok := m.(doneMonitor)
	if !ok {
		t.Fatalf("%T has no Done method", m)
	}
	return d.Done()
}

func waitGroup(t *testing.T, g Group, timeout time.Duration) {
	t.Helper()
	w, ok := any(g).(interface{ Wait(time.Duration) })
	if !ok {
		t.Fatalf("%T has no Wait method", g)
	}
	w.Wait(timeout)
}

// doner is a Group member with a controllable Done channel.
type doner struct{ done chan struct{} }

func (*doner) Apply(*Settings)         {}
func (d *doner) Done() <-chan struct{} { return d.done }

// Group.Wait gives up at the timeout on a member whose Done never closes.
func TestGroupWaitBounded(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		stuck := &doner{done: make(chan struct{})}
		start := time.Now()
		waitGroup(t, Group{nil, stuck}, 2*time.Second)
		if got := time.Since(start); got != 2*time.Second {
			t.Errorf("Wait on a stuck member took %v, want the 2s timeout", got)
		}
	})
}

// The timeout covers the whole group, not each member, and Wait does not
// return before the slowest member is done.
func TestGroupWaitWholeGroup(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		waitGroup(t, Group{
			&doner{done: make(chan struct{})},
			&doner{done: make(chan struct{})},
		}, 2*time.Second)
		if got := time.Since(start); got != 2*time.Second {
			t.Errorf("Wait on two stuck members took %v, want one 2s timeout", got)
		}

		fast, slow := &doner{done: make(chan struct{})}, &doner{done: make(chan struct{})}
		go func() {
			time.Sleep(time.Second)
			close(fast.done)
			time.Sleep(500 * time.Millisecond)
			close(slow.done)
		}()
		start = time.Now()
		waitGroup(t, Group{fast, slow}, 5*time.Second)
		if got := time.Since(start); got != 1500*time.Millisecond {
			t.Errorf("Wait took %v, want 1.5s (until the slowest member was done)", got)
		}
	})
}

func TestGroupWaitsForLateMember(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		late := &doner{done: make(chan struct{})}
		go func() {
			time.Sleep(time.Second)
			close(late.done)
		}()
		start := time.Now()
		waitGroup(t, Group{late}, 5*time.Second)
		if got := time.Since(start); got != time.Second {
			t.Errorf("Wait took %v, want 1s (until the member was done)", got)
		}
	})
}

// A Host that no RunHost goroutine drives has nothing to wait for.
func TestNewHostDoneAlreadyClosed(t *testing.T) {
	t.Parallel()
	s := hostSettings()
	select {
	case <-doneOf(t, NewHost(nil, nil, newRecPub(), &s)):
	default:
		t.Error("Done of a Host with no goroutine is open, want closed")
	}
}

// RunHost's Done stays open while the goroutine runs and closes once it has
// returned.
func TestRunHostDoneClosesOnReturn(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s := hostSettings()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := doneOf(t, RunHost(ctx, nil, nil, newRecPub(), &s))
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("Done closed while the monitor was still running")
		default:
		}
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("Done still open after the goroutine returned")
		}
	})
}
