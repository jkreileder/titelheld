# Changelog

<!-- markdownlint-disable MD024 -->

Notable changes, newest first. Versions are [SemVer](https://semver.org/); the project is
pre-1.0, so the minor number moves for anything that would otherwise be a major bump.

Releases are cut by hand from a signed tag — see
[Cutting a release](README.md#cutting-a-release). The release workflow refuses to run while the
section for the tag being released still says *Unreleased*, so dating this file is a required
step rather than a habit.

## [v0.7.5] – Unreleased

The segment-copy loop is closed. Within two days of going live on a new narrator, a segment name
was written verbatim as a title, recycled into the few-shot examples as the service's own row,
and the next ride's title was the sibling segment — the prompt's callback invitation scoring the
copy highest. Model-independent, so the fix is structural: what teaches, what is refused, and
what a callback may build on all change.

- **Few-shot examples come from the athlete only.**
  EXAMPLES admits `SourceHuman` rows and no other; the synthetic set remains the fallback until
  one exists. A title this service wrote is the floor the model produced under this same prompt,
  and admitting it taught the model its own weakest habit within 48 hours. Service titles stay
  in RECENT — repeating one is still the thing to avoid — and the human channel grows the
  corpus: every rename the reconcile records, every skip-gate decline.
- **A segment's name is never the title itself.**
  The prompt says so, and the pipeline makes it binding the way the franchise guard binds its
  rule: a candidate normalized-equal to any ACHIEVEMENTS name — or to its article-dropped core —
  is refused, the activity stays queued, and the next sweep draws again at temperature. Equality
  and not containment, deliberately: a title *about* the stretch is the invited angle and stands.
- **The variety rule outranks the callback.**
  The prompt no longer prefers a callback to a fresh idea. A move already visible in the last
  few titles under RECENT disqualifies it, and a callback is invited only where the ride's own
  data supplies new material to carry it — an unconditional preference scored the segment-name
  copy highest two rides in a row, because continuing the copy *was* the callback.

## [v0.7.4] – 2026-09-01

The newest Flash models are reachable without leaving Europe. A probe on 2026-08-31 found
`gemini-3.7-flash` and `gemini-3.6-flash` served from the EU multi-region as well as from `global`,
and from no European region — so the choice is no longer between a newer narrator and keeping a
prompt built from the athlete's GPS traces inside Europe.

- **`VERTEX_LOCATION=eu` reaches the EU multi-region.**
  Its host is spelled `aiplatform.eu.rep.googleapis.com` rather than by the
  `LOCATION-aiplatform.googleapis.com` pattern every region follows, and the name that pattern
  would build serves nothing at all — so `eu` is a case the endpoint builder handles, next to
  `global`. The startup check that bounds `VERTEX_LOCATION` admits `eu` by name rather than by a
  shorter lower bound, so `us` — a real Vertex multi-region this service has no host for — is
  still refused there instead of failing as a 404 on the model.
  The shipped default is unchanged: `europe-west3` and `gemini-3.5-flash`, with a newer model an
  opt-in through configuration.
- **The output ceiling moves from 256 tokens to 1024.**
  Gemini 3.x cannot be stopped from thinking, and thinking tokens are billed inside
  `maxOutputTokens`: `thinkingBudget: 0` buys a best-effort minimum of a few hundred tokens
  instead of none, and three of four live calls at the old ceiling ended at `MAX_TOKENS` with a
  truncated title. The request still asks for a zero budget, which is the minimum-thinking
  spelling on both model generations — `gemini-3.5-flash` honors it exactly.
  Anthropic and OpenRouter share the constant, where it remains what it reads as: a bound on a
  runaway response.
- **Terraform carries the location.**
  A `vertex_location` variable reaches the container only when it is set, the same way the three
  `llm_*` variables do, so a deployment that names none is byte-for-byte what the loader's
  dormancy test sees.

## [v0.7.3] – 2026-08-31

The athlete's rename becomes a record, from the one place that can check it. The capability was
built once for v0.7.2, reviewed, and **dropped before release** on a security finding; the finding
is this design. Strava signs no webhook `POST` — the verify token is checked on the `GET`
handshake, Cloud Run grants `allUsers`, the unguessable path is the whole protection — so
recording `updates.title` would have let anyone holding that path write text, as the athlete, on
any already-named activity, where a planted row is eligible to become a few-shot example the model
is told to imitate.

- **Intake keeps its trust class; the sweep does the work.**
  An update event on an activity already in the named log now queues it to be *re-examined*
  instead of being dropped. Nothing else changes at the intake: a queue entry holds an athlete, an
  activity and a deadline, and still holds nothing a request body said. A forged or replayed event
  therefore costs one queue entry and one redundant read, which is the trust class the service
  had before and is worth keeping.
  The sweep re-reads the activity and records **the title Strava holds**. Which of the two things
  a sweep does — name, or reconcile — is decided from the named log at sweep time rather than
  carried in the entry, so nothing about the decision comes from an unauthenticated caller.
- **Three limits dissolve rather than being carried across.**
  A re-read is always current, so there is no stale echo to suppress: the redelivery the review
  found — this service names a ride, the echo's acknowledgement is lost, the athlete renames, the
  echo arrives again claiming a title that is no longer live — resolves against Strava like any
  other event.
  The source gate that closed it is superseded. It is no longer needed as a stand-in for
  authentication, and its *other* justification does not apply either: a row is now replaced
  whatever it said before, because a renamed commute's template row has to follow the rename or it
  records a title that is not on Strava.
  **A second rename is recorded**, which the intake design documented as a limit it could not
  lift. And the 256-rune bound stops being load-bearing, because the title's provenance is Strava.
- **An imported row is still structurally unable to become an example.**
  The title follows the rename like any other — the row would otherwise record a title that is not
  on Strava — but the source stays `imported`. Tidying up a ten-year-old ride does not make its
  title current voice, and promoting the row would put the bare town names and private shorthand
  the import exists to keep out of `EXAMPLES` straight back into them. `RECENT` takes every source
  and sees the new title regardless.
- **A deleted activity leaves the queue.**
  A `404` from Strava is a finished entry rather than a failed one, on the naming path as well as
  the reconcile path. An activity that was deleted cannot come back, and the entry would otherwise
  spend one request every five minutes forever against a budget of a hundred per fifteen minutes.
  The reconcile path made this newly reachable — a rename queues work, and the delete that follows
  is ignored at intake — but the naming path had it too.
- **A title Strava holds is still not automatically a person's.**
  A Strava default the athlete reverted to, another tool's overwrite, and this service's own
  commute templates are recognized and not recorded — the filter the history import already
  applies. `SourceHuman` feeds the few-shot examples, so a row saying the athlete named a ride
  "Morning Ride" would teach a model to answer with a Strava default.
- **The attribution line comes out when the title stops being ours.**
  The line says this service named the activity; after a rename that is false. It is removed by a
  description-only write — the exact line and the blank line after it, every other byte left where
  it was. A *line*: the same text inside a sentence the athlete wrote is stepped over rather than
  cut out of the middle of one, and an older wording of the line is left alone, because the
  presence check matches the URL on purpose and deleting text has to be exact.
  The write omits `name` from the form rather than sending the stored title back: Strava rewrites
  a title it reads as containing a link, and a title this service no longer owns must not make
  that round trip.
  Removal is keyed on the row's source rather than on what one sweep did, which is what makes it
  converge — after a crash between the record and the write, and after dry run, where the row is
  recorded, the write is withheld and the entry stays queued so that turning writes on finishes
  the job.
- `store.NamedLog.Named` returns the row rather than the title alone; `processor.Result` gains
  `Reconciled` and the sweep's JSON reports it.

## [v0.7.2] – 2026-08-30

The counts release. A ride was titled **"Neun auf einen Streich für den Silbernen"**, and no two
surfaces agreed on what it had achieved: the Strava web feed showed 3, the mobile feed 5, and the
API's `achievement_count` 9 — the last of which is where the nine came from, and the only one the
athlete could not reach by counting. The prompt had stated `Personal records: 1` and `Other
achievements: 8`, and the model added them.

- **The figure rule.**
  A title may contain a figure only if the prompt states that figure, and the prompt states only
  figures consistent across Strava's own surfaces.
  No count of what a ride achieved qualifies, so the tallies leave the prompt: `Personal records`
  and `Other achievements` go from `RIDE`, the PR and achievement counts go from a derived
  example's situation, and `Ride.Records`, `Ride.OtherAchievements` and `countEfforts` are deleted
  rather than left unprinted.
  The athlete counts five for that ride, two of them local legends — which appear nowhere in the
  segment-effort data this service reads, so no arithmetic over it could ever have reached their
  number. The counts were not bucketed wrongly; they came from a source without the answer.
  The `ACHIEVEMENTS` names stay: a segment name is checkable by opening the ride page, which is
  exactly what a tally is not.
  No every-integer-must-be-a-stated-figure check is built — with no figure in the prompt there is
  nothing for a title to quote.
- **The variety rule replaces the escalation instance.**
  The system prompt told the model to write those titles: *"escalate a number when this ride's
  figures support it — after 'Fünf auf einen Streich', a ride with eight records is 'Acht auf einen
  Streich'"*, with the title it names sitting permanently in `RECENT` as an imported row. Two of
  the three LLM titles judged since go-live were number titles. The specification was the
  operator's; the model did what it was told.
  In its place, pre-registered before the evidence: **a move already visible in the last few
  `RECENT` titles is a reason to choose a different angle**, because a pattern repeated is a
  formula and not a voice. The invitation to build on `RECENT` stays.
  The synthetic escalation example is deleted rather than reworded, and the two rules that pointed
  at the retired counts are rewritten. `TestDataGuardsAreVerbatim` pins the variety rule word for
  word.
Not in this release, by decision: **turning the athlete's rename into training data.** It was built
and dropped. Strava does not sign its webhook POSTs, so recording `updates.title` straight into the
named log would have let anyone holding the unguessable path write text as the athlete's on any
already-named activity — where a planted row is eligible to become a few-shot example. Intake's
trust class is that a forged event can at most enqueue an activity ID, which the sweep then
re-validates against Strava; that is worth keeping. v0.7.3 builds it sweep-side, where the title
recorded is the one Strava actually holds. Removing this service's attribution line when a rename
means the title is no longer ours belongs in the same place, for the same reason.

## [v0.7.1] – 2026-08-29

The first incident under real writes, and what it exposed.
On 2026-08-29 the service wrote **"Son of the Pink Panther"** to a real activity — a film the
athlete had reserved, on a ride the rotation had deliberately offered nothing for.
Nothing malfunctioned: reserving governs what is *offered*, and nothing had ever governed what a
model may *produce*.

- **A title that claims a franchise entry is refused.**
  The gear-name motif rule invites a title to take color from the bike, and a series named after
  that bike shares its vocabulary, so a model reaches a film's exact wording with no franchise
  offered at all — three of three calls on the incident ride reached for the series.
  Ask and enforce, as everywhere else: the prompt now says the athlete's franchise entries are
  theirs to spend, and a check after the model has spoken refuses the title that ignores it.
  Every entry of the matched series is guarded except the one being offered — reserved, already
  spent, and *future* entries alike, because claiming one spends a film the position will not
  record and the rotation would offer again.
  The comparison is the one that decides spending, so casing, punctuation and an adaptation are
  all the entry.
  An entry that says no more than the bike's name is matched by equality instead, so a title
  merely themed on the bike stays legal and only the named work is not.
  A refusal fails the activity and leaves it queued; the next sweep draws again.
- **The first JSON value is decoded, not the whole response.**
  On two of three calls the model returned well-formed JSON followed by one stray closing brace,
  and both were rejected as "response is not JSON" — ten minutes and two model calls spent on a
  byte that says nothing about the title inside the object.
  Worse than the cost: each rejection re-rolls, so the title that lands is the first that parses,
  which is a sampling loop nobody chose.
  The schema is still enforced strictly; the leniency is about where the object ends.
  Trailing text is reported and logged rather than discarded, because a provider that trails bytes
  is drifting.
- **`LOG_PROMPT` is its own flag, on by default.**
  It used to follow the dry-run state, so prompt logging switched itself off at the moment writes
  came on — and the first incident under writes had to be diagnosed from counters, which say how
  much the prompt carried and never what it said.
  It no longer consults `DRY_RUN`; `LOG_PROMPT=0` turns it off.
  Terraform sets nothing, so this needs no infrastructure change.
- **The public docs describe the live state.**
  The scheduler fires every five minutes and `DRY_RUN` is `0`, so the README's paused-and-dry-run
  callouts, copilot-instructions and `configuration.md` say what binds now — including that the
  paused scheduler is gone from the reasons `max-instances=1` is tolerable in practice.
  Turning writes off again is written down where turning them on is, and says what a rollback does
  not undo.

No infrastructure change ships with this release. The Cloud Scheduler job was paused by hand
during the incident, which is drift from Terraform's `paused = false`; unpausing by hand after the
deploy is what resolves it.

Releasing onto a service with writes enabled still needs `WRITES_ACKNOWLEDGED`, re-dated to the
day of the release: the gate reads `DRY_RUN` off the running service and refuses without a value
inside the window. See
[Versions do not turn on writes](README.md#versions-do-not-turn-on-writes).

## [v0.7.0] – 2026-08-26

The voice release: the last build before writes are enabled, aimed at one thing the dry runs showed.
The prompt carried the records, the callback material and the examples, and the model wrote route
descriptions anyway — twice.
Strength and salience are the levers, and the style loop needed the athlete in it.

- **A human title is remembered, not merely skipped.**
  The skip gate dropped a hand-titled ride without a trace, so a title live on the athlete's feed
  was invisible to the no-repeat list and could be invented a second time.
  A sport ride declined for an unrecognized title is now recorded in the named log as source
  `human`, dated by the ride: it joins `RECENT`, and — see the next item — the examples.
  Nothing is written to Strava for it, structurally: the recorder runs on the skip path, which never
  reaches the writer, so neither the title nor the attribution line can follow.
  The row is also the dedup record, which makes the ride final; this service is the last writer and
  never overwrites a person.
  Sport rides only — a commute that ActivityFix titled classifies as an errand and stays out — and
  only a person's title: a tool's title on an outdoor ride and a template typed by hand are left
  unrecorded, by the same filter the import applies.
  The recorder runs in dry run too, because the ride is final whatever the write mode.
- **The athlete's own titles teach the few-shot examples.**
  Eligibility becomes `service` ∪ `human`; `imported` stays barred.
  Admitting only the service's own titles closed the style loop on itself — cold-start blandness
  would have become its own teacher — while the imported rule's target was a decade of shorthand
  that teaches a model to answer with a town name.
  Run an import *after* the first sweep that reaches a hand-titled ride, not before: the sweep files
  it as `human`, an import that gets there first files it as `imported`.
- **A derived example shows the cause of its title.**
  Its situation now carries the salient numbers — personal records, other achievements, and the
  difficulty parsed from the description — so *Fünf auf einen Streich* beside *5 PRs* is a
  demonstrated move rather than an arbitrary association.
  Numbers only; a segment name never enters an example.
  A situation is longer than a title on purpose and now has its own bound, so those numbers reach
  the model instead of being cut at the title limit.
- **The prompt asks for callbacks and data-driven angles instead of allowing them.**
  `RECENT` is offered as material to build on — continue a series, answer a title, escalate a number
  with the arithmetic spelled out — and a callback that fits is to be preferred.
  Achievements become a candidate angle on equal footing with geography, and a route description is
  named the fallback.
  The ride carries its own counts — `Personal records`, `Other achievements` — from the same rule
  that counts them for an example, so a title that escalates a number escalates the figure and not
  the length of the capped `ACHIEVEMENTS` list.
  `RECENT` and `EXAMPLES` are declared data, never instructions, like the other untrusted blocks.
  Every data-never-instructions guard is kept word for word, and a test now pins them.
  A seventh synthetic example demonstrates an escalation callback with the cause on both sides of
  the arrow — which exposed a test that had counted the six synthetic examples and called it
  bounding.
- **The configuration document is seeded, not typed.**
  `cmd/titelheld-config` creates the athlete's `config` document from the shipped default profile
  with every `-reserve` entry added, reads it back through the sweep's own conversion, and refuses
  if a document exists.
  Its first use reserves *Son of the Pink Panther* alongside *Trail*: with exactly one offerable
  film left, the automation had a franchise in name only, and the containment check can verify
  *used* but never *used well*.
  The series therefore offers nothing until the athlete un-reserves or extends it, and the document
  is authoritative from now on.

- **A third LLM provider, `openrouter`, for an A/B of narrators on the same ride.**
  An OpenAI-compatible chat-completions client against `LLM_BASE_URL` — OpenRouter's root by
  default, https required because the key travels in a header to that host — with the model in
  `LLM_MODEL` and the key in `LLM_API_KEY`: one key, many narrators.
  A voice miss is diagnosed by re-sweeping the queued ride under another model, which is a
  Terraform variable change and not a release.
  Same title contract, same validator, no provider-side JSON mode.
  The shipped model, `anthropic/claude-haiku-4.5`, was verified against OpenRouter's live catalog
  on 2026-08-26.
  Dormant until asked for, and tested as such: with `LLM_PROVIDER` unset the resolution is Vertex
  and no key is read; `openrouter` without a key refuses at startup, naming both variables.

### Not in this release

- **Route repeats.**
  Still specified and unbuilt.
- **The rest of the configuration.**
  Tiers, geofences, banned words and language preferences are still the defaults shipped in code.
- **The provider A/B itself.**
  The provider ships; `LLM_API_KEY` stays unset until an A/B is actually needed, and no secret
  value exists.

## [v0.6.0] – 2026-08-23

What an external review found, and what the tests could not have.
Five changes, four of them defects that every existing test agreed were fine — because each test
built the thing it was testing.

- **The deployed service banned no words at all.**
  `BANNED_WORDS` unset produced an empty list and the empty list went straight to the validator,
  while the configuration field's own comment said "empty means the shipped list".
  Nothing anywhere made that true.
  The fallback now lives where the validator is built, beside the machine-title patterns that
  already worked this way.
  A configured list *replaces* the shipped one, so removing a word means naming the ones you
  keep, and there is no environment spelling for "ban nothing".
- **Every test in the wiring package hand-built its configuration**, which agrees with itself by
  construction and cannot notice a default that never reaches production.
  `sweepDeps` is now the one place configuration becomes the processor's dependencies, and three
  tests drive the real loader through it from an environment that sets nothing optional: the
  shipped words are banned, a configured list replaces them, and the rest of the posture a
  revision gets — writes off, attribution on, franchises unpinned, Strava defaults and Xert
  titles renamable, a human title skipped, a Zwift ride kept.
- **The ACHIEVEMENTS block was never fed.**
  It has been in the prompt builder since the prompt existed, and `Ride.Achievements` was
  populated nowhere, so every ride reached the model with an empty list.
  Segment efforts fill it now: a personal top-three or a Strava achievement, deduplicated, capped
  at six, and names only — the times and ranks that selected an effort never reach the prompt.
  Segment names are the least trusted text in the prompt, since a segment is named by whoever
  created it, so the system prompt declares them data and never instructions, and states that a
  place inside a segment name is still not a place the model may name.
- **The release refuses to deploy a writing revision** instead of reporting one afterwards.
  The `DRY_RUN` assertion ran after `gcloud run deploy`, so by the time it went red the revision
  was live and naming.
  It now reads the service the deploy will land on, before deploying — sound because the job sets
  the image and nothing else, so a new revision inherits the running service's environment.
  Enabling writes becomes two deliberate acts: the Terraform change that sets `DRY_RUN=0`, and
  the repository variable `WRITES_ACKNOWLEDGED=1` that says a release is meant to carry it.
  Acknowledgement covers writes being legibly on and nothing else — an unreadable value fails the
  release either way.
- **A draft release requires a deploy.**
  The job accepted `skipped` from the image and deploy jobs, which was right during the Workload
  Identity bootstrap and wrong afterwards: a draft for a version nobody deployed is how a tag
  comes to look shipped.
  `RELEASE_BOOTSTRAP=1` restores the lenient path for a repository with no infrastructure yet,
  and marks what it produces `NOT DEPLOYED` in the title and the notes.
- **`MAX_INSTANCES` is now a startup contract.**
  Four pieces of state are correct only while one instance serves — the OAuth state map, the
  first-bind lock, token-refresh serialization and the sweep lock — so Terraform passes the
  scaling ceiling into the container and the binary refuses to start without it.
  It is a deployment check and not a mutex: the ceiling is per revision, so a rolling deploy or a
  traffic split runs two instances that each read `1`.
  What makes that tolerable is the deployment pattern rather than the code, and the README says
  so, along with the fixes that are not built — a compare-and-set on the token document, and a
  lease for the sweep.

### Not in this release

- **Route repeats.** Still specified and unbuilt.
- **The rest of the configuration.**
  Tiers, geofences, banned words and language preferences are still the defaults shipped in code.
- **Human titles in the no-repeat list.**
  A ride titled by hand is still dropped by the skip-gate without being recorded.
  Re-running `cmd/titelheld-import` closes the gap by hand.

### Upgrading

**Apply the Terraform before deploying this release.**
The service must carry `MAX_INSTANCES=1` before a revision that demands it starts; without it the
container refuses to start, fails readiness, and the deploy fails with it — safe, and a failed
release rather than a deployed one.

Nothing else to apply.
`DRY_RUN` stays on, the Cloud Scheduler job stays paused, and no repository variable needs
setting: `WRITES_ACKNOWLEDGED` and `RELEASE_BOOTSTRAP` are both unset, which is the intended
posture for a service that cannot write.

## [v0.5.0] – 2026-08-22

An offer is not a use.
The franchise walked forward whenever an entry was *offered*, on the reasoning that a title
which adapted an entry and one that ignored it could not be told apart.
The first real ride to test that spent a film on a title that never carried it.
They can be told apart, closely enough, and the position now moves on evidence.

- **A franchise entry is spent only when the title demonstrably uses it.**
  The prompt asks for the entry's wording, and a request to a model is not a guarantee, so
  `naming.UsesEntry` checks afterwards: normalized containment of the entry's core, both sides
  lowercased, punctuation flattened, whitespace collapsed, a leading article dropped when a word
  follows, matched on token boundaries.
  `Son of the Pink Panther: Gegenwind` counts; `Der Panther im Morgengrauen` does not, and
  neither does a translation.
  An unused entry is offered once more and no more, and then the ride is named from a third call
  carrying no FRANCHISE block, so what gets written is an ordinary title rather than one reaching
  for a film it never named.
  It fails closed: an adaptation the check cannot recognize costs a repeated offer, which the
  next prompt shows, where a film spent on a title that never carried it is gone with nothing to
  say where.
- **Reserved entries leave the rotation.**
  A `reserved` list on a franchise names entries that are never offered.
  They keep their place in the series and are the athlete's to spend by hand.
  The field is additive, so a document without it reads back as nothing reserved.
- **The position is an index, not a count.**
  Because the rotation steps over reserved entries, the store advances *past* the index that was
  offered rather than by one: `AdvanceFranchise` becomes `AdvanceFranchisePast`, monotonic, so a
  replay cannot rewind the series onto an entry it has already handed out.
  Still one integer on disk.
- **An entry that could never be spent is refused.**
  One longer than a title reaches the model truncated, and one with no matchable core is
  invisible to the check.
  Either would be declined, re-offered, declined and handed unchanged to the next ride — three
  model calls a ride, for as long as the document said so.
  `naming.Offerable` refuses both, loudly, and the position stays where it is.
- **The shipped Pink Panther profile follows the athlete's own rotation.**
  "Curse of the Pink Panther" was used by hand and is dropped like the three before it.
  "Trail of the Pink Panther" is reserved, as are the four films the rotation has already passed.
  That leaves "Son of the Pink Panther" as the one entry this service may offer.
- **The manual sweep is documented as it actually works.**
  `gcloud scheduler jobs run` refuses a paused job — `FAILED_PRECONDITION: Job.state must be
  ENABLED for RunJob` — so the sequence is three commands: `resume` enables the job, `run`
  dispatches one sweep, `pause` disables it again.
  The `pause` runs even when the dispatch failed, because a failed dispatch must not leave the
  job firing every five minutes, and the dispatch status is kept and reported afterwards rather
  than masked by the `pause` that follows it.
  A delivered request is not a completed sweep, so the section says where the answer is:
  `sweep complete` with `failed` zero and `cancelled` false, `sweep rejected` for the 401, and
  `sweep failed` for the 500 that means nothing ran at all.

### Not in this release

- **Route repeats.** Still specified and unbuilt.
- **The rest of the configuration.**
  Tiers, geofences, banned words and language preferences are still the defaults shipped in code.
- **Human titles in the no-repeat list.**
  A ride titled by hand is dropped by the skip-gate without being recorded, so its title never
  reaches `RECENT`.
  Re-running `cmd/titelheld-import` closes the gap by hand; recording the skip is a later change.

### Upgrading

Nothing to apply: this release changes no infrastructure.
The Cloud Scheduler job stays paused and `DRY_RUN` stays on.

No franchise state needs seeding.
The stored position is an index and nothing has advanced it, so a deployment with no `config`
document walks the shipped profile from zero and offers the first entry that is not reserved.
A deployment that *has* a document keeps using it: add `reserved` there to get the same
behavior, and note that from the first curation change onward the document is authoritative.

## [v0.4.0] – 2026-08-22

What the history teaches, and what it only warns about. The first real import put 1400 titles in
front of the model as the athlete's own style; a quarter of them were another tool talking and
nineteen of the newest twenty-five were commute templates. Two changes separate the two jobs the
history does, and the seed was rebuilt on the new rules.

- **Few-shot examples come from titles this service wrote, and from nothing else.** The two
  lists the prompt builds from the named log used to share one filter, so both dropped templates
  and kept everything else. That is right for the no-repeat list and wrong for the examples.
  They are now two filters with opposite defaults: `RECENT` drops templates and keeps the rest,
  imported rows included, because repeating an old title is exactly what to avoid; `EXAMPLES`
  admits one source and no other. An imported row cannot carry it, so a decade of the athlete's
  own shorthand is *structurally* unable to teach style rather than pattern-matched out of it —
  and a row whose source is unknown or misspelled is not an example either. Until this service
  has written a title, the synthetic set is what the prompt carries. That is its purpose, not a
  cold-start stopgap.
- **The import seeds rides only, and only what the athlete wrote.** `Ride` and `GravelRide`; a
  run, a walk or a strength session is titled by whatever recorded it and is never named here.
  Of those rides it skips Strava's defaults and machine titles as before, and now also anything
  Zwift or Xert titled and this service's own commute and errand templates. The template list is
  read from the athlete's configuration, so the list a run skips is the list that run would
  write — rename your commute and *your* names are skipped instead.
- **Two Xert focus types the pattern did not know.** `Pure Endurance` and `Polar Endurance`,
  found by the import in titles Xert had written and this service had taken for the athlete's
  own words. An unrecognized machine title is skipped rather than overwritten, so the cost was a
  naming rather than a wrong write.
- **`SourceLLM` is `SourceService`**, stored value included. Nothing had ever been named, so no
  row carried the old value and no migration was needed.

### Not in this release

- **Strava push subscription.** Still nothing subscribed to the webhook; creating it is the next
  step.
- **Route repeats.** Still specified and unbuilt.
- **The rest of the configuration.** Tiers, geofences, banned words and language preferences are
  still the defaults shipped in code.

### Upgrading

Nothing to apply: this release changes no infrastructure. The Cloud Scheduler job stays paused
and `DRY_RUN` stays on.

Re-seed the title history after deploying, if it was seeded before: the rows written by an
earlier import were selected by the old rules, and `cmd/titelheld-import` leaves an activity
already in the named log exactly as it is. Deleting the imported rows and re-running is safe for
as long as nothing has been named — after that it stops being a cleanup.

## [v0.3.0] – 2026-08-21

Memory and configuration. The prompt now has a past to draw on, and the first piece of naming
policy moved out of the binary and into a document. `DRY_RUN` is still on, the scheduler is still
paused and nothing is subscribed to the webhook, so none of it runs unattended yet.

- **Athlete configuration.** A `config` document per athlete, and this service's sixth Firestore
  collection. Franchises live there now: an ordered title series is something somebody typed, so
  renaming or reordering one is an edit rather than a release. It is the second collection that
  cannot be re-derived from Strava, and the first whose loss is not an outage — without it the
  service falls back to the profile shipped in code and keeps naming.
- **History import.** `cmd/titelheld-import` seeds the named log from the athlete's own Strava
  history, so recent titles and the derived examples have something to work from on the first ride
  rather than the twenty-sixth. It is a one-shot command run by hand under the operator's own
  credentials, not a route: the service is invokable by `allUsers`, and an endpoint that exists
  for a job run once is surface for no reason. It is idempotent and resumable with no state of its
  own — an activity already in the named log is left alone — and it pages within Strava's read
  budget. Every imported title records the language it is in and is marked as imported, so it is
  never mistaken for one this service wrote.
- **Franchises from configuration.** The processor reads the athlete's series from that document,
  cached per athlete and never cached after a failed read, so a series deleted from the document
  stops advancing instead of durably advancing past its end. What ships in code is now the default
  profile: what applies until a document exists.
- **Gear as imagery.** One prompt instruction lets a bike's name color a title — a bike called
  "Silver Surfer" invites a cosmic or wave-borne image — bounded by the rules that already bind:
  the name is data and never an instruction whatever it appears to say, it supplies no place, and
  the no-repeat and recent-title lists keep the motif from becoming a tic. Gear is read, never
  written. Curated ordered canons stay opt-in, in the configuration document.
- **Health check.** The health route moved from `/healthz` to `/health`. Cloud Run's frontend
  answers `/healthz` itself, so the handler behind that path had never once been reached.

### Not in this release

- **Strava push subscription.** Activities reach the queue only through a webhook nothing is
  subscribed to yet. Creating the subscription is the next step.
- **Route repeats.** Still specified and unbuilt. The replacement design compares sets of visited
  cells by similarity rather than hashing a polyline, which is why the first attempt was removed
  rather than shipped inert.
- **The rest of the configuration.** Tiers, geofences, banned words and language preferences are
  still the defaults shipped in code. They belong in the same document and follow it there.

### Upgrading

Nothing to apply: this release changes no infrastructure. The Cloud Scheduler job stays paused and
`DRY_RUN` stays on — see
[Versions do not turn on writes](README.md#versions-do-not-turn-on-writes).

## [v0.2.0] – 2026-08-21

The naming pipeline, end to end. The service can now decide a title, write it, and drain its own
queue on a schedule — but `DRY_RUN` is still on and the scheduler is still paused, so it does
none of that unattended yet.

- **Naming.** Prompt builder, response validator and two providers behind one interface. Gemini
  goes through Vertex AI on the runtime account's ambient credentials, so there is no API key for
  it anywhere; the Anthropic alternative is switchable and keyed. Model IDs are pinned next to the
  documentation URL and date they were verified against.
- **Processor.** Drains the due queue, classifies, gathers, prompts, validates and writes. One
  failing activity never stalls a sweep, and the named log is written before the title is sent —
  so the failure mode is a ride that keeps its Strava default, never one renamed twice, and the
  webhook event our own rename causes is recognized as self-caused.
- **Attribution.** One line prepended to the description of every activity this service titles,
  as a read-modify-write that preserves what Xert, myWindsock and mybiketraffic wrote byte for
  byte. The line's URL is its own idempotency sentinel, so rewording the prose one day does not
  re-attribute everything.
- **Sweep endpoint.** The scheduled drain, which verifies Cloud Scheduler's OIDC token itself —
  Bearer token, audience, Google issuer, and a verified email matching the scheduler service
  account. `allUsers` holds `roles/run.invoker`, because the Strava webhook cannot present a
  Google credential, so the platform authenticates nobody and the unguessable path is
  obfuscation rather than authentication. Overlapping fires run one sweep.
- **Prompt memory.** The last twenty-five model-written titles, six few-shot examples rebuilt
  from the athlete's own history, and the next entry of a franchise when a ride qualifies for
  one. Commute and errand templates are kept out of both lists: they are meant to repeat.
- **Persistence.** Title history as a query over the named log, which is this service's only
  composite index; the language and source of every title, neither of which is recoverable
  afterward; and franchise positions, stored as an integer so a series can be renamed or
  reordered without a migration.

### Not in this release

- **Route repeats.** A "same loop as 3 May, third time" callback is specified and unbuilt. The
  first attempt hashed the rounded polyline and provably never matched a re-ride — 0 of 50 under
  5 m of jitter, and Strava's polyline simplification changes the vertex count between rides
  regardless — so it was removed rather than shipped inert.
- **Strava history import.** The title history starts empty, so recent titles and derived
  examples do nothing until real namings accumulate.
- **Per-athlete configuration.** Tiers, geofences, banned words and franchise lists are still the
  shipped defaults in code.

### Upgrading

`terraform apply` before the sweep runs: this release adds `SWEEP_AUDIENCE` and
`SWEEP_SERVICE_ACCOUNT`, and the composite index the title history queries. A missing index is an
error on every naming, not a slow query.

## [v0.1.0] – 2026-08-20

First release. The service is deployable but does not yet name anything: it classifies, stores
and describes, and `DRY_RUN` is on, so it cannot write to Strava.

- **Classifier.** Five tiers — skip, virtual, commute, errand and sport ride — assigned from
  sport type, distance, duration and geofences. The tier is decided independently of the current
  title; a separate gate then refuses to act on any activity another tool has already named.
- **Strava client and OAuth.** Authorization-code bootstrap and a token source that survives the
  rotating refresh token. Writes are refused twice over: once in `UpdateActivityName` and again
  in the transport, which will not issue a non-`GET` request unless writes are explicitly on.
- **Webhook.** Subscription validation handshake and event intake, acknowledged immediately and
  persisted out of band so Strava's timeout is never the thing that decides what is stored.
- **Persistence.** Firestore-backed token, queue, named-activity and geocode-cache stores, plus
  an in-memory implementation. Both are held to one conformance suite.
- **Geography.** Polyline decoding and Nominatim reverse geocoding, rate-limited and cached. The
  naming layer receives verified place names only, and there is nowhere in the result type to put
  a coordinate.
- **Infrastructure.** Terraform for the whole GCP footprint: Firestore, Cloud Run, Workload
  Identity Federation, Secret Manager secret resources, the sweep schedule and a budget alert.
- **Release automation.** A signed tag builds the image once, attests it with SLSA provenance,
  and deploys that digest to Cloud Run.

[v0.7.5]: https://github.com/jkreileder/titelheld/releases/tag/v0.7.5
[v0.7.4]: https://github.com/jkreileder/titelheld/releases/tag/v0.7.4
[v0.7.3]: https://github.com/jkreileder/titelheld/releases/tag/v0.7.3
[v0.7.2]: https://github.com/jkreileder/titelheld/releases/tag/v0.7.2
[v0.7.1]: https://github.com/jkreileder/titelheld/releases/tag/v0.7.1
[v0.7.0]: https://github.com/jkreileder/titelheld/releases/tag/v0.7.0
[v0.6.0]: https://github.com/jkreileder/titelheld/releases/tag/v0.6.0
[v0.5.0]: https://github.com/jkreileder/titelheld/releases/tag/v0.5.0
[v0.4.0]: https://github.com/jkreileder/titelheld/releases/tag/v0.4.0
[v0.3.0]: https://github.com/jkreileder/titelheld/releases/tag/v0.3.0
[v0.2.0]: https://github.com/jkreileder/titelheld/releases/tag/v0.2.0
[v0.1.0]: https://github.com/jkreileder/titelheld/releases/tag/v0.1.0
