package httpapi

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sergey-yakushevich/jobhub/internal/store"
)

// handleProfilesJSON lists every profile with its description and lead count.
// This is the read the prep and apply stages want: who the board hunts for,
// and what to say about them, without a file kept beside the app.
func (s *Server) handleProfilesJSON(w http.ResponseWriter, _ *http.Request) {
	profiles, err := s.Store.ListProfiles()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if profiles == nil {
		profiles = []store.Profile{}
	}
	writeJSON(w, http.StatusOK, profiles)
}

func (s *Server) handleProfileJSON(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.ProfileBySlug(r.PathValue("slug"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such profile"})
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// handleProfileUpsert writes a profile's description:
//
//	POST /api/profiles {"slug":"sergey","conditions":"$4,500/month B2B"}
//
// Non-empty-only, like a job push: the body above updates the conditions and
// leaves the summary, skills and experience exactly as they were.
func (s *Server) handleProfileUpsert(w http.ResponseWriter, r *http.Request) {
	var req store.ProfileParams
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	p, err := s.Store.UpsertProfile(store.ProfileParams{
		Slug:     clip(strings.ToLower(strings.TrimSpace(req.Slug)), 40),
		Name:     clip(strings.TrimSpace(req.Name), 120),
		Headline: clip(strings.TrimSpace(req.Headline), 200),
		// The four description fields are the point of the table, so they get
		// room: a summary is a paragraph and an experience section is a career.
		Summary:    clip(strings.TrimSpace(req.Summary), 8000),
		Skills:     clip(strings.TrimSpace(req.Skills), 8000),
		Experience: clip(strings.TrimSpace(req.Experience), 20000),
		Conditions: clip(strings.TrimSpace(req.Conditions), 4000),
		Links:      clip(strings.TrimSpace(req.Links), 2000),
	}, time.Now())
	if errors.Is(err, store.ErrProfileSlugRequired) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		log.Printf("[profiles] upsert: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "save failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"profile": p,
		"link":    fmt.Sprintf("%s/profiles/%s?k=%s", s.BasePath, p.Slug, s.LinkKey(jobsScope())),
	})
}

// handleJobProfiles shares one lead with another profile, or takes it back:
//
//	POST /api/jobs/12/profiles {"profile":"polina"}
//	POST /api/jobs/12/profiles {"profile":"polina","linked":false}
//
// This is the many-to-many in one call. Sharing beats pushing the same posting
// under a second profile, because a second push makes a second row with its own
// viewed, approved and applied state — two people then work the same job twice
// and neither can see that the other has.
//
// The owner link — the board whose sweep found the lead — cannot be removed.
func (s *Server) handleJobProfiles(w http.ResponseWriter, r *http.Request) {
	req := struct {
		Profile string `json:"profile"`
		// Linked is tri-state for the same reason applied is: omitting it means
		// the obvious thing (share it), and false is a real instruction.
		Linked *bool `json:"linked"`
	}{}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req)
	slug := clip(store.NormalizeProfile(req.Profile), 40)
	if strings.TrimSpace(req.Profile) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "profile is required"})
		return
	}
	id, err := s.Store.ResolveJobRef(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such job"})
		return
	}
	linked := req.Linked == nil || *req.Linked
	if linked {
		err = s.Store.LinkJobProfile(id, slug, time.Now())
	} else {
		err = s.Store.UnlinkJobProfile(id, slug)
	}
	switch {
	case errors.Is(err, store.ErrOwnerLinkRequired):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	case errors.Is(err, sql.ErrNoRows):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such job or link"})
		return
	case err != nil:
		log.Printf("[profiles] link %d %s: %v", id, slug, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "link failed"})
		return
	}
	links, err := s.Store.ProfilesForJob(id)
	if err != nil {
		log.Printf("[profiles] links of %d: %v", id, err)
	}
	slugs := make([]string, 0, len(links))
	for _, l := range links {
		slugs = append(slugs, l.Slug)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "profile": slug, "linked": linked, "profiles": slugs,
	})
}

// ---- pages ----

// handleProfilesIndex is the list of seekers behind the board, guarded by the
// board's own key: whoever can read the board can read who it hunts for.
func (s *Server) handleProfilesIndex(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r, jobsScope()) {
		return
	}
	profiles, err := s.Store.ListProfiles()
	if err != nil {
		log.Printf("[profiles] list: %v", err)
		http.NotFound(w, r)
		return
	}
	key := s.LinkKey(jobsScope())
	data := profilesIndexData{
		Base:      s.BasePath,
		BoardLink: fmt.Sprintf("%s/jobs?k=%s", s.BasePath, key),
	}
	for _, p := range profiles {
		data.Profiles = append(data.Profiles, profileCardView{
			Slug:      p.Slug,
			Name:      p.Name,
			Headline:  p.Headline,
			Leads:     p.Leads,
			Described: p.Described(),
			Link:      s.profileLink(p.Slug),
		})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := profilesIndexTmpl.Execute(w, data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

// handleProfileShow is one seeker's page: the description, and the way into
// their leads.
func (s *Server) handleProfileShow(w http.ResponseWriter, r *http.Request) {
	if !s.guard(w, r, jobsScope()) {
		return
	}
	p, err := s.Store.ProfileBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	key := s.LinkKey(jobsScope())
	board := func(extra string) string {
		return fmt.Sprintf("%s/jobs?k=%s&profile=%s%s", s.BasePath, key, url.QueryEscape(p.Slug), extra)
	}
	counts, err := s.Store.JobStats(store.JobFilter{Profile: p.Slug}, time.Now())
	if err != nil {
		log.Printf("[profiles] stats %s: %v", p.Slug, err)
	}
	data := profileShowData{
		Base:       s.BasePath,
		P:          p,
		Described:  p.Described(),
		BoardLink:  board(""),
		ReviewLink: board("&prepped=1&approved=0&rejected=0"),
		SendLink:   board("&approved=1&applied=0&rejected=0"),
		IndexLink:  fmt.Sprintf("%s/profiles?k=%s", s.BasePath, key),
		Links:      splitLines(p.Links),
	}
	if counts != nil {
		data.Leads, data.ToReview, data.ToSend, data.Applied = counts.Total, counts.ToReview, counts.ToSend, counts.Applied
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := profileShowTmpl.Execute(w, data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

// profileLink is a profile page carrying the board key, which is the key that
// opens it. Every who-tag on the board and on a lead uses this, so a reader who
// has forgotten what "siarhei" means is one tap from the answer.
func (s *Server) profileLink(slug string) string {
	return fmt.Sprintf("%s/profiles/%s?k=%s", s.BasePath, url.PathEscape(slug), s.LinkKey(jobsScope()))
}

// splitLines turns the links field into one entry per line, dropping blanks.
func splitLines(v string) []string {
	var out []string
	for _, line := range strings.Split(v, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

type profileCardView struct {
	Slug      string
	Name      string
	Headline  string
	Leads     int64
	Described bool
	Link      string
}

type profilesIndexData struct {
	Base      string
	BoardLink string
	Profiles  []profileCardView
}

type profileShowData struct {
	Base string
	P    *store.Profile
	// Described is false for a profile that exists only because leads mention
	// it; the page then says how to fill it in instead of showing empty boxes.
	Described bool
	Leads     int64
	ToReview  int64
	ToSend    int64
	Applied   int64
	BoardLink string
	// The two queue links are the same ones the board's own chips build, so a
	// profile page is a working entry point and not just a description.
	ReviewLink string
	SendLink   string
	IndexLink  string
	Links      []string
}

var profilesIndexTmpl = template.Must(template.New("profiles").Funcs(pageFuncs).Parse(profilesIndexHTML))
var profileShowTmpl = template.Must(template.New("profile").Funcs(pageFuncs).Parse(profileShowHTML))

// profileCSS is shared by both profile pages: the card list on the index and
// the description sections on the detail page.
const profileCSS = `
  .sec { color:var(--hint); font-size:13px; font-weight:500; text-transform:uppercase; letter-spacing:.05em; margin:24px 16px 8px; }
  .box { padding:14px 16px; }
  .body { white-space:pre-wrap; word-break:break-word; }
  .head { padding:16px; }
  .plist { overflow:hidden; margin-top:4px; }
  .prow { display:flex; align-items:center; gap:12px; padding:12px 16px; border-bottom:1px solid var(--divider); color:inherit; }
  .prow:last-child { border-bottom:0; }
  .prow:hover { text-decoration:none; background:color-mix(in srgb, var(--accent) 7%, transparent); }
  .prow .pn { flex:1 1 auto; min-width:0; }
  .prow .nm { font-weight:600; }
  .prow .hl { color:var(--hint); font-size:13px; margin-top:2px; word-break:break-word; }
  .prow .ct { flex:0 0 auto; color:var(--hint); font-size:13px; white-space:nowrap; }
  .tiles { display:grid; grid-template-columns:repeat(auto-fit,minmax(96px,1fr)); gap:8px; margin-top:10px; }
  .tile { padding:10px 12px; }
  .tile .n { font-size:20px; font-weight:700; }
  .tile .k { color:var(--hint); font-size:13px; }
  .actions { display:flex; gap:8px; flex-wrap:wrap; margin-top:14px; }
  .btn { display:inline-flex; align-items:center; box-sizing:border-box; min-height:40px; border:0; cursor:pointer;
         font-size:15px; line-height:20px; font-weight:600; border-radius:10px; padding:9px 14px;
         background:color-mix(in srgb, var(--accent) 13%, transparent); color:var(--accent); }
  a.btn:hover { text-decoration:none; opacity:.85; }
  .back { font-size:17px; font-weight:600; }
  .empty { padding:14px 16px; margin:0; }
  @media (max-width: 480px) { main { padding:12px 10px 48px; } .prow { padding:12px; } }
`

const profilesIndexHTML = `<!doctype html>
` + pageHead + `
<title>profiles · jobs</title>
<style>` + sharedCSS + profileCSS + `
  h1 { margin:0; font-size:28px; line-height:36px; font-weight:700; }
</style>
<main>
  <div class="top-bar">
    <div><h1>Profiles</h1><div class="sub">who this board hunts for</div></div>
    ` + themeSeg + `
  </div>
  <a class="back" href="{{.BoardLink}}">‹ Jobs</a>
  <div class="card plist">
  {{range .Profiles}}
    <a class="prow" href="{{.Link}}">
      <div class="pn">
        <div class="nm">{{if .Name}}{{.Name}}{{else}}{{.Slug}}{{end}} <span class="tag who-tag">{{.Slug}}</span></div>
        <div class="hl">{{if .Headline}}{{.Headline}}{{else}}no description yet{{end}}</div>
      </div>
      <div class="ct">{{.Leads}} lead(s)</div>
    </a>
  {{else}}
  <p class="sub empty">no profiles yet</p>
  {{end}}
  </div>
</main>
` + themeJS

const profileShowHTML = `<!doctype html>
` + pageHead + `
<title>{{.P.Slug}} · jobs</title>
<style>` + sharedCSS + profileCSS + `
  h1 { display:flex; align-items:center; gap:10px; flex-wrap:wrap; font-size:20px; line-height:24px; font-weight:700; margin:0; }
  .meta { color:var(--hint); font-size:13px; line-height:20px; margin-top:8px; }
  .meta .tag.who-tag { background:color-mix(in srgb, var(--accent) 15%, transparent); color:var(--accent); }
  .links a { display:block; word-break:break-all; }
</style>
<main>
  <div class="top-bar">
    <a class="back" href="{{.BoardLink}}">‹ Jobs</a>
    ` + themeSeg + `
  </div>
  <div class="card head">
    <h1><span>{{if .P.Name}}{{.P.Name}}{{else}}{{.P.Slug}}{{end}}</span></h1>
    <div class="meta"><span class="tag who-tag">{{.P.Slug}}</span>{{with .P.Headline}} {{.}}{{end}}{{if .P.UpdatedAt.IsZero}}{{else}} · updated {{when .P.UpdatedAt}}{{end}}</div>
    <div class="tiles">
      <div class="card tile"><div class="n">{{.Leads}}</div><div class="k">leads</div></div>
      <div class="card tile"><div class="n">{{.ToReview}}</div><div class="k">to review</div></div>
      <div class="card tile"><div class="n">{{.ToSend}}</div><div class="k">to send</div></div>
      <div class="card tile"><div class="n">{{.Applied}}</div><div class="k">applied</div></div>
    </div>
    <div class="actions">
      <a class="btn" href="{{.BoardLink}}">their leads</a>
      <a class="btn" href="{{.ReviewLink}}">to review</a>
      <a class="btn" href="{{.SendLink}}">to send</a>
      <a class="btn" href="{{.IndexLink}}">all profiles</a>
    </div>
  </div>
  {{if .Described}}
  {{with .P.Summary}}<div class="sec">about</div><div class="card box"><div class="body">{{.}}</div></div>{{end}}
  {{with .P.Skills}}<div class="sec">skills</div><div class="card box"><div class="body">{{.}}</div></div>{{end}}
  {{with .P.Experience}}<div class="sec">experience</div><div class="card box"><div class="body">{{.}}</div></div>{{end}}
  {{with .P.Conditions}}<div class="sec">working conditions</div><div class="card box"><div class="body">{{.}}</div></div>{{end}}
  {{else}}
  <div class="sec">description</div>
  <div class="card box"><div class="body">No description yet. POST /api/profiles with {"slug":"{{.P.Slug}}", "summary": "…", "skills": "…", "experience": "…", "conditions": "…"} to fill it in.</div></div>
  {{end}}
  {{if .Links}}<div class="sec">links</div><div class="card box links">{{range .Links}}<a href="{{.}}" target="_blank" rel="noopener">{{.}}</a>{{end}}</div>{{end}}
</main>
` + themeJS

// profileRows is the board's profiles breakdown with each label pointing at
// that seeker's page, so "who is siarhei" is answered from the board itself.
func (s *Server) profileRows(rows []store.LabelStat) []statRowView {
	out := plainRows(rows)
	for i := range out {
		out[i].Link = s.profileLink(out[i].Label)
	}
	return out
}

// profileRefView is one who-tag on a lead: the profile, whether it owns the
// lead, and the page that describes it.
type profileRefView struct {
	Slug  string
	Owner bool
	Link  string
}

// jobProfileRefs turns a lead's links into who-tags. A lookup failure costs
// the tags, not the page — the same rule the repeats block follows.
func (s *Server) jobProfileRefs(jobID int64) []profileRefView {
	links, err := s.Store.ProfilesForJob(jobID)
	if err != nil {
		log.Printf("[profiles] links of %d: %v", jobID, err)
		return nil
	}
	out := make([]profileRefView, 0, len(links))
	for _, l := range links {
		out = append(out, profileRefView{Slug: l.Slug, Owner: l.Owner, Link: s.profileLink(l.Slug)})
	}
	return out
}
