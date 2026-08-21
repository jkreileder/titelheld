# Changelog

<!-- markdownlint-disable MD024 -->

Notable changes, newest first. Versions are [SemVer](https://semver.org/); the project is
pre-1.0, so the minor number moves for anything that would otherwise be a major bump.

Releases are cut by hand from a signed tag — see
[Cutting a release](README.md#cutting-a-release). The release workflow refuses to run while the
section for the tag being released still says *Unreleased*, so dating this file is a required
step rather than a habit.

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

[v0.3.0]: https://github.com/jkreileder/titelheld/releases/tag/v0.3.0
[v0.2.0]: https://github.com/jkreileder/titelheld/releases/tag/v0.2.0
[v0.1.0]: https://github.com/jkreileder/titelheld/releases/tag/v0.1.0
