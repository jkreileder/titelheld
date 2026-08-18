# Titelheld – Copilot Onboarding

## Quick Pitch

- Single-athlete backend service that renames Strava activities with context-aware,
  LLM-generated titles, and leaves everything else alone.
- It is the **last writer** in a chain of Strava automations (ActivityFix, Xert):
  it waits a configurable delay and only names activities nothing else has titled.
- Go (latest stable, pinned in `go.mod`), standard library first, destined for GCP
  Cloud Run with Firestore state.

## Where Things Live

- `internal/classifier/` decides which naming tier an activity belongs to and what
  may be done with it. No HTTP, no Firestore, no Strava SDK — pure rules over an
  `Activity` value.
  - `activity.go` — the transport-neutral `Activity` input.
  - `defaults.go` — the Strava default-title table and `IsDefaultTitle`.
  - `classifier.go` — `Config`, tiers, actions, and `Classify`.
- `internal/config/` reads every setting from the environment and nothing else,
  so a secret has no route into the working tree. `DRY_RUN` is parsed here and
  defaults to on.
- `internal/strava/` is the only package that talks to Strava: OAuth
  (`oauth.go`), the auto-refreshing token source (`tokensource.go`), the HTTP
  client with rate-limit accounting and 429 backoff (`client.go`), and the two
  API calls this service makes (`activity.go`).
- `internal/store/` holds the persistence interfaces and an in-memory
  implementation. The Firestore one lands with the store phase.
- `internal/webhook/` serves the subscription handshake and the event intake,
  and queues activities. It acknowledges before it touches the store.
- `internal/server/` assembles the HTTP surface: health, the one-time OAuth
  bootstrap, and the webhook at its unguessable path.
- `internal/app/` wires everything together and serves; it takes `getenv` as a
  parameter so the whole service can be started in a test.
- `internal/logsafe/` neutralises untrusted text before it reaches a log. Use it
  for anything from a webhook body, Strava, a geocoder or an LLM.
- `cmd/titelheld/` is a shim with no behaviour, and is excluded from coverage.

Not built yet: the Firestore store, geocoding, the prompt builder and LLM
interface, the sweep and writer, and the Cloud Run deployment configuration.
No Strava push subscription has been created yet.

## Design Rules That Are Not Negotiable

- **Rename only.** Sport type, gear and descriptions belong to other tools.
- **Fail closed.** An unrecognised title means someone else named the activity;
  skip it. Never widen the default-title table by guessing.
- **Tier before gate.** `Classify` assigns a tier regardless of the current title,
  then the default-title gate decides whether an action may write. Keep those two
  concerns separate.
- **Core packages stay dependency-free.** `classifier` (and the packages that
  follow it) must not import HTTP, Firestore or a Strava SDK. The Cloud Run `main`
  wires everything together.
- **Config is data, per athlete.** Thresholds, `zwift_mode`, geofences, banned
  words and franchises are configuration, not code, and every field is safe at its
  zero value.
- **No new dependencies without asking.** Allowed: a polyline decoder, the
  Firestore client, an HTTP router. Everything else needs a decision first.
  There are still none: OAuth, the HTTP client and routing are standard library.
- **Dry run is the safe zero value.** `strava.WriteMode` and
  `config.Config.WritesEnabled` are both written so that an unset or
  zero-valued configuration cannot write to Strava. Writes are refused in
  `UpdateActivityName` *and* again in the transport; keep both.

## Daily Flow

- `make` runs lint, test and build. `make lint` is `go vet` plus golangci-lint at
  the version pinned in the Makefile; `make test` runs with `-race` and coverage;
  `make vulncheck` runs govulncheck.
- Install hooks with `pre-commit install` or `prek install`.
- Core packages are expected to stay above 95% statement coverage.

## CI & Quality Gates

- `go.yaml` — build, race tests with coverage, `go mod tidy -diff`, golangci-lint,
  gofmt check, govulncheck, and the Codecov upload (isolated in its own job).
- Additional workflows cover CodeQL (Go and Actions), dependency review, OpenSSF
  Scorecard, TruffleHog secret scanning, pre-commit hooks, PR-title validation
  and test-result publication.
- Every workflow hardens the runner with a blocked egress policy and pins actions
  by commit SHA. Renovate keeps the pins and the Go toolchain current.

## Privacy Rules

- This is a **public** repository for a service that handles one person's GPS data.
  Fixtures, tests and examples use synthetic coordinates and fake identifiers only.
- Titles must never be derived from reverse-geocoded points of interest; only an
  explicit whitelist and generic area templates.

## Gotchas

- Make recipes run under `bash -eu -o pipefail`; avoid zshisms.
- Commit style: Conventional Commits (`<type>[optional scope]: <description>`),
  signed off with `git commit -s` and cryptographically signed.
- Markdown style: aim for 100-character lines and wrap prose at sentence
  boundaries. Do not reflow code blocks, tables or long URLs. Update the README
  table of contents when sections change.
