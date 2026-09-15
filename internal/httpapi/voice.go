package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// ElevenLabsSTT turns a recorded voice note into text via the ElevenLabs
// speech-to-text API (POST /v1/speech-to-text, multipart file + model_id,
// transcript in the response's "text" field).
//
// BaseURL exists for the tests and for anyone routing through a proxy; the
// zero value of Model means the current general model rather than pinning one
// here and in every deployment's env.
type ElevenLabsSTT struct {
	APIKey  string
	Model   string
	BaseURL string
	Client  *http.Client
}

func NewElevenLabsSTT(apiKey, model string) *ElevenLabsSTT {
	if model == "" {
		model = "scribe_v2"
	}
	return &ElevenLabsSTT{
		APIKey:  apiKey,
		Model:   model,
		BaseURL: "https://api.elevenlabs.io",
		// A minute-long note uploads and transcribes well inside this; without
		// a timeout a stuck upstream would pin the board's request forever.
		Client: &http.Client{Timeout: 60 * time.Second},
	}
}

// sttExt names the upload after its container so the API's format detection
// has something to go on beyond magic bytes. Browsers record audio/webm
// (Chrome) or audio/mp4 (Safari); anything unrecognised still uploads, just
// under the webm name.
func sttExt(mime string) string {
	mime = strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0]))
	switch mime {
	case "audio/mp4", "video/mp4":
		return "mp4"
	case "audio/ogg":
		return "ogg"
	case "audio/wav", "audio/x-wav", "audio/wave":
		return "wav"
	case "audio/mpeg", "audio/mp3":
		return "mp3"
	default:
		return "webm"
	}
}

func (c *ElevenLabsSTT) Transcribe(audio []byte, mime string) (string, error) {
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	head := textproto.MIMEHeader{}
	head.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name="file"; filename="note.%s"`, sttExt(mime)))
	if mime != "" {
		head.Set("Content-Type", mime)
	}
	part, err := w.CreatePart(head)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(audio); err != nil {
		return "", err
	}
	if err := w.WriteField("model_id", c.Model); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", c.BaseURL+"/v1/speech-to-text", body)
	if err != nil {
		return "", err
	}
	req.Header.Set("xi-api-key", c.APIKey)
	req.Header.Set("Content-Type", w.FormDataContentType())
	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	// The error body is worth keeping — "invalid api key" and "quota exceeded"
	// look identical as bare status codes.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("elevenlabs: %d %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("elevenlabs: bad response: %w", err)
	}
	return strings.TrimSpace(out.Text), nil
}

// handleJobVoice is the composer's voice path: the browser posts the recorded
// audio raw, the transcript becomes the lead's review note, and the lead is
// approved — one recording is the spoken version of typing a note and hitting
// send, so it carries the same decision.
//
// Guarded by the lead's link key like the other board toggles. An empty
// transcript (silence, a pocket recording) still approves but leaves the
// existing note alone: wiping a typed caveat because the mic heard nothing
// would be the worst possible reading of "no words".
func (s *Server) handleJobVoice(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !s.guard(w, r, jobScope(id)) {
		return
	}
	if s.STT == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]string{"error": "voice notes are not configured (set ELEVENLABS_API_KEY)"})
		return
	}
	job, err := s.Store.JobByID(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// 16MB holds several minutes of opus; anything bigger is not a note.
	audio, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "audio too large"})
		return
	}
	if len(audio) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no audio"})
		return
	}
	text, err := s.STT.Transcribe(audio, r.Header.Get("Content-Type"))
	if err != nil {
		// The upstream detail goes to the log, not the page: the reviewer can
		// do nothing with a quota message except retype the note anyway.
		log.Printf("[jobs] voice %d: %v", id, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "transcription failed"})
		return
	}
	notes := job.ReviewNotes
	if text != "" {
		notes = clip(text, 4000)
	}
	if err := s.Store.SetJobApproved(id, true, notes, time.Now()); err != nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "on": true, "notes": notes})
}
