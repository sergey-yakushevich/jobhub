package seed

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sergey-yakushevich/jobhub/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "seed.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// Every shipped profile lands described: a slug, a name and all four sections
// of the description. A seed with an empty section would render as a missing
// box on the page and read as "we do not know", which is worse than no seed.
func TestApplyWritesEveryProfile(t *testing.T) {
	st := testStore(t)
	now := time.Now()
	written, err := Apply(st, now)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(written) != len(Profiles) {
		t.Fatalf("wrote %v, want all %d profiles", written, len(Profiles))
	}
	for _, want := range []string{"sergey", "polina"} {
		p, err := st.ProfileBySlug(want)
		if err != nil {
			t.Fatalf("%s: %v", want, err)
		}
		if p.Name == "" || p.Headline == "" || !p.Described() {
			t.Fatalf("%s came out thin: %+v", want, p)
		}
		for field, v := range map[string]string{
			"summary": p.Summary, "skills": p.Skills,
			"experience": p.Experience, "conditions": p.Conditions, "links": p.Links,
		} {
			if strings.TrimSpace(v) == "" {
				t.Fatalf("%s has an empty %s", want, field)
			}
		}
	}
	// Exactly these two ship. Siarhei was retired, and a seed must not bring
	// the identity back.
	if len(Profiles) != 2 {
		t.Fatalf("Profiles has %d entries, want 2", len(Profiles))
	}
	for _, p := range Profiles {
		if p.Slug != "sergey" && p.Slug != "polina" {
			t.Fatalf("unexpected shipped profile %q", p.Slug)
		}
	}
	if p, err := st.ProfileBySlug("siarhei"); err == nil {
		t.Fatalf("siarhei was seeded: %+v", p)
	}
}

// Applying twice writes nothing the second time, and never argues with an
// edit made through the API in between.
func TestApplyIsIdempotentAndYieldsToEdits(t *testing.T) {
	st := testStore(t)
	now := time.Now()
	if _, err := Apply(st, now); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if _, err := st.UpsertProfile(store.ProfileParams{
		Slug: "polina", Conditions: "Remote, EU hours only.",
	}, now); err != nil {
		t.Fatalf("edit: %v", err)
	}
	written, err := Apply(st, now)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if len(written) != 0 {
		t.Fatalf("second apply rewrote %v", written)
	}
	p, err := st.ProfileBySlug("polina")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if p.Conditions != "Remote, EU hours only." {
		t.Fatalf("the seed overwrote an edit: %q", p.Conditions)
	}
}
