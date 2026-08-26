<!-- The GCP deployment: what Terraform owns, how it is bootstrapped, and the
order things have to happen in. Split out of the README, which is about what
the service does rather than how it is deployed. -->

# Infrastructure

Everything in GCP is Terraform, under [`infra/`](../infra). `make tf-check` runs what CI runs on
a branch of this repository: `terraform fmt -check`, `validate` and `tflint`. A pull request
from a fork gets `fmt -check` alone — `init`, `validate` and `tflint` all execute providers or
plugins named by files in the pull request, so they wait until the change is on `main`.

**CI never applies.** It formats, validates and lints; applies are run by hand.

## What Terraform manages

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

## Why the service is publicly invocable

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

## One-time bootstrap

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

## Apply order

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

   `llm-api-key` is read only by a keyed provider (`anthropic`, `openrouter`), and
   the shipped default, Vertex, is keyless — so this one may be left without a
   version until an A/B of narrators is wanted. Switching narrators later is: a
   new version of this secret holding the selected provider's key, the variables
   `llm_provider`, `llm_model` and `llm_base_url` in your tfvars, and an apply.
   The variables reach the container only when set; unset, the binary sees
   nothing and resolves Vertex, exactly as its dormancy test asserts.

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
   signed `v*` tag replaces it — see [Cutting a release](../README.md#cutting-a-release) — and Terraform
   ignores the image from then on.

## The push subscription

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
curl -sf -G https://www.strava.com/api/v3/push_subscriptions \
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
them until the Cloud Scheduler job is unpaused or a sweep is triggered by hand.

Triggering one by hand is three commands, not one. `gcloud scheduler jobs run` refuses a paused
job:

```text
FAILED_PRECONDITION: Job.state must be ENABLED for RunJob
```

First read what the service will do with what it names, because the sequence below resumes the
recurring job and a tick can fire while it is resumed:

```sh
gcloud run services describe titelheld --project="$PROJECT" --region="$REGION" \
  --format='value(spec.template.spec.containers[0].env.filter("name:DRY_RUN").extract("value"))'
```

`1` is the precondition for the rest of this section: a tick that lands mid-sequence then costs
duplicated work and nothing else. Anything else — `0`, or an empty line, which means the read did
not find the variable rather than that writes are off — and this is not the procedure to run.
Writes are enabled by hand, and a sweep under them needs a sequence that cannot overlap the
schedule; there is no such sequence here yet. The same read gates a release, in
`.github/scripts/writes-gate.sh`.

So the job is resumed, run, and paused again — and the pause happens whatever the run did, or a
failed dispatch leaves the job firing every five minutes:

```sh
job() { gcloud scheduler jobs "$@" titelheld-sweep --location="$REGION" --project="$PROJECT"; }

if job resume; then
  job run && dispatched=yes || dispatched=no
  job pause || echo "STILL RUNNING: pause it by hand" >&2
  [ "$dispatched" = yes ] || echo "the sweep was never dispatched" >&2
else
  echo "resume failed; the job is still paused and nothing ran" >&2
fi

job describe --format='value(state)'    # want PAUSED
```

No `exit`, because this is meant to be pasted into the shell you are sitting in, where `exit`
closes the terminal. The `pause` runs whatever the `run` did — a failed dispatch must not leave
the job firing every five minutes — and it survives an `errexit` shell too: a failing command in
an `&&`/`||` list does not trigger it.

A function rather than a variable of flags, because zsh does not word-split an unquoted expansion
and bash does. Verified in `bash`, `zsh`, `dash` and `sh`.

Run the lines one at a time if you prefer; only the order matters, and that the `pause` runs
whatever the `run` did.

A dispatch that succeeded says nothing about what the sweep did — Cloud Scheduler reports that it
sent the request, not what came back. The service's own log lines are the answer:

```sh
gcloud logging read 'resource.type="cloud_run_revision" AND
  resource.labels.service_name="titelheld" AND
  (jsonPayload.msg="sweep complete" OR jsonPayload.msg="sweep rejected"
   OR jsonPayload.msg="sweep failed"
   OR jsonPayload.msg="sweep already running; skipping this fire")' \
  --project="$PROJECT" --limit=10 --freshness=10m
```

Scoped to this service and to the last ten minutes, so what comes back is this dispatch rather
than the last time anything swept. `sweep already running; skipping this fire` is in there
because it is a real answer for a manual run: the scheduled tick got there first, your request
was answered `200` without starting a second sweep, and the `sweep complete` line next to it
belongs to that tick rather than to you.

`sweep complete` carries the counts, and the sweep drained the queue only if `failed` is zero
**and** `cancelled` is false. A cancelled sweep is a `200` with a clean `failed` count: Cloud Run
took the instance away and the sweep stopped at an activity boundary, leaving the rest queued for
a run that is not coming while the job is paused. `sweep rejected` is the `401` — it names the
claim that did not check out, and an audience mismatch is silent in every other place you could
look: Cloud Scheduler reports a delivered request, and the response body is the bare word
`unauthorized`. `sweep failed` is the `500`, and it means no sweep happened at all: the queue
itself could not be read, so nothing was fetched, named or dequeued. Find out why before running
it again — everything is still queued, so nothing is lost by waiting.

**There is a race, and it is deliberate.** While the job is resumed its own schedule can fire —
five minutes is the interval, the three commands take seconds, so it usually does not, but it
can. A fire that lands while a sweep is running is answered `already running` without starting a
second one; one that lands after it is a second full sweep — the queue read again, activities
fetched and classified again, the model called again for each one still due. Under `DRY_RUN=1`
none of that reaches Strava and every named activity stays queued, so what it costs is duplicated
work and a duplicated log line. It stops being free the day writes are enabled: a tick between
`resume` and `pause` would then rename activities, which is what the `DRY_RUN` precondition
above refuses.
