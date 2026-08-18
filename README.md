<!-- omit in toc -->
# Titelheld

A single-athlete backend service that gives Strava activities context-aware titles — and leaves
everything that should stay boring untouched.

*Titelheld* is German for the character a piece is named after: the one in the title role.

> **Status: early construction.** The classifier, configuration, the store (in-memory *and*
> Firestore), the Strava client with OAuth, the webhook with its delay-queue enqueue, and
> geocoding are implemented, and the binary runs. The prompt builder, the LLM interface, the
> sweep and writer, and the Cloud Run deployment are not built yet. Operator documentation
> (GCP setup, Strava app registration, config schema, franchises) lands with those phases.
>
> **No Strava push subscription exists yet.** With `FIRESTORE_PROJECT` unset the service runs
> on the in-memory store and forgets the OAuth token on restart; see
> [docs/firestore-iam.md](docs/firestore-iam.md).
>
> **Nothing can write to Strava yet.** Dry run is the default and the zero value throughout;
> see [Writes and dry run](#writes-and-dry-run).

- [What it does](#what-it-does)
- [Repository layout](#repository-layout)
- [The classifier](#the-classifier)
- [Writes and dry run](#writes-and-dry-run)
- [Configuration](#configuration)
- [HTTP surface](#http-surface)
- [Development](#development)
- [Security and privacy](#security-and-privacy)
- [Attribution](#attribution)
- [License](#license)

## What it does

The service is the **last writer** in a chain of Strava automations. Other tools
(ActivityFix, Xert) fix up sport type, gear and workout summaries first; this service waits a
configurable delay, then names an activity only if nothing else has already titled it.

An activity is only ever renamed. Sport type, gear and descriptions are never touched.

## Repository layout

| Path                        | Purpose                                                                                |
| --------------------------- | -------------------------------------------------------------------------------------- |
| `internal/classifier/`      | Tier rules and the Strava default-title gate. No I/O, no deps.                         |
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

| Tier | Name       | Matches                                            | Action when still at a Strava default |
| ---- | ---------- | -------------------------------------------------- | ------------------------------------- |
| 1    | Skip       | `WeightTraining`, `Workout`, `Walk`, `Hike`, Whoop | never written                         |
| 2    | Virtual    | `VirtualRide`, or a ride with the trainer flag     | `zwift_mode`: keep, or indoor LLM     |
| 3    | Commute    | short ride to or from the work geofence            | deterministic commute title           |
| 4    | Errand     | commute-tagged ride                                | deterministic errand title            |
| 5    | Sport ride | ride ≥ 15 km or ≥ 45 min                           | full LLM naming pipeline              |
| —    | None       | anything else (runs, swims, short rides)           | never written                         |

Tiers 3 to 5 apply to rides only: the trainer flag does not make a treadmill run a virtual
ride, and a commute-tagged run does not become an errand. Tier 3's geofence match is capped by
the tier-5 thresholds, so a long ride that merely finishes at work stays a sport ride; a title
ActivityFix already wrote is taken at face value whatever the ride's size.

The **skip gate** runs after tier assignment: unless the activity's current title is a recognised
Strava default, the action is downgraded to skip. The gate fails closed — an unrecognised title is
assumed to be authored by a human or another tool.

## Writes and dry run

This service renames real activities on a real account, so the safe state is the one you get
by doing nothing:

- `config.Config.WritesEnabled` is expressed positively, so a zero-valued config is dry run.
- `strava.WriteMode`'s zero value is `WriteModeDryRun`, so a client built without thinking
  about it refuses to write.
- `DRY_RUN` stays on unless it holds an explicit falsy value (`0`, `false`, `no`, `off`).
  Anything unrecognised is reported as an error *and* leaves dry run on — a typo must never be
  what lets the service loose.
- `UpdateActivityName` refuses with `ErrDryRun` before building a request, and the transport
  refuses every non-GET method again, so a future write path cannot slip past the first check.

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

## HTTP surface

| Route                    | Purpose                                                     |
| ------------------------ | ----------------------------------------------------------- |
| `GET /healthz`           | Liveness check                                              |
| `GET /auth`              | Starts the one-time authorization; redirects to Strava      |
| `GET /auth/callback`     | Completes it, verifies the granted scopes, stores the token |
| `GET /webhook/<secret>`  | Strava's subscription validation handshake                  |
| `POST /webhook/<secret>` | Event intake; queues the activity after the delay           |

Both the webhook and the authorization start are mounted at their full secret paths, so
guessing the prefix but not the segment is a 404 from the router. Starting the flow is what
needs protecting: a bare `/auth` would let anyone authorize their own Strava account and have
this service store their token. The callback stays at a fixed, registered URL and is guarded
by the single-use state that only the start route issues; with no `STRAVA_ATHLETE_ID` set, the
service binds to whoever authorizes first and refuses anyone else afterwards.

The verify token is compared in constant time over hashes, so neither its contents nor its
length leak. `WEBHOOK_PATH_SECRET` is validated at load: a segment containing a space would
panic the router at registration, and one of the form `{x}` would register as a wildcard and
remove the unguessable-path defence entirely.

Events are acknowledged before the queue is written, which is the order Strava's two-second
budget assumes. A delivery that is never acknowledged is retried, and the queue is idempotent,
so the ordering costs nothing that is not already handled.

The delay is served by a **Cloud Scheduler sweep** rather than Cloud Tasks: it needs no second
GCP service and no client library, a failed activity simply stays queued until the next sweep
instead of needing its own retry policy, and ten-minute precision makes the scheduler's coarse
granularity irrelevant. This phase only enqueues; the sweep endpoint lands with the writer.

## Development

```sh
make          # lint, test, build
make test     # go test -race with coverage
make lint     # go vet + golangci-lint
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

## Attribution

Titelheld is an independent integration, **"Titelheld for Strava"**. It is not endorsed by,
sponsored by, or affiliated with Strava, and Strava takes no responsibility for it.

No Strava logos, marks, or trade dress are used anywhere in this project. "Strava" appears only
as a plain-text reference to the service this integration talks to.

## License

[Apache License 2.0](LICENSE).
