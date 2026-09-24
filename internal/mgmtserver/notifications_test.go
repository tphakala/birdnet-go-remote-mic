package mgmtserver

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
)

const testBootID = "abcd1234"

// fakeSnapshotter returns a fixed snapshot for the handler mapping test.
type fakeSnapshotter struct{ snap notify.Snapshot }

func (f *fakeSnapshotter) Snapshot() notify.Snapshot { return f.snap }

func TestNotificationWireParity(t *testing.T) {
	// The SSE stream marshals notify.Notification directly; the REST endpoint
	// serves the generated mgmtapi.Notification. The /events contract says both
	// carry the same Notification shape, so a notify.Notification's JSON must
	// unmarshal into mgmtapi.Notification with every field preserved. This guards
	// against the two encoders drifting (there is no schema drift guard binding
	// the hand-tagged struct to the generated one).
	now := time.Unix(1_700_000_000, 0).UTC()
	src := notify.Notification{
		ID: 7, BootID: testBootID, Time: now, UptimeMs: 93_500,
		Severity: notify.SeverityWarning, Category: notify.CategoryStream, Kind: notify.KindOnset,
		Key: "stream:garden:flap", Source: "garden from 10.0.0.5",
		Title: "Client reconnecting repeatedly", Message: "more than 3 connects in 60s",
	}
	data, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire mgmtapi.Notification
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("notify.Notification JSON does not fit mgmtapi.Notification: %v", err)
	}
	if wire.Id != int64(src.ID) || wire.BootId != src.BootID || !wire.Time.Equal(now) || wire.UptimeMs != src.UptimeMs {
		t.Errorf("scalar drift: %+v", wire)
	}
	if string(wire.Severity) != string(src.Severity) ||
		string(wire.Category) != string(src.Category) ||
		string(wire.Kind) != string(src.Kind) {
		t.Errorf("enum drift: severity=%q category=%q kind=%q", wire.Severity, wire.Category, wire.Kind)
	}
	if wire.Key == nil || *wire.Key != src.Key || wire.Source == nil || *wire.Source != src.Source {
		t.Errorf("key/source drift: key=%v source=%v", wire.Key, wire.Source)
	}
	if wire.Title != src.Title || wire.Message != src.Message {
		t.Errorf("title/message drift: title=%q message=%q", wire.Title, wire.Message)
	}
}

func TestWithNotificationsRejectsNilSources(t *testing.T) {
	// Both an untyped nil and a typed-nil pointer must leave the endpoint
	// unmounted (501), never mount a non-nil interface that serves an empty
	// snapshot on a nil receiver.
	cases := map[string]Option{
		"untyped nil": WithNotifications(nil),
		"typed nil":   WithNotifications((*notify.Center)(nil)),
	}
	for name, opt := range cases {
		t.Run(name, func(t *testing.T) {
			s := New(&fakeProvider{}, opt)
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
		})
	}
}

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
		UptimeMs:   120_000,
		Capacity:   500,
		NextID:     3,
		Notifications: []notify.Notification{
			{
				ID: 1, BootID: testBootID, Time: now, UptimeMs: 1_500,
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
	if got.UptimeMs != 120_000 || got.Capacity != 500 {
		t.Errorf("envelope uptimeMs/capacity = %d/%d, want 120000/500", got.UptimeMs, got.Capacity)
	}
	if len(got.Notifications) != 2 {
		t.Fatalf("notifications len = %d, want 2", len(got.Notifications))
	}
	n0 := got.Notifications[0]
	if n0.Id != 1 || n0.Severity != mgmtapi.Error || n0.Category != mgmtapi.NotificationCategoryDevice || n0.Kind != mgmtapi.Onset {
		t.Errorf("entry 0 scalar fields wrong: %+v", n0)
	}
	if n0.UptimeMs != 1_500 {
		t.Errorf("entry 0 uptimeMs = %d, want 1500", n0.UptimeMs)
	}
	if n0.Title != "Device failed" || n0.Message != "Capture stopped" || !n0.Time.Equal(now) {
		t.Errorf("entry 0 title/message/time wrong: title=%q message=%q time=%v", n0.Title, n0.Message, n0.Time)
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
