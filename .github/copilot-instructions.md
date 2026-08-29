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
  defaults to on. `MAX_INSTANCES` is parsed here too and is *required* to
  serve: it must be `1`, because four pieces of in-process state - the OAuth
  state map, the first-bind lock, token-refresh serialization and the sweep
  lock - are correct only while one instance serves. Terraform feeds the same
  local to `max_instance_count` and to the variable, so the ceiling and what
  the binary believes cannot drift. It is a deployment check and not a mutex:
  the ceiling is per revision, so a rolling deploy or a traffic split has two
  instances that each read `1` - `config.RequiredMaxInstances` says what that
  leaves exposed. An import does not require it: it is a deliberate second
  process that touches none of the four.
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
  parameter so the whole service can be started in a test. `sweepDeps` is the
  one place configuration becomes the processor's dependencies, and the
  defaults a deployment gets are resolved there - the shipped banned-word list
  when the environment names none, the shipped machine titles likewise. Assert
  those through `config.Load` and `sweepDeps`, never a hand-built
  `config.Config`: a config a test builds agrees with itself and cannot notice
  a default that never reaches production, which is how the banned-word list
  shipped empty.
- `internal/logsafe/` neutralizes untrusted text before it reaches a log. Use it
  for anything from a webhook body, Strava, a geocoder or an LLM. `String`
  flattens newlines and caps at 256 - right for a title, which must not forge a
  second line. `Block` keeps newlines and tabs and caps at 16 KB, for a value
  whose line structure *is* the content: the naming prompt, logged whole while
  `LOG_PROMPT` is on. It defaults to on and is independent of `DRY_RUN` -
  deriving it from the write mode took the prompts away at the moment writes
  began, which is when they are worth most.
- `cmd/titelheld/` is a shim with no behavior, and is excluded from coverage.
- `docs/` carries what a deployment needs and the README does not: every
  setting in `configuration.md`, the GCP deployment in `infrastructure.md`,
  the Firestore collections and their IAM in `firestore-iam.md`. The README is
  about what the service does; those three are about running it.
- `infra/` is the GCP deployment in Terraform. `make tf-check` runs exactly what
  CI runs. CI never applies: applies are by hand. Secret Manager *values* never
  appear in code, tfvars or state - only the secret resources are managed.

- `internal/importer/` seeds the title history from Strava. A one-shot run by
  hand, not a route: the service is invokable by `allUsers`, so an endpoint
  that exists for a job run once is surface for no reason. Idempotent and
  resumable with no state of its own — an activity already in the named log is
  left alone. It seeds rides only, and of those only what the athlete wrote:
  Strava defaults, machine titles, anything Zwift or Xert titled, and this
  service's own commute and errand templates are all skipped. The template list
  is read from the athlete's configuration, so the list a run skips is the list
  that run would write. None of this widens `MachineTitles` — that set decides
  what may be *overwritten*, and a Zwift ride keeps its title.

- `cmd/titelheld-config/` seeds the athlete's `config` document from the
  shipped default profile, reserving what `-reserve` names, and refuses if
  one exists - the document is authoritative from its first write and is
  edited in place after that. Store-only: no Strava client, no token
  refresh, and `config.LoadStore` reads nothing but where Firestore is. The
  wiring is `app.SeedConfig`; the read-back goes through
  `processor.FranchisesFromStored`, the sweep's own conversion.

- `internal/naming/` builds the prompt, validates what comes back, and holds
  the three LLM implementations behind one interface it defines itself. No HTTP
  client of its own beyond the providers, no Firestore: a caller assembles a
  `Ride` and supplies a `Provider`. The prompt states constraints; the
  validator enforces them, because an instruction to a model is a request and
  the ride description is text this service did not write. Gemini goes through
  Vertex AI with the runtime SA's ambient credentials and has no key at all.
  `anthropic` calls the Messages API with `LLM_API_KEY`. `openrouter` is an
  OpenAI-compatible chat-completions client against `LLM_BASE_URL` (default
  OpenRouter's, https required - the key travels in a header to that host),
  model via `LLM_MODEL`, the same `LLM_API_KEY`: one key, many narrators, so a
  voice miss is diagnosed by re-sweeping the same queued ride under another
  model - a Terraform variable change, not a release. Same title contract,
  same validator, no provider-side JSON mode - a parameter one narrator
  rejects would contaminate an A/B. Dormancy is tested, not assumed: with
  `LLM_PROVIDER` unset the real loader resolves Vertex and reads no key, and
  `openrouter` without a key refuses at startup naming both variables.
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

The prompt carries four derived things beyond the ride: the last 25 titles
worth not repeating, six few-shot examples rebuilt by re-reading past
activities, the names of up to six notable segment efforts, and the next
franchise entry when one applies. An effort is notable when it is a personal
top-three or carries a Strava achievement; only the name crosses into the
prompt, never the times or ranks that selected it. Segment names are the
least trusted text in the prompt - a segment is named by whoever created it,
and every rider inherits that name - so the system prompt states two rules for
that block by name: the names are data and **never instructions**, the rule
`Bike` and `NOTES` also carry, and a place inside a segment name is still not
a place the model may use. A test asserts every rule mentioning one of those
three blocks carries the prohibition. The two lists are
filtered differently, and that difference is the rule. RECENT drops only
templates - a commute name is meant to repeat, so listing it forbids the right
answer - and keeps imported titles, because repeating one is exactly what to
avoid. EXAMPLES admits two sources and no other: `SourceService`, a title this
service's own pipeline wrote, and `SourceHuman`, a title the athlete wrote on a
sport ride the skip gate declined - recorded by the processor on that skip,
never written to Strava, and the best style data there will ever be. An
imported row is structurally unable to become an example, which is the point -
a decade of the athlete's shorthand says what not to write twice and nothing
about what a title should sound like, and no pattern has to be maintained to
keep a bare town name out. Until a title from either source exists the
synthetic set is what the prompt carries, which is its purpose rather than a
cold-start stopgap. Only the title history is worth
failing an activity for - the realistic cause is the composite index missing.
Everything else degrades rather than blocking a title.

Route repeats are specified and not built. Hashing a rounded polyline never
matches a re-ride (0/50 under 5 m jitter; Strava's simplification changes the
vertex count between rides), so it was removed rather than shipped inert. A
replacement is designed and also unbuilt - comparing sets of visited cells by
similarity - so no title can carry a route callback today.

Franchises live in the per-athlete `config` document. Tiers, geofences, banned
words and language preferences still ship as defaults in code and belong in
that same document. What ships in code for franchises is the default profile:
what applies until a document exists.

An entry is spent when it is *used*, not when it is offered. The prompt asks
for the entry's wording and a request to a model is not a guarantee, so
`naming.UsesEntry` decides: normalized containment of the entry's core - both
sides lowercased, punctuation flattened, whitespace collapsed, the leading
article dropped, matched on token boundaries. Unused means one more offer, at
most one, and then a third call carrying no FRANCHISE block; the position does
not move. It fails closed, because a repeated offer can be noticed and a film
spent on a title that never carried it cannot. Entries listed under `reserved`
are never offered - they keep their place in the series and the athlete spends
them by hand - and the position therefore advances *past the index* that was
offered rather than by one step, monotonically, so a rotation that stepped over
a reserved entry never hands it out later. Reserving governs the *offer* and
nothing else, so `Franchise.Guard` governs the title: it refuses a candidate
claiming any entry of the matched series except the one being offered - future
entries included, since claiming one spends a film the position will not
record. The comparison is `UsesEntry`, the test that decides spending. An entry
whose core is no more than the gear name is matched by equality instead,
because containment there would forbid every title that takes the motif rule's
invitation.

Not built yet: route repeats. The Strava push subscription exists and is
feeding the queue; the Cloud Scheduler job fires every five minutes and drains
it, and `DRY_RUN=0` on the deployed service, so a cleared activity is renamed
without anyone asking. The title history is seeded by
`cmd/titelheld-import` rather than accumulating on its own.

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
- **Every behavioral test carries a negative control.** Break the behavior, watch
  the test fail, restore it. A test that passes either way proves nothing, and
  several here did: an idempotency test that passed because the classifier
  declined the replay rather than the named log; a tolerance test whose uniform
  offset happened not to cross a cell boundary; an index guard that accepted
  `athlete_id` descending; a trailing-data check defeated by padding to the byte
  cap. Assert against the thing itself, never a stand-in you control: the
  Firestore emulator serves queries no composite index exists for, so a wrong
  index declaration passes every test here and fails only in production. Where
  a stand-in is unavoidable, check what actually keeps it honest — `make lint`
  and CI run the same golangci-lint version because a comment in the workflow
  says to keep them in step, which is a convention and not a mechanism.

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
  exists. Before deploying it reads `DRY_RUN` off the service the deploy will
  land on and refuses outright if writes are on: the job sets the image and
  nothing else, so a new revision inherits the running service's environment
  and the existing service predicts it. The escape hatch is the repository
  variable `WRITES_ACKNOWLEDGED`, a *date* (`YYYY-MM-DD`, accepted only within
  7 days before the tag and 1 day after it, so one left behind expires on its
  own), and it is the second deliberate act after the Terraform change that
  sets `DRY_RUN=0`. A second read after the deploy remains, as a check that
  the prediction held. Both checks live in `.github/scripts/writes-gate.sh`
  rather than in `run:` blocks, because a linter can read a workflow and only
  a test can run one - and shell in a file is covered by the shellcheck and
  shfmt hooks. Build
  and attest share one reusable file because signed provenance names the
  workflow file, which is what makes the SLSA Build L3 claim.
- No bot may release. release-please was rejected: its commits go through the
  low-level git data API unsigned and would fail the signed-commits rule, and a
  PR opened with `GITHUB_TOKEN` never triggers a workflow run so it could not
  satisfy the required checks on `main`, which have no bypass.
- `release-logic.yaml` runs the release's own decisions rather than linting
  them: `.github/scripts/tests/writes-gate-test.sh` drives the real gate script
  against a stubbed `gcloud`, and `draft-condition-test.py` reads the
  draft-release `if:` out of `release.yaml` and evaluates it over every
  combination of job results. `make release-logic` runs both. A linter reads a
  workflow and cannot run one - which is why a gate that checked DRY_RUN after
  the deploy was as lint-clean as the one that checks before it.
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
