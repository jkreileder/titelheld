# Titelheld

Single-athlete backend service that renames Strava activities with context-aware, LLM-generated
titles and leaves everything else alone. Go (latest stable, pinned in `go.mod`), standard library
first, GCP Cloud Run with Firestore state. It is the **last writer** in a chain of Strava
automations (ActivityFix, Xert): it waits a configurable delay and names only activities nothing
else has titled.

This file is the single project-instruction file for every agent and reviewer. It is loaded
automatically at the start of every Claude Code session; nothing in it needs to be repeated in a
prompt.

## The spec of record

`HANDOFF.md` in the repository root is the specification of record. It is **local and untracked**
(gitignored) and it wins over this file, the README, and any prompt. Every session starts by
re-reading it: it changes between sessions, and a session that works from a stale reading of it has
produced the wrong code more than once.

Precedence when documents disagree: `HANDOFF.md` → this file → `docs/` → README → code
comments.
Code that contradicts the spec is a finding; a spec that contradicts the code is a finding. Either
way, report it and fix the spec first.

## Session protocol

Every build session follows the same shape. The shape is what has kept a security-relevant service
shipping weekly without a rollback.

- **Spec before code.** A behavioral change is written into `HANDOFF.md` before it is implemented,
  and the wording is the one the code is then held to. When a session's instructions and the spec
  disagree, the spec wins and the disagreement is reported. Never let one fact live in two places
  in the spec; the second copy is where the contradiction will be found.
- **Delegation is the default regardless of item count.** The main session specs, briefs, reviews,
  and merges; Opus subagents implement. One item still means one subagent — the separation of
  author and reviewer is the point, parallelism is the bonus. The main session implements directly
  only when the brief would be longer than the diff (dating a changelog, a one-line fix).
- **One git worktree per subagent.** A sibling directory on its own branch, created and removed by
  the main session (`git worktree add ../titelheld-<item> -b <branch>` … `git worktree remove`
  after merge). Subagents touch only the files their item owns. Shared files — `CHANGELOG.md`,
  `README.md`, `docs/`, CI workflows, `go.mod`, this file — are edited by the main session alone
  after the branches land, so anything two items would collide on has one author.
- **Briefs carry the spec.** `HANDOFF.md` is untracked, so a subagent in a fresh worktree cannot
  see it. Every brief quotes the relevant spec text and includes: spec-before-code, a negative
  control per behavioral change, the evidence rule, and the stop conditions below. House rules do
  not propagate into subagents on their own.
- **Whole-stack review before merging.** Run `/code-review` over the **entire** stack as one diff
  from the integrated top, not per PR. Findings that live between PRs — a fix in one undoing an
  invariant from another — are the ones per-PR review misses, and two of them have been caught
  that way. Resolve or explicitly decline every finding with a reason on the thread before the
  merge command. CodeRabbit runs per PR as well; its threads must be resolved, not dismissed.
- **Report at checkpoints, work in between.** A checkpoint report states what was done with the
  evidence that it works, what was deferred and why, judgment calls the operator should see, and
  the verified standing state (`DRY_RUN`, scheduler, subscription, release variables). A merge is
  not a deploy; say so when it matters.
- **File, do not decide.** Prompt tuning, model changes, franchise curation, and anything that
  changes what a title may say are the operator's calls. Write the finding up with evidence and
  the candidate directions; touch nothing until told. Fixes to bugs, security findings, and drift
  between spec and code are yours to make.

### Stop conditions

Stop and ask before any of these; none may be done on an assumption.

- Any Strava write while `DRY_RUN` is unset or `0` in development.
- Creating the push subscription, changing the OAuth binding, or any change to what is registered
  on Strava's side.
- Changing the scheduler's state. During **dry-run** observation windows sweeps are triggered by
  hand (resume → run → pause); a live observation week runs with the scheduler enabled. The
  scheduler's state is stated in every report either way.
- Adding a dependency. Allowed without asking: a polyline decoder, the Firestore client, an HTTP
  router. There are still none: OAuth, the HTTP client and routing are standard library.
- Creating paid GCP resources or anything with a nonzero monthly baseline.
- A schema migration once a collection holds data; deleting any file.
- Changing the LLM model. Model changes are decided by a controlled replay — logged prompts from
  real rides, several samples per candidate, same endpoint and settings — never by release date.
  A newer model is a candidate, not an upgrade.
- Running `terraform apply` while a release run is in flight (the writes gate reads the service
  before the deploy; an apply in that window defeats it).

### Evidence

- **Report the evidence, not the interpretation.** A precondition or error path prints the
  underlying tool's own message. A DNS block reported as "service does not exist" cost two rounds
  in IAM once; the check now prints gcloud's words and the next failure was diagnosed on first
  read.
- **A green check proves nothing without a failing case behind it.** Attestation verification is
  reported alongside the wrong digest and wrong signer that exit non-zero; a gate is reported with
  the run that would have refused. State what could not be verified and why.
- **Verify in the environment that runs it.** A README snippet tested in bash and shipped to a
  zsh operator, a Docker Hub hook that worked only where it was written — "verified in the wrong
  environment" is a named failure class here. Say which environment a thing was verified in.
- Every prompt the LLM sees is logged whole while `LOG_PROMPT` is on (independent of `DRY_RUN`),
  and every Vertex call logs its token usage. Judge a title against the prompt that produced it,
  never against the counters alone.

## Where things live

- `internal/classifier/` decides which naming tier an activity belongs to and what may be done
  with it. No HTTP, no Firestore, no Strava SDK — pure rules over an `Activity` value.
  `activity.go` is the transport-neutral input, `defaults.go` the Strava default-title table and
  `IsDefaultTitle`, `classifier.go` the `Config`, tiers, actions, and `Classify`.
- `internal/config/` reads every setting from the environment and nothing else, so a secret has
  no route into the working tree. `DRY_RUN` is parsed here and defaults to on. `MAX_INSTANCES` is
  parsed here too and is *required* to serve: it must be `1`, because four pieces of in-process
  state — the OAuth state map, the first-bind lock, token-refresh serialization and the sweep
  lock — are correct only while one instance serves. Terraform feeds the same local to
  `max_instance_count` and to the variable, so the ceiling and what the binary believes cannot
  drift. It is a deployment check and not a mutex: the ceiling is per revision, so a rolling
  deploy or a traffic split has two instances that each read `1` — `config.RequiredMaxInstances`
  says what that leaves exposed. An import does not require it: it is a deliberate second process
  that touches none of the four.
- `internal/strava/` is the only package that talks to Strava: OAuth (`oauth.go`), the
  auto-refreshing token source (`tokensource.go`), the HTTP client with rate-limit accounting and
  429 backoff (`client.go`), and the calls this service makes: the activity read, the two rename
  writes and the description-only write that takes the attribution line back out
  (`activity.go`), and the gear read a franchise matches on (`gear.go`). The description-only
  write omits `name` from the form rather than sending the stored title back: Strava rewrites a
  title it reads as containing a link, and what is not in the form cannot be rewritten. Gear is
  *read*, never written — the name is a string the athlete typed into Strava, and a franchise
  keys on it.
- `internal/store/` holds the persistence interfaces, an in-memory implementation, and
  `storetest`, the conformance suite both implementations must pass. `internal/store/firestore/`
  is the persistent one; its IAM is documented in `docs/firestore-iam.md`. `NamedLog.Named`
  returns the whole row rather than the title alone — a reconcile decides from the source — and
  `MarkNamed` replaces a row rather than refusing a second write, which is what lets a second
  rename be recorded.
- `internal/webhook/` serves the subscription handshake and the event intake, and queues
  activities. It acknowledges before it touches the store. It writes nothing else: a POST carries
  no signature Strava will vouch for, so a forged event may at most enqueue an activity ID, which
  the sweep re-validates against Strava. Recording anything from a request body would spend that —
  which is why an update event on an *already named* activity is queued too, rather than having
  its `updates.title` written anywhere. Which of the two a sweep performs, naming or reconciling,
  is decided from the named log at sweep time and is not carried in the entry: a queue entry
  holds an athlete, an activity and a deadline, plus `Pending.Aspect` — `create` or `update`,
  checked before the enqueue — and provenance that nothing reads. No free text from a request
  body has ever reached the store.
- `internal/server/` assembles the HTTP surface: health, the one-time OAuth bootstrap, and the
  webhook at its unguessable path.
- `internal/app/` wires everything together and serves; it takes `getenv` as a parameter so the
  whole service can be started in a test. `sweepDeps` is the one place configuration becomes the
  processor's dependencies, and the defaults a deployment gets are resolved there — the shipped
  banned-word list when the environment names none, the shipped machine titles likewise. Assert
  those through `config.Load` and `sweepDeps`, never a hand-built `config.Config`: a config a test
  builds agrees with itself and cannot notice a default that never reaches production, which is
  how the banned-word list shipped empty.
- `internal/logsafe/` neutralizes untrusted text before it reaches a log. Use it for anything
  from a webhook body, Strava, a geocoder or an LLM. `String` flattens newlines and caps at 256 —
  right for a title, which must not forge a second line. `Block` keeps newlines and tabs and caps
  at 16 KB, for a value whose line structure *is* the content: the naming prompt, logged whole
  while `LOG_PROMPT` is on. It defaults to on and is independent of `DRY_RUN` — deriving it from
  the write mode took the prompts away at the moment writes began, which is when they are worth
  most.
- `cmd/titelheld/` is a shim with no behavior, and is excluded from coverage.
- `internal/importer/` seeds the title history from Strava; `cmd/titelheld-import/` is its shim,
  like `cmd/titelheld`. A one-shot run by hand, not a route: the service is invokable by
  `allUsers`, so an endpoint that exists for a job run once is
  surface for no reason. Idempotent and resumable with no state of its own — an activity already
  in the named log is left alone. It seeds rides only, and of those only what the athlete wrote:
  Strava defaults, machine titles, anything Zwift or Xert titled, and this service's own commute
  and errand templates are all skipped. The template list is read from the athlete's
  configuration, so the list a run skips is the list that run would write. None of this widens
  `MachineTitles` — that set decides what may be *overwritten*, and a Zwift ride keeps its title.
  **Precondition:** nothing else may be naming while an import runs. Pause the scheduler first.
- `cmd/titelheld-config/` seeds the athlete's `config` document from the shipped default profile,
  reserving what `-reserve` names, and refuses if one exists — the document is authoritative from
  its first write and is edited in place after that. Store-only: no Strava client, no token
  refresh, and `config.LoadStore` reads nothing but where Firestore is. The wiring is
  `app.SeedConfig`; the read-back goes through `processor.FranchisesFromStored`, the sweep's own
  conversion.
- `internal/naming/` builds the prompt, validates what comes back, and holds the three LLM
  implementations behind one interface it defines itself. No HTTP client of its own beyond the
  providers, no Firestore: a caller assembles a `Ride` and supplies a `Provider`. **The prompt
  states constraints; the validator enforces them**, because an instruction to a model is a
  request and the ride description is text this service did not write. Gemini goes through Vertex
  AI with the runtime SA's ambient credentials and has no key at all. `anthropic` calls the
  Messages API with `LLM_API_KEY`. `openrouter` is an OpenAI-compatible chat-completions client
  against `LLM_BASE_URL` (default OpenRouter's, https required — the key travels in a header to
  that host), model via `LLM_MODEL`, the same `LLM_API_KEY`: one key, many narrators, so a voice
  miss is diagnosed by re-sweeping the same queued ride under another model — a Terraform variable
  change, not a release. Same title contract, same validator, no provider-side JSON mode — a
  parameter one narrator rejects would contaminate an A/B. Dormancy is tested, not assumed: with
  `LLM_PROVIDER` unset the real loader resolves Vertex and reads no key, and `openrouter` without
  a key refuses at startup naming both variables. Model IDs are pinned and carry the doc URL and
  date they were verified against.
- `internal/geo/` decodes the summary polyline and reverse-geocodes samples via Nominatim into
  verified place names. The privacy allow-list and the 1 req/s limit live there and are enforced
  in code, not by convention. The naming layer receives a `Summary` that has no field a
  coordinate could occupy.
- `internal/processor/` is where the pieces meet: it drains the due queue, classifies, gathers,
  prompts, validates and writes. Per-activity failures are isolated — one bad ride never stalls a
  sweep — and the named log is written *before* the title is sent, so the failure mode is a ride
  that keeps its default title rather than one renamed twice. `reconcile.go` handles an activity
  already in the log: it re-reads the activity and records the title *Strava* holds, never one an
  event claimed. Three endings — the recorded title, in which case nothing changed; a title a
  person typed, which the row becomes under `SourceHuman` and dated by the ride, except that a
  `SourceImported` row keeps its source so a rename cannot promote a decade of shorthand into
  EXAMPLES; and anything else, which is not recorded, because `SourceHuman` feeds the few-shot
  examples and a row claiming the athlete named a ride "Morning Ride" would teach exactly that.
  Once the row is the athlete's, the attribution line comes out — keyed on the row's source rather
  than on what this sweep did, which is what makes it converge after a crash and after dry run. A
  re-read is always current, so no stale echo has to be suppressed and a second rename is recorded
  like the first; the delay is retained because it collapses a burst of edits and does not race an
  athlete who is still typing.
- `internal/sweep/` serves the scheduled drain. It verifies Cloud Scheduler's OIDC token itself —
  a Bearer token that validates against the configured audience, an issuer of
  `https://accounts.google.com`, and an `email` that is the scheduler service account with
  `email_verified` true — because `allUsers` holds `roles/run.invoker` and the platform
  authenticates nobody. The response is a bare 401. The log names the issuer, email and
  `email_verified` failures individually; signature, expiry and audience arrive as one error from
  the validator and are reported as it words them, with the audience that was required. Known
  limit: the per-claim rejection tests inject a stub validator; nothing can prove offline that the
  wired `idtoken.Validate` rejects a forged Google signature.
- `docs/` carries what a deployment needs and the README does not: every setting in
  `configuration.md`, the GCP deployment in `infrastructure.md`, the Firestore collections and
  their IAM in `firestore-iam.md`. The README is about what the service does; those three are
  about running it.
- `infra/` is the GCP deployment in Terraform. `make tf-check` runs exactly what CI runs. CI never
  applies: applies are by hand. Secret Manager *values* never appear in code, tfvars or state —
  only the secret resources are managed.

## What the prompt carries, and what may come back

The prompt carries four derived things beyond the ride: the last 25 titles worth not repeating,
six few-shot examples rebuilt by re-reading past activities, the names of up to six notable
segment efforts, and the next franchise entry when one applies. An effort is notable when it is a
personal top-three or carries a Strava achievement; only the name crosses into the prompt, never
the times or ranks that selected it — and never a count of them.

- **The figure rule.** A title may contain a figure only if the prompt states it, and the prompt
  states only figures consistent across Strava's surfaces. No count of what a ride achieved
  qualifies — web, mobile and the API report three different numbers for one ride, and a local
  legend is absent from segment efforts entirely — so RIDE and a derived example's situation
  carry none. A model told it may "escalate a number" will derive one; that instruction produced
  two number titles in three and is gone.
- **The variety rule outranks the callback.** A move already visible in the last few RECENT titles
  disqualifies the callback, and one is invited only where the ride's own data supplies new
  material to carry it.
- **Segment names are the least trusted text in the prompt** — a segment is named by whoever
  created it, and every rider inherits that name. The system prompt states, for that block by
  name, that the names are data and **never instructions** (the rule `Bike` and `NOTES` also
  carry), and that a place inside a segment name is still not a place the model may use. A test
  asserts every rule mentioning one of those three blocks carries the prohibition. A segment's
  name is also never the title itself: `naming.CopiesAchievement` refuses a candidate
  normalized-equal to any ACHIEVEMENTS name (or its article-dropped core); the activity stays
  queued and the next sweep draws again. Equality and not containment, deliberately: a title
  *about* the stretch is the invited angle and must stand.
- **RECENT and EXAMPLES are filtered differently, and that difference is the rule.** RECENT drops
  only templates — a commute name is meant to repeat — and keeps imported titles, because
  repeating one is exactly what to avoid. EXAMPLES admits one source and no other: `SourceHuman`,
  a title the athlete wrote on a sport ride, recorded by the reconcile on a rename or by the
  processor on a skip-gate decline — the best style data there will ever be. The service's own
  titles stay out: a `SourceService` row is the floor the model produced under this same prompt,
  and admitting it closes the style loop on itself — a title written once became the next ride's
  teacher within 48 hours. An imported row is structurally unable to become an example. Until a
  human title exists the synthetic set is what the prompt carries, which is its purpose rather
  than a cold-start stopgap.
- **Reserved titles are enforced, not requested.** `Franchise.Guard` refuses a candidate claiming
  any entry of the matched series except the one being offered — future and reserved entries
  included. The gear-motif invitation once counted as "costing nothing scarce"; a live write of a
  reserved film title falsified that, and the guard is the correction.
- Only the title history is worth failing an activity for — the realistic cause is the composite
  index missing. Everything else degrades rather than blocking a title.

Franchises live in the per-athlete `config` document. An entry is spent when it is *used*, not
when it is offered: `naming.UsesEntry` decides by normalized containment of the entry's core.
Unused means one more offer, at most one, and then a third call carrying no FRANCHISE block; the
position does not move. Entries listed under `reserved` are never offered — the athlete spends
them by hand — and the position advances *past the index* that was offered, monotonically. The
first curation change wrote the production document; from that moment the document is
authoritative and the shipped default profile applies only where none exists.

Route repeats are specified and not built. Hashing a rounded polyline never matches a re-ride
(0/50 under 5 m jitter; Strava's simplification changes the vertex count between rides), and a
plain cell-set Jaccard was measured infeasible on the athlete's real rides at 100 and 200 m. The
current design — route families with rarity-weighted overlap and a novelty share — is in
`HANDOFF.md` step 3 with its measured constraints, and is the subject of a bake-off: an
implementation written blind against that spec, judged against an external reference
implementation on pre-registered criteria. Do not read the `spike/route-cells` *branch* — it is
not a directory in the tree — while implementing it.

## Design rules that are not negotiable

- **The title, and one line of the description.** Sport type, gear and workout summaries belong
  to other tools and are never touched. The description is touched in exactly two ways, and they
  are the same line: the attribution line is prepended to activities this service titled, and it
  is removed again once the record says the title is the athlete's. Both are a read-modify-write
  that preserves every other byte; the removal takes the exact line and the blank line after it
  and nothing else. Nothing at all is ours to change on an activity we did not name.
- **Fail closed.** An unrecognized title means someone else named the activity; skip it. Never
  widen the default-title table by guessing. The same rule decides what a reconcile records: a
  title that is not recognizably a person's is not recorded as one.
- **Tier before gate.** `Classify` assigns a tier regardless of the current title, then the
  default-title gate decides whether an action may write. Keep those two concerns separate.
- **Core packages stay dependency-free.** `classifier` (and the packages that follow it) must not
  import HTTP, Firestore or a Strava SDK. The Cloud Run `main` wires everything together.
- **Config is data, per athlete.** Thresholds, `zwift_mode`, geofences, banned words and
  franchises are configuration, not code, and every field is safe at its zero value. Franchises
  live in the `config` collection now; the rest still ship as defaults in code and follow into
  the same document.
- **Every behavioral test carries a negative control.** Break the behavior, watch the test fail,
  restore it. A test that passes either way proves nothing, and several here did: an idempotency
  test that passed because the classifier declined the replay rather than the named log; a
  tolerance test whose uniform offset happened not to cross a cell boundary; an index guard that
  accepted `athlete_id` descending; a trailing-data check defeated by padding to the byte cap; a
  round-trip test that only round-tripped descriptions this service produces. Assert against the
  thing itself, never a stand-in you control: the Firestore emulator serves queries no composite
  index exists for, so a wrong index declaration passes every test here and fails only in
  production. A control that breaks the build is not a control — `go test` reports a compile
  error as FAIL too.
- **Dry run is the safe zero value.** `strava.WriteMode` and `config.Config.WritesEnabled` are
  both written so that an unset or zero-valued configuration cannot write to Strava. Writes are
  refused in `UpdateActivityName` *and* again in the transport; keep both. An unparseable
  `DRY_RUN` refuses to start.
- **Human titles win, permanently.** A human-titled activity is never renamed, never prefixed,
  and its title is recorded so the model can learn from it. Silence is not approval: a
  service-written title that nobody renamed is still the floor, not a teacher.

## Operations

- **A release is a signed `v*` tag pushed by hand.** Changelog branch with the dated entry →
  PR → merge → signed annotated tag → verify digest against attestation, non-vacuously. Nothing
  in CI can start one; `release.yaml` has no `workflow_dispatch`. No bot may release:
  release-please was rejected because its unsigned commits fail the signed-commits rule and a
  `GITHUB_TOKEN` PR never triggers the required checks. A merge is not a deploy.
- **The writes gate.** Before deploying, `release.yaml` reads `DRY_RUN` off the service the deploy
  will land on and refuses if writes are on: the job sets the image and nothing else, so the
  existing service predicts the new revision's environment. The escape hatch is the repository
  variable `WRITES_ACKNOWLEDGED`, a *date* (`YYYY-MM-DD`, accepted within 7 days before the tag
  and 1 day after), re-dated for every writes-enabled release so one left behind expires on its
  own. It is the second deliberate act after the Terraform change that sets `DRY_RUN=0`. Both
  checks live in `.github/scripts/writes-gate.sh` and are *executed* by `release-logic.yaml`
  against a stubbed `gcloud` — a linter reads a workflow and only a test can run one.
- **Rollback** is a documented procedure, not an emergency: `dry_run = true`, apply, writes off,
  the queue resumes accumulating. Pausing the scheduler by hand is the faster act when the fault
  is in what titles say rather than in whether they are written; reconcile the drift in the next
  infra PR.
- **Manual sweep** (observation windows, incident response): `gcloud scheduler jobs run` refuses
  a paused job, so the sequence is resume → run → pause, with explicit flags — the shell the
  snippet was verified in is named in the README. Dry-run rides stay queued and are re-evaluated
  on every sweep; that is what lets a flip name the rides that were reviewed, and why the
  scheduler stays paused during dry-run observation.
- **Model switches** are a tfvar (`llm_model`, `vertex_location`) and an apply, not a release —
  decided by replay, per the stop conditions.
- **Production state changes by hand** (a row edit, a wipe, a config document) are guarded
  one-offs that print their preconditions and their evidence before and after, run with the
  operator's ADC on the operator's word, and are not kept as standing tooling when their
  precondition expires.

## Daily flow

- `make` runs lint, test and build. `make lint` is `go vet` plus golangci-lint at the version
  pinned in the Makefile; `make test` runs with `-race` and coverage; `make vulncheck` runs
  govulncheck; `make release-logic` runs the release's own decision tests.
- Install hooks with `pre-commit install` or `prek install`.
- Core packages are expected to stay above 95% statement coverage. Coverage is not a target for
  OAuth flows and workflow scripts; the fixture-driven end-to-end and idempotency tests are.

## CI and quality gates

- `go.yaml` — build, race tests with coverage, `go mod tidy -diff`, golangci-lint, gofmt check,
  govulncheck, and the Codecov upload (isolated in its own job).
- Additional workflows cover CodeQL (Go and Actions), dependency review, OpenSSF Scorecard,
  TruffleHog secret scanning, pre-commit hooks, PR-title validation and test-result publication.
- `terraform.yaml` formats, validates and lints `infra/`; it holds no credentials and never
  applies. A fork's pull request gets `fmt -check` only. `docker.yaml` builds the image on every PR
  and push to main, scans it with Grype, and publishes nothing. `rescan.yaml` rebuilds the newest
  released final tag daily and rescans it, and fails on any fixable finding.
- `release.yaml` verifies the tag and the changelog, calls `release-image.yaml` to build and attest
  the image once, and deploys that **digest** to Cloud Run through Workload Identity Federation —
  no exported service-account key exists. Build and attest share one reusable file because signed
  provenance names the workflow file, which is what makes the SLSA Build L3 claim.
- Every workflow hardens the runner with a blocked egress policy and pins actions by commit SHA.
  Renovate keeps the pins and the Go toolchain current. New egress needs surface only when a step
  first runs; expect the first execution of any new step to add an allowlist entry.

## Privacy rules

- This is a **public** repository for a service that handles one person's GPS data. Fixtures,
  tests and examples use synthetic coordinates and fake identifiers only; the athlete's rides
  start at a front door.
- Titles are never derived from reverse-geocoded points of interest. Errand titles come from a
  template pool that consults no geography at all. Sport-ride titles may use only place names the
  geocoder verified and segment names as they stand.
- The geocode cache and any future route cell sets are plaintext location data, accepted for the
  single-athlete deployment. Multi-user is the trigger to key them or drop them.

## Gotchas

- Make recipes run under `bash -eu -o pipefail`; avoid zshisms. The operator's shell is zsh.
- Commit style: Conventional Commits (`<type>[optional scope]: <description>`), signed off with
  `git commit -s` and cryptographically signed. Unsigned commits cannot reach `main`.
- Markdown style: aim for 100-character lines and wrap prose at sentence boundaries. Do not reflow
  code blocks, tables or long URLs. Update the README table of contents when sections change.
- The Strava API's `start_date_local` carries a fake `Z`; treat it as wall-clock, not UTC.
- Strava delivers webhook events at least once and in no guaranteed order. Nothing may depend on
  an event arriving once, first, or at all.
