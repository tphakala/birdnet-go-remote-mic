package reload

import (
	"reflect"
	"testing"

	"github.com/tphakala/birdnet-go-remote-mic/internal/config"
)

// twoStreamDev builds an enabled device fanning out two PCM streams from one
// capture, at paths /<name>1 and /<name>2.
func twoStreamDev(name, hw string, rate int) config.Device {
	return config.Device{
		Name: name, Device: hw, Rate: rate, Format: "s16",
		Streams: []config.Stream{
			{Path: "/" + name + "1", Mode: config.ModePCM, Channels: []int{1}},
			{Path: "/" + name + "2", Mode: config.ModePCM, Channels: []int{2}},
		},
	}
}

func TestReconcileMultiStreamUnchangedIsNoop(t *testing.T) {
	running := map[string]config.Device{"a": twoStreamDev("a", "hw:0", 48000)}
	p := Reconcile(running, cfg(twoStreamDev("a", "hw:0", 48000)))
	if !p.Empty() {
		t.Fatalf("identical multi-stream device produced a plan: Start=%v Stop=%v Restart=%v",
			namesOf(p.Start), p.Stop, namesOf(p.Restart))
	}
}

func TestReconcileRestartsOnStreamAdd(t *testing.T) {
	running := map[string]config.Device{"a": dev("a", "hw:0", "/a1", 48000)}
	desired := dev("a", "hw:0", "/a1", 48000)
	desired.Streams = append(desired.Streams, config.Stream{Path: "/a2", Mode: config.ModePCM, Channels: []int{2}})
	p := Reconcile(running, cfg(desired))
	if got := namesOf(p.Restart); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("adding a stream: Restart = %v, want [a]", got)
	}
}

func TestReconcileRestartsOnStreamRemove(t *testing.T) {
	running := map[string]config.Device{"a": twoStreamDev("a", "hw:0", 48000)}
	desired := twoStreamDev("a", "hw:0", 48000)
	desired.Streams = desired.Streams[:1] // drop the second stream
	p := Reconcile(running, cfg(desired))
	if got := namesOf(p.Restart); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("removing a stream: Restart = %v, want [a]", got)
	}
}

func TestReconcileRestartsOnStreamChannelEdit(t *testing.T) {
	running := map[string]config.Device{"a": twoStreamDev("a", "hw:0", 48000)}
	desired := twoStreamDev("a", "hw:0", 48000)
	desired.Streams[1].Channels = []int{3} // re-route the second stream to a different channel
	p := Reconcile(running, cfg(desired))
	if got := namesOf(p.Restart); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("editing a stream's channels: Restart = %v, want [a]", got)
	}
}
