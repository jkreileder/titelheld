# Changelog

<!-- markdownlint-disable MD024 -->

Notable changes, newest first. Versions are [SemVer](https://semver.org/); the project is
pre-1.0, so the minor number moves for anything that would otherwise be a major bump.

Releases are cut by hand from a signed tag — see
[Cutting a release](README.md#cutting-a-release). The release workflow refuses to run while the
section for the tag being released still says *Unreleased*, so dating this file is a required
step rather than a habit.

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

[v0.5.0]: https://github.com/jkreileder/titelheld/releases/tag/v0.5.0
[v0.4.0]: https://github.com/jkreileder/titelheld/releases/tag/v0.4.0
[v0.3.0]: https://github.com/jkreileder/titelheld/releases/tag/v0.3.0
[v0.2.0]: https://github.com/jkreileder/titelheld/releases/tag/v0.2.0
[v0.1.0]: https://github.com/jkreileder/titelheld/releases/tag/v0.1.0
