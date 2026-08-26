<!-- omit in toc -->
# Titelheld

A single-athlete backend service that gives Strava activities context-aware titles — and leaves
everything that should stay boring untouched.

*Titelheld* is German for the character a piece is named after: the one in the title role.

> **Status: under construction.** The naming pipeline is complete end to end — classifier,
> configuration, both stores, the Strava client with OAuth, the webhook and its delay queue,
> geocoding, the prompt builder and LLM providers, and the sweep that drains the queue and
> writes the title. The infrastructure is applied and the service is deployed.
>
> Not built: the athlete's tiers, geofences, banned words and language preferences, which still
> ship as defaults in code and belong in the same configuration document franchises now live in;
> and route repeats — see [What the prompt carries](#what-the-prompt-carries).
>
> **The scheduler is paused.** Nothing fires the sweep until it is unpaused by hand, which is
> deliberate: the naming pipeline is reviewed end to end before it runs unattended.
>
> **The push subscription is live**, so real events accumulate in the queue — see
> [The push subscription](docs/infrastructure.md#the-push-subscription). Nothing drains them
> while the scheduler is paused; that queue is the material the dry-run review reads. With
> `FIRESTORE_PROJECT` unset the service runs on the in-memory store and forgets the OAuth token
> on restart; see [docs/firestore-iam.md](docs/firestore-iam.md).
>
> **Nothing can write to Strava yet.** Dry run is the default and the zero value throughout;
> see [Writes and dry run](#writes-and-dry-run).

- [What it does](#what-it-does)
- [Repository layout](#repository-layout)
- [The classifier](#the-classifier)
- [Writes and dry run](#writes-and-dry-run)
- [What gets written](#what-gets-written)
- [What the prompt carries](#what-the-prompt-carries)
- [Configuration](docs/configuration.md) — every setting, and why it exists
- [HTTP surface](#http-surface)
- [Development](#development)
- [Security and privacy](#security-and-privacy)
- [Geography](#geography)
- [Seeding the title history](#seeding-the-title-history)
- [Franchises](#franchises)
  - [An entry is spent when it is used, not when it is offered](#an-entry-is-spent-when-it-is-used-not-when-it-is-offered)
  - [Reserved entries](#reserved-entries)
  - [What is stored](#what-is-stored)
- [Local development](#local-development)
- [Infrastructure](docs/infrastructure.md) — the GCP deployment, bootstrap and apply order
- [Cutting a release](#cutting-a-release)
  - [The steps](#the-steps)
  - [What the tag push does](#what-the-tag-push-does)
  - [A draft means it is running](#a-draft-means-it-is-running)
  - [Versions do not turn on writes](#versions-do-not-turn-on-writes)
- [Attribution](#attribution)
- [License](#license)

Two things live in [`docs/`](docs) rather than here, because they are about running a deployment
rather than about what the service does: every [configuration](docs/configuration.md) setting, and
the [infrastructure](docs/infrastructure.md) that Terraform owns. The Firestore collections and
their IAM are in [docs/firestore-iam.md](docs/firestore-iam.md).

## What it does

The service is the **last writer** in a chain of Strava automations. Other tools
(ActivityFix, Xert) fix up sport type, gear and workout summaries first; this service waits a
configurable delay, then names an activity only if nobody has already titled it by hand.

An activity is renamed, and one line is added to the front of its description: the
attribution line, on activities this service titled and no others. Sport type, gear and the
workout summaries other tools write are never touched, and the rest of the description comes
back byte for byte — see [What gets written](#what-gets-written).

## Repository layout

| Path                        | Purpose                                                                                |
| --------------------------- | -------------------------------------------------------------------------------------- |
| `internal/classifier/`      | Tier rules and the title gate. No I/O, no deps.                                        |
| `internal/config/`          | Runtime configuration, read from the environment only.                                 |
| `internal/store/`           | Persistence interfaces, an in-memory implementation, and the shared conformance suite. |
| `internal/store/firestore/` | The Firestore implementation. See [docs/firestore-iam.md](docs/firestore-iam.md).      |
| `internal/strava/`          | The only package that talks to Strava: OAuth, client, API calls.                       |
| `internal/webhook/`         | Subscription handshake, event intake, delay-queue enqueue.                             |
| `internal/server/`          | HTTP surface: health, OAuth bootstrap, webhook route.                                  |
| `cmd/titelheld/`            | Cloud Run entry point; wiring only.                                                    |

Core logic lives in packages with no HTTP, Firestore or Strava-SDK imports, so a future
multi-athlete deployment needs no changes there.

## The classifier

`classifier.Classify` assigns each activity a **tier** and an **action**. The tier is the taxonomy
bucket; the action is what the caller may do about it. They are separate because an activity that
another tool already titled still belongs to a tier — it just must not be written.

Tier rules are evaluated in order, first match wins:

| Tier | Name       | Matches                                            | Action when the title gate allows a write |
| ---- | ---------- | -------------------------------------------------- | ----------------------------------------- |
| 1    | Skip       | `WeightTraining`, `Workout`, `Walk`, `Hike`, Whoop | never written                             |
| 2    | Virtual    | `VirtualRide`, or a ride with the trainer flag     | `zwift_mode`: keep, or indoor LLM         |
| 3    | Commute    | short ride to or from the work geofence            | deterministic commute title               |
| 4    | Errand     | commute-tagged ride                                | deterministic errand title                |
| 5    | Sport ride | ride ≥ 15 km or ≥ 45 min                           | full LLM naming pipeline                  |
| —    | None       | anything else (runs, swims, short rides)           | never written                             |

Tiers 3 to 5 apply to rides only: the trainer flag does not make a treadmill run a virtual
ride, and a commute-tagged run does not become an errand. Tier 3's geofence match is capped by
the tier-5 thresholds, so a long ride that merely finishes at work stays a sport ride; a title
ActivityFix already wrote is taken at face value whatever the ride's size.

The **skip gate** runs after tier assignment: unless the current title is a recognized Strava
default *or* a recognized machine title, the action is downgraded to skip. The gate fails closed —
an unrecognized title is assumed to have been written by a person.

Machine titles are the exception the gate needs because Xert renames sport rides with its focus
pattern (`Difficult Mixed Breakaway Specialist Ride`) shortly after upload, so an activity can
arrive titled without anyone having named it. The shipped pattern is anchored on Xert's own
vocabulary at both ends, and every configured pattern is anchored to the whole title, because a
pattern that matches too much overwrites the athlete's own words. ActivityFix's commute titles are
deliberately not in the list: `Zur Arbeit` is already the right title for that ride. Extend the
list with `MACHINE_TITLE_PATTERNS`.

## Writes and dry run

This service renames real activities on a real account, so the safe state is the one you get
by doing nothing:

- `config.Config.WritesEnabled` is expressed positively, so a zero-valued config is dry run.
- `strava.WriteMode`'s zero value is `WriteModeDryRun`, so a client built without thinking
  about it refuses to write.
- `DRY_RUN` stays on unless it holds an explicit falsy value (`0`, `false`, `no`, `off`).
  Anything unrecognized is reported as an error *and* leaves dry run on — a typo must never be
  what lets the service loose.
- `UpdateActivityName` refuses with `ErrDryRun` before building a request, and the transport
  refuses every non-GET method again, so a future write path cannot slip past the first check.

## What gets written

Two things, and only on an activity this service titled.

**The title.** Only where the classifier cleared it: a Strava default, or a machine title
another tool wrote. A human's title is never overwritten — on a sport ride it is *remembered*
instead, recorded in the named log as source `human` so the prompt can neither repeat it nor
miss the style it shows. That record is also what makes the ride final: it is never
reconsidered, and nothing about it — not the title, not the description — is ever written.

**One line at the front of the description**, from pipeline step 7:

```text
Title by titelheld — https://github.com/jkreileder/titelheld
```

Strava's `PUT` replaces the whole description, so adding a line is a read-modify-write: the
current description is fetched immediately before the write and prepended to, and everything
already there comes back byte for byte — it belongs to Xert, myWindsock and mybiketraffic, and
this service is the last writer, not the only one.

The line is its own idempotency sentinel, and the sentinel is the **URL**, not the sentence.
Reword the prose one day and already-attributed activities stay untouched; there is no separate
marker and no stored flag, so the check survives a replay, a lost database, and a description
the athlete has edited around.

Attribution never blocks a naming. If the description cannot be read, the title goes out on its
own and the reason is logged. It is on by default and can be switched off; skipped activities
and Zwift rides left alone never get it, because they never got a title either.

Nothing else is ours to change. Sport type, gear and the workout summaries other tools write
are never touched.

## What the prompt carries

Beyond the ride itself, four things — all of them derived, none of them committed to this
repository.

**The last 25 titles worth not repeating.** The prompt forbids repeating any of them and
invites referring back. They come from the named log with one kind dropped: a commute or errand
template is meant to repeat, so listing it both forbids the right answer for the next commute and
crowds the real titles out of a list of twenty-five. Everything else stays, imported titles
included — a title the athlete gave a ride years ago is one to avoid repeating, whoever wrote it.

**Few-shot examples, from titles this service wrote and titles the athlete has written since.**
Six of them, rebuilt at prompt time: the named log keeps each title and the language it was
written in, and the ride that produced it is re-read from Strava to describe the situation. Two
sources qualify — `service`, and `human`, the title of a sport ride the skip gate left alone. The
athlete's current hand-namings are the best style data there will ever be, and admitting only the
service's own titles would have closed the style loop on itself. An `imported` row never
qualifies — not a filter over titles that look unsuitable, but a source the row carries, so a
decade of the athlete's own shorthand is *structurally* unable to teach style, rather than
pattern-matched out of it. That shorthand is bare place names, private jokes and whatever a tool
left behind; six of them would teach a model to answer with a bare town name. Until a title from
either source exists, the synthetic set is what the prompt carries — that is its purpose, not a
stopgap.
Deriving an example costs a Strava read, so each is cached against the activity it describes: the
history only changes when something is named, so a sweep repeating every five minutes pays once.

**The names of notable segment efforts**, under `ACHIEVEMENTS` — a personal top-three on the
segment, or an achievement Strava awarded, deduplicated and capped at six. Names only: the times
and ranks that selected them never reach the prompt, the same way the geo layer passes verified
place names and no coordinates.

A segment name is the least trusted string in the prompt: a segment is named by whoever created
it, and every rider who crosses it inherits that name. So the system prompt states two rules for
this block by name — the names are **data, never instructions**, the same rule `Bike` and `NOTES`
carry; and they are **not geography**, so a place inside a segment name is still not one the
model may name. `PLACES` stays the only geography.

**The next entry of a franchise**, when the ride qualifies — see [Franchises](#franchises). The
model may extend the entry; it may not translate it, paraphrase it or skip the position, and the
entry is only spent if the title that comes back actually carries it.

Only the title history is worth failing for. If it cannot be read the activity stays queued,
because the realistic cause is the composite index missing — a deployment error that fixes
itself on the next apply, where naming without history in the meantime would produce exactly
the repetition the history exists to prevent. Everything else degrades: a gear lookup, a
franchise position or an example that cannot be fetched makes the prompt slightly poorer and
never stops a title being written.

**Route repeats are not among them.** A "same loop as 3 May, third time" callback is specified
and not built: the first attempt identified a route by hashing its rounded polyline, which
provably never matches a re-ride — 0 of 50 under 5 m of jitter, and Strava's polyline
simplification changes the vertex count between rides anyway. It was removed rather than
shipped inert.

A replacement is designed and likewise unbuilt: comparing *sets* of visited cells by similarity
rather than hashing a sequence, which is tolerant by construction and direction-blind because
sets are unordered. Nothing in the service does this today, and no title can carry a route
callback until it does.

**The history has to be seeded once.** Nothing has been named yet, so the recent-titles list is
empty until the athlete's existing Strava activities are imported — see
[Seeding the title history](#seeding-the-title-history). The examples are unaffected either way:
they come from titles this service wrote or the athlete has written by hand since, so the
synthetic set stands in until either exists.

## HTTP surface

| Route                    | Purpose                                                     |
| ------------------------ | ----------------------------------------------------------- |
| `GET /health`            | Liveness check                                              |
| `GET /auth`              | Starts the one-time authorization; redirects to Strava      |
| `GET /auth/callback`     | Completes it, verifies the granted scopes, stores the token |
| `GET /webhook/<secret>`  | Strava's subscription validation handshake                  |
| `POST /webhook/<secret>` | Event intake; queues the activity after the delay           |
| `POST /sweep/<secret>`   | Drains the delay queue; Cloud Scheduler only                |

Both the webhook and the authorization start are mounted at their full secret paths, so
guessing the prefix but not the segment is a 404 from the router. Starting the flow is what
needs protecting: a bare `/auth` would let anyone authorize their own Strava account and have
this service store their token. The callback stays at a fixed, registered URL and is guarded
by the single-use state that only the start route issues; with no `STRAVA_ATHLETE_ID` set, the
service binds to whoever authorizes first and refuses anyone else afterward.

The verify token is compared in constant time over hashes, so neither its contents nor its
length leak. `WEBHOOK_PATH_SECRET` is validated at load: a segment containing a space would
panic the router at registration, and one of the form `{x}` would register as a wildcard and
remove the unguessable-path defense entirely.

Events are acknowledged before the queue is written, which is the order Strava's two-second
budget assumes. A delivery that is never acknowledged is retried, and the queue is idempotent,
so the ordering costs nothing that is not already handled.

The delay is served by a **Cloud Scheduler sweep** rather than Cloud Tasks: it needs no second
GCP service and no client library, a failed activity simply stays queued until the next sweep
instead of needing its own retry policy, and ten-minute precision makes the scheduler's coarse
granularity irrelevant.

The sweep is the one route that makes this service act on its own initiative rather than
answering somebody, so it is the one route with an identity check. It has to do that check
itself: `allUsers` holds `roles/run.invoker` — Strava cannot present a Google credential, and
the webhook and the sweep share one service — so Cloud Run authenticates nobody, and a request
arriving here has been let through rather than vouched for. The unguessable path is
obfuscation; the OIDC token is the authentication.

Four things are checked, and a request that fails any of them gets a bare `401`:

- the `Authorization` header carries a Bearer token,
- the token validates against `SWEEP_AUDIENCE`, signature and expiry included,
- its issuer is `https://accounts.google.com`,
- its `email` is `SWEEP_SERVICE_ACCOUNT` and its `email_verified` is `true`.

The response never says which failed; the log says as much as it honestly can. Issuer, `email`
and `email_verified` are named individually. Signature, expiry and audience are not
distinguishable — they come back as one error from the token validator — so the log records the
validator's own wording alongside the audience that was required, which is enough to recognize
a misconfigured audience without claiming to have identified it. A caller who guessed the path
learns only "no".

The scheduler account is separate from the runtime account and holds invoke permission and
nothing else, so the identity that can trigger work cannot read the athlete's data.

Terraform feeds one value to both sides of the audience, because a mismatch does not announce
itself: the scheduler keeps firing, the handler keeps answering `401`, and the queue quietly
stops draining.

Overlapping fires run one sweep, not two. The scheduler's attempt deadline is longer than the
interval between fires, so this is a scheduled event rather than a hypothetical, and two sweeps
over one queue can both read the named log for an activity before either writes it — and rename
it twice. The second fire is answered immediately rather than made to wait, because waiting
would only spend its deadline on a queue the first sweep is already draining.

A sweep that names nothing, or that fails on every activity, is still a `200`. Failed
activities stay queued and the next fire retries them; a non-2xx would make Cloud Scheduler
retry at once, straight back into whatever rate limit caused the failure. Only a queue that
could not be read at all is a `500`, and its body does not repeat the internal error.

A shutdown is not a failure either. Cloud Run gives a container a short grace period, and the
sweep stops at an activity boundary rather than part-way through one — so the worst case is an
entry left queued, never a rename sent with nothing recorded. The response is a `200` whose
`cancelled` field is true, carrying the counts of what was finished before it stopped:

```json
{"due":12,"named":3,"skipped":1,"failed":0,"cancelled":true}
```

## Development

```sh
make               # lint, test, build
make test          # go test -race with coverage
make lint          # go vet + golangci-lint
make vulncheck     # govulncheck
make release-logic # run the release workflow's decisions, not just lint them
```

Install the pre-commit hooks with `pre-commit install` or `prek install`. They cover Go, Markdown,
workflows (actionlint, zizmor) and shell scripts (shellcheck, shfmt). Terraform is not among them:
`make tf-check` and `terraform.yaml` cover `infra/`.

`make release-logic` exists because a linter reads a workflow and cannot run one. The writes gate
is a script the release calls, so a test drives it against a stubbed `gcloud`; the draft-release
condition is read out of `release.yaml` and evaluated over every combination of job results and
the bootstrap variable. Reverting either — the gate back after the deploy, a bare
`WRITES_ACKNOWLEDGED`, a draft on a skipped deploy — fails these rather than passing every hook,
which is what the earlier versions of both did.

## Security and privacy

- Runtime secrets never live in this repository. See [SECURITY.md](SECURITY.md) for reporting
  vulnerabilities.
- Every coordinate and identifier in tests and fixtures is **synthetic**. Real GPS positions,
  athlete IDs and tokens must never be committed — see
  [CONTRIBUTING.md](.github/CONTRIBUTING.md).
- Activity titles are never derived from reverse-geocoded points of interest; only an explicit
  whitelist and generic area templates are used.

## Geography

A route becomes place names, never coordinates. `internal/geo` decodes Strava's summary
polyline, samples the start, the four bounding-box extremes and three evenly spaced waypoints,
and reverse-geocodes them through Nominatim.

Two properties are enforced in code rather than documented and hoped for:

- **No points of interest.** Only administrative names (village, hamlet, town, city,
  municipality, suburb, district, county) and named natural features reach the output.
  Nominatim's `amenity`, `shop`, `office`, `building`, `road` and `house_number` are dropped,
  and the free-text `display_name` is never read at all. A title can therefore never reveal the
  athlete's doctor — or their front door.

  "Natural feature" is an allow-list of specific OSM *types* — rivers, lakes, woods, peaks,
  ridges — not of categories. A category is far too coarse: OSM's `leisure` covers
  `fitness_centre` and `swimming_pool`, and `place` covers `isolated_dwelling`, which on a rural
  route is the athlete's own house. The naming fallback fires only when no settlement resolved,
  which is exactly where those would otherwise be the only name on offer.
- **At most one request per second**, per Nominatim's usage policy, enforced by the client
  itself so no caller can exceed it by looping. The configured interval is clamped *up* to one
  second; a config file may not relax the policy.

Results are cached in the store under a coordinate rounded to three decimals (~110 m), which is
also the only place a coordinate is persisted. Samples are deduplicated by that key, so an
out-and-back does not spend the budget geocoding its start twice.

The naming layer receives a `geo.Summary`, whose fields are all names. There is nowhere in it to
put a coordinate.

## Seeding the title history

The prompt asks a model not to repeat a recent title, and the `RECENT` list it reads that from
is the named log — which starts empty, so a fresh deployment has nothing to avoid repeating until
the history is seeded from Strava. (The examples are a separate matter: they come only from titles
this service wrote, so seeding does not affect them — see
[What the prompt carries](#what-the-prompt-carries).)

`cmd/titelheld-import` does that. It is a one-shot run by hand, not a route on the service: every
endpoint added to a service `allUsers` can invoke is another thing that has to authenticate
correctly, and a job that runs once under the operator's own credentials, with no request timeout
over it, has no business being one.

```sh
FIRESTORE_PROJECT=titelheld-… FIRESTORE_DATABASE=titelheld \
STRAVA_CLIENT_ID=… STRAVA_CLIENT_SECRET=… \
go run ./cmd/titelheld-import
```

Four variables, and no more. An import serves no HTTP and completes no authorization flow, so
the webhook's verify token and unguessable path, and the public base URL the OAuth redirect is
built from, are nothing to it — Strava's token endpoint takes no `redirect_uri` on a refresh.
Requiring them would mean inventing values for a job that never reads them.

It authenticates to Firestore with your own application-default credentials, and to Strava with
the stored token — so **run the authorization flow first**. It resolves the athlete the way the
OAuth callback binds one, from the single stored token, refusing if there is none or more than
one; there is no athlete flag to mistype.

It never changes an activity. The client is built in its dry-run zero value, so its transport
refuses anything that is not a `GET`. The one request that does not go through it is the OAuth
token refresh — a `POST` to `/oauth/token`, which issues tokens and cannot touch an activity, and
which an import needs because a stored access token is only valid for a few hours.

**What it seeds, and what it leaves out.** Rides only — `Ride` and `GravelRide` — and of those,
only the titles the athlete wrote themselves.
A run, a walk or a strength session is titled by whatever recorded it and will never be named
here, so its title is neither a repeat to avoid nor anything to learn from.
Among rides, four kinds are skipped:

- **Strava's own defaults**, because a default is not a title. They repeat by design, so listing
  them under "never repeat" forbids the right answer.
- **Recognized machine titles**, which are the style this service exists to replace.
- **Anything Zwift or Xert titled** — a `Zwift - …` prefix or a suffix of `" - Xert"`. That is
  the tool talking, whatever the sport type says: a Zwift session recorded by a head unit arrives
  as a plain `Ride`.
- **This service's own templates**, the commute pair and the errand pool. They are the correct
  title for those rides and are meant to repeat, which is the opposite of a style.

The template list is the configured one, not a fixed list of German words: an athlete who renames
their commute has *their* names skipped, and the shipped ones become ordinary titles again. None
of this widens what the service may overwrite — that is `MachineTitles`, and a Zwift ride still
keeps the title Zwift gave it.

**Idempotent and resumable, with no state of its own.** An activity already in the named log is
left exactly as it is, so a second run writes nothing and an interrupted one continues where the
log ends. Re-listing costs a handful of requests, which is cheaper than keeping a cursor honest.
A title this service wrote is never relabelled as imported.

Entries are dated by the ride rather than the import, so the history keeps meaning "the most
recent rides"; each carries a language guessed from the title and a source marker distinguishing
imported from service-written. The language is a heuristic, applied to titles this service did
not write — imported ones, and the ones the skip gate records as the athlete's — while a title
this service writes carries the language the model reported.

Run an import *after* the first sweep that reaches a hand-titled ride, not before: the sweep
records such a title as `human`, which may teach style, and an import that gets there first
files it as `imported`, which may not.

## Franchises

A franchise is an ordered list of titles walked one entry at a time. The shipped example is the
Pink Panther films, for gravel rides on the gear of the same name: each such ride takes the next
film in the sequence. The model may extend an entry — it may not translate it, paraphrase it or
skip the order.

Franchises are **data, not code**. Adding one is a configuration change and needs no release.

### An entry is spent when it is used, not when it is offered

The prompt asks for the entry's wording; a request to a model is not a guarantee. So the position
moves only when the title that comes back **demonstrably uses** the entry: containment of the
entry's core after normalizing both sides — lowercased, punctuation flattened to spaces,
whitespace collapsed, a leading `the`, `a` or `an` dropped when something follows it — matched on
token boundaries. `Son of the Pink Panther: Gegenwind` counts. `Der Panther im Morgengrauen` does
not, and neither does a translation.

Dropping the article is what lets `Pink Panther im Nebel` count as `The Pink Panther`. It only
applies when a word follows, so a one-word entry keeps that word and stays matchable rather than
becoming an empty core that matches everything.

If the entry goes unused the model is offered it **once more**, and no more than that: a model
that has declined an entry twice will not be argued into it, and each attempt is a paid call.
If the second title does not use it either, the ride is named from a third call carrying no
FRANCHISE block — an ordinary title, rather than one that was reaching for a film it never
named — and the position does not move.

The check fails closed. An adaptation it cannot recognize leaves the entry unspent and the next
ride is offered it again, which costs a repeat. The opposite error spends a film on a title that
never carried it, and that one cannot be noticed afterwards.

A call that fails after a usable title is already in hand never costs the title: the ride is
named with what there is, without the franchise.

### Reserved entries

An entry listed under `reserved` is never offered. It keeps its place in the series and it is
yours to spend by hand — which is what the shipped profile does with the films the athlete has
already walked past. Reserving is not deleting: the rotation steps over a reserved entry, and
un-reserving it later puts it back, behind the position if the rotation has already moved past
it.

### What is stored

Firestore stores a single integer per athlete per franchise: the index the rotation resumes at.
Not "how many entries are used" — spending a reserved entry by hand does not move it, which is
the point of reserving one. It never stores the titles, so editing a series never migrates
anything — but the integer is an index into the list you edited, and two edits move what it
means:

| Edit | What happens to the position |
| --- | --- |
| append entries | nothing; the series simply has more to go |
| insert, delete or reorder **after** the position | nothing; the rotation has not reached them |
| insert, delete or reorder **at or before** it | the index names a different entry — review it |
| delete enough to pass the end | the index runs off the list, which reads as a finished series: rides are named normally, and appending fewer entries than the position does not revive it |
| rename the franchise | the name keys the position, so the series starts again at zero |
| remove the franchise | it stops being read, not deleted — re-adding the same name resumes where it left off |

The last row is worth knowing in both directions. Re-adding a franchise under its old name picks
the rotation up where it stopped, which is usually what you want; to start it over instead, set
that `position` document to `0`.

`FranchisePosition` returns `0` for a franchise never used and for one that does not exist, and
those two cases are deliberately indistinguishable — which is what makes removing one safe.

`AdvanceFranchisePast` takes the index of the entry that was used and moves the position to
`index + 1`, in one transaction, so the store decides the number rather than a caller reading,
adding one and writing back. The index rather than a step, because the rotation steps over
reserved entries. The move is monotonic — a lower index leaves the position alone — so a replay
cannot hand out a title the series has already spent.

Franchises live in the athlete's Firestore configuration document, one document per athlete in
the `config` collection keyed by athlete ID. Adding one is an edit to that document — no release,
no deploy. An athlete with no document gets the shipped default profile.

The document's shape:

```json
{
  "athlete_id": 12345678,
  "franchises": [
    {
      "name": "silver-surfer",
      "sport_types": ["GravelRide", "Ride"],
      "gear_name": "Silver Surfer",
      "titles": ["Herald of Galactus", "The Power Cosmic", "Rise of the Silver Surfer"],
      "reserved": ["Rise of the Silver Surfer"]
    }
  ],
  "updated_at": "2026-08-21T12:00:00Z"
}
```

`sport_types` empty means any sport; `gear_name` empty means any bike. `reserved` names entries of
`titles` the rotation never offers, matched case-insensitively and trimmed; absent means nothing
is reserved, and a reserved title that is not in `titles` is inert. The first matching franchise
wins, so order is precedence. A franchise with no `name` is discarded: the name keys the stored
position, and an empty one would store it under an empty document ID.

Four states, and they are not the same:

| The document | What applies |
| --- | --- |
| absent | the shipped default profile |
| present, with franchises | exactly those; the default profile does not apply |
| present, `"franchises": []` | none — the athlete has no series, and the defaults stay off |
| unreadable | the default profile, for that ride only |

The last two are the ones worth keeping apart. An empty list is a decision and is remembered; a
failed read is not remembered, so the next ride tries again — otherwise one transient Firestore
error would pin the defaults for the life of the process, and a series you had removed would go
on being offered and its position go on advancing.

To add one:

1. Pick a `name` that will not change. It keys the stored position, so renaming it sends the
   athlete back to the first title.
2. Add the entry to the document, in the Firestore console or through the REST API — `gcloud`
   has no command for editing documents, only for import, export and bulk delete. The service
   reads the document once per process, so the change takes effect at the next **cold start**.
   `min_instance_count = 0` allows scaling to zero but does not promise it between sweeps five
   minutes apart — Cloud Run keeps an idle instance for as long as it likes — so the next sweep
   may well still be the old process with the old configuration. Do not force it with an
   out-of-band `gcloud run services update`: that creates a revision Terraform did not describe.
   Check instead — the process says which answer it got when it first needed one:

   ```sh
   gcloud logging read 'resource.type="cloud_run_revision" AND
     resource.labels.service_name="titelheld" AND
     (jsonPayload.msg="loaded the athlete configuration"
      OR jsonPayload.msg="no athlete configuration; using the default franchise profile")' \
     --project="$PROJECT" --limit=5 --freshness=1h
   ```

   A line newer than your edit is a process that has read it, and `franchises` on that line is
   how many series it found — the quickest way to see that a new one arrived.
3. **If some entries have already been used by hand**, leave those titles out of `titles` — what
   the shipped Pink Panther profile does, where the four films already used are simply absent.
   For entries you have walked past but want to keep, list them under `reserved` instead: they
   stay in the series, in order, and are never offered.
   Seeding the position also works and is sometimes clearer: it lives in
   `franchise/{athleteID}-{name}` as a single `position` integer, and setting it to the index the
   rotation should resume at is the whole of it. That index is **zero-based** — `0` is the first
   entry of `titles`, so a series to be resumed at the fourth entry gets `3`. Counting from one
   silently skips a title.

**Still shipped in code:** the *default profile* — what applies when the document is absent or
cannot be read. Tiers,
geofences, banned words and language preferences have not moved yet; they belong in this same
document and will follow.

## Local development

Secrets reach the process from the environment, never from the working tree. Keep them in a
file **outside the repository** so they survive `make clean`, a re-clone and a rename:

```sh
mkdir -p ~/.config/titelheld && chmod 700 ~/.config/titelheld
$EDITOR ~/.config/titelheld/env      # export STRAVA_CLIENT_ID=… etc.
chmod 600 ~/.config/titelheld/env

set -a; . ~/.config/titelheld/env; set +a
MAX_INSTANCES=1 go run ./cmd/titelheld
```

`MAX_INSTANCES=1` is required to serve — see [Configuration](docs/configuration.md). It belongs on the
command line rather than in the secrets file: it is not a secret, and a local run is exactly the
place where a stale copy of it would be wrong.

A gitignored `.env` inside the repository would also work, and `.gitignore` covers one, but it
is reachable by `git add -f`, by editor plugins and by folder-sync tools — and `git clean -xdf`
would delete it. `make clean` excludes `/.env` for that reason, but the file outside the tree
is the safer habit.

`DRY_RUN` defaults to on. Nothing can write to Strava until it is set to an explicit falsy
value, and the deployed service keeps it on.

## Cutting a release

A release is a signed tag you push by hand. Nothing in CI can start one, and there is no bot
with permission to tag this repository.

That is deliberate. The alternative — a bot maintaining a release PR — cannot work here without
weakening the repository's own rules: release-please writes its commits through the low-level
git data API, which does not sign them, so its release branch would fail the signed-commits
rule; and a pull request opened with
`GITHUB_TOKEN` never triggers a workflow run, so it could never satisfy the required checks on
`main`, which have no bypass. Making it work would mean storing a long-lived token with write
access to the repository that deploys this service. A tag you sign yourself costs one command
and stores nothing.

### The steps

1. **Write the changelog entry.** Replace *Unreleased* in [CHANGELOG.md](CHANGELOG.md) with
   today's date. The release workflow refuses to run while it still says *Unreleased*, so this
   is enforced rather than remembered.

2. **Merge that to `main`**, through a pull request like anything else.

3. **Tag it, signed and annotated.**

   ```sh
   git switch main && git pull
   git tag -s v0.1.0 -m "v0.1.0"
   git push origin v0.1.0
   ```

   The tag must be annotated (`-s` implies it) and the version final — `v0.1.0`, not
   `v0.1.0-rc1`. The workflow checks both before building anything, because there is one
   production service and a pre-release tag would deploy straight to it.

4. **Publish the draft release.** The workflow opens it with generated notes; edit and publish
   when you are happy with them. Nothing downstream depends on it, so there is no rush.

### What the tag push does

| Step   | Where                | What it produces                                                                                              |
| ------ | -------------------- | ------------------------------------------------------------------------------------------------------------- |
| Check  | `release.yaml`       | Fails fast on a lightweight tag, a pre-release version, or a stale changelog                                  |
| Build  | `release-image.yaml` | One image, cache-free, tagged `0.1.0`, `0.1`, `0` and `sha-<commit>`                                          |
| Attest | `release-image.yaml` | Sigstore-signed SLSA provenance, stored in GitHub and pushed to Artifact Registry as a referrer of the digest |
| Deploy | `release.yaml`       | `gcloud run deploy` of that **digest**, via Workload Identity Federation                                      |
| Draft  | `release.yaml`       | A draft release for you to publish — only if the image built *and* deployed                                   |

The image is built exactly once. Everything after it refers to the digest, never to a version
tag, so moving a tag afterward cannot change what is running.

The build and attest steps live together in `release-image.yaml` rather than in the calling
workflow. Signed provenance records the workflow *file* that produced it, so keeping both jobs
in one reusable file is what lets the attestation name a single trusted builder — the SLSA
Build Level 3 claim. Verify it:

```sh
gh attestation verify \
  "oci://${REGION}-docker.pkg.dev/${PROJECT}/containers/titelheld@${DIGEST}" \
  --repo jkreileder/titelheld \
  --signer-workflow jkreileder/titelheld/.github/workflows/release-image.yaml
```

### A draft means it is running

The draft release is created only when the image job succeeded **and** the deploy job succeeded.
Not "did not fail": a skipped deploy produces no draft either, because a draft for a version
nobody deployed is how a tag comes to look shipped when it is not.

That leniency existed for a reason and outlived it. Before the Workload Identity bootstrap, the
image and deploy jobs skipped on purpose — the repository variables they need did not exist yet —
and a draft was still the right outcome. The variables are set now.

`RELEASE_BOOTSTRAP=1` brings the old behavior back for a repository that has no infrastructure
yet. The draft it produces is titled `vX.Y.Z - NOT DEPLOYED` and opens with a caution block
naming how each job finished, so nothing can be published by accident that is not running
anywhere. Bootstrap is a variable rather than something inferred from the job results, because
"the deploy was skipped" and "the deploy should have been skipped" look identical from inside the
workflow.

### Versions do not turn on writes

`DRY_RUN` is Terraform's to set, and it is set to `1`. There is no version number that means
*now write to Strava*: turning writes on is a deliberate infrastructure change, never a side
effect of shipping.

The release checks that **before** deploying, not after. It reads `DRY_RUN` off the service the
deploy will land on and refuses to deploy at all if writes are enabled — which works because
this job sets the image and nothing else: every environment variable belongs to Terraform, so a
new revision inherits the environment of the one running now. The check used to run after
`gcloud run deploy`, which meant the revision was live and naming by the time the job went red.
A second read after the deploy stays, no longer as the gate but as a check that the prediction
held.

Turning writes on is therefore two deliberate acts, and the second one is this:

| | |
| --- | --- |
| **1. Terraform** | set `DRY_RUN=0` on the service, review it, apply it |
| **2. Repository variable** | set `WRITES_ACKNOWLEDGED` to today's date, `YYYY-MM-DD` |

**The acknowledgement is a date, and it expires.** A flag would not: set once during a flip and
never removed, `WRITES_ACKNOWLEDGED=1` would still be sitting there months later, and the day an
accidental Terraform change reintroduced `DRY_RUN=0` — the drift this gate exists for — the next
signed tag would ship a writing revision on a warning. Two deliberate acts would have decayed
into one stale click and one unnoticed diff.

So the value must parse as `YYYY-MM-DD`, dated no more than **7 days before** the tag and no more
than **1 day after** it. A bare `1` is refused for being the wrong shape, `2026-02-30` for not
being a date, and any date more than 7 days before the tag for being stale. Every refusal quotes
the value it saw; the ones about age name the tag's own date beside it. The single day of slack
in the future is for a tag pushed late in the UTC evening, which is already tomorrow for the
person pushing it.

The comparison is against the **tag's** timestamp, not the clock: re-running an old release
cannot make a stale acknowledgement fresh again.

With the variable set, a release whose target service has writes enabled proceeds and says so
loudly in the log. Without it, that release fails before deploying anything. A repository
variable rather than something on the tag: it is visible, dated and revocable, and a tag is
none of those.

Acknowledgement covers writes being **on**, not any reading at all. `DRY_RUN` must come back as
`1` or `0`; anything else — empty, a shape gcloud changed, a field that moved — fails the release
whether or not the variable is set. Otherwise setting it once would turn the gate off
permanently, including for the failure the gate exists to catch.

## Attribution

Titelheld is an independent integration, **"Titelheld for Strava"**. It is not endorsed by,
sponsored by, or affiliated with Strava, and Strava takes no responsibility for it.

No Strava logos, marks, or trade dress are used anywhere in this project. "Strava" appears only
as a plain-text reference to the service this integration talks to.

## License

[Apache License 2.0](LICENSE).
