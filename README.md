# JobHub

A small, private job manager and append-only timeline. Agents do the work elsewhere;
JobHub records the jobs, applications, recruiter conversations, and next steps.
It does **not** send messages, apply to jobs, run a browser, or create calendar invites.

Go + SQLite. Server-rendered UI, no frontend build or external assets.

## Local preview

```sh
sh scripts/dev.sh
# http://127.0.0.1:8096
```

On first run this generates a 256-bit API key in **`.env.local`** (mode `0600`,
Git-ignored), builds `.local/jobhub`, and seeds **three clearly fictional jobs**
in `.local/preview.db`. Existing data is never reset on restart. The existing
provider-credential `.env` is not sourced or modified. No production connection.

Sign in with `JOBHUB_API_KEY` from `.env.local`. The browser receives an opaque,
HttpOnly, SameSite=Strict session; the API key is never stored in browser JavaScript
or URLs. Sessions expire in seven days and are revoked on server restart.

## Interface

- Compact jobs list with All/Sergey/Polina selection, search, and clickable stage/attention stats.
- Timeline visible from the first contact, before or after applying.
- Actual messages, actor, channel, direction, source links and verification state.
- Salary range/currency/period; remote **and** geographic eligibility; match score
  with strengths and gaps. Unknown salary/score remains `null`.
- Next action, waiting-on owner, needs-you flag, and interview time/link.
- Production JobHub styling: system font, rounded cards, blue accents and dark/light themes.
- Profile overview: jobs, needs-you, in-progress, offers, hired and rejected;
  30-day jobs/activity chart and profile/work/contract/waiting-on breakdowns.
  These are full-profile counts, not just the current list page or filters.
- Manual job creation, editing, and timeline logging. None of these sends anything.

## Agent API

All routes use `Authorization: Bearer $JOBHUB_API_KEY`. Read the in-app
**Agent API** page or fetch **`GET /api/v1/openapi.json`** for the full OpenAPI 3.1
contract. Generate the checked-in specification with `python3 scripts/generate-openapi.py`.

| Method | Route | Purpose |
| --- | --- | --- |
| GET / POST | `/api/v1/jobs` | Filter jobs / create with stable `source_key` |
| GET / PATCH | `/api/v1/jobs/{id}` | Read / merge editable fields |
| GET / POST | `/api/v1/jobs/{id}/events` | Read / append timeline |
| GET | `/api/v1/stats?profile=sergey` | Full profile totals, breakdowns and 30 UTC days |

```sh
set -a; . ./.env.local; set +a
curl -fsS "$BASE_URL/api/v1/jobs?profile=sergey" \
  -H "Authorization: Bearer $JOBHUB_API_KEY"

curl -fsS "$BASE_URL/api/v1/jobs" \
  -H "Authorization: Bearer $JOBHUB_API_KEY" \
  -H 'Content-Type: application/json' --data '{
    "source_key":"acme:backend-123:sergey", "profile":"sergey",
    "company":"Acme", "title":"Senior Go Engineer", "remote":"remote",
    "location":"EMEA; Georgia eligibility unconfirmed"
  }'
```

### Write guarantees

- Job identity is `(profile, source_key)`. An identical create retry returns the
  same job, including after later edits; a different create payload is `409`.
- Event identity is `(job_id, source_key)`. Identical retries return the original
  event; changed content is `409`. New writes return `201`; replays return `200`
  with `Idempotent-Replay: true`.
- Read the job `ETag` and pass it as `If-Match: "3"` when PATCHing. Missing version:
  `428`; stale version: `409`. New events also increment the job version.
- An event may carry `stage` to record a stage transition in the same transaction.
  A PATCH stage change adds a status event. No implicit application/HR send.
- Only `check` events move `last_checked_at`, using the actual check time and never
  moving it backward. This is an overview field, **not** a per-channel message cursor.
- Events are append-only. Record corrections as new notes. `confirmed` is the
  caller's assertion, not external verification performed by JobHub.
- Lists return `{items, next_cursor}`, newest record ID first; pass `cursor` and
  `limit` (1–100) to continue. Unknown fields and invalid types/enums are rejected.
- `stage=active` filters applied/talking/interview jobs. It is not a writable stage.
  `needs_you=true` excludes hired, rejected and archived jobs. Stats use the same definitions;
  their profiles breakdown is workspace-wide, while other values use the requested profile.
- Errors: `{"error":{"code":"invalid_request","message":"…"}}`.

## Authentication and scope

One owner API key grants access to the whole workspace, including every profile.
Profiles are organizational labels, **not** separate authorization tenants.
Active profiles are `sergey` and `polina`; new jobs accept only these two.
Retired profile history is preserved, but is not offered in the profile selector.
The All view includes Sergey and Polina only; new jobs always belong to one profile.
Browser cookies never authorize `/api/v1`. Browser writes use a separate `/ui`
surface requiring the session, same-origin request, and per-session CSRF token.
Login attempts are throttled; pages have no-store, a same-origin referrer policy
(no referrer sent to external sites), and a restrictive CSP. Native sign-in/sign-out
forms retain their origin for the strict request-origin check.
This is stronger than short URL keys, but not multi-user RBAC or per-agent scopes.

Rotate the key by replacing `JOBHUB_API_KEY` in the private env file and restarting
the process. Existing browser sessions stop working. Keep the key out of logs,
screenshots, URLs, conversation records, and source control.

## Configuration

| Variable | Meaning |
| --- | --- |
| `JOBHUB_API_KEY` | Required; at least 32 characters. Generate randomly. |
| `HOST` | Default `127.0.0.1`; local by default. |
| `PORT` | Default `8096`. |
| `BASE_URL` | Canonical origin; no path. Browser writes must match it. |
| `DB_PATH` | Default `jobs.db`; dev script uses `.local/preview.db`. |
| `JOBHUB_DEMO` | `1` to seed fictional data into an empty workspace. |

Non-loopback binding requires an HTTPS `BASE_URL`; use a TLS reverse proxy and
private application listener for deployment. Forwarded auth/origin headers are
not trusted. Docker binds to `0.0.0.0:8080` inside its private network and requires
an HTTPS `BASE_URL`; do not publish that port publicly.

## Existing data / API compatibility

The new default executable serves only the new workspace/API. The old `?k=` links,
`API_TOKEN`/`VIEW_KEY` login flow, and `/api/jobs` approval/prep routes are not mounted.
Agent integrations must use `/api/v1`; legacy endpoints intentionally return 404.

An additive, one-time migration copies legacy `jobs` and `job_events` into tracker
tables and preserves the original tables unchanged, including old prep artifacts.
Existing event IDs are retained; legacy application/rejection dates become
explicit imported timeline events, without changing the current job stage.
It is **not a live two-way sync**; do not run old and new binaries writing the same
database. Before deployment, take a consistent SQLite backup and rehearse migration
on a copy. Verify legacy table fingerprints, IDs, event counts and integrity.

## Production release

- Existing service: `jobhub` on the private `kamal` Docker network, persistent
  `jobhub_data:/data`, behind the `jobs.cyberjosef.dev` TLS proxy (`/up` health check).
- Build an immutable Linux amd64 image from the committed revision. Keep secrets,
  databases, and `.local` outside the build context and Git.
- Configure `JOBHUB_API_KEY`, `BASE_URL=https://jobs.cyberjosef.dev`,
  `HOST=0.0.0.0`, `PORT=8080`, `DB_PATH=/data/jobs.db`, `JOBHUB_DEMO=0` in a
  private mode-0600 env file. Local production credentials live in `.env.production`.
- Stop the old writer, make the final backup, then start the new image on the same
  volume. Keep the old stopped container and its environment for rollback.
- Verify public HTTPS health, rejected unauthenticated API requests, authenticated
  stats and board pages, and unchanged legacy rows before declaring success.
- Rollback: stop the new writer before starting the retained old container. Legacy
  tables are unchanged; preserve the current database too if any new-workspace
  writes occurred, since those do not synchronize back to the old tables.

## Verify

```sh
go test ./...
go test -race ./internal/workspace ./internal/store
go vet ./...
```

Legacy code and its tests remain in `internal/httpapi` for historical compatibility
reference, not as a reachable authentication bypass. MIT licensed.
