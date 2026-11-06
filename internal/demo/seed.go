// Package demo supplies clearly fictional data only when JOBHUB_DEMO is set.
package demo

import (
	"fmt"
	"github.com/sergey-yakushevich/jobhub/internal/store"
	"time"
)

func num(v float64) *float64 { return &v }

// Seed is deliberately non-destructive: an existing workspace is never reset.
func Seed(st *store.Store, now time.Time) error {
	p, err := st.ListTrackedJobs(store.TrackerFilter{Limit: 1})
	if err != nil {
		return err
	}
	if len(p.Items) > 0 {
		return nil
	}
	t := func(hours int) string { return now.Add(time.Duration(hours) * time.Hour).UTC().Format(time.RFC3339) }
	jobs := []store.TrackedJob{
		{SourceKey: "demo:atlas-commerce", Profile: "sergey", Company: "Atlas Commerce", Title: "Senior Backend Engineer", URL: "https://atlas.example/careers/backend", Description: "Fictional role for this preview. Build the order and inventory services behind a multi-market commerce platform. Primarily Ruby on Rails, with a small TypeScript services team.", Stage: "applied", Remote: "remote", Location: "Europe · Georgia eligibility not yet confirmed", Employment: "full-time", FitScore: num(7.2), FitSummary: "Strong Rails background. Product domain and location eligibility need a closer look.", Strengths: []string{"Deep Rails and PostgreSQL experience", "Production experience with high-volume systems"}, Gaps: []string{"Salary has not been shared", "Confirm Georgia-based B2B contracting"}, Skills: []string{"Ruby", "Rails", "PostgreSQL", "TypeScript"}, RecruiterName: "Jamie Lee", RecruiterContact: "jamie@atlas.example", NextAction: "Follow up if there is no reply", NextActionAt: t(96), WaitingOn: "recruiter"},
		{SourceKey: "demo:northstar-cloud", Profile: "sergey", Company: "Northstar Cloud", Title: "Staff Backend Engineer · Platform", URL: "https://northstar.example/jobs/platform", Description: "Fictional role for this preview. Own the Go control plane for a developer infrastructure product. Work on multi-tenant APIs, Kubernetes provisioning, and service reliability.", Stage: "talking", Remote: "remote", Location: "EMEA · B2B confirmed in mock conversation", Employment: "contract", SalaryMin: num(90000), SalaryMax: num(120000), Currency: "USD", SalaryPeriod: "year", FitScore: num(8.1), FitSummary: "Go and infrastructure are a good match. The role has more platform ownership than payments work.", Strengths: []string{"Go, Kubernetes, and AWS overlap", "Experience designing reliable APIs"}, Gaps: []string{"Clarify staff-level scope and on-call rotation"}, Skills: []string{"Go", "Kubernetes", "AWS", "gRPC"}, RecruiterName: "Maya Chen", RecruiterContact: "maya@northstar.example", NextAction: "Wait for Maya to confirm the on-call expectations", NextActionAt: t(48), WaitingOn: "recruiter"},
		{SourceKey: "demo:relaypay", Profile: "sergey", Company: "RelayPay", Title: "Senior Go Engineer · Payments", URL: "https://relaypay.example/careers/go-engineer", Description: "Fictional role for this preview. Build payment processing and reconciliation services for a remote-first fintech team. Focus on transaction correctness, event-driven services, and PostgreSQL performance.", Stage: "interview", Remote: "remote", Location: "Worldwide · Georgia / B2B accepted in mock brief", Employment: "contract", SalaryMin: num(6500), SalaryMax: num(8500), Currency: "USD", SalaryPeriod: "month", FitScore: num(9.2), FitSummary: "Your payments experience maps directly to the role. Strong backend stack overlap and compatible working terms.", Strengths: []string{"Payment systems at production scale", "Go, PostgreSQL, and Kafka experience", "Remote B2B arrangement fits"}, Gaps: []string{"Confirm ownership of reconciliation services"}, Skills: []string{"Go", "PostgreSQL", "Kafka", "AWS"}, RecruiterName: "Alex Morgan", RecruiterContact: "alex@relaypay.example", NextAction: "Attend the 30-minute introduction with Alex", NextActionAt: t(72), WaitingOn: "you", NeedsYou: true, InterviewAt: t(72), InterviewURL: "https://meet.example/relaypay-intro"},
	}
	for index, j := range jobs {
		created, _, err := st.CreateTrackedJob(j, now.Add(-time.Duration(5-index)*24*time.Hour))
		if err != nil {
			return err
		}
		events := []store.TimelineEvent{
			{Kind: "discovered", Summary: "Role saved and matched to your profile", Body: "Mock discovery. The facts and match assessment shown here are fictional preview data.", Actor: "Job agent", Channel: "system", Direction: "internal", HappenedAt: t(-96 + index*5)},
			{Kind: "application", Summary: "Application recorded with backend CV", Body: "Mock application event. No real application was sent and no CV was uploaded.", Actor: "Job agent", Channel: "ats", Direction: "outbound", HappenedAt: t(-72 + index*5)},
		}
		switch index {
		case 0:
			events = append(events, store.TimelineEvent{Kind: "check", Summary: "Inbox checked · no reply yet", Actor: "Job agent", Channel: "email", Direction: "internal", HappenedAt: t(-2)})
		case 1:
			events = append(events, store.TimelineEvent{Kind: "message", Summary: "Maya asked about your platform experience", Body: "Hi Sergey, thanks for applying. Your Go experience looks relevant. Could you share a little more about your Kubernetes work? We can work with a contractor based in Georgia.", Actor: "Maya Chen · recruiter", Contact: j.RecruiterContact, Channel: "email", Direction: "inbound", ThreadID: "mock-northstar-thread", ExternalID: "mock-msg-1", HappenedAt: t(-25)}, store.TimelineEvent{Kind: "message", Summary: "Replied with relevant experience and an on-call question", Body: "I've worked with Go services running on Kubernetes and AWS, mostly in payment systems. Happy to talk through the architecture. How is on-call handled on this team?", Actor: "Job agent · for Sergey", Account: "sergey@candidate.example", Channel: "email", Direction: "outbound", ThreadID: "mock-northstar-thread", ExternalID: "mock-msg-2", HappenedAt: t(-23)})
		case 2:
			events = append(events, store.TimelineEvent{Kind: "message", Summary: "Alex invited you to an introductory call", Body: "Hi Sergey, your payments background caught our attention. We'd like to set up a 30-minute introduction. Would next week work? The role is fully remote and our budget is $6,500–8,500 per month on a B2B contract.", Actor: "Alex Morgan · recruiter", Contact: j.RecruiterContact, Channel: "email", Direction: "inbound", ThreadID: "mock-relaypay-thread", ExternalID: "mock-msg-1", HappenedAt: t(-30)}, store.TimelineEvent{Kind: "message", Summary: "Shared availability and confirmed contracting terms", Body: "Thanks, Alex. The role sounds relevant. I can do the proposed slot. I'm based in Georgia and work on a B2B contract. Please send over the invite.", Actor: "Job agent · for Sergey", Account: "sergey@candidate.example", Channel: "email", Direction: "outbound", ThreadID: "mock-relaypay-thread", ExternalID: "mock-msg-2", HappenedAt: t(-28)}, store.TimelineEvent{Kind: "interview", Summary: "Introduction scheduled · 30 minutes", Body: "Mock invitation accepted. No real calendar event was created. Discussion: team, payments scope, and next interview stages.", Actor: "Job agent", Channel: "calendar", Direction: "internal", HappenedAt: t(-22)}, store.TimelineEvent{Kind: "note", Summary: "Before the call: review transaction correctness and reconciliation examples", Actor: "Job agent", Channel: "manual", Direction: "internal", HappenedAt: t(-1)})
		}
		for i, e := range events {
			e.SourceKey = fmt.Sprintf("%s:event-%d", j.SourceKey, i+1)
			e.Verification = "recorded"
			recorded, _ := time.Parse(time.RFC3339, e.HappenedAt)
			if _, _, err := st.AddTimelineEvent(created.ID, e, recorded); err != nil {
				return err
			}
		}
	}
	return nil
}
