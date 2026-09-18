# jobhub

Run your job hunt on autopilot — without handing over the wheel. Point your
AI agents at jobhub and they scan the internet for roles that match your
skills, score them onto your board, prep each application, and apply — but
nothing is ever sent until you've approved it.

jobhub is the self-hosted hub of that loop: one Go binary, one SQLite file,
no accounts. A small JSON API for the agents, a dark little dashboard for
you, and a human approval gate between the two.

![the board](docs/board.png)

## How it works

Leads arrive as batches on `POST /api/jobs` and upsert by `dedupe_key`, so
re-running a sweep refreshes scores without duplicating rows or resetting
what you've already read. Each lead then walks a pipeline that lives in
columns on its own row:

- **to-prep** → found and scored, with a mandatory `score_reason` (a bare `8`
  is not a judgement anyone can re-read a week later)
- **to-review** → an agent attached the review artifact: summary, fit, what
  the application form asks, open questions, a tailored-CV link
- **approved / rejected** → the human gate, one click each on the lead's own
  page; nothing gets applied to without an approval, and rejections require a
  written reason
- **applied** → the application is out and monitoring takes over

Every lead carries the derived `status` field in JSON — `to-prep`,
`to-review`, `approved`, `applied`, `rejected`, `hired` — so an agent and the
board always read the same ladder.

Once applied, the lead's page grows an **application** section on top: where
the application stands (`in_process` → `rejected` / `hired`), when its
progress was last checked, and an append-only timeline of everything that
happened — emails, DMs, status changes — newest first. Monitoring agents feed
it through the API: push an event when the inbox held something, stamp
`/checked` when it held nothing, and both move the "last checked" clock.

The board is shared by any number of seekers via `profile`, filters compose
through query params (`?profile=&net=&type=&new=1&approved=1&since=7d…`), and
every board URL is also an API call — the JSON endpoint takes the same params.

Each seeker is a row in `profiles` with a description the agents can read:
summary, skills, experience and working conditions, on a page of its own at
`/profiles/{slug}`. Leads and profiles are many-to-many. Every lead has one
owner — the board whose sweep found it, which is also its dedupe scope — and
can be shared with any number of other profiles through `job_profiles`. A
shared lead appears on both boards as **one row**: share it instead of pushing
it twice, or two people work the same posting without either of them seeing it.

![one lead's review page](docs/lead.png)

Pages are gated by short per-page keys derived from a single secret
(`hex(HMAC-SHA256(VIEW_KEY, scope))[:8]`), so a shared link exposes one page,
not the whole board. Key guessing is rate-limited, everything is served with
`X-Robots-Tag: noindex` and a deny-all robots.txt.

## Run it

```sh
API_TOKEN=change-me VIEW_KEY=change-me-too go run ./cmd/jobhub
```

or with Docker:

```sh
docker build -t jobhub .
docker run -d -p 8080:8080 -v jobhub_data:/data \
  -e API_TOKEN=change-me -e VIEW_KEY=change-me-too jobhub
```

Push a first lead and open the link the response gives you:

```sh
curl -X POST localhost:8080/api/jobs -H 'Authorization: Bearer change-me' \
  -H 'Content-Type: application/json' -d '{"jobs":[{
    "dedupe_key":"example-1","network":"reddit","author":"someone",
    "title":"[HIRING] Senior Go dev","url":"https://example.com/post",
    "score":8,"score_reason":"stack match, remote, contract ok"}]}'
# -> {"added":1,"link":"/jobs?k=..."}
```

## Environment

| Var | Meaning |
| --- | --- |
| `API_TOKEN` | Bearer token for `/api/*` (required) |
| `VIEW_KEY` | Secret the page link keys are derived from (required) |
| `BASE_URL` | Public URL of the app, e.g. `https://board.example.com` (default `http://localhost:8080`) |
| `DB_PATH` | SQLite file (default `jobs.db`; the Docker image uses `/data/jobs.db`) |
| `PORT` | Listen port (default `8080`) |
| `DEFAULT_PROFILE` | Who owns leads pushed without a `profile` (default `me`) |
| `PROFILES` | Comma-separated seekers whose board chips exist before their first lead |
| — | The binary ships with three described profiles (`internal/seed`). They are written on boot into empty fields only, so an edit made through `POST /api/profiles` always wins |
| `ELEVENLABS_API_KEY` | Enables voice notes: the approve composer's recordings are transcribed and saved as the review note. Unset, recording stays an on-page preview |
| `ELEVENLABS_STT_MODEL` | Speech-to-text model (default `scribe_v2`) |
| `ELEVENLABS_BASE_URL` | Override the ElevenLabs endpoint (proxies, tests) |

## API

All `/api/*` calls take `Authorization: Bearer $API_TOKEN`. `{id}` accepts a
row id or a `dedupe_key`.

| Endpoint | What it does |
| --- | --- |
| `POST /api/jobs` | Ingest a batch (`{"jobs":[…]}`); upserts by `dedupe_key`, non-empty fields win |
| `GET /api/jobs` | Leads as JSON; same filter params as the board |
| `POST /api/jobs/{id}/applied` | Mark applied (`{"applied":false}` undoes) |
| `POST /api/jobs/{id}/rejected` | Rule out: `{"reason":"…"}` required |
| `POST /api/jobs/{id}/approved` | Clear for applying (`{"approved":false}` undoes) |
| `POST /api/jobs/{id}/prep` | Attach the review artifact (opaque JSON; empty clears) |
| `POST /api/jobs/{id}/duplicate` | Link a repost to its canonical row (`{"of":"…"}`) |
| `POST /api/jobs/{id}/appstatus` | Where the sent application stands: `{"status":"in_process"\|"rejected"\|"hired"}`; a change writes its own timeline event |
| `POST /api/jobs/{id}/events` | Append to the application timeline: `{"kind":"email","note":"…","at":"RFC3339?"}`; also stamps `checked_at` |
| `GET /api/jobs/{id}/events` | The timeline, newest first |
| `POST /api/jobs/{id}/checked` | "Checked, nothing new" — moves `checked_at` and nothing else |
| `POST /api/jobs/{id}/profiles` | Share the lead with another seeker (`{"profile":"polina"}`; `{"linked":false}` unshares, the owner cannot be unlinked) |
| `GET /api/profiles` | Every seeker, with description and lead count |
| `GET /api/profiles/{slug}` | One seeker |
| `POST /api/profiles` | Write a description (`{"slug":"sergey","conditions":"…"}`); non-empty fields win, like a lead push |
| `DELETE /api/jobs`, `DELETE /api/jobs/{id}` | Purge everything / delete one lead |

The board's own buttons (approve, reject, applied, application status, notes)
post with the page's link
key instead of the API token, so the review works from a phone. That includes
`POST /jobs/{id}/voice`: the composer records a voice note in the browser,
posts the audio there, and the ElevenLabs transcript is saved as the review
note while the lead is approved — the spoken version of typing a caveat and
hitting send.

## License

[MIT](LICENSE)
