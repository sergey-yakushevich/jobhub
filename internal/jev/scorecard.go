package jev

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sergey-yakushevich/jobhub/internal/store"
)

// The scorecard splits judgement into two groups, per the four Jev limits:
//
//   - Group A, knockouts: four Choice questions that gate a lead regardless of
//     fit. Their outcome is `workable` — yes / blocked / unknown — never a
//     number, because "cofounder equity-only" is not a low score, it is a no.
//   - Group B, fit axes: Score/Choice questions with named levels. The fit
//     number is the weighted expectation over Jev's calibrated distribution,
//     with weights owned here, derived from the board's own approve/reject
//     history — never a raw probability used as a rank.
//
// Anything Jev is bad at stays out of its questions: counting and dates are
// parsed in derivedFacts and handed over as plain text, and dead-posting
// detection stays with the resolver (an HTTP check) — a lead with no resolved
// posting can never be `workable: yes`, only unknown.

// Workable values. Empty string on a Job means "never scored".
const (
	WorkableYes     = "yes"
	WorkableBlocked = "blocked"
	WorkableUnknown = "unknown"
)

// gateConfidence is the floor under a blocking verdict: a knockout fires only
// when Jev is at least this sure. A shakier answer downgrades to unknown —
// killing a lead on a coin-flip is worse than leaving it for a human.
const gateConfidence = 0.65

// gate is one knockout question plus the decision rule over its options.
type gate struct {
	q Question
	// blocking options force workable=blocked; unknown options cap it at
	// unknown. Any option in neither set passes.
	blocking map[string]bool
	unknown  map[string]bool
}

// axis is one fit dimension: a question with named levels, a value per level
// (0..1, what that level is worth for THIS candidate), and a weight (how much
// the dimension matters). Weights sum to 10 so the fit reads as 0-10.
type axis struct {
	q      Question
	values map[string]float64
	weight float64
}

// scorecard is everything one profile's leads are judged with.
type scorecard struct {
	candidate string
	gates     map[string]gate
	axes      map[string]axis
}

// gateOrder / axisOrder keep the audit JSON and the lead page readable in a
// stable, meaningful order.
var gateOrder = []string{"is_it_a_job", "pay_model", "location_rule", "apply_route"}
var axisOrder = []string{"go_depth", "stack_overlap", "domain", "seniority", "frontend_load"}

// scorecards is keyed by profile. Only profiles listed here get scored; the
// axes encode one person's stack and history, and reusing them for another
// seeker would rank her leads with his weights.
var scorecards = map[string]*scorecard{
	"sergey": sergeyScorecard(),
}

// sergeyScorecard encodes the board's own history (47 approved/applied vs 89
// rejected leads as of 2026-09-20, profile=sergey):
//
//   - ~50% of rejections were gate-shaped, not fit-shaped: not-a-job posts
//     (~20), equity-only cofounder hunts (~10), location walls the aggregator
//     lied about (~10), apply routes that do not work from this account (~8).
//     Those become Group A.
//   - Among gate-passing leads the strongest approve/reject separator is the
//     primary language (approved: Go first, Rails second; rejected: Java,
//     Rust, C#, C++, PHP, Python-first) — go_depth and stack_overlap carry
//     half the fit weight between them.
//   - Domain: fintech/crypto/iGaming approved consistently; data/ML rejected
//     on hard requirements; devops/SRE/platform roles were repeatedly skipped
//     AT THE GATE despite 8+ scores (Buildkite, Algolia, Perplexity, vCluster,
//     Contabo) — so devops scores low here even though K8s is in his stack.
//   - Full-stack/frontend-heavy roles were skipped or down-scored (Block
//     full-stack skip; "heavy React/Next expectation" scored 6): frontend_load
//     is a real axis, not a tiebreaker.
func sergeyScorecard() *scorecard {
	return &scorecard{
		candidate: "CANDIDATE: senior backend engineer, 8 years. Expert: Go, Ruby/Rails, PostgreSQL. " +
			"Solid: Kafka, AWS, Kubernetes, Docker. Domains: payments/fintech. " +
			"Based in Georgia (the country). Remote only; employee or B2B contractor both fine.",
		gates: map[string]gate{
			"is_it_a_job": {
				q: Question{Type: "choice",
					Instructions: "What is this text, at its core?",
					Criteria: map[string]string{
						"live_paid_role":        "a company or client hiring for a real, paid role",
						"cofounder_equity_only": "a cofounder / partner search, compensation is equity or a future promise",
						"advice_or_discussion":  "an advice thread, discussion or story with no hiring intent",
						"job_board_ad":          "an aggregator or job-board repost of someone else's listing",
						"filled_or_dead":        "the text itself says the role is filled, closed or expired",
					}},
				blocking: map[string]bool{"cofounder_equity_only": true, "advice_or_discussion": true, "filled_or_dead": true},
				unknown:  map[string]bool{"job_board_ad": true},
			},
			"pay_model": {
				q: Question{Type: "choice",
					Instructions: "How does this role pay?",
					Criteria: map[string]string{
						"salary":       "a salary as an employee",
						"b2b_contract": "contract / freelance / B2B invoicing",
						"equity_only":  "equity or revenue share only, no cash",
						"unpaid":       "explicitly unpaid, volunteer, or an unpaid internship",
						"not_stated":   "the text does not say how it pays",
					}},
				blocking: map[string]bool{"equity_only": true, "unpaid": true},
				unknown:  map[string]bool{"not_stated": true},
			},
			"location_rule": {
				q: Question{Type: "choice",
					Instructions: "Judge only from what the text states: where must the person be? The candidate is in Georgia (the country).",
					Criteria: map[string]string{
						"worldwide":        "remote from anywhere, no country restriction stated",
						"includes_georgia": "restricted to a region that includes Georgia (e.g. EMEA, Europe, CET±4, EU-friendly lists naming Georgia)",
						"excludes_georgia": "restricted to countries or regions that exclude Georgia (US-only, Canada, LATAM, EU-member-states-only, US work authorization)",
						"hybrid_or_onsite": "hybrid or on-site somewhere, not fully remote",
						"not_stated":       "the text does not state a location rule",
					}},
				blocking: map[string]bool{"excludes_georgia": true, "hybrid_or_onsite": true},
				unknown:  map[string]bool{"not_stated": true},
			},
			"apply_route": {
				q: Question{Type: "choice",
					Instructions: "What is the concrete way to apply that the text offers?",
					Criteria: map[string]string{
						"ats_form":    "a link to an application form (Greenhouse, Ashby, Lever, a careers page...)",
						"email":       "an email address to write to",
						"telegram_dm": "a Telegram handle or link to message",
						"linkedin_dm": "only a LinkedIn DM / 'message me on LinkedIn'",
						"none_given":  "no concrete route: 'comment below', a signup-walled platform, or nothing",
					}},
				blocking: map[string]bool{},
				// linkedin_dm is unknown, not a pass: free messaging is gated
				// to 3rd-degree contacts behind Sales Navigator on this
				// account, and four leads died exactly there.
				unknown: map[string]bool{"linkedin_dm": true, "none_given": true},
			},
		},
		axes: map[string]axis{
			// Weights sum to 10.0. Language carries 5 of it (go_depth 2.5 +
			// stack_overlap 2.5): no other signal separated the history as
			// cleanly. Domain 2.0, seniority and frontend 1.5 each.
			"go_depth": {
				weight: 2.5,
				q: Question{Type: "score",
					Instructions: "How central is Go to this role?",
					Criteria:     []string{"not used", "nice to have", "primary language", "deep/expert internals work"}},
				values: map[string]float64{"0": 0.1, "1": 0.5, "2": 0.95, "3": 1.0},
			},
			"stack_overlap": {
				weight: 2.5,
				q: Question{Type: "score",
					Instructions: "How many of the candidate's tools — Go, Ruby, PostgreSQL, Kafka, AWS, Kubernetes — does the role actually use?",
					Criteria:     []string{"none of them", "1-2 of them", "3-4 of them", "5-6 of them"}},
				values: map[string]float64{"0": 0.0, "1": 0.45, "2": 0.75, "3": 1.0},
			},
			"domain": {
				weight: 2.0,
				q: Question{Type: "choice",
					Instructions: "What domain is the product in?",
					Criteria: map[string]string{
						"payments_fintech": "payments, banking, fintech",
						"crypto":           "crypto, blockchain, web3, trading",
						"general_saas":     "general SaaS / product engineering",
						"data_ml":          "data engineering or ML platform",
						"devops_sre":       "devops, SRE, infrastructure or platform operations",
						"other":            "anything else (gaming, e-commerce, ...)",
					}},
				values: map[string]float64{"payments_fintech": 1.0, "crypto": 0.9,
					"general_saas": 0.75, "data_ml": 0.35, "devops_sre": 0.25, "other": 0.55},
			},
			"seniority": {
				weight: 1.5,
				q: Question{Type: "score",
					Instructions: "What seniority is this role pitched at?",
					Criteria:     []string{"junior/intern", "mid", "senior", "staff/lead", "head/director"}},
				values: map[string]float64{"0": 0.2, "1": 0.6, "2": 1.0, "3": 0.95, "4": 0.5},
			},
			"frontend_load": {
				weight: 1.5,
				q: Question{Type: "score",
					Instructions: "How much of the role is frontend work?",
					Criteria:     []string{"none, pure backend", "light, occasional UI touches", "about half, full-stack", "mostly frontend"}},
				values: map[string]float64{"0": 1.0, "1": 0.85, "2": 0.5, "3": 0.1},
			},
		},
	}
}

// Scoreable reports whether a profile has a scorecard at all.
func Scoreable(profile string) bool {
	_, ok := scorecards[profile]
	return ok
}

// yearsRe pulls "5+ years", "3 yrs" style phrases out of the posting. Jev
// reads numbers as text, so the extraction is done here and the result handed
// to it as a stated fact.
var yearsRe = regexp.MustCompile(`(?i)\b(\d{1,2})\s*\+?\s*(?:years?|yrs?)\b`)

// derivedFacts is the deterministic pre-parse: everything Jev cannot be
// trusted to compute — ages, counts, and whether the posting behind the blurb
// was ever verified — parsed in code and stated as plain text.
func derivedFacts(j *store.Job, now time.Time) string {
	var facts []string
	when := j.PostedAt
	if when.IsZero() {
		when = j.CreatedAt
	}
	if when.IsZero() {
		facts = append(facts, "posting age unknown")
	} else {
		facts = append(facts, fmt.Sprintf("posted %d day(s) ago", int(now.Sub(when).Hours()/24)))
	}
	if ms := yearsRe.FindAllString(textFor(j), 4); len(ms) > 0 {
		seen := map[string]bool{}
		var uniq []string
		for _, m := range ms {
			m = strings.ToLower(strings.Join(strings.Fields(m), " "))
			if !seen[m] {
				seen[m] = true
				uniq = append(uniq, m)
			}
		}
		facts = append(facts, "experience figures found in the text: "+strings.Join(uniq, ", "))
	}
	if j.PostingText != "" {
		facts = append(facts, "TEXT is the employer's own posting, fetched live by the resolver")
	} else {
		facts = append(facts, "TEXT is only the aggregator/social blurb; the employer's posting was NOT verified")
	}
	return "FACTS (machine-parsed, trust these): " + strings.Join(facts, ". ") + "."
}

// textFor prefers the resolved posting over the blurb — the resolver exists
// because the two disagree, and the posting is the one that is true.
func textFor(j *store.Job) string {
	if j.PostingText != "" {
		return j.PostingText
	}
	return j.Body
}

// buildState assembles the tight summary Jev evaluates. TypeSafe lists
// degraded accuracy on large states as a known failure, so this is a
// four-block digest — candidate, parsed facts, lead metadata, clipped text —
// not the full posting stapled to the full CV.
func buildState(sc *scorecard, j *store.Job, now time.Time) string {
	meta := []string{"network=" + j.Network}
	if j.Subreddit != "" {
		meta = append(meta, "subreddit=r/"+j.Subreddit)
	}
	if j.JobType != "" {
		meta = append(meta, "type="+j.JobType)
	}
	if j.Author != "" {
		meta = append(meta, "author="+j.Author)
	}
	var b strings.Builder
	b.WriteString(sc.candidate)
	b.WriteString("\n")
	b.WriteString(derivedFacts(j, now))
	b.WriteString("\nLEAD: ")
	b.WriteString(strings.Join(meta, " "))
	if j.Title != "" {
		b.WriteString("\nTITLE: ")
		b.WriteString(clipStr(j.Title, 200))
	}
	b.WriteString("\nTEXT: ")
	b.WriteString(clipStr(textFor(j), 2800))
	return b.String()
}

func clipStr(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// questionsFor is the whole block sent in one Evaluate call — Jev's server
// time is flat across questions in a request, so gates and axes go together.
func questionsFor(sc *scorecard) map[string]Question {
	qs := make(map[string]Question, len(sc.gates)+len(sc.axes))
	for k, g := range sc.gates {
		qs[k] = g.q
	}
	for k, a := range sc.axes {
		qs[k] = a.q
	}
	return qs
}

// Verdict is what a scored lead gets written back: the two board columns plus
// the audit detail.
type Verdict struct {
	Fit      float64
	Workable string
	// Detail is the audit JSON: every answer, every per-axis contribution,
	// the gate that blocked, model and usage. Raw probabilities are kept so a
	// weight change can be replayed against history without re-calling Jev.
	Detail string
}

// axisDetail / gateDetail are the audit entries serialized into Detail.
type gateDetail struct {
	Choice        string             `json:"choice"`
	Confidence    float64            `json:"confidence"`
	Verdict       string             `json:"verdict"` // pass | blocked | unknown
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

type axisDetail struct {
	Level         string             `json:"level"`
	Expected      float64            `json:"expected"` // E[value] over the distribution, 0..1
	Weight        float64            `json:"weight"`
	Points        float64            `json:"points"` // Expected * Weight, what it adds to fit
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

type verdictDetail struct {
	Model    string                `json:"model"`
	Workable string                `json:"workable"`
	Fit      float64               `json:"fit"`
	Why      string                `json:"why"`
	Gates    map[string]gateDetail `json:"gates"`
	Axes     map[string]axisDetail `json:"axes"`
	Usage    Usage                 `json:"usage"`
}

// compute turns Jev's answers into the two columns. Rules:
//
//   - A blocking option with confidence >= gateConfidence blocks the lead.
//   - Any unknown-class option, any low-confidence gate answer, or an
//     unresolved posting caps workable at unknown: yes is a promise the
//     pipeline acts on, and it is only made when everything checked out.
//   - Fit is the weighted expectation over each axis's distribution — Jev's
//     probabilities are calibrated, so the expectation uses ALL of them, but
//     the value each level is worth is decided here, not by the model.
func compute(sc *scorecard, j *store.Job, res *Result) Verdict {
	d := verdictDetail{Model: res.Model, Usage: res.Usage,
		Gates: map[string]gateDetail{}, Axes: map[string]axisDetail{}}

	workable := WorkableYes
	var blockedBy, unknownBy []string
	if j.PostingText == "" {
		workable = WorkableUnknown
		unknownBy = append(unknownBy, "posting unresolved")
	}
	for _, name := range gateOrder {
		g := sc.gates[name]
		ans, ok := res.Answers[name]
		if !ok {
			workable = WorkableUnknown
			unknownBy = append(unknownBy, name+" unanswered")
			continue
		}
		gd := gateDetail{Choice: ans.Choice, Confidence: ans.Confidence,
			Probabilities: ans.Probabilities, Verdict: "pass"}
		switch {
		case g.blocking[ans.Choice] && ans.Confidence >= gateConfidence:
			gd.Verdict = "blocked"
			blockedBy = append(blockedBy, fmt.Sprintf("%s=%s", name, ans.Choice))
		case g.blocking[ans.Choice] || g.unknown[ans.Choice] || ans.Confidence < gateConfidence:
			gd.Verdict = "unknown"
			if workable == WorkableYes {
				workable = WorkableUnknown
			}
			unknownBy = append(unknownBy, fmt.Sprintf("%s=%s", name, ans.Choice))
		}
		d.Gates[name] = gd
	}
	if len(blockedBy) > 0 {
		workable = WorkableBlocked
	}

	fit := 0.0
	for _, name := range axisOrder {
		a := sc.axes[name]
		ans, ok := res.Answers[name]
		if !ok {
			continue
		}
		expected, level := expectedValue(a, ans)
		points := expected * a.weight
		fit += points
		d.Axes[name] = axisDetail{Level: level, Expected: round2(expected),
			Weight: a.weight, Points: round2(points),
			Confidence: ans.Confidence, Probabilities: ans.Probabilities}
	}
	fit = round1(fit)

	d.Workable = workable
	d.Fit = fit
	switch workable {
	case WorkableBlocked:
		d.Why = "blocked: " + strings.Join(blockedBy, ", ")
	case WorkableUnknown:
		d.Why = "unknown: " + strings.Join(unknownBy, ", ")
	default:
		d.Why = "all gates pass on a resolved posting"
	}
	d.Why += " · fit " + trimFloat(fit) + " = " + axisSum(d.Axes)

	detail, _ := json.Marshal(d)
	return Verdict{Fit: fit, Workable: workable, Detail: string(detail)}
}

// expectedValue folds an answer's calibrated distribution over the axis's
// level values. The named level with the highest probability is reported for
// reading; the number the fit uses is the expectation.
func expectedValue(a axis, ans Answer) (expected float64, level string) {
	if len(ans.Probabilities) == 0 {
		// No distribution: fall back to the flat answer.
		key := ans.Choice
		if ans.Type == "score" {
			key = strconv.Itoa(int(ans.Score + 0.5))
		}
		return a.values[key], levelName(ans, key)
	}
	best, bestP := "", -1.0
	for key, p := range ans.Probabilities {
		expected += p * a.values[key]
		if p > bestP {
			best, bestP = key, p
		}
	}
	return expected, levelName(ans, best)
}

// levelName renders a probability key readably: score answers carry a legend,
// choice answers are already words.
func levelName(ans Answer, key string) string {
	if ans.Legend != nil {
		if name, ok := ans.Legend[key]; ok {
			return name
		}
	}
	if ans.Type == "choice" && ans.Choice != "" && key == "" {
		return ans.Choice
	}
	return key
}

func axisSum(axes map[string]axisDetail) string {
	parts := make([]string, 0, len(axes))
	for _, name := range axisOrder {
		if a, ok := axes[name]; ok {
			parts = append(parts, fmt.Sprintf("%s %s", name, trimFloat(a.Points)))
		}
	}
	return strings.Join(parts, " + ")
}

func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }
func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }
