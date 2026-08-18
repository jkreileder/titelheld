# strava-namer – Copilot Onboarding

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

Not built yet: the Strava client and OAuth, the webhook, the store, geocoding, the
prompt builder and LLM interface, the delay queue and writer, and the Cloud Run
deployment configuration.

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
