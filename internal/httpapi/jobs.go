package httpapi

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sergey-yakushevich/jobhub/internal/store"
)

// Scopes for the jobs pages. One key opens the whole board (like a site key);
// each lead's detail and redirect links carry their own key, so a leaked
// detail link exposes one lead, not the board.
func jobsScope() string        { return "jobs" }
func jobScope(id int64) string { return fmt.Sprintf("j:%d", id) }

// jobsIngestRequest is what a sweep posts: a batch of leads. Field names match
// the store's JSON tags, so the sweep's aggregate output maps straight in.
type jobsIngestRequest struct {
	Jobs []jobIngest `json:"jobs"`
}

type jobIngest struct {
	DedupeKey string `json:"dedupe_key"`
	// Profile is whose hunt this lead belongs to. Optional, and omitting it
	// means the board's default owner — so a sweep written before profiles
	// existed keeps working unchanged.
	Profile   string  `json:"profile"`
	Network   string  `json:"network"`
	JobType   string  `json:"job_type"`
	Score     float64 `json:"score"`
	Author    string  `json:"author"`
	Title     string  `json:"title"`
	Body      string  `json:"body"`
	URL       string  `json:"url"`
	Subreddit string  `json:"subreddit"`
	Emails    string  `json:"emails"`
	Signals   string  `json:"signals"`
	Draft     string  `json:"draft"`
	// PostingURL / PostingText are the real listing the resolve pass found
	// behind the aggregator blurb, and ScoreReason is why the judge landed on
	// Score. A non-zero score without a reason is refused, the same way a
	// rejection without one is.
	PostingURL  string `json:"posting_url"`
	PostingText string `json:"posting_text"`
	ScoreReason string `json:"score_reason"`
	// Prep is the review artifact, opaque JSON re-encoded as a string. It is
	// normally written through POST /api/jobs/{id}/prep; accepting it here too
	// lets one push both refresh a lead and attach its prep.
	Prep string `json:"prep"`
	// PostedAt is RFC3339, optional — a sweep often only knows "3 days ago"
	// roughly, and an absent value renders as nothing rather than a fake date.
	PostedAt string `json:"posted_at"`
	// Applied is optional and tri-state: omit it and the lead's outreach state
	// is untouched, so a re-sweep never un-applies work already done. Sending
	// it makes `{dedupe_key, network, applied}` a one-line "I applied to this".
	Applied *bool `json:"applied"`
	// Rejected works the same way, but `rejected: true` must carry a
	// reject_reason or the whole batch is refused.
	Rejected     *bool  `json:"rejected"`
	RejectReason string `json:"reject_reason"`
	// Approved is the review gate, tri-state for the same reason: a sweep that
	// says nothing about approval must never undo a decision already taken.
	Approved    *bool  `json:"approved"`
	ReviewNotes string `json:"review_notes"`
}

func (s *Server) handleJobsIngest(w http.ResponseWriter, r *http.Request) {
	var req jobsIngestRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	if len(req.Jobs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "jobs required"})
		return
	}
	batch := make([]store.JobParams, 0, len(req.Jobs))
	for _, j := range req.Jobs {
		posted, _ := time.Parse(time.RFC3339, j.PostedAt)
		batch = append(batch, store.JobParams{
			DedupeKey: clip(j.DedupeKey, 200),
			Profile:   clip(strings.ToLower(strings.TrimSpace(j.Profile)), 40),
			Network:   clip(strings.ToLower(j.Network), 20),
			JobType:   clip(strings.ToLower(j.JobType), 20),
			Score:     j.Score,
			Author:    clip(j.Author, 120),
			Title:     clip(j.Title, 300),
			Body:      clip(j.Body, 4000),
			URL:       clip(j.URL, 600),
			Subreddit: clip(j.Subreddit, 80),
			Emails:    clip(j.Emails, 300),
			Signals:   clip(j.Signals, 300),
			Draft:     clip(j.Draft, 4000),
			// The real posting is the longest field here by far — a full job
			// description runs well past a Reddit post — and it is the one the
			// judging and prep stages actually read.
			PostingURL:  clip(j.PostingURL, 600),
			PostingText: clip(j.PostingText, 20000),
			ScoreReason: clip(j.ScoreReason, 500),
			Prep:        clip(j.Prep, 20000),
			PostedAt:    posted,
			Applied:     j.Applied,

			Rejected:     j.Rejected,
			RejectReason: clip(j.RejectReason, 500),
			Approved:     j.Approved,
			ReviewNotes:  clip(j.ReviewNotes, 4000),
		})
	}
	res, err := s.Store.UpsertJobs(batch, time.Now())
	if errors.Is(err, store.ErrRejectReasonRequired) || errors.Is(err, store.ErrScoreReasonRequired) {
		// A caller-side mistake, not a server fault: say which rule was broken
		// rather than returning a bare 500. Both are all-or-nothing — the batch
		// is one transaction — so the message has to be specific enough to find
		// the offending row in a push of two hundred.
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		log.Printf("[jobs] ingest: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "ingest failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"added":   res.Added,
		"updated": res.Updated,
		"link":    fmt.Sprintf("%s/jobs?k=%s", s.BasePath, s.LinkKey(jobsScope())),
	})
}

// handleJobApplied is the token-guarded way to flip one lead's outreach flag:
//
//	POST /api/jobs/12/applied           -> applied
//	POST /api/jobs/12/applied {"applied": false} -> back to not applied
//
// The id may also be a dedupe_key, so an agent that just pushed
// `t3_1w6rw62` can mark it applied without learning trackhub's numeric id
// first. An empty body means true, because "mark this applied" is the only
// thing anyone posts here by hand.
func (s *Server) handleJobApplied(w http.ResponseWriter, r *http.Request) {
	req := struct {
		Applied *bool `json:"applied"`
	}{}
	// A missing or empty body is not an error here; it means the default.
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req)
	applied := req.Applied == nil || *req.Applied

	ref := r.PathValue("id")
	err := byIDOrKey(ref,
		func(id int64) error { return s.Store.SetJobApplied(id, applied, time.Now()) },
		func(key string) error { return s.Store.SetJobAppliedByKey(key, applied, time.Now()) })
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such job"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": ref, "applied": applied})
}

// byIDOrKey resolves a path segment that may be either a numeric row id or a
// dedupe_key, and the order matters: X leads use the tweet id as their
// dedupe_key, so "2095405525227704692" parses cleanly as an int64 and would
// otherwise be looked up as a row id that does not exist. Trying the id first
// and FALLING BACK to the key on a miss is what makes those rows addressable
// by the key an agent actually holds.
func byIDOrKey(ref string, byID func(int64) error, byKey func(string) error) error {
	if id, convErr := strconv.ParseInt(ref, 10, 64); convErr == nil {
		if err := byID(id); err == nil {
			return nil
		}
	}
	return byKey(ref)
}

// handleJobRejected rules a lead out, or puts it back in play:
//
//	POST /api/jobs/12/rejected {"reason": "US-only, no work authorization"}
//	POST /api/jobs/12/rejected {"rejected": false}
//
// A reason is required whenever rejected is true, so the board never shows a
// red card whose cause nobody recorded.
func (s *Server) handleJobRejected(w http.ResponseWriter, r *http.Request) {
	req := struct {
		Rejected *bool  `json:"rejected"`
		Reason   string `json:"reason"`
		// reject_reason is accepted too, so the field name matches the one the
		// ingest payload and the JSON output already use.
		RejectReason string `json:"reject_reason"`
	}{}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req)
	rejected := req.Rejected == nil || *req.Rejected
	reason := clip(strings.TrimSpace(cmp.Or(req.Reason, req.RejectReason)), 500)
	if rejected && reason == "" {
		writeJSON(w, http.StatusBadRequest,
			map[string]string{"error": store.ErrRejectReasonRequired.Error()})
		return
	}

	ref := r.PathValue("id")
	err := byIDOrKey(ref,
		func(id int64) error { return s.Store.SetJobRejected(id, rejected, reason, time.Now()) },
		func(key string) error { return s.Store.SetJobRejectedByKey(key, rejected, reason, time.Now()) })
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such job"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": ref, "rejected": rejected, "reject_reason": reason})
}

// handleJobApproved is the token-guarded review gate:
//
//	POST /api/jobs/12/approved {"notes": "ask about the on-call rota"}
//	POST /api/jobs/12/approved {"approved": false}
//
// Notes are optional, unlike a reject reason. An approval usually has nothing
// to add, and requiring a sentence for those would only produce empty ones —
// whereas a rejection with no stated cause is the note you actually miss later.
func (s *Server) handleJobApproved(w http.ResponseWriter, r *http.Request) {
	req := struct {
		Approved *bool  `json:"approved"`
		Notes    string `json:"notes"`
		// review_notes is accepted too, matching the ingest payload and the JSON
		// output, the same courtesy reject_reason gets above.
		ReviewNotes string `json:"review_notes"`
	}{}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req)
	approved := req.Approved == nil || *req.Approved
	notes := clip(strings.TrimSpace(cmp.Or(req.Notes, req.ReviewNotes)), 4000)

	ref := r.PathValue("id")
	err := byIDOrKey(ref,
		func(id int64) error { return s.Store.SetJobApproved(id, approved, notes, time.Now()) },
		func(key string) error { return s.Store.SetJobApprovedByKey(key, approved, notes, time.Now()) })
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such job"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": ref, "approved": approved, "review_notes": notes})
}

// handleJobPrep attaches the review artifact the prep stage produces — the job
// summary, what the application form asks, the questions needing a decision and
// the links to the tailored CV.
//
// The body is stored verbatim as a JSON string. This endpoint validates that it
// parses and nothing more: the shape belongs to the prep stage and the page
// that renders it, and a store that knew the shape would need a migration every
// time prep learned to record one more thing.
func (s *Server) handleJobPrep(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body too large"})
		return
	}
	prep := strings.TrimSpace(string(raw))
	// Empty clears the artifact, which is how a bad CV or a stale summary gets
	// sent back for a redo.
	if prep != "" && !json.Valid([]byte(prep)) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "prep must be json"})
		return
	}
	ref := r.PathValue("id")
	id, err := s.Store.ResolveJobRef(ref)
	if err == nil {
		err = s.Store.SetJobPrep(id, clip(prep, 20000))
	}
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such job"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":   id,
		"prep": prep != "",
		"link": fmt.Sprintf("%s/jobs/%d?k=%s", s.BasePath, id, s.LinkKey(jobScope(id))),
	})
}

// handleJobApproveToggle is the board's own approve button, guarded by the
// lead's link key rather than the API token so the gate can be cleared from the
// phone that is reading the review.
//
// The note travels with the decision. The textarea and this button are one
// form, so approving with something typed in the box saves both in a single
// post — the alternative was a reviewer writing a caveat, clicking approve, and
// finding the caveat had never been saved. A request with no `notes` field at
// all (the JSON callers) leaves the existing note alone.
func (s *Server) handleJobApproveToggle(w http.ResponseWriter, r *http.Request) {
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
	notes := job.ReviewNotes
	if err := r.ParseForm(); err == nil {
		if _, sent := r.PostForm["notes"]; sent {
			notes = clip(strings.TrimSpace(r.PostFormValue("notes")), 4000)
		}
	}
	on := !job.Approved()
	if err := s.Store.SetJobApproved(id, on, notes, time.Now()); err != nil {
		http.NotFound(w, r)
		return
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "on": on})
		return
	}
	http.Redirect(w, r, s.safeBack(r.URL.Query().Get("back")), http.StatusSeeOther)
}

// handleJobNotes saves the review notes textarea. Empty is legal — clearing the
// notes is a real edit — so this is a plain form post that redirects back to
// the lead, exactly like the leads board's conversation field.
//
// Notes are deliberately writable whether or not the lead is approved: the
// common shape is to write the note first and then decide.
func (s *Server) handleJobNotes(w http.ResponseWriter, r *http.Request) {
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
	notes := clip(strings.TrimSpace(r.PostFormValue("notes")), 4000)
	// The approval flag is untouched: this endpoint edits the note, not the
	// decision, and the note is usually written before the decision is made.
	if err := s.Store.SetJobReviewNotes(id, notes); err != nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("%s/jobs/%d?k=%s&noted=1", s.BasePath, id, s.LinkKey(jobScope(id))), http.StatusSeeOther)
}

// handleJobAppliedToggle is the board's own button. It is guarded by the
// lead's link key rather than the API token, so the flag can be flipped from
// the phone that is reading the board, and it bounces back to the page the
// click came from.
func (s *Server) handleJobAppliedToggle(w http.ResponseWriter, r *http.Request) {
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
	on := !job.Applied()
	if err := s.Store.SetJobApplied(id, on, time.Now()); err != nil {
		http.NotFound(w, r)
		return
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "on": on})
		return
	}
	http.Redirect(w, r, s.safeBack(r.URL.Query().Get("back")), http.StatusSeeOther)
}

// safeBack keeps the post-toggle redirect inside this app. An attacker who
// could pick the target would have a redirector on a trusted host, so anything
// that is not one of our own absolute paths falls back to the board.
func (s *Server) safeBack(back string) string {
	if strings.HasPrefix(back, s.BasePath+"/jobs") && !strings.HasPrefix(back, "//") {
		return back
	}
	return fmt.Sprintf("%s/jobs?k=%s", s.BasePath, s.LinkKey(jobsScope()))
}

// handleJobDuplicate links one lead to another as the same underlying role:
//
//	POST /api/jobs/69/duplicate {"of": "8"}     -> 69 mirrors 8
//	POST /api/jobs/69/duplicate {"of": ""}      -> 69 is canonical again
//
// Both ids accept a row id or a dedupe_key.
func (s *Server) handleJobDuplicate(w http.ResponseWriter, r *http.Request) {
	req := struct {
		Of string `json:"of"`
	}{}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req)

	dupID, err := s.Store.ResolveJobRef(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such job"})
		return
	}
	if strings.TrimSpace(req.Of) == "" {
		if err := s.Store.UnlinkDuplicate(dupID); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such job"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": dupID, "duplicate_of": 0})
		return
	}
	canonID, err := s.Store.ResolveJobRef(strings.TrimSpace(req.Of))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such canonical job"})
		return
	}
	if err := s.Store.LinkDuplicate(dupID, canonID); err != nil {
		status := http.StatusNotFound
		if errors.Is(err, store.ErrDuplicateCycle) {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	job, _ := s.Store.JobByID(dupID)
	writeJSON(w, http.StatusOK, map[string]any{"id": dupID, "duplicate_of": job.DuplicateOf})
}

func (s *Server) handleJobsPurge(w http.ResponseWriter, _ *http.Request) {
	n, err := s.Store.PurgeJobs()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"deleted": n})
}

// handleJobDelete removes one lead for good; {id} may be a dedupe_key, and a
// key takes every profile's copy of the post with it. Rejection is the softer
// call for a lead that was judged and lost — this is for rows that should not
// be on the board at all.
func (s *Server) handleJobDelete(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("id")
	err := byIDOrKey(ref,
		func(id int64) error { return s.Store.DeleteJob(id) },
		func(key string) error { return s.Store.DeleteJobByKey(key) })
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such lead"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": ref, "deleted": true})
}

func (s *Server) handleJobsJSON(w http.ResponseWriter, r *http.Request) {
	f := jobFilterFromQuery(r.URL.Query())
	jobs, err := s.Store.ListJobs(f, time.Now())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if jobs == nil {
		jobs = []store.Job{}
	}
	writeJSON(w, http.StatusOK, jobs)
}

// jobFilterFromQuery reads the shared filter params: ?profile= ?net= ?type=
// ?new=1 ?since=24h|7d|30d ?sort=new. The HTML page and the JSON endpoint parse
// the same way, so a page URL can be turned into an API call by swapping the
// path.
func jobFilterFromQuery(q url.Values) store.JobFilter {
	tri := func(v string) string {
		if v != "1" && v != "0" {
			return ""
		}
		return v
	}
	return store.JobFilter{
		// An absent profile means every board, which is what makes an
		// un-parameterised /jobs URL still show everything.
		Profile:      clip(strings.ToLower(strings.TrimSpace(q.Get("profile"))), 40),
		Network:      clip(q.Get("net"), 20),
		JobType:      clip(q.Get("type"), 20),
		UnviewedOnly: q.Get("new") == "1",
		Applied:      tri(q.Get("applied")),
		Rejected:     tri(q.Get("rejected")),
		// `?approved=1&applied=0&rejected=0` is the apply stage's work queue and
		// `?prepped=0` is the prep stage's, which keeps both of them pointed at a
		// board URL rather than a list somebody had to assemble.
		Approved:   tri(q.Get("approved")),
		Prepped:    tri(q.Get("prepped")),
		Duplicates: tri(q.Get("dups")),
		Since:      parseSince(q.Get("since")),
		// The dedupe pass reads the whole board history in one call, so an
		// explicit limit may exceed the page-sized default (the store still
		// caps it).
		Limit:      atoiOr(q.Get("limit"), 0),
		SortNewest: q.Get("sort") == "new",
	}
}

func atoiOr(v string, def int) int {
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n
	}
	return def
}

// parseSince turns "24h", "7d", "30d" into a window; anything else means all
// time. Days are the natural unit here — job hunts think in days, not hours.
func parseSince(v string) time.Duration {
	if v == "" || v == "all" {
		return 0
	}
	if n, err := strconv.Atoi(strings.TrimSuffix(v, "d")); err == nil && strings.HasSuffix(v, "d") && n > 0 {
		return time.Duration(n) * 24 * time.Hour
	}
	if n, err := strconv.Atoi(strings.TrimSuffix(v, "h")); err == nil && strings.HasSuffix(v, "h") && n > 0 {
		return time.Duration(n) * time.Hour
	}
	return 0
}

// handleJobsBoard renders the board: tiles, chart and breakdowns on top, the
// lead list under them, everything narrowed by the same filter.
func (s *Server) handleJobsBoard(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r, jobsScope()) {
		return
	}
	f := jobFilterFromQuery(r.URL.Query())
	now := time.Now()
	jobs, err := s.Store.ListJobs(f, now)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// A failed dashboard must not take the list down with it.
	stats, err := s.Store.JobStats(f, now)
	if err != nil {
		log.Printf("[jobs] stats: %v", err)
	}
	data := jobsBoardData{
		Base: s.BasePath,
		// The proxy strips BasePath before the mux sees the path, so the
		// browser-visible URL has to be put back for the return trip.
		Rows:  s.jobRows(jobs, now, s.BasePath+r.URL.RequestURI(), f.Profile == ""),
		Dash:  s.buildJobsDash(stats, f),
		Chips: s.jobChips(f, s.jobProfileChoices()),
		Sub:   jobsSubtitle(len(jobs), stats),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := jobsBoardTmpl.Execute(w, data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

// jobsSubtitle carries the pulse numbers the tiles used to: how fresh the
// board is and how far through it the reader is. The tiles keep the six
// decision states; everything descriptive lives up here.
func jobsSubtitle(listed int, st *store.JobStats) string {
	if st == nil {
		return fmt.Sprintf("%d lead(s)", listed)
	}
	lead := fmt.Sprintf("%d lead(s)", st.Total)
	if st.Total > int64(listed) {
		lead = fmt.Sprintf("top %d of %d lead(s)", listed, st.Total)
	}
	parts := []string{lead, fmt.Sprintf("%d last 24h", st.Last24h)}
	if st.Total > 0 {
		parts = append(parts, fmt.Sprintf("%d%% worked through", (st.Total-st.Unviewed)*100/st.Total))
	}
	if st.Duplicates > 0 {
		parts = append(parts, fmt.Sprintf("%d repeat(s)", st.Duplicates))
	}
	return strings.Join(parts, " · ")
}

// handleJobShow is one lead's detail page. Opening it counts as looking at the
// lead, so it stamps viewed_at — that is the page's whole meaning.
func (s *Server) handleJobShow(w http.ResponseWriter, r *http.Request) {
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
	wasViewed := job.Viewed()
	if err := s.Store.MarkJobViewed(id, time.Now()); err != nil {
		log.Printf("[jobs] mark viewed %d: %v", id, err)
	}
	markLabel := "mark as applied"
	if job.Applied() {
		markLabel = "undo applied"
	}
	// The voice link only exists when transcription is configured, and its
	// absence is what tells the page to keep recordings a preview.
	voiceLink := ""
	if s.STT != nil {
		voiceLink = fmt.Sprintf("%s/jobs/%d/voice?k=%s", s.BasePath, job.ID, s.LinkKey(jobScope(job.ID)))
	}
	// A malformed blob costs the reader the prep boxes, not the page. Prep is
	// written by an agent against a schema this struct does not enforce, so the
	// one thing that must not happen is the whole lead becoming unreadable
	// because a summary came back as a number.
	var prep *prepView
	if job.Prep != "" {
		var p prepView
		if err := json.Unmarshal([]byte(job.Prep), &p); err != nil {
			log.Printf("[jobs] prep %d: %v", id, err)
		} else {
			prep = &p
		}
	}
	data := jobShowData{
		Base:      s.BasePath,
		J:         job,
		Net:       s.netTag(job.Network),
		Viewed:    wasViewed,
		ViewedAt:  job.ViewedAt,
		Applied:   job.Applied(),
		AppliedAt: job.AppliedAt,
		Age:       jobAge(job, time.Now()),
		Emails:    splitList(job.Emails, ";"),
		Signals:   splitList(job.Signals, ","),
		GoLink:    fmt.Sprintf("%s/jobs/%d/go?k=%s", s.BasePath, job.ID, s.LinkKey(jobScope(job.ID))),
		MarkLink:  s.jobAppliedLink(job.ID, s.BasePath+r.URL.RequestURI()),
		MarkLabel: markLabel,
		BackTo:    fmt.Sprintf("%s/jobs?k=%s", s.BasePath, s.LinkKey(jobsScope())),
		DupOf:     job.DuplicateOf,

		Prep:        prep,
		PrepRaw:     job.Prep,
		Approved:    job.Approved(),
		ApprovedAt:  job.ApprovedAt,
		ReviewNotes: job.ReviewNotes,
		Noted:       r.URL.Query().Get("noted") == "1",
		ApproveLink: fmt.Sprintf("%s/jobs/%d/approved?k=%s&back=%s",
			s.BasePath, job.ID, s.LinkKey(jobScope(job.ID)),
			url.QueryEscape(s.BasePath+r.URL.RequestURI())),
		VoiceLink: voiceLink,
		NotesLink: fmt.Sprintf("%s/jobs/%d/notes?k=%s",
			s.BasePath, job.ID, s.LinkKey(jobScope(job.ID))),
		// The resolved posting is shown as its own link rather than replacing
		// "open the post": the two disagreeing is the single most useful thing
		// on this page, so both stay clickable.
		PostingURL:    job.PostingURL,
		HasPostingURL: job.PostingURL != "" && job.PostingURL != job.URL,
	}
	if job.IsDuplicate() {
		data.DupLink = fmt.Sprintf("%s/jobs/%d?k=%s",
			s.BasePath, job.DuplicateOf, s.LinkKey(jobScope(job.DuplicateOf)))
	}
	// A failed lookup here must not take the page down; the repeats block just
	// does not render.
	if reps, err := s.Store.DuplicatesOf(job.ID); err != nil {
		log.Printf("[jobs] duplicates of %d: %v", job.ID, err)
	} else {
		for _, rep := range reps {
			label := rep.Title
			if label == "" {
				label = rep.Body
			}
			data.Repeats = append(data.Repeats, dupRefView{
				ID:    rep.ID,
				Label: rep.Network + " · " + clip(label, 70),
				Link:  fmt.Sprintf("%s/jobs/%d?k=%s", s.BasePath, rep.ID, s.LinkKey(jobScope(rep.ID))),
			})
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := jobShowTmpl.Execute(w, data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

// handleJobGo marks the lead viewed and bounces to the original post. This is
// the "open" link on the board: one click, and the lead stops counting as new.
func (s *Server) handleJobGo(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !s.guard(w, r, jobScope(id)) {
		return
	}
	job, err := s.Store.JobByID(id)
	if err != nil || job.URL == "" {
		http.NotFound(w, r)
		return
	}
	if err := s.Store.MarkJobViewed(id, time.Now()); err != nil {
		log.Printf("[jobs] mark viewed %d: %v", id, err)
	}
	http.Redirect(w, r, job.URL, http.StatusFound)
}

// ---- view models ----

func splitList(s, sep string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, sep)
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// jobAge prefers when the post was written; a sweep that could not date the
// post falls back to when the lead was found, labelled honestly. Days take
// over past 48 hours — "29d" reads, "694h 46m" does not.
func jobAge(j *store.Job, now time.Time) string {
	verb, since := "posted", j.PostedAt
	if since.IsZero() {
		verb, since = "found", j.CreatedAt
	}
	d := now.Sub(since)
	if d >= 48*time.Hour {
		return fmt.Sprintf("%s %dd ago", verb, int(d.Hours()/24))
	}
	return verb + " " + humanDuration(d) + " ago"
}

type jobRowView struct {
	Score string
	// Profile is set only on a board showing more than one seeker. Repeating
	// "polina" down a list already filtered to Polina is noise; on a mixed
	// board it is the first thing you need per row.
	Profile  string
	Net      template.HTML
	Type     string
	Author   string
	Where    string
	Snippet  string
	Age      string
	Viewed   bool
	Applied  bool
	Rejected bool
	// Approved marks a lead that has cleared review and is waiting to go out,
	// and Prepped one whose review artifact is ready to read. Both matter most
	// in their gap: prepped-but-not-approved is the reading queue, and
	// approved-but-not-applied is the sending queue.
	Approved bool
	Prepped  bool
	// DupOf is the canonical lead's id when this row mirrors it, and DupLink
	// opens that lead, so a repeat is one click from the row it repeats.
	DupOf     int64
	DupLink   string
	DupCount  int64
	HasDraft  bool
	ShowLink  string
	GoLink    string
	MarkLink  string
	MarkLabel string
}

// jobRows renders the list. back is the board URL the rows are being shown
// under, so the applied button returns to the same filtered view instead of
// dumping the reader back at an unfiltered board.
//
// showProfile is false when the board is already narrowed to one seeker, so
// the tag appears exactly when it carries information.
func (s *Server) jobRows(jobs []store.Job, now time.Time, back string, showProfile bool) []jobRowView {
	// How many mirrors each canonical row has, counted within this page. A
	// canonical row says "+2 repeats" so the reader knows the list is shorter
	// than the number of postings behind it.
	dupCounts := map[int64]int64{}
	for _, j := range jobs {
		if j.IsDuplicate() {
			dupCounts[j.DuplicateOf]++
		}
	}
	out := make([]jobRowView, len(jobs))
	for i, j := range jobs {
		dupLink := ""
		if j.IsDuplicate() {
			dupLink = fmt.Sprintf("%s/jobs/%d?k=%s",
				s.BasePath, j.DuplicateOf, s.LinkKey(jobScope(j.DuplicateOf)))
		}
		// A rejected lead shows why it was ruled out instead of its pitch. The
		// description is what you read when deciding; once decided, the only
		// thing worth surfacing is the verdict.
		snippet := j.Title
		if snippet == "" {
			snippet = j.Body
		}
		if j.Rejected() {
			snippet = j.RejectReason
		}
		if len(snippet) > 160 {
			snippet = snippet[:160] + "…"
		}
		// Only reddit has a second locus (the subreddit); the network label
		// already names the platform for everything else.
		where := ""
		if j.Subreddit != "" {
			where = "r/" + j.Subreddit
		}
		markLabel := "mark applied"
		if j.Applied() {
			markLabel = "undo"
		}
		profile := ""
		if showProfile {
			profile = j.Profile
		}
		out[i] = jobRowView{
			Score:     strconv.FormatFloat(j.Score, 'f', -1, 64),
			Profile:   profile,
			Net:       s.netTag(j.Network),
			Type:      j.JobType,
			Author:    j.Author,
			Where:     where,
			Snippet:   snippet,
			Age:       jobAge(&j, now),
			Viewed:    j.Viewed(),
			Applied:   j.Applied(),
			Rejected:  j.Rejected(),
			Approved:  j.Approved(),
			Prepped:   j.Prepped(),
			DupOf:     j.DuplicateOf,
			DupLink:   dupLink,
			DupCount:  dupCounts[j.ID],
			HasDraft:  j.Draft != "",
			ShowLink:  fmt.Sprintf("%s/jobs/%d?k=%s", s.BasePath, j.ID, s.LinkKey(jobScope(j.ID))),
			GoLink:    fmt.Sprintf("%s/jobs/%d/go?k=%s", s.BasePath, j.ID, s.LinkKey(jobScope(j.ID))),
			MarkLink:  s.jobAppliedLink(j.ID, back),
			MarkLabel: markLabel,
		}
	}
	return out
}

// chipView is one filter pill. On marks the active choice; Link applies it
// (or, for the active one, removes it).
type chipView struct {
	Label string
	On    bool
	Link  string
}

type chipGroup struct {
	Name  string
	Chips []chipView
}

// jobAppliedLink is the POST target for the applied toggle: the lead's own
// scoped key, plus where to land afterwards.
func (s *Server) jobAppliedLink(id int64, back string) string {
	q := url.Values{"k": {s.LinkKey(jobScope(id))}}
	if back != "" {
		q.Set("back", back)
	}
	return fmt.Sprintf("%s/jobs/%d/applied?%s", s.BasePath, id, q.Encode())
}

// jobsLink renders one complete filter as a board URL. Every chip builds a
// copy of the active filter with exactly one field changed and hands it here,
// so no chip can drop a filter another chip set — and adding a dimension to
// JobFilter cannot silently miss a chip, the way a positional argument list
// could.
//
// since is passed alongside the filter because JobFilter stores a duration and
// the URL wants the label the reader clicked ("7d", not "168h0m0s").
func (s *Server) jobsLink(f store.JobFilter, since string) string {
	q := url.Values{"k": {s.LinkKey(jobsScope())}}
	for k, v := range map[string]string{
		"profile": f.Profile, "net": f.Network, "type": f.JobType, "since": since,
		"applied": f.Applied, "rejected": f.Rejected, "dups": f.Duplicates,
		"approved": f.Approved, "prepped": f.Prepped} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if f.UnviewedOnly {
		q.Set("new", "1")
	}
	if f.SortNewest {
		q.Set("sort", "new")
	}
	return s.BasePath + "/jobs?" + q.Encode()
}

// jobChips builds the filter rows. Every link is the board URL with exactly
// one parameter changed, so filters compose: profile + network + type + window
// + new all narrow together.
//
// profiles is every seeker the board should offer, so a profile whose first
// sweep has not landed yet still gets a chip to click.
func (s *Server) jobChips(f store.JobFilter, profiles []string) []chipGroup {
	sinceStr := ""
	switch f.Since {
	case 24 * time.Hour:
		sinceStr = "24h"
	case 7 * 24 * time.Hour:
		sinceStr = "7d"
	case 30 * 24 * time.Hour:
		sinceStr = "30d"
	case 0:
	default:
		sinceStr = fmt.Sprintf("%dh", int(f.Since.Hours()))
	}
	// with copies the active filter and changes one field, which is the whole
	// composition rule in one place: a chip states its own dimension and
	// inherits every other.
	with := func(mutate func(*store.JobFilter, *string)) string {
		next, since := f, sinceStr
		mutate(&next, &since)
		return s.jobsLink(next, since)
	}
	// Clicking the active chip clears its filter — the rule every group follows.
	toggle := func(cur, want string) string { //nolint:unparam // cur is the field's current value
		if cur == want {
			return ""
		}
		return want
	}
	groups := []chipGroup{}
	// Profile leads the rows: it is the outermost lens, and reading a board
	// without knowing whose leads it holds is the one confusion worth designing
	// out. "everyone" is a real choice rather than an implicit default.
	if len(profiles) > 1 {
		pg := chipGroup{Name: "profile", Chips: []chipView{
			{Label: "everyone", On: f.Profile == "",
				Link: with(func(n *store.JobFilter, _ *string) { n.Profile = "" })},
		}}
		for _, p := range profiles {
			want := toggle(f.Profile, p)
			pg.Chips = append(pg.Chips, chipView{Label: p, On: f.Profile == p,
				Link: with(func(n *store.JobFilter, _ *string) { n.Profile = want })})
		}
		groups = append(groups, pg)
	}
	groups = append(groups,
		chipGroup{Name: "show", Chips: []chipView{
			{Label: "not viewed", On: f.UnviewedOnly,
				Link: with(func(n *store.JobFilter, _ *string) { n.UnviewedOnly = !f.UnviewedOnly })},
			{Label: "newest first", On: f.SortNewest,
				Link: with(func(n *store.JobFilter, _ *string) { n.SortNewest = !f.SortNewest })},
		}},
		chipGroup{Name: "applied", Chips: []chipView{
			{Label: "to apply", On: f.Applied == "0",
				Link: with(func(n *store.JobFilter, _ *string) { n.Applied = toggle(f.Applied, "0") })},
			{Label: "applied", On: f.Applied == "1",
				Link: with(func(n *store.JobFilter, _ *string) { n.Applied = toggle(f.Applied, "1") })},
		}},
		chipGroup{Name: "status", Chips: []chipView{
			{Label: "still live", On: f.Rejected == "0",
				Link: with(func(n *store.JobFilter, _ *string) { n.Rejected = toggle(f.Rejected, "0") })},
			{Label: "rejected", On: f.Rejected == "1",
				Link: with(func(n *store.JobFilter, _ *string) { n.Rejected = toggle(f.Rejected, "1") })},
		}},
		// The two pipeline queues as one click each: what still needs reading,
		// and what has been cleared and is waiting to go out.
		chipGroup{Name: "pipeline", Chips: []chipView{
			{Label: "to prep", On: f.Prepped == "0",
				Link: with(func(n *store.JobFilter, _ *string) { n.Prepped = toggle(f.Prepped, "0") })},
			{Label: "to review", On: f.Prepped == "1",
				Link: with(func(n *store.JobFilter, _ *string) { n.Prepped = toggle(f.Prepped, "1") })},
			{Label: "approved", On: f.Approved == "1",
				Link: with(func(n *store.JobFilter, _ *string) { n.Approved = toggle(f.Approved, "1") })},
		}},
		chipGroup{Name: "roles", Chips: []chipView{
			{Label: "one per role", On: f.Duplicates == "0",
				Link: with(func(n *store.JobFilter, _ *string) { n.Duplicates = toggle(f.Duplicates, "0") })},
			{Label: "repeats", On: f.Duplicates == "1",
				Link: with(func(n *store.JobFilter, _ *string) { n.Duplicates = toggle(f.Duplicates, "1") })},
		}},
	)
	nets := chipGroup{Name: "network"}
	for _, n := range []string{"reddit", "x", "linkedin"} {
		want := toggle(f.Network, n)
		nets.Chips = append(nets.Chips, chipView{Label: n, On: f.Network == n,
			Link: with(func(nf *store.JobFilter, _ *string) { nf.Network = want })})
	}
	groups = append(groups, nets)
	types := chipGroup{Name: "type"}
	for _, tp := range []string{"job", "contract", "cofounder", "other"} {
		want := toggle(f.JobType, tp)
		types.Chips = append(types.Chips, chipView{Label: tp, On: f.JobType == tp,
			Link: with(func(n *store.JobFilter, _ *string) { n.JobType = want })})
	}
	groups = append(groups, types)
	windows := chipGroup{Name: "found"}
	for _, wdw := range []string{"24h", "7d", "30d"} {
		want := toggle(sinceStr, wdw)
		windows.Chips = append(windows.Chips, chipView{Label: wdw, On: sinceStr == wdw,
			Link: with(func(_ *store.JobFilter, since *string) { *since = want })})
	}
	groups = append(groups, windows)
	return groups
}

// jobProfileChoices is every seeker the board offers, whether or not their
// sweep has produced leads yet: the profiles already in the data, unioned with
// the ones trackhub knows by name. A brand-new profile therefore needs no code
// change to appear — it shows up as soon as it has one lead — while a profile
// set up before its first sweep still has a chip to click.
func (s *Server) jobProfileChoices() []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	stats, err := s.Store.JobProfiles()
	if err != nil {
		log.Printf("[jobs] profiles: %v", err)
	}
	for _, p := range stats {
		add(p.Label)
	}
	for _, p := range knownJobProfiles {
		add(p)
	}
	return out
}

// knownJobProfiles are the seekers whose chips exist before their first lead
// does — the PROFILES env var, or just the default profile. Ordering after
// the data-derived list keeps the busiest board first.
var knownJobProfiles = []string{store.DefaultJobProfile}

// SetKnownProfiles replaces the pre-seeded chip list (the PROFILES env var).
func SetKnownProfiles(names []string) {
	var out []string
	for _, n := range names {
		if n = store.NormalizeProfile(n); n != "" {
			out = append(out, n)
		}
	}
	if len(out) > 0 {
		knownJobProfiles = out
	}
}

type jobsDashView struct {
	Total    int64
	Unviewed int64
	Applied  int64
	Rejected int64
	// The two pipeline queues: waiting to be read, and waiting to be sent.
	ToReview   int64
	ToSend     int64
	Duplicates int64
	// Roles is Total minus Duplicates: distinct openings rather than postings.
	Roles     int64
	Last24h   int64
	ViewedPct int64

	Days     []dayBarView
	FirstDay string
	LastDay  string
	Cadence  string

	Profiles   []statRowView
	Networks   []statRowView
	Types      []statRowView
	Subreddits []statRowView
}

func (s *Server) buildJobsDash(st *store.JobStats, _ store.JobFilter) *jobsDashView {
	if st == nil {
		return nil
	}
	var peak int64
	for _, d := range st.Days {
		peak = max(peak, d.Visits)
	}
	weekly := st.BucketDays > 1
	days := make([]dayBarView, len(st.Days))
	for i, d := range st.Days {
		pct := 0
		if d.Visits > 0 && peak > 0 {
			pct = max(int(d.Visits*100/peak), 4)
		}
		when := d.Day
		if weekly {
			when = "week of " + d.Day
		}
		days[i] = dayBarView{Day: d.Day, Title: fmt.Sprintf("%s · %d lead(s)", when, d.Visits), Pct: pct}
	}
	cadence := "leads per day"
	if weekly {
		cadence = "leads per week"
	}
	viewedPct := int64(0)
	if st.Total > 0 {
		viewedPct = (st.Total - st.Unviewed) * 100 / st.Total
	}
	dash := &jobsDashView{
		Total: st.Total, Unviewed: st.Unviewed, Applied: st.Applied, Rejected: st.Rejected,
		ToReview: st.ToReview, ToSend: st.ToSend,
		Duplicates: st.Duplicates, Roles: st.Total - st.Duplicates,
		Last24h: st.Last24h, ViewedPct: viewedPct,
		Days: days, Cadence: cadence,
		Profiles:   plainRows(st.Profiles),
		Networks:   plainRows(st.Networks),
		Types:      plainRows(st.Types),
		Subreddits: plainRows(st.Subreddits),
	}
	if len(days) > 0 {
		first, last := days[0].Day, days[len(days)-1].Day
		if first[:4] == last[:4] {
			first, last = first[5:], last[5:]
		}
		dash.FirstDay, dash.LastDay = first, last
	}
	return dash
}

type jobsBoardData struct {
	Base  string
	Sub   string
	Chips []chipGroup
	Dash  *jobsDashView
	Rows  []jobRowView
}

type jobShowData struct {
	Base      string
	J         *store.Job
	Net       template.HTML
	Viewed    bool
	ViewedAt  time.Time
	Applied   bool
	AppliedAt time.Time
	Age       string
	Emails    []string
	Signals   []string
	GoLink    string
	MarkLink  string
	MarkLabel string
	BackTo    string
	// The review gate: Prep is the artifact to read, ApproveLink flips the
	// decision and NotesLink saves what was said while making it.
	Prep        *prepView
	PrepRaw     string
	Approved    bool
	ApprovedAt  time.Time
	ReviewNotes string
	// Noted is the one-shot "the note you just saved is really saved" flag,
	// carried by the save redirect's ?noted=1 and gone on the next reload.
	Noted       bool
	ApproveLink string
	NotesLink   string
	// VoiceLink is the transcription endpoint for this lead, empty when no
	// speech-to-text is configured; the composer reads it off a data attr.
	VoiceLink     string
	PostingURL    string
	HasPostingURL bool
	// DupOf/DupLink point at the canonical lead when this one is a repeat;
	// Repeats lists the mirrors when this one IS the canonical.
	DupOf   int64
	DupLink string
	Repeats []dupRefView
}

// prepView is the review artifact as the page renders it.
//
// The store keeps prep as opaque JSON precisely so it can grow without a
// migration, and this struct is the other half of that bargain: it names the
// fields the page knows how to show, and anything else in the blob is ignored
// rather than being an error. A prep stage that starts recording one more thing
// does not break the page it is written for.
type prepView struct {
	// Summary is the job in a paragraph — the thing that decides whether the
	// rest is worth reading.
	Summary string `json:"summary"`
	// Fit is why this lead was scored the way it was, restated for a reader who
	// is deciding rather than judging.
	Fit string `json:"fit"`
	// FormQuestions is what the application actually asks, so the answer to
	// "is this worth twenty minutes" is on the page instead of behind a click.
	// Anything the form asks that is not a formal field (name, phone, email)
	// carries the draft answer the apply stage intends to submit, because a
	// cover letter written in the reviewer's absence is the part most worth
	// reading before the gate opens.
	FormQuestions []formQuestion `json:"form_questions"`
	// OpenQuestions are the ones needing a decision that the persona's standing
	// answers do not cover. These are the reason the gate is human.
	OpenQuestions []string `json:"open_questions"`
	// Blockers are the reasons applying might be impossible — a signup wall, a
	// CAPTCHA, paid messaging. Surfacing them here keeps the reader from
	// approving something nobody can act on.
	Blockers []string `json:"blockers"`
	CVURL    string   `json:"cv_url"`
	CVPath   string   `json:"cv_path"`
	CVLabel  string   `json:"cv_label"`
}

// formQuestion is one question on the application form, with the draft answer
// the apply stage means to give. Formal fields — name, phone, email — come
// straight off the persona and carry no Answer: there is nothing to review in
// a phone number, and a page of them would bury the cover letter.
//
// It decodes from either a bare string or an object, because prep artifacts
// written before drafts existed are plain []string and re-running prep for
// every old lead to read one is not worth it.
type formQuestion struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
	// Source names where a formal answer came from ("persona: phone"), so the
	// reader can tell a standing answer from one written for this posting.
	Source string `json:"source"`
}

func (f *formQuestion) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		f.Question = s
		return nil
	}
	type raw formQuestion // no recursion back into this method
	var r raw
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	*f = formQuestion(r)
	return nil
}

// dupRefView is one linked repeat on a lead's detail page.
type dupRefView struct {
	ID    int64
	Label string
	Link  string
}

var jobsBoardTmpl = template.Must(template.New("jobs").Funcs(pageFuncs).Parse(jobsBoardHTML))
var jobShowTmpl = template.Must(template.New("job").Funcs(pageFuncs).Parse(jobShowHTML))

const jobsBoardHTML = `<!doctype html>
` + pageHead + `
<title>jobs</title>
<style>` + sharedCSS + chartCSS + `
  h1 { margin:0; font-size:28px; line-height:36px; font-weight:700; }
  .top-bar { margin-bottom:16px; }
  .top-bar .seg { margin-top:4px; }
  /* A label column plus a chip column: the grid keeps every chip row starting
     at the same x, which is what the old per-row flex could not do. Labels use
     the same section-header type as every other header on the page. */
  .filters { display:grid; grid-template-columns:max-content 1fr; gap:10px 14px; align-items:center; margin-bottom:16px; }
  .filters .g { color:var(--hint); font-size:13px; font-weight:500; letter-spacing:.05em; text-transform:uppercase; }
  .filters .cs { display:flex; flex-wrap:wrap; gap:6px; min-width:0; }
  .chip { background:var(--tertiary); color:var(--hint); border-radius:999px; padding:5px 13px; font-size:13px; font-weight:600; line-height:18px; }
  .chip:hover { text-decoration:none; color:var(--text); }
  .chip.on, .chip.on:hover { background:var(--accent); color:#FFFFFF; }
  /* Grid rather than flex: with six tiles a flex row leaves the last one
     stretched across a line of its own. */
  .tiles { display:grid; grid-template-columns:repeat(auto-fit,minmax(96px,1fr)); gap:8px; margin-top:10px; }
  .tile { padding:10px 12px; }
  .tile .n { font-size:20px; font-weight:700; }
  .tile .k { color:var(--hint); font-size:13px; }
  .grid { display:grid; grid-template-columns:repeat(auto-fit,minmax(150px,1fr)); gap:8px; margin-top:12px; }
  .bd { padding:12px 14px; }
  .bd h3 { color:var(--hint); font-size:13px; font-weight:500; letter-spacing:.05em; text-transform:uppercase; margin:0 0 6px; }
  .bd .r { display:flex; justify-content:space-between; gap:10px; padding:2px 0; }
  .bd .r .l { overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .bd .r .c { color:var(--hint); white-space:nowrap; }
  /* One section holds the whole list, Telegram style: rows divided, not boxed. */
  .list { margin-top:16px; overflow:hidden; }
  .row { display:flex; align-items:center; gap:2px; padding:0 8px 0 16px; border-bottom:1px solid var(--divider); }
  .row:last-child { border-bottom:0; }
  .row.seen, .row.dup { opacity:.6; }
  /* Applied is the one state worth seeing from across the list, so it tints
     the whole row and cancels the read-it-already fade. */
  .row.applied { opacity:1; background:color-mix(in srgb, var(--ok) 7%, transparent); box-shadow:inset 3px 0 var(--ok); }
  /* Rejected outranks applied: once a lead is ruled out, that is the fact you
     need from across the list, even if an application already went out. */
  .row.rejected { opacity:1; background:color-mix(in srgb, var(--bad) 7%, transparent); box-shadow:inset 3px 0 var(--bad); }
  .row.rejected .snippet { color:color-mix(in srgb, var(--bad) 60%, var(--text)); }
  .row .main { flex:1 1 auto; min-width:0; padding:12px 0; color:inherit; display:block; }
  .row .main:hover { text-decoration:none; }
  .row .top { display:flex; justify-content:space-between; flex-wrap:wrap; gap:2px 12px; }
  .row .who { font-weight:600; word-break:break-word; }
  .row.dup .score { background:var(--tertiary); color:var(--hint); }
  .meta { color:var(--hint); font-size:13px; margin-top:2px; }
  .meta .tag { margin:0 2px 0 0; }
  .snippet { word-break:break-word; margin-top:2px; }
  .row .out { flex:0 0 auto; font-size:13px; font-weight:600; white-space:nowrap; padding:8px 6px; }
  .row .mark { flex:0 0 auto; display:flex; margin:0; }
  .row .mark button { border:0; cursor:pointer; width:34px; height:34px; border-radius:50%; font-size:15px;
                      background:var(--tertiary); color:var(--hint); }
  .row .mark button:hover { color:var(--ok); }
  .row.applied .mark button { background:color-mix(in srgb, var(--ok) 14%, transparent); color:var(--ok); }
  .row.applied .mark button:hover { color:var(--bad); }
  /* Both chips are always in the DOM; the row's class picks which one shows.
     That lets the async toggle restyle the whole row by flipping one class. */
  .row:not(.applied) .tag.applied { display:none; }
  .row.applied .tag.new { display:none; }
  .empty { padding:14px 16px; margin:0; }
  @media (max-width: 480px) {
    main { padding:12px 10px 48px; }
    .row { padding-left:12px; }
  }
</style>
<main>
  <div class="top-bar">
    <div><h1>Jobs</h1><div class="sub">{{.Sub}}</div></div>
    ` + themeSeg + `
  </div>
  <div class="filters">
    {{range .Chips}}<span class="g">{{.Name}}</span><div class="cs">{{range .Chips}}<a class="chip{{if .On}} on{{end}}" href="{{.Link}}">{{.Label}}</a>{{end}}</div>{{end}}
  </div>
  {{/* Six tiles, one per decision state — an even grid instead of a ragged
       6+3 wrap. The pulse numbers (last 24h, worked through, repeats) moved
       into the subtitle. */}}
  {{with .Dash}}
  <div class="tiles">
    <div class="card tile"><div class="n">{{.Total}}</div><div class="k">leads</div></div>
    <div class="card tile"><div class="n">{{.Unviewed}}</div><div class="k">not viewed</div></div>
    <div class="card tile"><div class="n">{{.ToReview}}</div><div class="k">to review</div></div>
    <div class="card tile"><div class="n">{{.ToSend}}</div><div class="k">to send</div></div>
    <div class="card tile"><div class="n">{{.Applied}}</div><div class="k">applied</div></div>
    <div class="card tile"><div class="n">{{.Rejected}}</div><div class="k">rejected</div></div>
  </div>
  {{end}}
  {{/* The list is the page's point, so it comes right after the tiles; the
       chart and breakdowns are the appendix. */}}
  <div class="card list">
  {{range .Rows}}
  <div class="row{{if .Viewed}} seen{{end}}{{if .Applied}} applied{{end}}{{if .Rejected}} rejected{{end}}{{if .DupOf}} dup{{end}}">
    <a class="main" href="{{.ShowLink}}">
      <div class="top">
        <span class="who"><span class="score">{{.Score}}</span>{{.Author}}{{if .DupCount}} <span class="tag dup">+{{.DupCount}} repeat{{if gt .DupCount 1}}s{{end}}</span>{{end}}</span>
        <!-- One state chip, in order of what matters: ruled out beats applied,
             and applied beats new. Stacking all three reads as noise. -->
        <span>{{if .Rejected}}<span class="tag rejected">rejected</span>{{else}}<span class="tag applied">applied</span>{{if not .Viewed}}<span class="tag new">new</span>{{end}}{{end}}{{if .Approved}} <span class="tag approved">approved</span>{{else if .Prepped}} <span class="tag prepped">prepped</span>{{end}}{{if .HasDraft}} <span class="tag draft">draft</span>{{end}}</span>
      </div>
      <div class="meta">{{if .Profile}}<span class="tag who-tag">{{.Profile}}</span> {{end}}{{.Net}}{{if .Where}} · {{.Where}}{{end}}{{if .Type}} · {{.Type}}{{end}} · {{.Age}}</div>
      <div class="snippet">{{.Snippet}}</div>
    </a>
    {{if .DupOf}}<a class="out" href="{{.DupLink}}" title="same role as lead {{.DupOf}}">repeat of #{{.DupOf}}</a>{{end}}
    <a class="out" href="{{.GoLink}}" target="_blank" rel="noopener">open ↗</a>
    <form class="mark" method="post" action="{{.MarkLink}}" data-state="applied" data-mark="✓" data-undo="↩"><button type="submit" title="{{.MarkLabel}}">{{if .Applied}}↩{{else}}✓{{end}}</button></form>
  </div>
  {{else}}
  <p class="sub empty">no leads under this filter</p>
  {{end}}
  </div>
  {{with .Dash}}
  <div class="card panel">
    <div class="chart">
      {{range .Days}}<div class="col" title="{{.Title}}"><span class="bar" style="height:{{.Pct}}%"></span></div>{{end}}
    </div>
    <div class="axis"><span>{{.FirstDay}}</span><span>{{.Cadence}}</span><span>{{.LastDay}}</span></div>
  </div>
  ` + chartTip + `
  <div class="grid">
    {{if gt (len .Profiles) 1}}<div class="card bd"><h3>profiles</h3>{{range .Profiles}}<div class="r"><span class="l">{{.Label}}</span><span class="c">{{.Count}}</span></div>{{end}}</div>{{end}}
    {{if .Networks}}<div class="card bd"><h3>networks</h3>{{range .Networks}}<div class="r"><span class="l">{{.Label}}</span><span class="c">{{.Count}}</span></div>{{end}}</div>{{end}}
    {{if .Types}}<div class="card bd"><h3>types</h3>{{range .Types}}<div class="r"><span class="l">{{.Label}}</span><span class="c">{{.Count}}</span></div>{{end}}</div>{{end}}
    {{if .Subreddits}}<div class="card bd"><h3>subreddits</h3>{{range .Subreddits}}<div class="r"><span class="l">{{.Label}}</span><span class="c">{{.Count}}</span></div>{{end}}</div>{{end}}
  </div>
  {{end}}
</main>
` + themeJS + markJS

// gateJS drives the approve composer: one pill, one round button. Idle with an
// empty note the button is a mic and starts the Telegram-style recording UI;
// with text (or a recording running) it is a send button that approves. The
// "approve without a note" action under the pill is the empty-note approve —
// without it the only mouse path from an empty pill leads into a recording.
//
// When the form carries data-voice (transcription configured), the recording
// is real: a MediaRecorder captures the microphone and sending posts the audio
// to the voice endpoint, which transcribes it and saves the transcript as the
// note. Without data-voice — or when the mic never yielded audio — sending a
// recording falls back to a plain approve with the note left untouched, which
// is the old preview behaviour.
//
// Without JavaScript the form still posts notes + approve and redirects, the
// same fallback every other toggle on these pages has.
const gateJS = `<script>
(function () {
  var f = document.getElementById('gate')
  if (!f) return
  var main = document.querySelector('main')
  var input = f.querySelector('input[name=notes]')
  var send = f.querySelector('.send')
  var wave = f.querySelector('.rec-wave')
  var timeEl = f.querySelector('.rec-time')
  var errEl = f.querySelector('.gc-err')
  var voice = f.dataset.voice
  var state = 'idle', ms = 0, levels = [], last = 0.4, tick = null
  var stream = null, actx = null, an = null, mr = null, chunks = []

  function face () { f.classList.toggle('txt', state !== 'idle' || !!input.value.trim()) }
  input.addEventListener('input', face); face()

  function fmt (ms) {
    var t = Math.floor(ms / 100), d = t % 10, s = Math.floor(t / 10) % 60, m = Math.floor(t / 600)
    return m + ':' + String(s).padStart(2, '0') + ',' + d
  }
  function level () {
    if (an) {
      var d = new Uint8Array(an.frequencyBinCount)
      an.getByteFrequencyData(d)
      var s = 0; for (var i = 0; i < d.length; i++) s += d[i]
      return Math.min(1, (s / d.length) / 80)
    }
    last = Math.max(0.08, Math.min(1, last + (Math.random() - 0.5) * 0.35))
    return last
  }
  function draw () {
    var r = wave.getBoundingClientRect(), dpr = window.devicePixelRatio || 1
    var w = Math.max(1, Math.round(r.width * dpr)), h = Math.max(1, Math.round(r.height * dpr))
    if (wave.width !== w) { wave.width = w; wave.height = h }
    var ctx = wave.getContext('2d')
    ctx.clearRect(0, 0, w, h)
    var bw = 2 * dpr, gap = 2 * dpr, n = Math.floor(w / (bw + gap))
    var ls = levels.slice(-n)
    ctx.fillStyle = (getComputedStyle(document.documentElement).getPropertyValue('--accent') || '#2990FF').trim()
    ls.forEach(function (v, i) {
      var bh = Math.max(3 * dpr, v * h), x = i * (bw + gap), y = (h - bh) / 2
      ctx.beginPath()
      if (ctx.roundRect) ctx.roundRect(x, y, bw, bh, bw / 2); else ctx.rect(x, y, bw, bh)
      ctx.fill()
    })
  }
  function start () {
    state = 'rec'; ms = 0; levels = []; chunks = []
    errEl.textContent = ''
    f.classList.add('rec'); f.classList.remove('paused'); face()
    if (navigator.mediaDevices && navigator.mediaDevices.getUserMedia) {
      navigator.mediaDevices.getUserMedia({ audio: true }).then(function (st) {
        if (state === 'idle') { st.getTracks().forEach(function (t) { t.stop() }); return }
        stream = st
        try {
          actx = new (window.AudioContext || window.webkitAudioContext)()
          var src = actx.createMediaStreamSource(st)
          an = actx.createAnalyser(); an.fftSize = 256
          src.connect(an)
        } catch (e) {}
        try {
          mr = new MediaRecorder(st)
          mr.ondataavailable = function (e) { if (e.data && e.data.size) chunks.push(e.data) }
          mr.start(250)
          if (state === 'paused') { try { mr.pause() } catch (e) {} }
        } catch (e) { mr = null }
      }).catch(function () { errEl.textContent = "mic unavailable — recording won't be saved" })
    } else {
      errEl.textContent = "mic unavailable — recording won't be saved"
    }
    tick = setInterval(function () {
      if (state !== 'rec') return
      levels.push(level()); ms += 100
      timeEl.textContent = fmt(ms)
      draw()
    }, 100)
  }
  function stopAll () {
    clearInterval(tick); tick = null
    if (mr && mr.state !== 'inactive') { try { mr.stop() } catch (e) {} }
    if (stream) stream.getTracks().forEach(function (t) { t.stop() })
    if (actx) { try { actx.close() } catch (e) {} }
    stream = actx = an = mr = null
  }
  function discard () {
    stopAll(); state = 'idle'; chunks = []
    f.classList.remove('rec', 'paused')
    timeEl.textContent = '0:00,0'
    face()
  }
  f.querySelector('.gc-trash').addEventListener('click', discard)
  f.querySelector('.rec-pause').addEventListener('click', function () {
    if (state === 'rec') {
      state = 'paused'; f.classList.add('paused')
      if (mr && mr.state === 'recording') { try { mr.pause() } catch (e) {} }
    } else if (state === 'paused') {
      state = 'rec'; f.classList.remove('paused')
      if (mr && mr.state === 'paused') { try { mr.resume() } catch (e) {} }
    }
  })
  function approved (d, note) {
    if (note !== null) {
      input.value = note; face()
      document.querySelector('.gate-note').textContent = note
    }
    document.querySelector('.gd-when').textContent = 'approved just now'
    main.classList.toggle('approved', d.on !== false)
  }
  function doSend (asVoice) {
    var note = input.value.trim()
    errEl.textContent = ''
    send.disabled = true
    fetch(f.action, {
      method: 'POST',
      headers: { 'Accept': 'application/json' },
      body: asVoice ? null : new URLSearchParams({ notes: note })
    })
      .then(function (r) { if (!r.ok) throw new Error(r.status); return r.json() })
      .then(function (d) {
        if (asVoice) discard()
        approved(d, asVoice ? null : note)
      })
      .catch(function () { errEl.textContent = 'approving failed — try again' })
      .then(function () { send.disabled = false })
  }
  function uploadVoice (blob) {
    if (!blob.size) { doSend(true); return }
    errEl.textContent = ''
    send.disabled = true
    fetch(voice, {
      method: 'POST',
      headers: { 'Accept': 'application/json', 'Content-Type': blob.type || 'audio/webm' },
      body: blob
    })
      .then(function (r) { if (!r.ok) throw new Error(r.status); return r.json() })
      .then(function (d) { approved(d, d.notes || '') })
      .catch(function () { errEl.textContent = 'transcription failed — type the note instead' })
      .then(function () { send.disabled = false })
  }
  function sendRecording () {
    var rec = mr
    if (voice && rec && rec.state !== 'inactive') {
      // Let the recorder flush its last chunk before the blob is built.
      rec.onstop = function () {
        var blob = new Blob(chunks, { type: rec.mimeType || 'audio/webm' })
        discard()
        uploadVoice(blob)
      }
      try { rec.stop() } catch (e) { discard(); doSend(true) }
    } else {
      discard()
      doSend(true)
    }
  }
  input.addEventListener('keydown', function (e) {
    if (e.key === 'Enter') { e.preventDefault(); doSend(false) }
  })
  // The mic owns the round button when the pill is empty, so this is the
  // click that approves with no note at all.
  var skip = f.querySelector('.gc-skip')
  if (skip) skip.addEventListener('click', function () { doSend(false) })
  f.addEventListener('submit', function (e) {
    e.preventDefault()
    if (state === 'idle' && !input.value.trim()) { start(); return }
    if (state !== 'idle') sendRecording()
    else doSend(false)
  })
})()
</script>`

const jobShowHTML = `<!doctype html>
` + pageHead + `
<title>{{.J.Author}} · jobs</title>
<style>` + sharedCSS + `
  .back { font-size:17px; font-weight:600; }
  .head { padding:16px; }
  h1 { display:flex; align-items:center; gap:8px; flex-wrap:wrap; font-size:20px; line-height:24px; font-weight:700; margin:0; }
  h1 .score { font-size:15px; border-radius:8px; padding:2px 9px; margin:0; }
  /* One status chip on its own row under the title, picked by the classes on
     <main> so the async toggles (applied, approved) move it without a reload.
     Priority: ruled out beats applied, applied beats approved, approved beats
     needing review. Its own row because a chip pushed right of a long title
     wraps into a lone right-aligned island. */
  .st-row { margin-top:8px; }
  .st { display:none; font-size:13px; font-weight:600; border-radius:999px; padding:4px 12px; white-space:nowrap; }
  main.rejected .st-rejected { display:inline-block; background:color-mix(in srgb, var(--bad) 14%, transparent); color:var(--bad); }
  main:not(.rejected).applied .st-applied { display:inline-block; background:color-mix(in srgb, var(--ok) 14%, transparent); color:var(--ok); }
  main:not(.rejected):not(.applied).approved .st-approved { display:inline-block; background:color-mix(in srgb, var(--ok) 14%, transparent); color:var(--ok); }
  main:not(.rejected):not(.applied):not(.approved).prepped .st-review { display:inline-block; background:color-mix(in srgb, var(--accent) 15%, transparent); color:var(--accent); }
  main:not(.rejected):not(.applied):not(.approved):not(.prepped) .st-none { display:inline-block; background:var(--tertiary); color:var(--hint); }
  .meta { color:var(--hint); font-size:13px; line-height:20px; margin-top:8px; }
  .meta .tag { margin:0 2px 0 0; }
  .actions { display:flex; gap:8px; flex-wrap:wrap; margin-top:14px; }
  .btn { display:inline-block; border:0; cursor:pointer; font-size:15px; font-weight:600; border-radius:10px; padding:9px 14px;
         background:color-mix(in srgb, var(--accent) 13%, transparent); color:var(--accent); }
  a.btn:hover { text-decoration:none; opacity:.85; }
  .btn.ok { background:color-mix(in srgb, var(--ok) 13%, transparent); color:var(--ok); }
  .mark { display:inline; margin:0; }
  /* Section label outside, content card under it — the Telegram list shape. */
  .sec { color:var(--hint); font-size:13px; font-weight:500; text-transform:uppercase; letter-spacing:.05em; margin:24px 16px 8px; }
  .box { padding:14px 16px; }
  .body { white-space:pre-wrap; word-break:break-word; }
  .fit { color:var(--hint); margin-top:8px; }
  .box.bad { background:color-mix(in srgb, var(--bad) 8%, var(--section)); }
  .qs { margin:0; padding-left:18px; }
  .qs li { margin-bottom:5px; }
  .qs li:last-child { margin-bottom:0; }
  .blockers { color:color-mix(in srgb, var(--bad) 60%, var(--text)); }
  /* One question per row: the draft answer is the thing being reviewed, so the
     question above it is just the label. */
  .fq { padding:12px 0; border-bottom:1px solid var(--divider); }
  .fq:first-child { padding-top:0; }
  .fq:last-child { border-bottom:0; padding-bottom:0; }
  .fq-q { color:var(--hint); font-size:13px; line-height:18px; }
  .fq-a { white-space:pre-wrap; word-break:break-word; margin-top:2px; }
  .fq-formal { color:var(--hint); font-size:13px; font-style:italic; margin-top:2px; }
  .sigs { display:flex; gap:8px; flex-wrap:wrap; }
  .sig { background:var(--tertiary); color:var(--hint); font-size:13px; border-radius:999px; padding:3px 12px; }
  a.mail { word-break:break-all; }
  .copy { border:0; border-radius:10px; background:var(--tertiary); color:var(--hint); padding:9px 14px; font:inherit; font-size:15px; font-weight:600; cursor:pointer; margin-top:8px; }
  .copy:hover { color:var(--text); }
  /* The approve composer. main.approved swaps it for the approved card, so the
     async approve/withdraw round-trip is one class flip. */
  main.approved .gate-compose { display:none; }
  main:not(.approved) .gate-done { display:none; }
  .gate-compose { display:block; padding:12px; }
  .gc-hint { color:var(--hint); font-size:13px; line-height:20px; padding:0 6px 10px; }
  .gc-err { color:var(--bad); font-size:13px; line-height:20px; padding:0 6px 10px; }
  .gc-err:empty { display:none; }
  .gc-row { display:flex; align-items:center; gap:8px; }
  .gc-trash { display:none; background:none; border:0; color:var(--bad); line-height:0; cursor:pointer; padding:8px; flex:0 0 auto; }
  .gc-trash svg { width:20px; height:20px; fill:currentColor; }
  .gc-skip { display:block; background:none; border:0; color:var(--accent); font-size:13px; font-weight:600; cursor:pointer; padding:10px 6px 0; }
  .gc-skip:hover { text-decoration:underline; }
  .gate-compose.txt .gc-skip, .gate-compose.rec .gc-skip { display:none; }
  .pill { flex:1; display:flex; align-items:center; gap:10px; background:var(--input); border-radius:999px; height:46px; padding:0 14px; min-width:0; }
  .pill input { flex:1; background:none; border:0; outline:none; color:var(--text); font:inherit; font-size:15px; min-width:0; }
  .pill input::placeholder { color:var(--hint); }
  .rec-dot { display:none; width:9px; height:9px; border-radius:50%; background:var(--bad); animation:jhPulse 1.2s ease-in-out infinite; flex:0 0 auto; }
  .rec-wave { display:none; flex:1; height:26px; min-width:0; }
  .rec-time { display:none; font-size:15px; font-variant-numeric:tabular-nums; flex:0 0 auto; }
  .rec-pause { display:none; background:none; border:0; cursor:pointer; padding:4px; align-items:center; gap:3px; flex:0 0 auto; }
  .rec-pause i { width:3px; height:13px; border-radius:2px; background:var(--hint); display:block; }
  .rec-pause .tri { display:none; width:0; height:0; border-left:11px solid var(--hint); border-top:7px solid transparent; border-bottom:7px solid transparent; }
  .gate-compose.rec .pill input { display:none; }
  .gate-compose.rec .rec-dot { display:block; }
  .gate-compose.rec .rec-wave { display:block; }
  .gate-compose.rec .rec-time { display:inline; }
  .gate-compose.rec .rec-pause { display:flex; }
  .gate-compose.rec .gc-trash { display:block; }
  .gate-compose.paused .rec-dot { animation:none; opacity:.4; }
  .gate-compose.paused .rec-pause i { display:none; }
  .gate-compose.paused .rec-pause .tri { display:block; }
  .send { width:46px; height:46px; border-radius:50%; background:var(--accent); border:0; cursor:pointer; display:flex; align-items:center; justify-content:center; flex:0 0 auto; }
  .send[disabled] { opacity:.6; cursor:default; }
  .send svg { width:24px; height:24px; display:block; fill:#FFFFFF; }
  .send .fly { display:none; width:22px; height:22px; margin-left:2px; }
  .gate-compose.txt .send .mic, .gate-compose.rec .send .mic { display:none; }
  .gate-compose.txt .send .fly, .gate-compose.rec .send .fly { display:block; }
  @keyframes jhPulse { 0%,100% { opacity:1 } 50% { opacity:.25 } }
  @media (prefers-reduced-motion: reduce) { .rec-dot { animation:none } }
  .gate-done { padding:14px 16px; }
  .ok-pill { display:inline-block; background:color-mix(in srgb, var(--ok) 14%, transparent); color:var(--ok); font-size:15px; font-weight:600; border-radius:999px; padding:5px 14px; }
  .gd-when { color:var(--hint); font-size:13px; margin-left:8px; }
  .gate-note { background:var(--input); border-radius:12px; padding:10px 14px; margin-top:12px; white-space:pre-wrap; word-break:break-word; }
  .gate-note:empty { display:none; }
  .withdraw { background:none; border:0; color:var(--bad); font-size:13px; cursor:pointer; padding:0; margin-top:12px; }
  @media (max-width: 480px) { main { padding:12px 10px 48px; } }
</style>
<main class="{{if .Applied}}applied {{end}}{{if .Approved}}approved {{end}}{{if .J.Rejected}}rejected {{end}}{{if .Prep}}prepped{{end}}">
  <div class="top-bar">
    <a class="back" href="{{.BackTo}}">‹ Jobs</a>
    ` + themeSeg + `
  </div>
  <div class="card head">
    <h1><span class="score">{{printf "%.1f" .J.Score}}</span><span>{{.J.Author}}</span></h1>
    <div class="st-row"><span class="st st-rejected">rejected</span><span class="st st-applied">✓ applied</span><span class="st st-approved">approved · ready to apply</span><span class="st st-review">prepped · needs your review</span><span class="st st-none">not reviewed yet</span></div>
    <div class="meta">{{with .J.Profile}}<span class="tag who-tag">{{.}}</span> {{end}}{{.Net}}{{with .J.Subreddit}} · r/{{.}}{{end}}{{with .J.JobType}} · {{.}}{{end}} · {{.Age}}{{if .Viewed}} · viewed {{when .ViewedAt}}{{end}}{{if .Applied}} · applied {{when .AppliedAt}}{{end}}{{if .Approved}} · approved {{when .ApprovedAt}}{{end}}</div>
    <div class="actions">
      <a class="btn" href="{{.GoLink}}" target="_blank" rel="noopener">open the post ↗</a>
      {{if .HasPostingURL}}<a class="btn" href="{{.PostingURL}}" target="_blank" rel="noopener">the real posting ↗</a>{{end}}
      {{if .Prep}}{{if .Prep.CVURL}}<a class="btn" href="{{.Prep.CVURL}}" target="_blank" rel="noopener">{{if .Prep.CVLabel}}{{.Prep.CVLabel}}{{else}}tailored CV{{end}} ↗</a>{{end}}{{end}}
      <form class="mark{{if .Applied}} on{{end}}" method="post" action="{{.MarkLink}}" data-state="applied" data-mark="mark as applied" data-undo="undo applied"><button type="submit" class="btn ok">{{.MarkLabel}}</button></form>
    </div>
  </div>
  {{if .J.Rejected}}<div class="sec">ruled out {{when .J.RejectedAt}}</div><div class="card box bad"><div class="body">{{.J.RejectReason}}</div></div>{{end}}
  {{if .DupOf}}<div class="sec">repeat</div><div class="card box"><div class="body">Same role as <a href="{{.DupLink}}">lead #{{.DupOf}}</a>. Work that one.</div></div>{{end}}
  {{if .Repeats}}<div class="sec">also posted as</div><div class="card box">{{range .Repeats}}<div><a href="{{.Link}}">#{{.ID}} · {{.Label}}</a></div>{{end}}</div>{{end}}
  {{/* The review gate: one composer, one send. Sending approves; the note in
       the pill rides along (empty is fine). The mic face starts the voice-note
       preview — the recording UI is real, the audio is not kept yet. */}}
  <div class="sec">approve for applying</div>
  <form id="gate" class="mark gate-toggle gate-compose card{{if .ReviewNotes}} txt{{end}}" method="post" action="{{.ApproveLink}}"{{if .VoiceLink}} data-voice="{{.VoiceLink}}"{{end}}>
    <div class="gc-hint">Send approves this lead for the AI apply stage — note optional, typed{{if .VoiceLink}} or recorded{{else}} (voice is a preview, not saved yet){{end}}.</div>
    <div class="gc-err"></div>
    <div class="gc-row">
      <button type="button" class="gc-trash" title="delete recording"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M9 3h6l1 2h4v2H4V5h4l1-2zm-3.5 6h13l-.95 11.4a1.8 1.8 0 0 1-1.8 1.6H8.25a1.8 1.8 0 0 1-1.8-1.6L5.5 9zm4.4 2.2.35 8h1.5l-.35-8h-1.5zm4.7 0-.35 8h1.5l.35-8h-1.5z"></path></svg></button>
      <div class="pill">
        <input name="notes" value="{{.ReviewNotes}}" placeholder="note for the AI apply stage (optional)" autocomplete="off">
        <span class="rec-dot"></span>
        <canvas class="rec-wave"></canvas>
        <span class="rec-time">0:00,0</span>
        <button type="button" class="rec-pause" title="pause / resume"><i></i><i></i><span class="tri"></span></button>
      </div>
      <button type="submit" class="send" title="approve for applying">
        <svg class="mic" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 15.5a3.5 3.5 0 0 0 3.5-3.5V6a3.5 3.5 0 1 0-7 0v6a3.5 3.5 0 0 0 3.5 3.5z"></path><path d="M18.5 12a.9.9 0 0 0-1.8 0 4.7 4.7 0 0 1-9.4 0 .9.9 0 0 0-1.8 0 6.5 6.5 0 0 0 5.6 6.44V20.5a.9.9 0 0 0 1.8 0v-2.06A6.5 6.5 0 0 0 18.5 12z"></path></svg>
        <svg class="fly" viewBox="0 0 24 24" aria-hidden="true"><path d="M2.5 21.5l19-9.5-19-9.5-.01 7.5L15.5 12 2.49 14z"></path></svg>
      </button>
    </div>
    <button type="button" class="gc-skip">approve without a note</button>
  </form>
  <div class="card gate-done">
    <div><span class="ok-pill">✓ approved · ready to apply</span><span class="gd-when">{{if .Approved}}approved {{when .ApprovedAt}}{{end}}</span></div>
    <div class="gate-note">{{.ReviewNotes}}</div>
    <form class="mark" method="post" action="{{.ApproveLink}}" data-state="approved" data-mark="withdraw approval" data-undo="withdraw approval"><button type="submit" class="withdraw">withdraw approval</button></form>
  </div>
  {{with .Prep}}
  <div class="sec">summary</div>
  <div class="card box"><div class="body">{{.Summary}}</div>{{with .Fit}}<div class="body fit">{{.}}</div>{{end}}</div>
  {{if .Blockers}}<div class="sec">blockers</div><div class="card box"><ul class="qs blockers">{{range .Blockers}}<li>{{.}}</li>{{end}}</ul></div>{{end}}
  {{if .OpenQuestions}}<div class="sec">needs your decision</div><div class="card box"><ul class="qs">{{range .OpenQuestions}}<li>{{.}}</li>{{end}}</ul></div>{{end}}
  {{if .FormQuestions}}<div class="sec">what the form asks</div><div class="card box">
    {{range .FormQuestions}}<div class="fq">
      <div class="fq-q">{{.Question}}</div>
      {{if .Answer}}<div class="fq-a">{{.Answer}}</div>
      {{else}}<div class="fq-formal">{{if .Source}}{{.Source}}{{else}}filled from the persona{{end}}</div>{{end}}
    </div>{{end}}
  </div>{{end}}
  {{end}}
  {{if .PrepRaw}}{{if not .Prep}}<div class="sec">prep unreadable</div><div class="card box bad"><div class="body">The prep artifact on this lead is not valid JSON, so it could not be rendered. Re-run the prep stage for it.</div></div>{{end}}{{end}}
  {{if .J.Title}}<div class="sec">title</div><div class="card box"><div class="body">{{.J.Title}}</div></div>{{end}}
  {{if .J.ScoreReason}}<div class="sec">why this score</div><div class="card box"><div class="body">{{.J.ScoreReason}}</div></div>{{end}}
  {{if .J.Body}}<div class="sec">post</div><div class="card box"><div class="body">{{.J.Body}}</div></div>{{end}}
  {{if .J.PostingText}}<div class="sec">the real posting</div><div class="card box"><div class="body">{{.J.PostingText}}</div></div>{{end}}
  {{if .Emails}}<div class="sec">contacts</div><div class="card box">{{range .Emails}}<div><a class="mail" href="mailto:{{.}}">{{.}}</a></div>{{end}}</div>{{end}}
  {{if .J.Draft}}
  <div class="sec">message draft</div>
  <div class="card box"><div class="body" id="draft">{{.J.Draft}}</div></div>
  <button class="copy" onclick="navigator.clipboard.writeText(document.getElementById('draft').innerText).then(()=>{this.textContent='copied ✓'})">copy draft</button>
  {{end}}
  {{if .Signals}}<div class="sec">signals</div><div class="card box sigs">{{range .Signals}}<span class="sig">{{.}}</span>{{end}}</div>{{end}}
</main>
` + themeJS + markJS + gateJS
