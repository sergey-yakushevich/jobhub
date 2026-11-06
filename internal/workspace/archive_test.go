package workspace

import (
	"fmt"
	"strings"
	"testing"
)

func TestArchiveViewHidesHistoryFromNormalBoardWithoutDeletingIt(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:8096")
	createTestJob(t, s, `{"source_key":"live","company":"Current Company","title":"Current Role"}`)
	archived, _ := createTestJob(t, s, `{"source_key":"old","company":"Archived Company","title":"Archived Role","stage":"archived"}`)
	cookie, _ := login(t, s)
	for _, profile := range []string{"sergey", "all"} {
		r := getWithCookie(t, s, "/jobs?profile="+profile, cookie)
		html := r.Body.String()
		if r.Code != 200 || !strings.Contains(html, "Current Company") || strings.Contains(html, "Archived Company") {
			t.Fatal("normal board includes archive", r.Code)
		}
		if !strings.Contains(html, `<span class="n">1</span>`) || !strings.Contains(html, `href="/jobs?profile=`+profile+`&stage=archived">archived · 1`) {
			t.Fatal("archive navigation or job count incorrect")
		}
		r = getWithCookie(t, s, "/jobs?profile="+profile+"&stage=archived", cookie)
		html = r.Body.String()
		if r.Code != 200 || strings.Contains(html, "Current Company") || !strings.Contains(html, "Archived Company") || !strings.Contains(html, `class="chip on" href="/jobs?profile=`+profile+`&stage=archived"`) {
			t.Fatal("archive view missing history", r.Code)
		}
	}
	r := getWithCookie(t, s, fmt.Sprintf("/jobs/%d", archived), cookie)
	if r.Code != 200 || !strings.Contains(r.Body.String(), "Archived Role") {
		t.Fatal("archived job details unavailable")
	}
	r = apiRequest(t, s, "GET", "/api/v1/jobs", "")
	if r.Code != 200 || !strings.Contains(r.Body.String(), "Archived Company") || !strings.Contains(r.Body.String(), "Current Company") {
		t.Fatal("API lost archived records")
	}
}
