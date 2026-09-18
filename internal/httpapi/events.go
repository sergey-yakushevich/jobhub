package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sergey-yakushevich/jobhub/internal/store"
)

// handleJobEventAdd appends one moment to a lead's application timeline:
//
//	POST /api/jobs/12/events {"kind":"email","note":"HR asks for a call Tue","at":"2026-09-18T09:15:00Z"}
//
// kind is free-form ("email", "dm", "note", "interview", …) — the funnel's
// closed set lives in app_status, the timeline records whatever happened.
// at is optional and defaults to now. Adding an event stamps checked_at,
// since an event arriving means somebody just looked.
func (s *Server) handleJobEventAdd(w http.ResponseWriter, r *http.Request) {
	req := struct {
		Kind string `json:"kind"`
		Note string `json:"note"`
		At   string `json:"at"`
	}{}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	if strings.TrimSpace(req.Kind) == "" && strings.TrimSpace(req.Note) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "kind or note required"})
		return
	}
	at, _ := time.Parse(time.RFC3339, req.At)
	id, err := s.Store.ResolveJobRef(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such job"})
		return
	}
	ev, err := s.Store.AddJobEvent(id, clip(req.Kind, 40), clip(req.Note, 4000), at, time.Now())
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such job"})
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

// handleJobEventsList is the timeline, newest first:
//
//	GET /api/jobs/12/events
func (s *Server) handleJobEventsList(w http.ResponseWriter, r *http.Request) {
	id, err := s.Store.ResolveJobRef(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such job"})
		return
	}
	events, err := s.Store.JobEvents(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if events == nil {
		events = []store.JobEvent{}
	}
	writeJSON(w, http.StatusOK, events)
}

// handleJobAppStatus records where the sent application stands:
//
//	POST /api/jobs/12/appstatus {"status":"hired","note":"offer signed"}
//
// Status is one of in_process / rejected / hired, or "" to clear the explicit
// one. A change writes a "status" event onto the timeline by itself, and any
// call stamps checked_at.
func (s *Server) handleJobAppStatus(w http.ResponseWriter, r *http.Request) {
	req := struct {
		Status string `json:"status"`
		Note   string `json:"note"`
	}{}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req)
	id, err := s.Store.ResolveJobRef(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such job"})
		return
	}
	if err := s.Store.SetJobAppStatus(id, req.Status, clip(req.Note, 500), time.Now()); err != nil {
		if errors.Is(err, store.ErrBadAppStatus) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such job"})
		return
	}
	job, _ := s.Store.JobByID(id)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "app_status": job.AppState(), "status": job.Status})
}

// handleJobChecked stamps "the application's progress was just looked at" —
// the call a monitoring pass makes even when the inbox held nothing new:
//
//	POST /api/jobs/12/checked
func (s *Server) handleJobChecked(w http.ResponseWriter, r *http.Request) {
	id, err := s.Store.ResolveJobRef(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such job"})
		return
	}
	now := time.Now()
	if err := s.Store.TouchJobChecked(id, now); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such job"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "checked_at": now.UTC().Format(time.RFC3339)})
}

// handleJobAppStatusForm is the job page's own status buttons, guarded by the
// lead's link key so the funnel can be moved from the phone reading it.
func (s *Server) handleJobAppStatusForm(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !s.guard(w, r, jobScope(id)) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	status := r.PostFormValue("status")
	if err := s.Store.SetJobAppStatus(id, status, "", time.Now()); err != nil {
		if errors.Is(err, store.ErrBadAppStatus) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, s.safeBack(r.URL.Query().Get("back")), http.StatusSeeOther)
}

// handleJobRejectToggle is the job page's reject button — the other half of
// the approve/reject gate, key-guarded like every button on these pages. A
// rejection carries the reason typed into the composer pill; clicking on an
// already-rejected lead puts it back in play.
func (s *Server) handleJobRejectToggle(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !s.guard(w, r, jobScope(id)) {
		return
	}
	job, err := s.Store.JobByID(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	reason := clip(strings.TrimSpace(r.PostFormValue("reason")), 500)
	on := !job.Rejected()
	if on && reason == "" {
		if wantsJSON(r) {
			writeJSON(w, http.StatusBadRequest,
				map[string]string{"error": store.ErrRejectReasonRequired.Error()})
			return
		}
		http.Error(w, store.ErrRejectReasonRequired.Error(), http.StatusBadRequest)
		return
	}
	if err := s.Store.SetJobRejected(id, on, reason, time.Now()); err != nil {
		http.NotFound(w, r)
		return
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "on": on})
		return
	}
	http.Redirect(w, r, s.safeBack(r.URL.Query().Get("back")), http.StatusSeeOther)
}
