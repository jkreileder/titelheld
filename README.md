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
> [The push subscription](#the-push-subscription). Nothing drains them while the scheduler is
> paused; that queue is the material the dry-run review reads. With `FIRESTORE_PROJECT` unset the
> service runs on the in-memory store and forgets the OAuth token on restart; see
> [docs/firestore-iam.md](docs/firestore-iam.md).
>
> **Nothing can write to Strava yet.** Dry run is the default and the zero value throughout;
> see [Writes and dry run](#writes-and-dry-run).

- [What it does](#what-it-does)
- [Repository layout](#repository-layout)
- [The classifier](#the-classifier)
- [Writes and dry run](#writes-and-dry-run)
- [What gets written](#what-gets-written)
- [What the prompt carries](#what-the-prompt-carries)
- [Configuration](#configuration)
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
- [Infrastructure](#infrastructure)
  - [What Terraform manages](#what-terraform-manages)
  - [Why the service is publicly invocable](#why-the-service-is-publicly-invocable)
  - [One-time bootstrap](#one-time-bootstrap)
  - [Apply order](#apply-order)
  - [The push subscription](#the-push-subscription)
- [Cutting a release](#cutting-a-release)
  - [The steps](#the-steps)
  - [What the tag push does](#what-the-tag-push-does)
  - [Versions do not turn on writes](#versions-do-not-turn-on-writes)
- [Attribution](#attribution)
- [License](#license)

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
another tool wrote. A human's title is never overwritten.

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

Beyond the ride itself, three things — all of them derived, none of them committed to this
repository.

**The last 25 titles worth not repeating.** The prompt forbids repeating any of them and
invites referring back. They come from the named log with one kind dropped: a commute or errand
template is meant to repeat, so listing it both forbids the right answer for the next commute and
crowds the real titles out of a list of twenty-five. Everything else stays, imported titles
included — a title the athlete gave a ride years ago is one to avoid repeating, whoever wrote it.

**Few-shot examples, from titles this service wrote and no others.** Six of them, rebuilt at
prompt time: the named log keeps each title and the language it was written in, and the ride that
produced it is re-read from Strava to describe the situation. Only rows marked `service` qualify
— not a filter over titles that look unsuitable, but a source an imported row cannot carry, so a
decade of the athlete's own shorthand is *structurally* unable to teach style, rather than
pattern-matched out of it. That shorthand is bare place names, private jokes and whatever a tool
left behind; six of them would teach a model to answer with a bare town name. Until this service
has named
something, the synthetic set is what the prompt carries — that is its purpose, not a stopgap.
Deriving an example costs a Strava read, so each is cached against the activity it describes: the
history only changes when something is named, so a sweep repeating every five minutes pays once.

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
they come only from titles this service wrote, so the synthetic set stands in until it has
written one.

## Configuration

All configuration comes from the environment. Nothing is read from a file, so secrets have no
route into the working tree; on Cloud Run they are injected from Secret Manager.

| Variable               | Required | Default | Purpose                                      |
| ---------------------- | -------- | ------- | -------------------------------------------- |
| `STRAVA_CLIENT_ID`     | yes      | —       | Strava API application ID                    |
| `STRAVA_CLIENT_SECRET` | yes      | —       | Strava API application secret                |
| `STRAVA_VERIFY_TOKEN`  | yes      | —       | Shared secret for the subscription handshake |
| `WEBHOOK_PATH_SECRET`  | yes      | —       | Unguessable segment of the webhook path      |
| `BASE_URL`             | yes      | —       | Public base URL, used for the OAuth redirect |
| `STRAVA_ATHLETE_ID`    | no       | any     | Restrict processing to one athlete           |
| `PROCESS_DELAY`        | no       | `10m`   | How long to wait before naming               |
| `DRY_RUN`              | no       | on      | Set to `0` to permit writes                  |
| `PORT`                 | no       | `8080`  | Listen port; Cloud Run sets this             |

The sweep that drains the delay queue is configured by three variables. Terraform sets all
three; nothing else does, and none of them is written by hand.

Set none of them and the sweep route is not mounted at all, which is the local case. Set some
of them and the service refuses to start: ignoring the rest would leave the queue silently
undrained, and inferring the missing one would mean inventing the identity the endpoint trusts.

`SWEEP_AUDIENCE` is the one exception, and it is not a special case so much as the first apply.
The path is generated and the service account exists from the start, so Terraform always sets
both; the audience is built from the service's own URL, which Cloud Run has not minted yet. An
empty audience therefore means "no sweep route yet" and the service starts normally — treating
it as fatal would stop the very apply that creates the service. The route appears on the second
apply, with the audience filled in. The unsafe reading, *accept any audience*, is the one thing
that never happens.

| Variable                | Required | Default | Purpose                                          |
| ----------------------- | -------- | ------- | ------------------------------------------------ |
| `SWEEP_PATH`            | no       | —       | Full sweep path; Terraform generates the segment |
| `SWEEP_AUDIENCE`        | no       | —       | Audience the Scheduler's OIDC token must carry   |
| `SWEEP_SERVICE_ACCOUNT` | no       | —       | Email of the only identity allowed to sweep      |

Naming. The default provider is keyless — Gemini is called through Vertex AI with the runtime
service account's own credentials, so `LLM_API_KEY` exists only for the Anthropic alternative
and is required only when that is selected.

| Variable                 | Required     | Default              | Purpose                                       |
| ------------------------ | ------------ | -------------------- | --------------------------------------------- |
| `LLM_PROVIDER`           | no           | `gemini`             | `gemini` (Vertex, keyless) or `anthropic`     |
| `LLM_MODEL`              | no           | provider default     | Overrides the shipped, pinned model ID        |
| `LLM_API_KEY`            | for Anthropic| —                    | Never read when the provider is `gemini`      |
| `VERTEX_PROJECT`         | no           | `FIRESTORE_PROJECT`  | Project the Vertex call bills to              |
| `VERTEX_LOCATION`        | no           | `europe-west3`       | Vertex region, or `global` — see below        |
| `BANNED_WORDS`           | no           | shipped list         | Comma-separated; rejected in a title          |
| `MACHINE_TITLE_PATTERNS` | no           | Xert's pattern       | Newline-separated regexes; see below          |

`MACHINE_TITLE_PATTERNS` is newline-separated rather than comma-separated because the entries
are regular expressions, and a comma inside `{1,3}` is not a separator.

The shipped model IDs are pinned, and each is recorded in the source next to the documentation
URL it was verified against and the date — `internal/naming/vertex.go` and
`internal/naming/anthropic.go`. They are not taken from a model's training data.

### Choosing the Vertex model and region

Gemini model availability is regional, and the documentation's model index does not describe it —
an index is a catalogue of models, not a statement about where each one is served. Reading the
publisher metadata per host does:

| Model              | `global` | `europe-west3` | `europe-west4` |
| ------------------ | -------- | -------------- | -------------- |
| `gemini-3.7-flash` | 200      | 404            | 404            |
| `gemini-3.6-flash` | 200      | 404            | 404            |
| `gemini-3.5-flash` | 200      | 200 (GA)       | 200 (GA)       |

The newest Flash models are real, but only behind the **global** endpoint, which routes the
request to whichever region has capacity. The prompt carries place names derived from your GPS
traces, and the rest of this deployment is `europe-west3`, so the shipped default is the newest
model served **in region**:

```sh
VERTEX_LOCATION=europe-west3   # default
LLM_MODEL=                     # unset -> gemini-3.5-flash
```

To use a newer model instead, at the cost of regional routing:

```sh
VERTEX_LOCATION=global
LLM_MODEL=gemini-3.7-flash
```

Both work with no code change. `global` is the one location whose host is unprefixed —
`aiplatform.googleapis.com`, not `global-aiplatform.googleapis.com`, which does not resolve.

Recheck availability the same way when a newer model lands — a metadata read, not an inference
call, so it costs nothing and generates no tokens:

```sh
PROJECT=titelheld-XXXXXX
MODEL=gemini-3.5-flash

for host in aiplatform.googleapis.com europe-west3-aiplatform.googleapis.com; do
  printf '%s: ' "$host"
  curl -s -o /dev/null -w '%{http_code}\n' \
    -H "Authorization: Bearer $(gcloud auth print-access-token)" \
    -H "x-goog-user-project: ${PROJECT}" \
    "https://${host}/v1/publishers/google/models/${MODEL}"
done
```

`200` means the model is served there, `404` that it is not. The `x-goog-user-project` header is
required: without it the call returns `403`, which says nothing about the model.

### Checking the naming path for real

The probe above proves a model exists. This proves the whole path — credentials, request shape,
response parsing and validation — with the same code the service runs. It sits behind a build tag,
so CI never runs it and no live call happens there:

```sh
VERTEX_PROJECT=titelheld-XXXXXX go test -tags smoke ./internal/naming/ -run TestLiveVertex -v
```

It needs application default credentials and spends a few tokens. A pass logs the validated title;
a failure names the step that broke, which is the point of running the real code rather than a
`curl` that resembles it.

That distinction is not academic. `gemini-3.5-flash` reasons by default and those tokens are
billed inside `maxOutputTokens`, so an early hand-written `curl` spent 241 of 256 thinking,
returned a truncated fragment, and stopped at `MAX_TOKENS`. The provider therefore sends
`thinkingConfig: {thinkingBudget: 0}` — naming a ride needs no chain of reasoning. Any hand-run
`curl` needs that field too, or it reproduces the original failure.

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
make           # lint, test, build
make test      # go test -race with coverage
make lint      # go vet + golangci-lint
make vulncheck # govulncheck
```

Install the pre-commit hooks with `pre-commit install` or `prek install`.

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
imported from service-written. The language is a heuristic and applies to imported titles only —
a title this service writes carries the language the model reported.

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
whitespace collapsed, the entry's leading article dropped — matched on token boundaries.
`Son of the Pink Panther: Gegenwind` counts. `Der Panther im Morgengrauen` does not, and neither
does a translation.

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

Firestore stores a single integer per athlete per franchise: where the rotation resumes, which is
the index of the first entry not yet used or stepped over. It never stores the titles. That is
what lets a franchise be renamed, reordered or extended without a migration, and it is why
removing a franchise from configuration is safe — a position for a franchise that no longer
exists is simply never read. `FranchisePosition` returns `0` for a franchise never used and for
one that does not exist, and those two cases are deliberately indistinguishable.

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
   reads the document once per process, so a running instance picks the change up on its next
   cold start — which, scaling to zero, is the next sweep.
3. **If some entries have already been used by hand**, leave those titles out of `titles` — what
   the shipped Pink Panther profile does, where the four films already used are simply absent.
   For entries you have walked past but want to keep, list them under `reserved` instead: they
   stay in the series, in order, and are never offered.
   Seeding the position also works and is sometimes clearer: it lives in
   `franchise/{athleteID}-{name}` as a single `position` integer, and setting it to the index the
   rotation should resume at is the whole of it.

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
go run ./cmd/titelheld
```

A gitignored `.env` inside the repository would also work, and `.gitignore` covers one, but it
is reachable by `git add -f`, by editor plugins and by folder-sync tools — and `git clean -xdf`
would delete it. `make clean` excludes `/.env` for that reason, but the file outside the tree
is the safer habit.

`DRY_RUN` defaults to on. Nothing can write to Strava until it is set to an explicit falsy
value, and the deployed service keeps it on.

## Infrastructure

Everything in GCP is Terraform, under [`infra/`](infra). `make tf-check` runs what CI runs on
a branch of this repository: `terraform fmt -check`, `validate` and `tflint`. A pull request
from a fork gets `fmt -check` alone — `init`, `validate` and `tflint` all execute providers or
plugins named by files in the pull request, so they wait until the change is on `main`.

**CI never applies.** It formats, validates and lints; applies are run by hand.

### What Terraform manages

| Resource                | Notes                                                                                                                       |
| ----------------------- | --------------------------------------------------------------------------------------------------------------------------- |
| Enabled APIs            | Run, Firestore, Secret Manager, Scheduler, Artifact Registry, Vertex AI, IAM, STS, budgets                                  |
| Firestore database      | Native mode, `europe-west3`, named `titelheld`, delete protection on                                                        |
| Runtime service account | `roles/datastore.user` on the one database, an accessor on five secrets, and `roles/aiplatform.user` so Gemini needs no key |
| Deploy service account  | Assumed by CI through WIF; `roles/run.developer` on the one service, not the project                                        |
| Workload Identity pool  | Provider condition requires the repository, the `production` environment and a `v*` tag ref                                 |
| Secret Manager          | Secret **resources only** — no versions, no values                                                                          |
| Artifact Registry       | Images CI pushes and Cloud Run runs                                                                                         |
| Cloud Run service       | min 0 / max 1, `ignore_changes` on the image so CI owns revisions                                                           |
| Cloud Scheduler         | The sweep, at an unguessable path, with an OIDC token the handler itself must verify                                        |
| Budget alert            | €1, at 50/90/100%                                                                                                           |

Secret **values** never appear in code, in tfvars, or in state. They are added out of band,
once each.

### Why the service is publicly invocable

Strava's webhook cannot present a Google credential, so the service has to accept
unauthenticated requests. Cloud Run's invoker permission is service-wide, so that admits
anonymous callers to **every** route, the sweep included. `ingress` controls the network path,
not authentication, and cannot narrow it.

What actually defends each route:

- **The webhook** — an unguessable path segment, plus the verify token compared in constant
  time over hashes.
- **The sweep** — an unguessable path segment, plus the OIDC token Cloud Scheduler attaches.
  **Cloud Run does not check that token** while the service is publicly invokable, so the
  application has to. That is a hard requirement for the phase that adds the sweep handler.

Two services, one public and one private, is the right answer if the sweep ever does anything
expensive. Today it would double the deployment to protect an endpoint whose only power is
draining the queue slightly early.

### One-time bootstrap

Terraform cannot create the project it stores its own state in, so this part is by hand.

First, credentials. `gcloud auth login` authenticates the CLI; Terraform's provider reads
*application default credentials*, which are separate, and without them the first `apply` fails
on authentication rather than on anything to do with the configuration:

```sh
gcloud auth login                        # if not already logged in
gcloud auth application-default login    # what Terraform itself uses
```

Then the project:

```sh
PROJECT=titelheld-XXXXXX          # project IDs are globally unique; pick a free one

# Pick the billing account deliberately rather than taking the first one: the
# project link and the budget must target the same account, or the budget
# quietly watches a different bill. `value(name)` yields billingAccounts/XXXXXX
# while --billing-account wants the bare ID, hence basename().
gcloud billing accounts list --filter='open=true' \
  --format='table(name.basename(), displayName, open)'
BILLING=XXXXXX-XXXXXX-XXXXXX      # the bare ID from the first column, also used in tfvars
REGION=europe-west3

gcloud projects create "$PROJECT"
gcloud billing projects link "$PROJECT" --billing-account="$BILLING"

# Terraform needs these two before it can enable the rest.
gcloud services enable cloudresourcemanager.googleapis.com serviceusage.googleapis.com   --project="$PROJECT"

# GCP creates a "default" VPC with every project and opens SSH and RDP on it
# to 0.0.0.0/0 — two HIGH findings in Security Command Center, for a network
# nothing here uses. Cloud Run is fully managed and Firestore, Secret Manager,
# Artifact Registry and Scheduler never touch a VPC, so the whole network goes.
# Terraform does not manage this: the network is created by GCP at project
# creation, not by any resource here.
gcloud compute firewall-rules delete default-allow-ssh default-allow-rdp \
  default-allow-icmp default-allow-internal --project="$PROJECT" --quiet
gcloud compute networks delete default --project="$PROJECT" --quiet

# State bucket: private, versioned, and never public. State holds no secret
# values, but it does hold every service-account email, the WIF pool and the
# generated sweep path.
gcloud storage buckets create "gs://${PROJECT}-tfstate"   --project="$PROJECT" --location="$REGION"   --uniform-bucket-level-access --public-access-prevention
gcloud storage buckets update "gs://${PROJECT}-tfstate" --versioning
```

The budget lives on the **billing account**, not the project, so applying it needs
billing-account permission that project-level roles do not grant. If `terraform apply` fails on
`google_billing_budget`, grant yourself `roles/billing.costsManager` on the billing account, or
create the budget by hand and leave that resource out.

### Apply order

Cloud Run resolves `version = "latest"` when it creates a revision, so the secrets need
values **before** the service is created. Applying everything in one go fails: the revision
cannot start, and the apply fails with it.

```sh
cd infra
terraform init -backend-config="bucket=${PROJECT}-tfstate"
cp terraform.tfvars.example terraform.tfvars   # fill in project_id and billing_account
```

1. **Create the APIs and the secret shells first.** Targeting a `for_each` resource without
   an index covers every instance of it.

   ```sh
   terraform apply \
     -target=google_project_service.this \
     -target=google_secret_manager_secret.this
   ```

2. **Add the secret values**, once each. They never pass through Terraform:

   ```sh
   add() { printf %s "$2" | gcloud secrets versions add "$1" --data-file=- --project="$PROJECT"; }

   add strava-client-id      "<client id from the Strava API settings>"
   add strava-client-secret  "<client secret>"
   add strava-verify-token   "$(openssl rand -hex 16)"   # you choose this; Strava echoes it
   add webhook-path-secret   "$(openssl rand -hex 16)"   # the unguessable path segment
   add llm-api-key           "<provider API key>"
   ```

   Each secret gets its own value, so they are added one at a time. A loop over a
   single variable writes the same value five times, or — if the variable is
   never set — five empty ones.

   Read the two generated values back when you need them: the path segment for the Strava
   callback URL, and the verify token for the subscription request.

   ```sh
   gcloud secrets versions access latest --secret=webhook-path-secret --project="$PROJECT"
   gcloud secrets versions access latest --secret=strava-verify-token --project="$PROJECT"
   ```

3. **Apply the rest.**

   ```sh
   terraform apply
   ```

4. **Set `base_url` and apply again.** Cloud Run mints the URL, so it cannot be known on the
   first apply. Read it from the `service_url` output, put it in `terraform.tfvars`, and
   re-apply.

5. **Wire CI.** Set the repository variables from the outputs:

   ```sh
   gh variable set WIF_PROVIDER --body "$(terraform output -raw workload_identity_provider)"
   gh variable set DEPLOY_SERVICE_ACCOUNT --body "$(terraform output -raw deploy_service_account)"
   gh variable set GCP_PROJECT --body "$PROJECT"
   gh variable set GCP_REGION --body "$REGION"
   ```

   Until those exist, the deploy job skips rather than failing.

   Create the `production` environment too. Federation requires the environment claim, so
   this is not optional: without the environment, the deploy job cannot assume the deploy
   identity at all.

   Create it **with a deployment tag policy**, not bare. The Workload Identity provider
   accepts any job in this repository that declares this environment, so the environment is
   what decides who may deploy — and an environment with no rules decides nothing. Restricting
   it to `v*` tags means a workflow added on a branch cannot mint a deploy token.

   ```sh
   gh api -X PUT "repos/jkreileder/titelheld/environments/production" \
     -F "deployment_branch_policy[protected_branches]=false" \
     -F "deployment_branch_policy[custom_branch_policies]=true" >/dev/null

   gh api -X POST "repos/jkreileder/titelheld/environments/production/deployment-branch-policies" \
     -f "name=v*" -f "type=tag" >/dev/null
   ```

   Add required reviewers as well if you want a human gate on every release; the provider's
   condition already limits federation to tag refs, so this is defense in depth rather than
   the only lock.

6. **Deploy.** The first Cloud Run revision runs a placeholder image
   (`us-docker.pkg.dev/cloudrun/container/hello`) purely so the service can exist. Pushing a
   signed `v*` tag replaces it — see [Cutting a release](#cutting-a-release) — and Terraform
   ignores the image from then on.

### The push subscription

Strava pushes an event per activity to one callback URL per API application. The subscription is
created by hand, once, and is not managed by Terraform: it lives in Strava's account, not in GCP,
and it depends on the deployed service answering before it can exist at all.

The current subscription is **id 367703**, pointing at `/webhook/<webhook-path-secret>` on the
Cloud Run service.

Creating one is a single request. Strava validates the callback *synchronously*: it issues a
`GET` with `hub.challenge` and expects the echo within two seconds, so warm the service first —
a cold start alone can spend most of that budget.

```sh
sec() { gcloud secrets versions access latest --secret="$1" --project="$PROJECT"; }
BASE_URL=$(cd infra && terraform output -raw service_url)

create_subscription() {
  curl -sf -o /dev/null "$BASE_URL/health" ||
    { echo "the service did not answer; not creating a subscription" >&2; return 1; }

  response=$(curl -s -w '\n%{http_code}' -X POST https://www.strava.com/api/v3/push_subscriptions \
    -F "client_id=$(sec strava-client-id)" \
    -F "client_secret=$(sec strava-client-secret)" \
    -F "callback_url=${BASE_URL}/webhook/$(sec webhook-path-secret)" \
    -F "verify_token=$(sec strava-verify-token)") ||
    { echo "create request failed" >&2; return 1; }

  code=$(printf '%s' "$response" | tail -n 1)
  body=$(printf '%s' "$response" | sed '$d')

  case $code in
    2??) ;;
    *) echo "create returned $code: $body" >&2; return 1 ;;
  esac

  id=$(printf '%s' "$body" | jq -r '.id // empty') ||
    { echo "unparseable response: $body" >&2; return 1; }

  case $id in
    '' | *[!0-9]*) echo "no subscription id in the response: $body" >&2; return 1 ;;
  esac

  echo "subscription $id created"
}

create_subscription
```

The warm-up is a precondition, not a courtesy, so its status is checked: creating against a
service that is not answering burns the callback validation for no reason. The `POST` is checked
the same way — `curl -s` alone exits 0 on a `4xx`, so a rejected callback would print Strava's
error and look like it worked. Any `2xx` is accepted rather than one exact code, and the id in the
body is what actually says a subscription exists.

A successful response is `{"id": <subscription id>}`. The handshake shows up in the service log as
`webhook subscription validated`; a rejection names its reason (`unexpected hub.mode`,
`verify token mismatch`) rather than failing silently.

Listing and deleting take the client credentials, not a token:

```sh
curl -s -G https://www.strava.com/api/v3/push_subscriptions \
  --data-urlencode "client_id=$(sec strava-client-id)" \
  --data-urlencode "client_secret=$(sec strava-client-secret)"
```

Deleting takes the id from that listing rather than from this page — recreating after a rotation
mints a new one, and a runbook that hard-codes the old id deletes nothing. As a function, so a
failed step returns instead of closing the shell you pasted it into:

```sh
delete_subscription() {
  subs=$(curl -sf -G https://www.strava.com/api/v3/push_subscriptions \
    --data-urlencode "client_id=$(sec strava-client-id)" \
    --data-urlencode "client_secret=$(sec strava-client-secret)") ||
    { echo "listing failed" >&2; return 1; }

  sub=$(printf '%s' "$subs" | jq -r '.[0].id // empty')
  case $sub in
    '' | *[!0-9]*) echo "no subscription to delete" >&2; return 1 ;;
  esac

  code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE -G \
    "https://www.strava.com/api/v3/push_subscriptions/${sub}" \
    --data-urlencode "client_id=$(sec strava-client-id)" \
    --data-urlencode "client_secret=$(sec strava-client-secret)") ||
    { echo "delete request failed" >&2; return 1; }

  [ "$code" = 204 ] || { echo "delete returned $code, not 204" >&2; return 1; }
}

delete_subscription
```

Every step is checked because none of them is loud on failure. `curl -s` says nothing about an
HTTP error, so the read needs `-f` *and* its exit status inspected before the output is parsed —
a pipeline reports `jq`'s status, not `curl`'s, so a failed listing can otherwise be parsed into a
plausible-looking id. An empty listing yields no id at all rather than a wrong one, which the
`case` catches. And a successful delete is `204` with an empty body, so the status code is the
only thing that distinguishes it from a `404` or an authentication failure.

The credentials go in the query string: Strava reads them as parameters, and `curl -d` would send
them as a request body it ignores.

There is **one subscription per application**: creating a second
fails while the first exists, so changing the callback URL means deleting and recreating. That is
also what to do after a `webhook-path-secret` rotation — the old URL stops existing the moment the
new secret is deployed, and Strava will keep posting to it until told otherwise.

Events accumulate in the `pending` queue from the moment the subscription exists. Nothing drains
them until the Cloud Scheduler job is unpaused or a sweep is triggered by hand:

```sh
gcloud scheduler jobs run titelheld-sweep --location="$REGION" --project="$PROJECT"
```

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
| Draft  | `release.yaml`       | A draft GitHub release for you to publish                                                                     |

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

### Versions do not turn on writes

`DRY_RUN` is Terraform's to set, and it is set to `1`. The deploy step reads it back from the
running service afterward and fails the release if it is anything else. There is no version
number that means *now write to Strava*: turning writes on is a deliberate infrastructure
change, never a side effect of shipping.

## Attribution

Titelheld is an independent integration, **"Titelheld for Strava"**. It is not endorsed by,
sponsored by, or affiliated with Strava, and Strava takes no responsibility for it.

No Strava logos, marks, or trade dress are used anywhere in this project. "Strava" appears only
as a plain-text reference to the service this integration talks to.

## License

[Apache License 2.0](LICENSE).
