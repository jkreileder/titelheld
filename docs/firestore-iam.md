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

Four collections, and nothing else. Adding a fifth means changing this document.

| Collection | Document ID              | Contents                                         | Re-derivable?      |
| ---------- | ------------------------ | ------------------------------------------------ | ------------------ |
| `tokens`   | `{athleteID}`            | OAuth access and refresh token, expiry, scopes   | **No**             |
| `pending`  | `{athleteID}-{activity}` | Queued activity and its `process_after` deadline | Yes                |
| `named`    | `{athleteID}-{activity}` | Title this service wrote, and when               | Yes, from Strava   |
| `geocache` | rounded coordinate key   | Verified place names from Nominatim              | Yes, by refetching |

Only `tokens` genuinely has to survive. Strava rotates the refresh token on every refresh and
invalidates the previous one immediately, so losing that document means re-running the
authorization flow by hand. The other three are a work queue and two caches.

No coordinates are stored. `geocache` documents hold place names; the coordinates that produced
them appear only as the rounded document ID.

## The role

```sh
gcloud projects add-iam-policy-binding "$PROJECT" \
  --member="serviceAccount:$RUNTIME_SA" \
  --role="roles/datastore.user"
```

`roles/datastore.user` is read/write access to data in a Firestore database — the
`datastore.entities.*` permissions plus metadata reads. It is the least-privileged predefined
role that lets an application read and write documents.

## Scoping to one database

Create a **named** database rather than using `(default)`, and attach an IAM condition so the
binding covers that database alone:

```sh
gcloud firestore databases create --database=titelheld --location="$REGION" --type=firestore-native
```

Firestore supports IAM Conditions for per-database access; see
[Security for server client libraries][iam-docs]. Confirm the exact condition attributes for
your setup with `gcloud iam list-testable-permissions` and the
[conditions attribute reference][attr-docs] before applying — the resource name has the form
`projects/$PROJECT/databases/titelheld`.

Set `FIRESTORE_PROJECT` and `FIRESTORE_DATABASE` on the service to match. With
`FIRESTORE_PROJECT` unset the service falls back to the in-memory store and says so loudly at
startup.

## What this does not grant

`roles/datastore.user` deliberately excludes:

- `roles/datastore.owner` — no creating or deleting databases.
- Import and export (`datastore.databases.import` / `.export`) — no bulk extraction of the
  athlete's data.
- Index administration — the queries here need no composite index, see below.
- Any other Google Cloud service. The runtime account needs Secret Manager access for the
  Strava and LLM credentials; that is a separate binding and is documented with the deployment.

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

None to create. The only non-trivial query is the sweep's due lookup:

```text
where process_after <= now  order by process_after asc
```

The inequality and the ordering are on the same field, so Firestore serves it from the
automatic single-field index. There is no composite index to create, and therefore no index
administration permission to grant.

## Local and CI

Neither touches a real project. Setting `FIRESTORE_EMULATOR_HOST` makes the client library talk
to the emulator and skip credentials entirely:

```sh
docker run --rm -p 8080:8080 \
  gcr.io/google.com/cloudsdktool/google-cloud-cli:emulators \
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
