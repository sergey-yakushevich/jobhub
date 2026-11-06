package workspace

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/sergey-yakushevich/jobhub/internal/store"
)

const maxBodyBytes = 256 << 10

// apiError is the only error shape the JSON surfaces produce.
type apiError struct {
	status  int
	code    string
	message string
}

func badRequest(message string) *apiError {
	return &apiError{http.StatusBadRequest, "invalid_request", message}
}

func invalidQuery(message string) *apiError {
	return &apiError{http.StatusBadRequest, "invalid_query", message}
}

func errNotFound(message string) *apiError {
	return &apiError{http.StatusNotFound, "not_found", message}
}

func unauthorized(message string) *apiError {
	return &apiError{http.StatusUnauthorized, "unauthorized", message}
}

func forbidden(message string) *apiError {
	return &apiError{http.StatusForbidden, "forbidden", message}
}

func conflict(message string) *apiError {
	return &apiError{http.StatusConflict, "conflict", message}
}

func internalError() *apiError {
	return &apiError{http.StatusInternalServerError, "internal", "internal error"}
}

func writeAPIError(w http.ResponseWriter, e *apiError) {
	writeJSON(w, e.status, map[string]any{
		"error": map[string]string{"code": e.code, "message": e.message},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiMethodNotAllowed(w http.ResponseWriter, allowed ...string) {
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	writeAPIError(w, &apiError{http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed"})
}

// serveAPI is bearer-only. A cookie, a query token or anything else is not
// accepted here; the UI JSON routes are separate.
func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request) {
	if !s.apiAuthorized(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeAPIError(w, unauthorized("a bearer token is required"))
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, "/api/v1")
	switch {
	case rel == "/stats":
		s.handleStats(w, r)
	case rel == "/openapi.json":
		if r.Method != http.MethodGet {
			apiMethodNotAllowed(w, http.MethodGet)
			return
		}
		s.handleOpenAPI(w, r)

	case rel == "/jobs" || rel == "/jobs/":
		switch r.Method {
		case http.MethodGet:
			s.handleListJobs(w, r)
		case http.MethodPost:
			s.handleCreateJob(w, r)
		default:
			apiMethodNotAllowed(w, http.MethodGet, http.MethodPost)
		}

	case strings.HasPrefix(rel, "/jobs/"):
		rest := strings.TrimPrefix(rel, "/jobs/")
		s.routeJobSubresource(w, r, rest, false, nil)

	default:
		writeAPIError(w, errNotFound("unknown endpoint"))
	}
}

// serveUI is the browser-JSON surface: cookie on reads, cookie + exact origin
// + CSRF header on writes.
func (s *Server) serveUI(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r)
	if sess == nil {
		writeAPIError(w, unauthorized("sign in required"))
		return
	}
	if r.Method == http.MethodPost || r.Method == http.MethodPatch {
		if !s.originMatches(r) {
			writeAPIError(w, forbidden("origin not allowed"))
			return
		}
		if !s.csrfOK(r, sess) {
			writeAPIError(w, forbidden("invalid csrf token"))
			return
		}
	}
	rel := strings.TrimPrefix(r.URL.Path, "/ui")
	switch {
	case rel == "/stats":
		s.handleStats(w, r)
	case rel == "/jobs" || rel == "/jobs/":
		switch r.Method {
		case http.MethodGet:
			s.handleListJobs(w, r)
		case http.MethodPost:
			s.handleCreateJob(w, r)
		default:
			apiMethodNotAllowed(w, http.MethodGet, http.MethodPost)
		}
	case strings.HasPrefix(rel, "/jobs/"):
		rest := strings.TrimPrefix(rel, "/jobs/")
		s.routeJobSubresource(w, r, rest, true, sess)
	default:
		writeAPIError(w, errNotFound("unknown endpoint"))
	}
}

// routeJobSubresource dispatches /jobs/{id} and /jobs/{id}/events. The
// handlers parse the id from the path themselves.
func (s *Server) routeJobSubresource(w http.ResponseWriter, r *http.Request, rest string, _ bool, _ *session) {
	parts := strings.Split(rest, "/")
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			s.handleGetJob(w, r)
		case http.MethodPatch:
			s.handlePatchJob(w, r)
		default:
			apiMethodNotAllowed(w, http.MethodGet, http.MethodPatch)
		}
		return
	}
	if len(parts) == 2 && parts[1] == "events" {
		switch r.Method {
		case http.MethodGet:
			s.handleListEvents(w, r)
		case http.MethodPost:
			s.handleCreateEvent(w, r)
		default:
			apiMethodNotAllowed(w, http.MethodGet, http.MethodPost)
		}
		return
	}
	writeAPIError(w, errNotFound("unknown endpoint"))
}

// ---- ids, cursors, versions ------------------------------------------------

func parsePositiveID(raw string) (int64, *apiError) {
	if raw == "" {
		return 0, badRequest("id must be a positive integer")
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0, badRequest("id must be a positive integer")
		}
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 {
		return 0, badRequest("id must be a positive integer")
	}
	return v, nil
}

func jobIDFromPath(path string) (int64, *apiError) {
	const marker = "/jobs/"
	i := strings.Index(path, marker)
	if i < 0 {
		return 0, errNotFound("unknown endpoint")
	}
	rest := path[i+len(marker):]
	seg := strings.SplitN(rest, "/", 2)[0]
	return parsePositiveID(seg)
}

// ifMatchVersion requires an explicit, quoted, numeric If-Match version.
func ifMatchVersion(r *http.Request) (int64, *apiError) {
	raw := strings.TrimSpace(r.Header.Get("If-Match"))
	if raw == "" {
		return 0, &apiError{http.StatusPreconditionRequired, "precondition_required", "If-Match is required"}
	}
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return 0, badRequest(`If-Match must be a quoted version such as "3"`)
	}
	v, err := parsePositiveID(raw[1 : len(raw)-1])
	if err != nil {
		return 0, badRequest(`If-Match must be a quoted version such as "3"`)
	}
	return v, nil
}

func parseCursor(raw string) (int64, *apiError) {
	if raw == "" {
		return 0, nil
	}
	v, err := parsePositiveID(raw)
	if err != nil {
		return 0, invalidQuery("cursor must be a positive integer id")
	}
	return v, nil
}

// ---- JSON bodies -----------------------------------------------------------

func isNull(raw json.RawMessage) bool {
	return string(bytesTrimSpace(raw)) == "null"
}

func bytesTrimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && (b[start] == ' ' || b[start] == '\n' || b[start] == '\r' || b[start] == '\t') {
		start++
	}
	for end > start && (b[end-1] == ' ' || b[end-1] == '\n' || b[end-1] == '\r' || b[end-1] == '\t') {
		end--
	}
	return b[start:end]
}

// readObject enforces content type, size, strict single JSON object decoding
// and rejects trailing data.
func readObject(w http.ResponseWriter, r *http.Request) (map[string]json.RawMessage, *apiError) {
	ct := r.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil || mediaType != "application/json" {
		return nil, &apiError{http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	var m map[string]json.RawMessage
	if err := dec.Decode(&m); err != nil {
		if isTooLarge(err) {
			return nil, &apiError{http.StatusRequestEntityTooLarge, "payload_too_large", "request body is too large"}
		}
		return nil, badRequest("body must be a JSON object")
	}
	if m == nil {
		return nil, badRequest("body must be a JSON object")
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		if isTooLarge(err) {
			return nil, &apiError{http.StatusRequestEntityTooLarge, "payload_too_large", "request body is too large"}
		}
		return nil, badRequest("body must contain exactly one JSON object")
	}
	return m, nil
}

func decodeString(raw json.RawMessage, name string) (string, *apiError) {
	if isNull(raw) {
		return "", badRequest(name + " must be a string")
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", badRequest(name + " must be a string")
	}
	return v, nil
}

func decodeNullableNumber(raw json.RawMessage, name string) (*float64, *apiError) {
	if isNull(raw) {
		return nil, nil
	}
	var v float64
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, badRequest(name + " must be a number or null")
	}
	return &v, nil
}

func decodeStrings(raw json.RawMessage, name string) ([]string, *apiError) {
	if isNull(raw) {
		return []string{}, nil
	}
	var v []string
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, badRequest(name + " must be an array of strings")
	}
	if v == nil {
		v = []string{}
	}
	return v, nil
}

func decodeBool(raw json.RawMessage, name string) (bool, *apiError) {
	if isNull(raw) {
		return false, badRequest(name + " must be a boolean")
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return false, badRequest(name + " must be a boolean")
	}
	return v, nil
}

// applyJobFields accepts only editable fields. Server-owned fields, ids,
// timestamps and (after creation) source_key/profile are rejected outright.
func applyJobFields(j *store.TrackedJob, m map[string]json.RawMessage, creating bool) *apiError {
	for k, raw := range m {
		var ae *apiError
		switch k {
		case "source_key":
			if !creating {
				return badRequest("source_key is immutable")
			}
			j.SourceKey, ae = decodeString(raw, k)
		case "profile":
			if !creating {
				return badRequest("profile is immutable")
			}
			j.Profile, ae = decodeString(raw, k)
		case "company":
			j.Company, ae = decodeString(raw, k)
		case "title":
			j.Title, ae = decodeString(raw, k)
		case "url":
			j.URL, ae = decodeString(raw, k)
		case "description":
			j.Description, ae = decodeString(raw, k)
		case "stage":
			j.Stage, ae = decodeString(raw, k)
		case "remote":
			j.Remote, ae = decodeString(raw, k)
		case "location":
			j.Location, ae = decodeString(raw, k)
		case "employment":
			j.Employment, ae = decodeString(raw, k)
		case "currency":
			j.Currency, ae = decodeString(raw, k)
		case "salary_period":
			j.SalaryPeriod, ae = decodeString(raw, k)
		case "salary_min":
			j.SalaryMin, ae = decodeNullableNumber(raw, k)
		case "salary_max":
			j.SalaryMax, ae = decodeNullableNumber(raw, k)
		case "fit_score":
			j.FitScore, ae = decodeNullableNumber(raw, k)
		case "fit_summary":
			j.FitSummary, ae = decodeString(raw, k)
		case "strengths":
			j.Strengths, ae = decodeStrings(raw, k)
		case "gaps":
			j.Gaps, ae = decodeStrings(raw, k)
		case "skills":
			j.Skills, ae = decodeStrings(raw, k)
		case "needs_you":
			j.NeedsYou, ae = decodeBool(raw, k)
		case "recruiter_name":
			j.RecruiterName, ae = decodeString(raw, k)
		case "recruiter_contact":
			j.RecruiterContact, ae = decodeString(raw, k)
		case "next_action":
			j.NextAction, ae = decodeString(raw, k)
		case "next_action_at":
			j.NextActionAt, ae = decodeString(raw, k)
		case "waiting_on":
			j.WaitingOn, ae = decodeString(raw, k)
		case "interview_at":
			j.InterviewAt, ae = decodeString(raw, k)
		case "interview_url":
			j.InterviewURL, ae = decodeString(raw, k)
		case "last_checked_at":
			return badRequest("last_checked_at is server-managed")
		default:
			return badRequest("unknown or immutable field: " + k)
		}
		if ae != nil {
			return ae
		}
	}
	return nil
}

// applyEventFields accepts only event content; ids, job_id and recorded_at are
// server-owned.
func applyEventFields(e *store.TimelineEvent, m map[string]json.RawMessage) *apiError {
	for k, raw := range m {
		var ae *apiError
		switch k {
		case "source_key":
			e.SourceKey, ae = decodeString(raw, k)
		case "kind":
			e.Kind, ae = decodeString(raw, k)
		case "summary":
			e.Summary, ae = decodeString(raw, k)
		case "body":
			e.Body, ae = decodeString(raw, k)
		case "actor":
			e.Actor, ae = decodeString(raw, k)
		case "channel":
			e.Channel, ae = decodeString(raw, k)
		case "direction":
			e.Direction, ae = decodeString(raw, k)
		case "contact":
			e.Contact, ae = decodeString(raw, k)
		case "account":
			e.Account, ae = decodeString(raw, k)
		case "thread_id":
			e.ThreadID, ae = decodeString(raw, k)
		case "external_id":
			e.ExternalID, ae = decodeString(raw, k)
		case "source_url":
			e.SourceURL, ae = decodeString(raw, k)
		case "verification":
			e.Verification, ae = decodeString(raw, k)
		case "happened_at":
			e.HappenedAt, ae = decodeString(raw, k)
		case "stage":
			e.Stage, ae = decodeString(raw, k)
		default:
			return badRequest("unknown or immutable field: " + k)
		}
		if ae != nil {
			return ae
		}
	}
	return nil
}

// ---- filters ---------------------------------------------------------------

func parseTrackerFilter(q url.Values) (store.TrackerFilter, *apiError) {
	var f store.TrackerFilter
	for k := range q {
		switch k {
		case "profile", "stage", "remote", "q", "needs_you", "limit", "cursor":
		default:
			return f, invalidQuery("unknown query parameter: " + k)
		}
	}
	f.Profile = q.Get("profile")
	f.Stage = q.Get("stage")
	f.Remote = q.Get("remote")
	switch f.Stage {
	case "", "active", "saved", "applied", "talking", "interview", "offer", "hired", "rejected", "archived":
	default:
		return f, invalidQuery("unknown stage")
	}
	switch f.Remote {
	case "", "unknown", "remote", "hybrid", "onsite":
	default:
		return f, invalidQuery("unknown remote arrangement")
	}
	f.Query = q.Get("q")
	if raw := q.Get("needs_you"); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return f, invalidQuery("needs_you must be true or false")
		}
		f.NeedsYou = v
	}
	limit, err := parseLimit(q.Get("limit"))
	if err != nil {
		return f, invalidQuery(err.Error())
	}
	f.Limit = limit
	cursor, ae := parseCursor(q.Get("cursor"))
	if ae != nil {
		return f, ae
	}
	f.BeforeID = cursor
	return f, nil
}

// ---- handlers --------------------------------------------------------------

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	f, ae := parseTrackerFilter(r.URL.Query())
	if ae != nil {
		writeAPIError(w, ae)
		return
	}
	page, err := s.Store.ListTrackedJobs(f)
	if err != nil {
		writeAPIError(w, storeError(err))
		return
	}
	items := page.Items
	if items == nil {
		items = []store.TrackedJob{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":       items,
		"next_cursor": page.NextCursor,
	})
}

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	m, ae := readObject(w, r)
	if ae != nil {
		writeAPIError(w, ae)
		return
	}
	j := &store.TrackedJob{}
	if ae := applyJobFields(j, m, true); ae != nil {
		writeAPIError(w, ae)
		return
	}
	if strings.TrimSpace(j.SourceKey) == "" {
		writeAPIError(w, badRequest("source_key is required"))
		return
	}
	j.Profile = strings.ToLower(strings.TrimSpace(j.Profile))
	if j.Profile == "" {
		j.Profile = "sergey"
	}
	if !isActiveProfile(j.Profile) {
		writeAPIError(w, badRequest("profile must be sergey or polina"))
		return
	}
	got, replay, err := s.Store.CreateTrackedJob(*j, s.now())
	if err != nil {
		writeAPIError(w, storeError(err))
		return
	}
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Location", "/api/v1/jobs/"+strconv.FormatInt(got.ID, 10))
	setJobETag(w, got.Version)
	writeJSON(w, statusForNew(!replay), got)
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	id, ae := jobIDFromPath(r.URL.Path)
	if ae != nil {
		writeAPIError(w, ae)
		return
	}
	j, err := s.Store.TrackedJob(id)
	if err != nil {
		writeAPIError(w, storeError(err))
		return
	}
	setJobETag(w, j.Version)
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) handlePatchJob(w http.ResponseWriter, r *http.Request) {
	id, ae := jobIDFromPath(r.URL.Path)
	if ae != nil {
		writeAPIError(w, ae)
		return
	}
	expected, ae := ifMatchVersion(r)
	if ae != nil {
		writeAPIError(w, ae)
		return
	}
	m, ae := readObject(w, r)
	if ae != nil {
		writeAPIError(w, ae)
		return
	}
	existing, err := s.Store.TrackedJob(id)
	if err != nil {
		writeAPIError(w, storeError(err))
		return
	}
	merged := *existing
	if ae := applyJobFields(&merged, m, false); ae != nil {
		writeAPIError(w, ae)
		return
	}
	updated, err := s.Store.UpdateTrackedJob(id, merged, expected, s.now())
	if err != nil {
		writeAPIError(w, storeError(err))
		return
	}
	setJobETag(w, updated.Version)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	id, ae := jobIDFromPath(r.URL.Path)
	if ae != nil {
		writeAPIError(w, ae)
		return
	}
	q := r.URL.Query()
	for k := range q {
		if k != "limit" && k != "cursor" {
			writeAPIError(w, invalidQuery("unknown query parameter: "+k))
			return
		}
	}
	limit, err := parseLimit(q.Get("limit"))
	if err != nil {
		writeAPIError(w, invalidQuery(err.Error()))
		return
	}
	if limit == 0 {
		limit = 50
	}
	before, ae := parseCursor(q.Get("cursor"))
	if ae != nil {
		writeAPIError(w, ae)
		return
	}
	page, err := s.Store.TimelineEvents(id, limit, before)
	if err != nil {
		writeAPIError(w, storeError(err))
		return
	}
	items := page.Items
	if items == nil {
		items = []store.TimelineEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":       items,
		"next_cursor": page.NextCursor,
	})
}

func (s *Server) handleCreateEvent(w http.ResponseWriter, r *http.Request) {
	id, ae := jobIDFromPath(r.URL.Path)
	if ae != nil {
		writeAPIError(w, ae)
		return
	}
	m, ae := readObject(w, r)
	if ae != nil {
		writeAPIError(w, ae)
		return
	}
	e := &store.TimelineEvent{JobID: id}
	if ae := applyEventFields(e, m); ae != nil {
		writeAPIError(w, ae)
		return
	}
	if strings.TrimSpace(e.SourceKey) == "" {
		writeAPIError(w, badRequest("source_key is required"))
		return
	}
	got, replay, err := s.Store.AddTimelineEvent(id, *e, s.now())
	if err != nil {
		writeAPIError(w, storeError(err))
		return
	}
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, statusForNew(!replay), got)
}

func statusForNew(created bool) int {
	if created {
		return http.StatusCreated
	}
	return http.StatusOK
}

// storeError maps the store's typed failures to API statuses. Only validation
// messages are echoed; every other failure stays generic.
func storeError(err error) *apiError {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return errNotFound("not found")
	case errors.Is(err, store.ErrTrackerVersion):
		return conflict("version conflict")
	case errors.Is(err, store.ErrTrackerConflict):
		return conflict("conflict")
	case errors.Is(err, store.ErrTrackerValidation):
		return badRequest(err.Error())
	default:
		return internalError()
	}
}
