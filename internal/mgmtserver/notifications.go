package mgmtserver

import (
	"context"
	"net/http"

	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
	"github.com/tphakala/birdnet-go-remote-mic/internal/notify"
)

// Snapshotter supplies the current notification snapshot for GET /notifications.
// *notify.Center implements it. When no snapshotter is mounted the endpoint
// returns 501. Its method is called from handler goroutines and must be safe for
// concurrent use.
type Snapshotter interface {
	Snapshot() notify.Snapshot
}

// WithNotifications mounts src as the source for GET /notifications. Omit this
// option to leave the endpoint returning 501. Pass a live source; do not pass a
// typed-nil pointer (for example a nil *notify.Center), which mounts a non-nil
// interface and makes the endpoint serve an empty snapshot rather than 501.
func WithNotifications(src Snapshotter) Option {
	return func(s *Server) { s.notifications = src }
}

// ListNotifications handles GET /notifications. Without a mounted source it
// reports 501.
func (s *Server) ListNotifications(_ context.Context, _ mgmtapi.ListNotificationsRequestObject) (mgmtapi.ListNotificationsResponseObject, error) {
	if s.notifications == nil {
		return mgmtapi.ListNotificationsdefaultApplicationProblemPlusJSONResponse{
			StatusCode: http.StatusNotImplemented,
			Body:       problem(http.StatusNotImplemented, "not implemented", "notifications are not available"),
		}, nil
	}
	snap := s.notifications.Snapshot()
	return mgmtapi.ListNotifications200JSONResponse(notificationSnapshotToWire(&snap)), nil
}

// notificationSnapshotToWire maps a notify.Snapshot to the generated wire type.
func notificationSnapshotToWire(snap *notify.Snapshot) mgmtapi.NotificationSnapshot {
	ns := make([]mgmtapi.Notification, 0, len(snap.Notifications))
	for i := range snap.Notifications {
		ns = append(ns, notificationToWire(&snap.Notifications[i]))
	}
	return mgmtapi.NotificationSnapshot{
		BootId:        snap.BootID,
		ServerTime:    snap.ServerTime,
		NextId:        int64(snap.NextID),
		Notifications: ns,
	}
}

// notificationToWire maps one notification to the generated wire type. Key and
// source are optional pointer fields on the wire, omitted when empty.
func notificationToWire(n *notify.Notification) mgmtapi.Notification {
	out := mgmtapi.Notification{
		Id:       int64(n.ID),
		BootId:   n.BootID,
		Time:     n.Time,
		Severity: mgmtapi.NotificationSeverity(n.Severity),
		Category: mgmtapi.NotificationCategory(n.Category),
		Kind:     mgmtapi.NotificationKind(n.Kind),
		Title:    n.Title,
		Message:  n.Message,
	}
	if n.Key != "" {
		out.Key = ptr(n.Key)
	}
	if n.Source != "" {
		out.Source = ptr(n.Source)
	}
	return out
}
