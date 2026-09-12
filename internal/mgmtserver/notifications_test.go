package mgmtserver

import (
	"context"
	"testing"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
)

const testBootID = "abcd1234"

// fakeSnapshotter returns a fixed snapshot for the handler mapping test.
type fakeSnapshotter struct{ snap notify.Snapshot }

func (f *fakeSnapshotter) Snapshot() notify.Snapshot { return f.snap }

func TestListNotificationsNotImplementedWithoutSource(t *testing.T) {
	s := New(&fakeProvider{})
	resp, err := s.ListNotifications(context.Background(), mgmtapi.ListNotificationsRequestObject{})
	if err != nil {
		t.Fatalf("ListNotifications: %v", err)
	}
	p, ok := resp.(mgmtapi.ListNotificationsdefaultApplicationProblemPlusJSONResponse)
	if !ok {
		t.Fatalf("ListNotifications returned %T, want default problem", resp)
	}
	if p.StatusCode != 501 {
		t.Errorf("status = %d, want 501", p.StatusCode)
	}
}

func TestListNotificationsMapsSnapshot(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	src := &fakeSnapshotter{snap: notify.Snapshot{
		BootID:     testBootID,
		ServerTime: now,
		NextID:     3,
		Notifications: []notify.Notification{
			{
				ID: 1, BootID: testBootID, Time: now,
				Severity: notify.SeverityError, Category: notify.CategoryDevice, Kind: notify.KindOnset,
				Key: "device:garden:down", Source: "garden",
				Title: "Device failed", Message: "Capture stopped",
			},
			{
				ID: 2, BootID: testBootID, Time: now,
				Severity: notify.SeverityInfo, Category: notify.CategorySystem, Kind: notify.KindEvent,
				Title: "Appliance started", Message: "Appliance started, version v1",
			},
		},
	}}
	s := New(&fakeProvider{}, WithNotifications(src))
	resp, err := s.ListNotifications(context.Background(), mgmtapi.ListNotificationsRequestObject{})
	if err != nil {
		t.Fatalf("ListNotifications: %v", err)
	}
	got, ok := resp.(mgmtapi.ListNotifications200JSONResponse)
	if !ok {
		t.Fatalf("ListNotifications returned %T, want 200", resp)
	}
	if got.BootId != testBootID || got.NextId != 3 || !got.ServerTime.Equal(now) {
		t.Errorf("envelope wrong: bootId=%q nextId=%d serverTime=%v", got.BootId, got.NextId, got.ServerTime)
	}
	if len(got.Notifications) != 2 {
		t.Fatalf("notifications len = %d, want 2", len(got.Notifications))
	}
	n0 := got.Notifications[0]
	if n0.Id != 1 || n0.Severity != mgmtapi.Error || n0.Category != mgmtapi.NotificationCategoryDevice || n0.Kind != mgmtapi.Onset {
		t.Errorf("entry 0 scalar fields wrong: %+v", n0)
	}
	// Key and source are present on the onset entry.
	if n0.Key == nil || *n0.Key != "device:garden:down" {
		t.Errorf("entry 0 key = %v, want device:garden:down", n0.Key)
	}
	if n0.Source == nil || *n0.Source != "garden" {
		t.Errorf("entry 0 source = %v, want garden", n0.Source)
	}
	// The discrete event omits key and source (nil pointers).
	n1 := got.Notifications[1]
	if n1.Key != nil {
		t.Errorf("entry 1 key = %v, want nil", n1.Key)
	}
	if n1.Source != nil {
		t.Errorf("entry 1 source = %v, want nil", n1.Source)
	}
	if n1.Kind != mgmtapi.Event || n1.Severity != mgmtapi.Info {
		t.Errorf("entry 1 kind/severity wrong: %+v", n1)
	}
}
