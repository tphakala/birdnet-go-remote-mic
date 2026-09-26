package mgmtserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/tphakala/birdnet-go-remote-mic/internal/mgmtapi"
	"github.com/tphakala/birdnet-go-remote-mic/internal/update"
)

// UpdateProvider is the release update subsystem behind GET /system's update
// field and the /system/update routes. *update.Manager implements it; its
// methods are called from handler goroutines and must be safe for concurrent
// use.
type UpdateProvider interface {
	Status() update.Status
	CheckNow(ctx context.Context) (update.Status, error)
	StartApply() (update.Status, error)
}

// WithUpdates mounts u behind the update routes. Without it GET /system omits
// the update field and the update routes return 501.
func WithUpdates(u UpdateProvider) Option {
	return func(s *Server) { s.updates = u }
}

// PostSystemUpdate handles POST /system/update.
func (s *Server) PostSystemUpdate(_ context.Context, _ mgmtapi.PostSystemUpdateRequestObject) (mgmtapi.PostSystemUpdateResponseObject, error) {
	if s.updates == nil {
		return mgmtapi.PostSystemUpdatedefaultApplicationProblemPlusJSONResponse{
			StatusCode: http.StatusNotImplemented,
			Body:       problem(http.StatusNotImplemented, "not implemented", "updates are not available"),
		}, nil
	}
	st, err := s.updates.StartApply()
	switch {
	case err == nil:
		return mgmtapi.PostSystemUpdate202JSONResponse(updateToWire(&st)), nil
	case errors.Is(err, update.ErrNoUpdate), errors.Is(err, update.ErrCannotApply), errors.Is(err, update.ErrBusy), errors.Is(err, update.ErrChecksDisabled):
		return mgmtapi.PostSystemUpdate409ApplicationProblemPlusJSONResponse(problem(http.StatusConflict, "update not possible", err.Error())), nil
	default:
		return mgmtapi.PostSystemUpdatedefaultApplicationProblemPlusJSONResponse{
			StatusCode: http.StatusInternalServerError,
			Body:       problem(http.StatusInternalServerError, "update failed", err.Error()),
		}, nil
	}
}

// PostSystemUpdateCheck handles POST /system/update/check. A failed check is
// a 200 carrying lastError: the request itself worked.
func (s *Server) PostSystemUpdateCheck(ctx context.Context, _ mgmtapi.PostSystemUpdateCheckRequestObject) (mgmtapi.PostSystemUpdateCheckResponseObject, error) {
	if s.updates == nil {
		return mgmtapi.PostSystemUpdateCheckdefaultApplicationProblemPlusJSONResponse{
			StatusCode: http.StatusNotImplemented,
			Body:       problem(http.StatusNotImplemented, "not implemented", "updates are not available"),
		}, nil
	}
	st, err := s.updates.CheckNow(ctx)
	switch {
	case err == nil:
		return mgmtapi.PostSystemUpdateCheck200JSONResponse(updateToWire(&st)), nil
	case errors.Is(err, update.ErrChecksDisabled), errors.Is(err, update.ErrNotRelease):
		return mgmtapi.PostSystemUpdateCheck409ApplicationProblemPlusJSONResponse(problem(http.StatusConflict, "cannot check for updates", err.Error())), nil
	default:
		return mgmtapi.PostSystemUpdateCheckdefaultApplicationProblemPlusJSONResponse{
			StatusCode: http.StatusInternalServerError,
			Body:       problem(http.StatusInternalServerError, "check failed", err.Error()),
		}, nil
	}
}

// updateToWire maps the update state to the generated wire type, omitting
// the optional fields that are empty.
func updateToWire(st *update.Status) mgmtapi.UpdateStatus {
	out := mgmtapi.UpdateStatus{
		CurrentVersion: st.Current,
		Supported:      st.Supported,
		CheckEnabled:   st.CheckEnabled,
		Available:      st.Available,
		InstallMethod:  mgmtapi.UpdateStatusInstallMethod(st.Install.Method),
		CanApply:       st.Install.CanApply,
		Phase:          mgmtapi.UpdateStatusPhase(st.Phase),
	}
	if st.Latest != "" {
		out.LatestVersion = new(st.Latest)
	}
	if st.NotesURL != "" {
		out.NotesUrl = new(st.NotesURL)
	}
	if !st.LastCheck.IsZero() {
		out.LastCheck = new(st.LastCheck.UTC())
	}
	if st.LastError != "" {
		out.LastError = new(st.LastError)
	}
	if st.Install.Hint != "" {
		out.UpgradeHint = new(st.Install.Hint)
	}
	if st.PhaseMessage != "" {
		out.PhaseMessage = new(st.PhaseMessage)
	}
	return out
}
