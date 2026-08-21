<!-- omit in toc -->
# Firestore access for the runtime service account

What the Cloud Run service account needs in order to run Titelheld, and — more usefully — what
it does not.

- [What is stored](#what-is-stored)
- [The role](#the-role)
- [Scoping to one database](#scoping-to-one-database)
- [What this does not grant](#what-this-does-not-grant)
- [The limit worth knowing](#the-limit-worth-knowing)
- [Indexes](#indexes)
- [Local and CI](#local-and-ci)

## What is stored

Five collections, and nothing else. Adding a sixth means changing this document.

| Collection  | Document ID               | Contents                                         | Re-derivable?           |
| ----------- | ------------------------- | ------------------------------------------------ | ----------------------- |
| `tokens`    | `{athleteID}`             | OAuth access and refresh token, expiry, scopes   | **No**                  |
| `pending`   | `{athleteID}-{activity}`  | Queued activity and its `process_after` deadline | Yes                     |
| `named`     | `{athleteID}-{activity}`  | Title written, its language and source, and when | Mostly, from Strava     |
| `geocache`  | rounded coordinate key    | Verified place names from Nominatim              | Yes, by refetching      |
| `franchise` | `{athleteID}-{franchise}` | Position in an ordered title series              | In principle, painfully |

`franchise` stores an integer, never the titles: the series is configuration, so renaming or
reordering one must not require migrating anything here. It is re-derivable only by matching past
titles against a series, which is why it is remembered rather than recomputed — and losing it
costs a repeated or skipped entry, not a wrong write.

`named` also stores the language each title was written in and whether a model or a template
produced it. Neither is re-derivable: re-reading an activity returns the title but never says
which language was chosen for it, or which tier named it.

Only `tokens` genuinely has to survive. Strava rotates the refresh token on every refresh and
invalidates the previous one immediately, so losing that document means re-running the
authorization flow by hand. The other four are a work queue, two caches, and franchise
position state.

Location data is minimized, not absent. No coordinate is stored as a *field*: `geocache`
documents hold place names only. The coordinate that produced a place does survive as the
document ID, rounded to three decimals — roughly 110 m, and enough to reconstruct the rough
shape of a route from the cache alone. Nothing finer is retained anywhere.

## The role

`roles/datastore.user` is read/write access to data in a Firestore database — the
`datastore.entities.*` permissions plus metadata reads. It is the least-privileged predefined
role that lets an application read and write documents.

Granted **without a condition it covers every database in the project**, so bind it with one:

```sh
gcloud projects add-iam-policy-binding "$PROJECT" \
  --member="serviceAccount:$RUNTIME_SA" \
  --role="roles/datastore.user" \
  --condition='expression=resource.name == "projects/'"$PROJECT"'/databases/titelheld",title=titelheld-database-only'
```

Verify the condition takes effect before relying on it — see the next section.

## Scoping to one database

Create a **named** database rather than using `(default)`, and attach an IAM condition so the
binding covers that database alone:

```sh
gcloud firestore databases create --database=titelheld --location="$REGION" --type=firestore-native
```

Firestore supports IAM Conditions for per-database access; see
[Security for server client libraries][iam-docs] and the
[conditions attribute reference][attr-docs]. The resource name has the form
`projects/$PROJECT/databases/titelheld`.

Confirm the binding actually landed with a condition, because a malformed expression is
accepted as an unconditioned grant in some tooling:

```sh
gcloud projects get-iam-policy "$PROJECT" \
  --flatten="bindings[].members" \
  --filter="bindings.members:$RUNTIME_SA AND bindings.role:roles/datastore.user" \
  --format="yaml(bindings.condition)"
```

Set `FIRESTORE_PROJECT` and `FIRESTORE_DATABASE` on the service to match. With
`FIRESTORE_PROJECT` unset the service falls back to the in-memory store and says so loudly at
startup.

## What this does not grant

`roles/datastore.user` deliberately excludes:

- `roles/datastore.owner` — no creating or deleting databases.
- Import and export (`datastore.databases.import` / `.export`) — no bulk extraction of the
  athlete's data.
- Index administration — Terraform declares the one composite index, see below.
- Any other Google Cloud service beyond the two the naming pipeline needs. The runtime account
  also holds a Secret Manager accessor on five named secrets, and `roles/aiplatform.user` so it
  can call Gemini on Vertex AI — which is what lets Gemini be keyless, since the call
  authenticates as this account rather than with an API key. `aiplatform.user` grants calling
  models and nothing else: no tuning, no training, no model or endpoint administration, and no
  access to data in this project. Both are separate bindings, documented with the deployment.

## The limit worth knowing

**Per-collection IAM restriction is not possible for a server service account.** The finest
grain IAM offers is the database. Firestore Security Rules can express per-collection and
per-document rules, but they apply only to the mobile and web SDKs — server client libraries
authenticated with a service account bypass them entirely.

So "least privilege" here means a dedicated database in a dedicated project, not a binding that
can reach `tokens` but not `geocache`. Anything holding this service account's credentials can
read every collection listed above, including the OAuth tokens. That is an argument for keeping
the project small and the service account used by nothing else, and it is why the token pair is
the only irreplaceable thing stored.

## Indexes

One, declared in Terraform and created by an apply. The runtime account still needs no index
administration permission: it uses indexes, it does not manage them.

The title history is the query that needs it:

```text
where athlete_id == N  order by named_at desc  limit 25
```

An equality on one field with an ordering on another is what a composite index is for, so
`google_firestore_index.named_recent` declares `athlete_id` ascending, `named_at` descending.
It must exist before the sweep runs: a missing index is not a slow query, it is an error on
every naming. That is why the apply comes before the scheduler is unpaused.

The sweep's due lookup needs nothing:

```text
where process_after <= now  order by process_after asc
```

The inequality and the ordering are on the same field, so Firestore serves it from the
automatic single-field index.

`franchise` needs nothing either. It is addressed by document ID in both directions — read the
position, or increment it in a transaction — so it runs no query at all.

**The emulator will not tell you when this is wrong.** It serves any query without index
definitions, so a mismatch between the Terraform declaration and the query in `RecentTitles`
passes every test in this repository and fails only against the real database.

## Local and CI

Neither touches a real project. Setting `FIRESTORE_EMULATOR_HOST` makes the client library talk
to the emulator and skip credentials entirely:

The image is pinned by digest, matching `.github/workflows/go.yaml`, so a local run and CI test
against the same emulator. Renovate updates both occurrences together — see the custom manager
in `.github/renovate.json`; changing one by hand will drift.

```sh
docker run --rm -p 8080:8080 \
  gcr.io/google.com/cloudsdktool/google-cloud-cli:emulators@sha256:25300472f1fa63b4df0e0c3a5dd67bdc6774b39f6dd440605e520a6d04ae0f26 \
  gcloud emulators firestore start --host-port=0.0.0.0:8080

FIRESTORE_EMULATOR_HOST=127.0.0.1:8080 go test ./internal/store/...
```

Without that variable the Firestore tests skip, so `go test ./...` still works on a machine with
no emulator. CI runs them against the emulator and then asserts they did not skip — a suite of
skips otherwise looks exactly like a suite of passes.

On macOS, Docker Desktop's published ports do not carry the emulator's HTTP/2 traffic; run the
tests inside the emulator's network namespace instead
(`docker run --network container:<emulator> ...`).

[iam-docs]: https://cloud.google.com/firestore/native/docs/security/iam
[attr-docs]: https://cloud.google.com/iam/docs/conditions-attribute-reference
