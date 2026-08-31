package levels

import (
	"encoding/json"
	"testing"
)

func TestRemoveMeterDropsDevice(t *testing.T) {
	h := NewHub()
	h.Meter("a", 1)
	h.Meter("b", 1)

	h.RemoveMeter("a")

	if len(h.meters) != 1 || h.meters[0].name != "b" {
		t.Fatalf("after RemoveMeter(a), meters = %+v, want only b", h.meters)
	}

	// The dropped device must no longer appear in a levels event.
	var ev LevelsEvent
	if err := json.Unmarshal(h.levelsEvent().Data, &ev); err != nil {
		t.Fatalf("unmarshal levels event: %v", err)
	}
	for _, d := range ev.Devices {
		if d.Name == "a" {
			t.Fatalf("removed device %q still present in levels event", d.Name)
		}
	}
}

func TestRemoveMeterUnknownIsNoop(t *testing.T) {
	h := NewHub()
	h.Meter("a", 1)
	h.RemoveMeter("does-not-exist")
	if len(h.meters) != 1 || h.meters[0].name != "a" {
		t.Fatalf("RemoveMeter of unknown name changed meters = %+v", h.meters)
	}
}

// TestRemoveMeterOnlyDropsNamed removes one of several like-positioned meters and
// confirms the remaining registration order is preserved for the others.
func TestRemoveMeterOnlyDropsNamed(t *testing.T) {
	h := NewHub()
	h.Meter("a", 1)
	h.Meter("b", 1)
	h.Meter("c", 1)

	h.RemoveMeter("b")

	if len(h.meters) != 2 || h.meters[0].name != "a" || h.meters[1].name != "c" {
		t.Fatalf("after RemoveMeter(b), meters = %+v, want a then c", h.meters)
	}
}
