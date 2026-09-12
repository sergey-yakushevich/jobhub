package httpapi

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestGatePreview(t *testing.T) {
	if os.Getenv("PREVIEW") == "" {
		t.Skip("preview only")
	}
	s, _ := testServer(t)
	ingestJobs(t, s)
	id := firstJobID(t, s)
	prep := `{"summary":"Senior Go engineer at a payments company. Remote EMEA, B2B invoicing accepted, EUR 5,000-6,500/mo.",
	 "fit":"Go since 2024 (about two years, they ask for five) but the payments depth is exact: 350M+ payments, PCI-DSS tokenisation, fraud and KYC.",
	 "form_questions":[
	   "Full name",{"question":"Email","source":"persona: owner@example.com"},
	   {"question":"Phone","source":"persona: +1 555 010 4477"},
	   {"question":"Expected monthly rate (EUR)?","answer":"Their band is EUR 5,000-6,500. I would say the band works and ask to discuss against it, rather than anchoring at the $4,500 floor."},
	   {"question":"Are you authorised to work in Poland?","answer":"Yes, with a note: the permit is B2B only, so I would contract and invoice rather than join as staff. On a closed option set with no plain Yes, pick 'requires renewal or sponsorship' — option 1 asserts something a B2B permit is not."},
	   {"question":"What was the hardest feature you worked on?","answer":"PCI-DSS tokenisation at Moyasar. Card data had to leave the application boundary entirely without breaking 350M+ payments in flight, so the vault went in behind a dual-write and a per-merchant cutover rather than a migration window. The hard part was not the crypto, it was proving the old path was dead before deleting it."},
	   {"question":"Cover letter","answer":"Straight to the gaps so you can filter fast: I have written Go in production since 2024, so about two years rather than five, and I have not run a team.\n\nWhat I do bring is payments at volume. At Moyasar I owned fraud and KYC services behind 350M+ payments, took a reconciliation pipeline 90% faster, and did the PCI-DSS tokenisation work end to end.\n\nI am in Batumi, Georgia (UTC+4), remote only, and I contract through my US LLC or my Georgian entity.\n\nHappy to walk through the tokenisation cutover if useful."}],
	 "open_questions":["Rate: their band tops out above the floor — confirm asking for 6,000?"],
	 "blockers":["Workable puts a Turnstile on submit, so the last click is yours"],
	 "cv_url":"https://buildcv.cc/u/s/acme-go","cv_label":"CV for Acme"}`
	if err := s.Store.SetJobPrep(id, prep); err != nil {
		t.Fatal(err)
	}
	get := func(name string) {
		req := httptest.NewRequest("GET", "/jobs/"+itoa(id)+"?k="+s.LinkKey(jobScope(id)), nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		os.WriteFile(name, []byte(rec.Body.String()), 0o644)
	}
	get(os.Getenv("OUT") + "/gate-before.html")

	req := httptest.NewRequest("POST", "/jobs/"+itoa(id)+"/approved?k="+s.LinkKey(jobScope(id)),
		strings.NewReader("notes=cap+the+rate+at+6000%2C+use+the+Warsaw+CV"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.ServeHTTP(httptest.NewRecorder(), req)
	get(os.Getenv("OUT") + "/gate-after.html")
}
