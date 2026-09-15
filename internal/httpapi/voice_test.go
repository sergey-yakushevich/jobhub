package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sttCapture is what the fake ElevenLabs server saw, so the tests can assert
// the request our client actually sends — auth header, field names, audio
// bytes — against the real API's contract.
type sttCapture struct {
	Path     string
	APIKey   string
	ModelID  string
	Filename string
	PartType string
	Audio    []byte
}

func fakeSTT(t *testing.T, status int, response string, saw *sttCapture) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		saw.Path = r.URL.Path
		saw.APIKey = r.Header.Get("xi-api-key")
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			t.Errorf("stt request is not multipart: %v", err)
		} else {
			saw.ModelID = r.FormValue("model_id")
			if file, head, err := r.FormFile("file"); err != nil {
				t.Errorf("stt request has no file part: %v", err)
			} else {
				saw.Filename = head.Filename
				saw.PartType = head.Header.Get("Content-Type")
				buf := make([]byte, head.Size)
				n, _ := file.Read(buf)
				saw.Audio = buf[:n]
				file.Close()
			}
		}
		w.WriteHeader(status)
		w.Write([]byte(response))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func voiceServer(t *testing.T, stt *httptest.Server) (*Server, int64) {
	t.Helper()
	s, _ := testServer(t)
	ingestJobs(t, s)
	if stt != nil {
		s.STT = NewElevenLabsSTT("k-test", "")
		s.STT.BaseURL = stt.URL
	}
	return s, firstJobID(t, s)
}

func postVoice(t *testing.T, s *Server, id int64, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/jobs/" + itoa(id) + "/voice"
	if key != "" {
		path += "?k=" + key
	}
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "audio/webm")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

// The whole feature in one pass: audio in, transcript out, lead approved with
// the transcript as its review note — and the upstream request shaped exactly
// as the ElevenLabs API expects it.
func TestVoiceNoteTranscribesAndApproves(t *testing.T) {
	saw := &sttCapture{}
	stt := fakeSTT(t, http.StatusOK, `{"text":"  cap the rate at 6000, use the Warsaw CV \n"}`, saw)
	s, id := voiceServer(t, stt)

	rec := postVoice(t, s, id, s.LinkKey(jobScope(id)), "FAKE-OPUS-BYTES")
	if rec.Code != http.StatusOK {
		t.Fatalf("voice: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		On    bool   `json:"on"`
		Notes string `json:"notes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.On || out.Notes != "cap the rate at 6000, use the Warsaw CV" {
		t.Fatalf("response = %+v", out)
	}

	job := jobState(t, s, id)
	if !job.Approved() {
		t.Error("a sent voice note should approve the lead")
	}
	if job.ReviewNotes != "cap the rate at 6000, use the Warsaw CV" {
		t.Errorf("transcript not saved as the note: %q", job.ReviewNotes)
	}

	// The upstream call, field by field.
	if saw.Path != "/v1/speech-to-text" {
		t.Errorf("stt path = %q", saw.Path)
	}
	if saw.APIKey != "k-test" {
		t.Errorf("xi-api-key = %q", saw.APIKey)
	}
	if saw.ModelID != "scribe_v2" {
		t.Errorf("model_id = %q", saw.ModelID)
	}
	if saw.Filename != "note.webm" || saw.PartType != "audio/webm" {
		t.Errorf("file part = %q (%q)", saw.Filename, saw.PartType)
	}
	if string(saw.Audio) != "FAKE-OPUS-BYTES" {
		t.Errorf("audio bytes did not arrive intact: %q", saw.Audio)
	}
}

// Without a key the endpoint must refuse loudly, not approve silently — the
// page keeps recordings a preview in that case and never posts here anyway.
func TestVoiceNoteUnconfigured(t *testing.T) {
	s, id := voiceServer(t, nil)
	rec := postVoice(t, s, id, s.LinkKey(jobScope(id)), "AUDIO")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured voice: %d", rec.Code)
	}
	if jobState(t, s, id).Approved() {
		t.Error("an unconfigured voice post must not approve the lead")
	}
}

// A failed transcription must not half-apply: no approval, note untouched.
func TestVoiceNoteTranscriptionFailure(t *testing.T) {
	stt := fakeSTT(t, http.StatusInternalServerError, `{"detail":"quota exceeded"}`, &sttCapture{})
	s, id := voiceServer(t, stt)
	if err := s.Store.SetJobReviewNotes(id, "typed earlier"); err != nil {
		t.Fatal(err)
	}

	rec := postVoice(t, s, id, s.LinkKey(jobScope(id)), "AUDIO")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("failed transcription: %d %s", rec.Code, rec.Body.String())
	}
	job := jobState(t, s, id)
	if job.Approved() {
		t.Error("a failed transcription must not approve the lead")
	}
	if job.ReviewNotes != "typed earlier" {
		t.Errorf("a failed transcription must not touch the note: %q", job.ReviewNotes)
	}
}

// Silence still means "send": the lead is approved, but an empty transcript
// must not wipe a note that was typed before recording.
func TestVoiceNoteEmptyTranscriptKeepsNote(t *testing.T) {
	stt := fakeSTT(t, http.StatusOK, `{"text":"   "}`, &sttCapture{})
	s, id := voiceServer(t, stt)
	if err := s.Store.SetJobReviewNotes(id, "keep me"); err != nil {
		t.Fatal(err)
	}

	rec := postVoice(t, s, id, s.LinkKey(jobScope(id)), "AUDIO")
	if rec.Code != http.StatusOK {
		t.Fatalf("silent voice note: %d %s", rec.Code, rec.Body.String())
	}
	job := jobState(t, s, id)
	if !job.Approved() {
		t.Error("a silent voice note should still approve")
	}
	if job.ReviewNotes != "keep me" {
		t.Errorf("a silent voice note wiped the existing note: %q", job.ReviewNotes)
	}
}

// The endpoint sits behind the lead's link key like every other board toggle.
func TestVoiceNoteRequiresLinkKey(t *testing.T) {
	stt := fakeSTT(t, http.StatusOK, `{"text":"pwned"}`, &sttCapture{})
	s, id := voiceServer(t, stt)

	if rec := postVoice(t, s, id, "", "AUDIO"); rec.Code == http.StatusOK {
		t.Fatalf("voice without a key answered %d", rec.Code)
	}
	if rec := postVoice(t, s, id, "wrong", "AUDIO"); rec.Code == http.StatusOK {
		t.Fatal("voice with a bad key succeeded")
	}
	if jobState(t, s, id).Approved() {
		t.Error("an unauthorised voice post approved the lead")
	}
}

// An empty body is a client bug, not a transcription job.
func TestVoiceNoteRejectsEmptyAudio(t *testing.T) {
	stt := fakeSTT(t, http.StatusOK, `{"text":"nothing"}`, &sttCapture{})
	s, id := voiceServer(t, stt)
	rec := postVoice(t, s, id, s.LinkKey(jobScope(id)), "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty audio: %d", rec.Code)
	}
}

// The page advertises the voice endpoint only when transcription is on, and
// keeps the honest preview wording when it is off.
func TestShowPageAdvertisesVoiceOnlyWhenConfigured(t *testing.T) {
	s, id := voiceServer(t, nil)
	body := getJobPage(t, s, id)
	if strings.Contains(body, "data-voice=") {
		t.Error("unconfigured page should not carry a voice endpoint")
	}
	if !strings.Contains(body, "not saved yet") {
		t.Error("unconfigured page lost the preview disclaimer")
	}

	s.STT = NewElevenLabsSTT("k", "")
	body = getJobPage(t, s, id)
	if !strings.Contains(body, `data-voice="/jobs/`+itoa(id)+`/voice?k=`) {
		t.Error("configured page should carry the voice endpoint")
	}
	if strings.Contains(body, "not saved yet") {
		t.Error("configured page still shows the preview disclaimer")
	}
}
