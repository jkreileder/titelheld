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
  client with rate-limit accounting and 429 backoff (`client.go`), and the
  calls this service makes: the activity read and the two rename writes
  (`activity.go`), and the gear read a franchise matches on (`gear.go`). Gear
  is *read*, never written — the name is a string the athlete typed into
  Strava, and a franchise keys on it.
- `internal/store/` holds the persistence interfaces, an in-memory
  implementation, and `storetest`, the conformance suite both implementations
  must pass. `internal/store/firestore/` is the persistent one; its IAM is
  documented in `docs/firestore-iam.md`.
- `internal/webhook/` serves the subscription handshake and the event intake,
  and queues activities. It acknowledges before it touches the store.
- `internal/server/` assembles the HTTP surface: health, the one-time OAuth
  bootstrap, and the webhook at its unguessable path.
- `internal/app/` wires everything together and serves; it takes `getenv` as a
  parameter so the whole service can be started in a test.
- `internal/logsafe/` neutralizes untrusted text before it reaches a log. Use it
  for anything from a webhook body, Strava, a geocoder or an LLM.
- `cmd/titelheld/` is a shim with no behavior, and is excluded from coverage.
- `infra/` is the GCP deployment in Terraform. `make tf-check` runs exactly what
  CI runs. CI never applies: applies are by hand. Secret Manager *values* never
  appear in code, tfvars or state - only the secret resources are managed.

- `internal/importer/` seeds the title history from Strava. A one-shot run by
  hand, not a route: the service is invokable by `allUsers`, so an endpoint
  that exists for a job run once is surface for no reason. Idempotent and
  resumable with no state of its own — an activity already in the named log is
  left alone.

- `internal/naming/` builds the prompt, validates what comes back, and holds
  the two LLM implementations behind one interface it defines itself. No HTTP
  client of its own beyond the providers, no Firestore: a caller assembles a
  `Ride` and supplies a `Provider`. The prompt states constraints; the
  validator enforces them, because an instruction to a model is a request and
  the ride description is text this service did not write. Gemini goes through
  Vertex AI with the runtime SA's ambient credentials and has no key at all.
  Model IDs are pinned and carry the doc URL and date they were verified
  against.

- `internal/geo/` decodes the summary polyline and reverse-geocodes samples via
  Nominatim into verified place names. The privacy allow-list and the 1 req/s
  limit live there and are enforced in code, not by convention.

- `internal/processor/` is where the pieces meet: it drains the due queue,
  classifies, gathers, prompts, validates and writes. Per-activity failures are
  isolated - one bad ride never stalls a sweep - and the named log is written
  *before* the title is sent, so the failure mode is a ride that keeps its
  default title rather than one renamed twice.
- `internal/sweep/` serves the scheduled drain. It verifies Cloud Scheduler's
  OIDC token itself - a Bearer token that validates against the configured
  audience, an issuer of `https://accounts.google.com`, and an `email` that is
  the scheduler service account with `email_verified` true - because `allUsers`
  holds `roles/run.invoker` and the platform authenticates nobody. The response
  is a bare 401. The log names the issuer, email and `email_verified` failures
  individually; signature, expiry and audience arrive as one error from the
  validator and are reported as it words them, with the audience that was
  required.

The prompt carries three derived things beyond the ride: the last 25 titles a
model wrote, six few-shot examples rebuilt by re-reading past activities, and
the next franchise entry when one applies. Both title lists are filtered to
model-written titles - a commute template is meant to repeat, and six of them
would teach the model to name a gravel ride "Zur Arbeit". Only the title history is worth failing an
activity for - the realistic cause is the composite index missing. Everything
else degrades rather than blocking a title.

Route repeats are specified and not built. Hashing a rounded polyline never
matches a re-ride (0/50 under 5 m jitter; Strava's simplification changes the
vertex count between rides), so it was removed rather than shipped inert. A
replacement is designed and also unbuilt - comparing sets of visited cells by
similarity - so no title can carry a route callback today.

Not built yet: the per-athlete configuration document, so tiers, geofences,
banned words and franchise lists are still shipped defaults in code; and the
Strava history import, which is what would give the title history and the
derived examples anything to work with before the first real naming. No Strava
push subscription has been created, and the Cloud Scheduler job is
deliberately paused.

## Design Rules That Are Not Negotiable

- **The title, and one line of the description.** Sport type, gear and workout
  summaries belong to other tools and are never touched. The description is
  touched in exactly one way: the attribution line from pipeline step 7 is
  prepended to activities this service titled, as a read-modify-write that
  preserves every other byte. Nothing else about a description is ours to
  change, and nothing at all is, on an activity we did not name.
- **Fail closed.** An unrecognized title means someone else named the activity;
  skip it. Never widen the default-title table by guessing.
- **Tier before gate.** `Classify` assigns a tier regardless of the current title,
  then the default-title gate decides whether an action may write. Keep those two
  concerns separate.
- **Core packages stay dependency-free.** `classifier` (and the packages that
  follow it) must not import HTTP, Firestore or a Strava SDK. The Cloud Run `main`
  wires everything together.
- **Config is data, per athlete.** Thresholds, `zwift_mode`, geofences, banned
  words and franchises are configuration, not code, and every field is safe at its
  zero value. Franchises live in the `config` collection now; the rest still
  ship as defaults in code and follow into the same document. What ships in
  code is the *default profile* — what applies until a document exists.
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
- `terraform.yaml` formats, validates and lints `infra/`; it holds no
  credentials and never applies. A fork's pull request gets `fmt -check` only:
  `init`, `validate` and `tflint` each run providers or plugins that files in
  the pull request name. `docker.yaml` builds the image on every PR and
  push to main, scans it with Grype, and holds no credentials - it publishes
  nothing. `rescan.yaml` rebuilds the newest released tag daily and rescans it,
  so advisories published after a release are noticed. It selects the newest
  *final* SemVer tag - `v1.2.3`, never `v1.2.3-rc1` - matching what
  `release.yaml` is willing to build, and fails on any fixable finding, which
  is how it reports.
- A release is a signed `v*` tag pushed by hand - nothing in CI can start one,
  and `release.yaml` has no `workflow_dispatch`, so no run can deploy without a
  tag behind it.
  `release.yaml` verifies the tag and the changelog, calls `release-image.yaml`
  to build and attest the image once, deploys that **digest** to Cloud Run
  through Workload Identity Federation - no exported service-account key
  exists - and fails if the deployed service does not have `DRY_RUN=1`. Build
  and attest share one reusable file because signed provenance names the
  workflow file, which is what makes the SLSA Build L3 claim.
- No bot may release. release-please was rejected: its commits go through the
  low-level git data API unsigned and would fail the signed-commits rule, and a
  PR opened with `GITHUB_TOKEN` never triggers a workflow run so it could not
  satisfy the required checks on `main`, which have no bypass.
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
