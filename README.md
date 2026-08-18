<!-- omit in toc -->
# strava-namer

A single-athlete backend service that gives Strava activities context-aware titles — and leaves
everything that should stay boring untouched.

> **Status: early construction.** Only the activity classifier is implemented. The Strava client,
> webhook, store, geocoding, prompt builder, LLM interface, delay queue and the Cloud Run
> deployment are not built yet, and there is no runnable binary. Operator documentation (GCP
> setup, Strava app registration, OAuth bootstrap, config schema, franchises) lands with those
> phases.

- [What it does](#what-it-does)
- [Repository layout](#repository-layout)
- [The classifier](#the-classifier)
- [Development](#development)
- [Security and privacy](#security-and-privacy)
- [License](#license)

## What it does

The service is the **last writer** in a chain of Strava automations. Other tools
(ActivityFix, Xert) fix up sport type, gear and workout summaries first; this service waits a
configurable delay, then names an activity only if nothing else has already titled it.

An activity is only ever renamed. Sport type, gear and descriptions are never touched.

## Repository layout

| Path                   | Purpose                                                        |
| ---------------------- | -------------------------------------------------------------- |
| `internal/classifier/` | Tier rules and the Strava default-title gate. No I/O, no deps. |

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
| 2    | Virtual    | `VirtualRide` or the trainer flag                  | `zwift_mode`: keep, or indoor LLM     |
| 3    | Commute    | ride to/from the work geofence                     | deterministic commute title           |
| 4    | Errand     | commute-tagged ride                                | deterministic errand title            |
| 5    | Sport ride | ride ≥ 15 km or ≥ 45 min                           | full LLM naming pipeline              |
| —    | None       | anything else (runs, swims, short rides)           | never written                         |

The **skip gate** runs after tier assignment: unless the activity's current title is a recognised
Strava default, the action is downgraded to skip. The gate fails closed — an unrecognised title is
assumed to be authored by a human or another tool.

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

## License

[Apache License 2.0](LICENSE).
