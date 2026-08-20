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
- [Geography](#geography)
- [Local development](#local-development)
- [Infrastructure](#infrastructure)
  - [What Terraform manages](#what-terraform-manages)
  - [Why the service is publicly invocable](#why-the-service-is-publicly-invocable)
  - [One-time bootstrap](#one-time-bootstrap)
  - [Apply order](#apply-order)
- [Cutting a release](#cutting-a-release)
  - [The steps](#the-steps)
  - [What the tag push does](#what-the-tag-push-does)
  - [Versions do not turn on writes](#versions-do-not-turn-on-writes)
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

The **skip gate** runs after tier assignment: unless the activity's current title is a recognized
Strava default, the action is downgraded to skip. The gate fails closed — an unrecognized title is
assumed to be authored by a human or another tool.

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
remove the unguessable-path defense entirely.

Events are acknowledged before the queue is written, which is the order Strava's two-second
budget assumes. A delivery that is never acknowledged is retried, and the queue is idempotent,
so the ordering costs nothing that is not already handled.

The delay is served by a **Cloud Scheduler sweep** rather than Cloud Tasks: it needs no second
GCP service and no client library, a failed activity simply stays queued until the next sweep
instead of needing its own retry policy, and ten-minute precision makes the scheduler's coarse
granularity irrelevant. This phase only enqueues; the sweep endpoint lands with the writer.

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

| Resource                | Notes                                                                                       |
| ----------------------- | ------------------------------------------------------------------------------------------- |
| Enabled APIs            | Run, Firestore, Secret Manager, Scheduler, Artifact Registry, IAM, STS, budgets             |
| Firestore database      | Native mode, `europe-west3`, named `titelheld`, delete protection on                        |
| Runtime service account | `roles/datastore.user` on the one database, plus an authoritative accessor on five secrets  |
| Deploy service account  | Assumed by CI through WIF; `roles/run.developer` on the one service, not the project        |
| Workload Identity pool  | Provider condition requires the repository, the `production` environment and a `v*` tag ref |
| Secret Manager          | Secret **resources only** — no versions, no values                                          |
| Artifact Registry       | Images CI pushes and Cloud Run runs                                                         |
| Cloud Run service       | min 0 / max 1, `ignore_changes` on the image so CI owns revisions                           |
| Cloud Scheduler         | The sweep, at an unguessable path, with an OIDC token the handler itself must verify        |
| Budget alert            | €1, at 50/90/100%                                                                           |

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

| Step | Where | What it produces |
| ---- | ----- | ---------------- |
| Check | `release.yaml` | Fails fast on a lightweight tag, a pre-release version, or a stale changelog |
| Build | `release-image.yaml` | One image, cache-free, tagged `0.1.0`, `0.1`, `0` and `sha-<commit>` |
| Attest | `release-image.yaml` | Sigstore-signed SLSA provenance, stored in GitHub and pushed to Artifact Registry as a referrer of the digest |
| Deploy | `release.yaml` | `gcloud run deploy` of that **digest**, via Workload Identity Federation |
| Draft | `release.yaml` | A draft GitHub release for you to publish |

The image is built exactly once. Everything after it refers to the digest, never to a version
tag, so moving a tag afterwards cannot change what is running.

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
running service afterwards and fails the release if it is anything else. There is no version
number that means *now write to Strava*: turning writes on is a deliberate infrastructure
change, never a side effect of shipping.

## Attribution

Titelheld is an independent integration, **"Titelheld for Strava"**. It is not endorsed by,
sponsored by, or affiliated with Strava, and Strava takes no responsibility for it.

No Strava logos, marks, or trade dress are used anywhere in this project. "Strava" appears only
as a plain-text reference to the service this integration talks to.

## License

[Apache License 2.0](LICENSE).
