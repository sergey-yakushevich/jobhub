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

func jobsSubtitle(listed int, st *store.JobStats) string {
	if st == nil {
		return fmt.Sprintf("%d lead(s)", listed)
	}
	if st.Total > int64(listed) {
		return fmt.Sprintf("top %d of %d lead(s) · %d not viewed", listed, st.Total, st.Unviewed)
	}
	return fmt.Sprintf("%d lead(s) · %d not viewed", listed, st.Unviewed)
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
	Noted         bool
	ApproveLink   string
	NotesLink     string
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
  h1 { margin:0 0 4px; }
  .sub { margin-bottom:14px; }
  .chips { display:flex; flex-wrap:wrap; gap:6px 8px; align-items:center; margin-bottom:8px; }
  .chips .g { color:#5a5a64; font-size:11px; letter-spacing:.08em; text-transform:uppercase; margin-right:2px; }
  .chip { display:inline-block; border:1px solid #2a2a33; border-radius:999px; padding:1px 10px; color:#8b8b96; }
  .chip:hover { border-color:#3a3a46; text-decoration:none; }
  .chip.on { border-color:#5cd58c; color:#5cd58c; }
  /* Grid rather than flex: with six tiles a flex row leaves the last one
     stretched across a line of its own. */
  .tiles { display:grid; grid-template-columns:repeat(auto-fit,minmax(104px,1fr)); gap:8px; margin:16px 0 18px; }
  .tile { border:1px solid #2a2a33; border-radius:8px; padding:9px 12px; }
  .tile .n { font-size:18px; font-weight:600; }
  .tile .k { color:#8b8b96; font-size:12px; }
  .grid { display:grid; grid-template-columns:repeat(auto-fit,minmax(190px,1fr)); gap:10px; margin-bottom:26px; }
  .card { border:1px solid #2a2a33; border-radius:8px; padding:10px 12px; min-width:0; }
  .card h3 { font-size:11px; font-weight:600; color:#8b8b96; letter-spacing:.08em; text-transform:uppercase; margin:0 0 7px; }
  .card .r { display:flex; justify-content:space-between; gap:10px; padding:2px 0; }
  .card .r .l { overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .card .r .c { color:#8b8b96; white-space:nowrap; }
  .row { display:flex; border:1px solid #2a2a33; border-radius:8px; margin-bottom:12px; }
  .row.seen { opacity:.55; }
  .row:hover { border-color:#3a3a46; }
  /* Applied is the one state worth seeing from across the list, so it colours
     the whole card and cancels the read-it-already fade. */
  .row.applied { opacity:1; border-color:#2f5d43; background:#111e18; border-left:3px solid #5cd58c; }
  .row.applied:hover { border-color:#3c7355; border-left-color:#5cd58c; }
  .row.applied .out, .row.applied .mark { border-left-color:#22392d; }
  /* Rejected outranks applied: once a lead is ruled out, that is the fact you
     need from across the list, even if an application already went out. */
  .row.rejected { opacity:1; border-color:#5d2f2f; background:#1e1213; border-left:3px solid #d96b6b; }
  .row.rejected:hover { border-color:#7a3d3d; border-left-color:#d96b6b; }
  .row.rejected .out, .row.rejected .mark { border-left-color:#3a2224; }
  .row.rejected .snippet { color:#e0a8a8; }
  .row.rejected .mark button { color:#8a6a6a; }
  .row .mark { flex:0 0 auto; display:flex; margin:0; border-left:1px solid #1b1b22; }
  .row .mark button { background:none; border:0; color:#6f6f7b; font:inherit; cursor:pointer; padding:0 14px; }
  .row .mark button:hover { color:#5cd58c; }
  .row.applied .mark button { color:#5cd58c; }
  .row.applied .mark button:hover { color:#d98b8b; }
  .row .main { flex:1 1 auto; min-width:0; padding:12px 14px; color:inherit; display:block; }
  .row .main:hover { text-decoration:none; }
  .row .out { flex:0 0 auto; display:flex; align-items:center; padding:0 16px; color:#7db5ff; border-left:1px solid #1b1b22; white-space:nowrap; }
  .row .top { display:flex; justify-content:space-between; flex-wrap:wrap; gap:2px 12px; }
  .row .who { font-weight:600; word-break:break-word; }
  .row .meta { color:#8b8b96; }
  .score { color:#101014; background:#d9a441; border-radius:4px; padding:0 7px; font-weight:600; font-size:12px; vertical-align:middle; margin-right:6px; }
  .tag.new { color:#101014; background:#5cd58c; }
  .tag.draft { color:#101014; background:#7db5ff; }
  .tag.applied { color:#101014; background:#5cd58c; }
  .tag.rejected { color:#101014; background:#d96b6b; }
  /* Approved means the gate is cleared but nothing has gone out, so it reads as
     pending rather than done: amber, the colour the score badge already uses.
     Prepped is quieter still — an artifact waiting to be read is not a state
     anyone acts on from the list. */
  .tag.approved { color:#101014; background:#d9a441; }
  .tag.prepped { color:#8b8b96; background:none; border:1px solid #3a3a46; }
  /* A repeat is bookkeeping, not news: outlined and quiet, so it never
     competes with new/applied/rejected for attention. */
  .tag.dup { color:#8b8b96; background:none; border:1px solid #3a3a46; }
  /* Whose lead this is. Violet keeps it distinct from every status colour —
     it answers a different question than new/applied/rejected do. */
  .tag.who-tag { color:#101014; background:#b48ce8; }
  .row.dup { opacity:.6; }
  .row.dup .score { background:#4a4a52; color:#c9c9d2; }
  /* Both chips are always in the DOM; the row's class picks which one shows.
     That lets the async toggle restyle the whole row by flipping one class. */
  .row:not(.applied) .tag.applied { display:none; }
  .row.applied .tag.new { display:none; }
  .snippet { color:#b9b9c4; word-break:break-word; margin-top:2px; }
  @media (max-width: 480px) {
    body { padding:12px; font-size:13px; }
    .row .main { padding:10px; }
    .row .out { padding:0 12px; }
    .row .mark button { padding:0 10px; }
    .tiles { grid-template-columns:repeat(auto-fit,minmax(88px,1fr)); }
    .tile { padding:8px 10px; }
    .tile .n { font-size:16px; }
  }
</style>
<main>
  <h1>💼 jobs</h1>
  <div class="sub">{{.Sub}}</div>
  {{range .Chips}}
  <div class="chips"><span class="g">{{.Name}}</span>
    {{range .Chips}}<a class="chip{{if .On}} on{{end}}" href="{{.Link}}">{{if .On}}✓ {{end}}{{.Label}}</a>{{end}}
  </div>
  {{end}}
  {{with .Dash}}
  <div class="tiles">
    <div class="tile"><div class="n">{{.Total}}</div><div class="k">leads</div></div>
    <div class="tile"><div class="n">{{.Roles}}</div><div class="k">roles{{if .Duplicates}} · {{.Duplicates}} repeat{{if gt .Duplicates 1}}s{{end}}{{end}}</div></div>
    <div class="tile"><div class="n">{{.Unviewed}}</div><div class="k">not viewed</div></div>
    <div class="tile"><div class="n">{{.ToReview}}</div><div class="k">to review</div></div>
    <div class="tile"><div class="n">{{.ToSend}}</div><div class="k">to send</div></div>
    <div class="tile"><div class="n">{{.Applied}}</div><div class="k">applied</div></div>
    <div class="tile"><div class="n">{{.Rejected}}</div><div class="k">rejected</div></div>
    <div class="tile"><div class="n">{{.Last24h}}</div><div class="k">last 24h</div></div>
    <div class="tile"><div class="n">{{.ViewedPct}}%</div><div class="k">worked through</div></div>
  </div>
  <div class="chart">
    {{range .Days}}<div class="col" title="{{.Title}}"><span class="bar" style="height:{{.Pct}}%"></span></div>{{end}}
  </div>
  <div class="axis"><span>{{.FirstDay}}</span><span>{{.Cadence}}</span><span>{{.LastDay}}</span></div>
  ` + chartTip + `
  <div class="grid">
    {{if gt (len .Profiles) 1}}<div class="card"><h3>profiles</h3>{{range .Profiles}}<div class="r"><span class="l">{{.Label}}</span><span class="c">{{.Count}}</span></div>{{end}}</div>{{end}}
    {{if .Networks}}<div class="card"><h3>networks</h3>{{range .Networks}}<div class="r"><span class="l">{{.Label}}</span><span class="c">{{.Count}}</span></div>{{end}}</div>{{end}}
    {{if .Types}}<div class="card"><h3>types</h3>{{range .Types}}<div class="r"><span class="l">{{.Label}}</span><span class="c">{{.Count}}</span></div>{{end}}</div>{{end}}
    {{if .Subreddits}}<div class="card"><h3>subreddits</h3>{{range .Subreddits}}<div class="r"><span class="l">{{.Label}}</span><span class="c">{{.Count}}</span></div>{{end}}</div>{{end}}
  </div>
  {{end}}
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
  <p class="sub">no leads under this filter</p>
  {{end}}
</main>
` + markJS

const jobShowHTML = `<!doctype html>
` + pageHead + `
<title>{{.J.Author}} · jobs</title>
<style>` + sharedCSS + `
  .back { display:inline-block; margin-bottom:16px; color:#8b8b96; }
  h1 { font-size:16px; }
  .meta { color:#8b8b96; margin-bottom:16px; }
  .score { color:#101014; background:#d9a441; border-radius:4px; padding:0 7px; font-weight:600; font-size:13px; margin-right:6px; }
  /* One status chip, picked by the classes on <main> so the async toggles
     (applied, approved) move it without a reload. Priority: ruled out beats
     applied, applied beats approved, approved beats needing review. */
  .status { margin:2px 0 10px; }
  .st { display:none; border-radius:6px; padding:3px 12px; font-weight:600; font-size:13px; }
  main.rejected .st-rejected { display:inline-block; color:#101014; background:#d96b6b; }
  main:not(.rejected).applied .st-applied { display:inline-block; color:#101014; background:#5cd58c; }
  main:not(.rejected):not(.applied).approved .st-approved { display:inline-block; color:#101014; background:#d9a441; }
  main:not(.rejected):not(.applied):not(.approved).prepped .st-review { display:inline-block; color:#e8e8ec; background:#3b6ea5; }
  main:not(.rejected):not(.applied):not(.approved):not(.prepped) .st-none { display:inline-block; color:#8b8b96; background:#2a2a33; }
  .box.rejected { border-color:#5d2f2f; background:#1e1213; }
  .box.rejected h3 { color:#d96b6b; }
  .box.rejected .body { color:#e0a8a8; }
  .mark { display:inline; }
  .mark button { border:1px solid #2f5d43; border-radius:8px; background:none; color:#5cd58c; padding:8px 16px; font:inherit; cursor:pointer; margin:4px 12px 16px 0; }
  .mark button:hover { background:#122019; }
  .mark.on button { border-color:#4a3340; color:#d98b8b; }
  /* The approve toggle wears the approval colour in the same top row. */
  .mark.gate-toggle button { border-color:#6b5320; color:#d9a441; font-weight:600; }
  .mark.gate-toggle button:hover { background:#221c10; }
  .mark.gate-toggle.on button { border-color:#4a3340; color:#d98b8b; font-weight:400; }
  .box { border:1px solid #2a2a33; border-radius:8px; padding:12px 14px; margin-bottom:14px; }
  .box h3 { font-size:11px; font-weight:600; color:#8b8b96; letter-spacing:.08em; text-transform:uppercase; margin:0 0 7px; }
  .body { white-space:pre-wrap; word-break:break-word; color:#d5d5dc; }
  .draft { white-space:pre-wrap; word-break:break-word; color:#e8e8ec; }
  .go { display:inline-block; border:1px solid #3b6ea5; border-radius:8px; padding:8px 16px; margin:4px 12px 16px 0; font-weight:600; }
  .copy { border:1px solid #2a2a33; border-radius:8px; background:none; color:#8b8b96; padding:8px 16px; font:inherit; cursor:pointer; }
  .copy:hover { border-color:#3a3a46; color:#e8e8ec; }
  .sig { display:inline-block; border:1px solid #2a2a33; border-radius:999px; padding:0 9px; color:#8b8b96; margin:0 4px 4px 0; font-size:12px; }
  a.mail { word-break:break-all; }
  .tag.approved { color:#101014; background:#d9a441; }
  /* The review block is the reason this page exists once prep has run, so it
     sits above the raw post and is boxed in the approval colour. */
  .review { border-color:#4a3f22; }
  .review h3 { color:#d9a441; }
  .qs { margin:0; padding-left:18px; color:#d5d5dc; }
  .qs li { margin-bottom:5px; }
  .blockers { color:#e0a8a8; }
  /* One question per row: the draft answer is the thing being reviewed, so it
     gets the readable colour and the question above it is just the label. */
  .fq { margin-bottom:11px; }
  .fq:last-child { margin-bottom:0; }
  .fq-q { color:#8b8b96; font-size:12px; margin-bottom:3px; }
  .fq-a { white-space:pre-wrap; word-break:break-word; color:#e8e8ec; border-left:2px solid #4a3f22; padding-left:9px; }
  .fq-formal { color:#6b6b76; font-size:12px; font-style:italic; }
  .notes textarea { width:100%; box-sizing:border-box; min-height:80px; background:#16161c; color:#e8e8ec; border:1px solid #2a2a33; border-radius:8px; padding:9px 11px; font:inherit; resize:vertical; }
  .gate-row { display:flex; flex-wrap:wrap; gap:10px; align-items:center; margin-top:9px; }
  .notes button { border:1px solid #2a2a33; border-radius:8px; background:none; color:#8b8b96; padding:8px 16px; font:inherit; cursor:pointer; }
  .notes button:hover { border-color:#3a3a46; color:#e8e8ec; }
  .notes button.primary { border-color:#6b5320; color:#d9a441; font-weight:600; }
  .notes button.primary:hover { background:#221c10; border-color:#8a6b2a; color:#e8b954; }
  .saved { color:#5cd58c; text-transform:none; letter-spacing:0; margin-left:8px; }
  .box.gate h3 { color:#d9a441; }
  @media (max-width: 480px) { body { padding:12px; font-size:13px; } }
</style>
<main class="{{if .Applied}}applied {{end}}{{if .Approved}}approved {{end}}{{if .J.Rejected}}rejected {{end}}{{if .Prep}}prepped{{end}}">
  <a class="back" href="{{.BackTo}}">← all jobs</a>
  <h1><span class="score">{{printf "%.1f" .J.Score}}</span>{{.J.Author}}</h1>
  <div class="status">
    <span class="st st-rejected">rejected</span>
    <span class="st st-applied">applied</span>
    <span class="st st-approved">approved · ready to apply</span>
    <span class="st st-review">prepped · needs your review</span>
    <span class="st st-none">not reviewed yet</span>
  </div>
  {{if .J.Rejected}}<div class="box rejected"><h3>ruled out {{ts .J.RejectedAt}} UTC</h3><div class="body">{{.J.RejectReason}}</div></div>{{end}}
  {{if .DupOf}}<div class="box"><h3>repeat</h3><div class="body">Same role as <a href="{{.DupLink}}">lead #{{.DupOf}}</a>. Work that one.</div></div>{{end}}
  {{if .Repeats}}<div class="box"><h3>also posted as</h3>{{range .Repeats}}<div><a href="{{.Link}}">#{{.ID}} · {{.Label}}</a></div>{{end}}</div>{{end}}
  <div class="meta">{{with .J.Profile}}{{.}} · {{end}}{{.Net}}{{with .J.Subreddit}} · r/{{.}}{{end}}{{with .J.JobType}} · {{.}}{{end}} · {{.Age}}{{if .Viewed}} · viewed {{ts .ViewedAt}} UTC{{end}}{{if .Applied}} · applied {{ts .AppliedAt}} UTC{{end}}{{if .Approved}} · approved {{ts .ApprovedAt}} UTC{{end}}</div>
  <a class="go" href="{{.GoLink}}" target="_blank" rel="noopener">open the post ↗</a>
  {{if .HasPostingURL}}<a class="go" href="{{.PostingURL}}" target="_blank" rel="noopener">the real posting ↗</a>{{end}}
  <form class="mark{{if .Applied}} on{{end}}" method="post" action="{{.MarkLink}}" data-state="applied" data-mark="mark as applied" data-undo="undo"><button type="submit">{{.MarkLabel}}</button></form>
  <form class="mark gate-toggle{{if .Approved}} on{{end}}" method="post" action="{{.ApproveLink}}" data-state="approved" data-mark="approve for applying" data-undo="withdraw approval" data-carry=".notes textarea"><button type="submit">{{if .Approved}}withdraw approval{{else}}approve for applying{{end}}</button></form>
  {{with .Prep}}
  <div class="box review"><h3>summary</h3><div class="body">{{.Summary}}</div>
    {{with .Fit}}<div class="body" style="margin-top:9px;color:#8b8b96">{{.}}</div>{{end}}</div>
  {{if .Blockers}}<div class="box review"><h3>blockers</h3><ul class="qs blockers">{{range .Blockers}}<li>{{.}}</li>{{end}}</ul></div>{{end}}
  {{if .OpenQuestions}}<div class="box review"><h3>needs your decision</h3><ul class="qs">{{range .OpenQuestions}}<li>{{.}}</li>{{end}}</ul></div>{{end}}
  {{if .FormQuestions}}<div class="box review"><h3>what the form asks</h3>
    {{range .FormQuestions}}<div class="fq">
      <div class="fq-q">{{.Question}}</div>
      {{if .Answer}}<div class="fq-a">{{.Answer}}</div>
      {{else}}<div class="fq-formal">{{if .Source}}{{.Source}}{{else}}filled from the persona{{end}}</div>{{end}}
    </div>{{end}}
  </div>{{end}}
  {{if .CVURL}}<a class="go" href="{{.CVURL}}" target="_blank" rel="noopener">{{if .CVLabel}}{{.CVLabel}}{{else}}tailored CV{{end}} ↗</a>{{end}}
  {{end}}
  {{if .PrepRaw}}{{if not .Prep}}<div class="box rejected"><h3>prep unreadable</h3><div class="body">The prep artifact on this lead is not valid JSON, so it could not be rendered. Re-run the prep stage for it.</div></div>{{end}}{{end}}
  {{/* The lead carries one note, and this box is both its display and its
       editor: the textarea always holds the saved text, saving overwrites it,
       and the ?noted=1 flag from the save redirect is the "it stuck"
       confirmation the box was missing. */}}
  <form class="notes" method="post" action="{{.NotesLink}}">
    <div class="box review gate">
      <h3>note for the AI apply stage{{if .Noted}} <span class="saved">✓ saved</span>{{end}}</h3>
      <textarea name="notes" placeholder="one note, editable — a correction, a caveat, an answer to a question above">{{.ReviewNotes}}</textarea>
      <div class="gate-row">
        <button type="submit" class="primary">{{if .ReviewNotes}}update note for AI{{else}}save note for AI{{end}}</button>
      </div>
    </div>
  </form>
  {{if .J.Title}}<div class="box"><h3>title</h3><div class="body">{{.J.Title}}</div></div>{{end}}
  {{if .J.ScoreReason}}<div class="box"><h3>why this score</h3><div class="body">{{.J.ScoreReason}}</div></div>{{end}}
  {{if .J.Body}}<div class="box"><h3>post</h3><div class="body">{{.J.Body}}</div></div>{{end}}
  {{if .J.PostingText}}<div class="box"><h3>the real posting</h3><div class="body">{{.J.PostingText}}</div></div>{{end}}
  {{if .Emails}}<div class="box"><h3>contacts</h3>{{range .Emails}}<div><a class="mail" href="mailto:{{.}}">{{.}}</a></div>{{end}}</div>{{end}}
  {{if .J.Draft}}
  <div class="box"><h3>message draft</h3><div class="draft" id="draft">{{.J.Draft}}</div></div>
  <button class="copy" onclick="navigator.clipboard.writeText(document.getElementById('draft').innerText).then(()=>{this.textContent='copied ✓'})">copy draft</button>
  {{end}}
  {{if .Signals}}<div class="box"><h3>signals</h3>{{range .Signals}}<span class="sig">{{.}}</span>{{end}}</div>{{end}}
</main>
` + markJS
