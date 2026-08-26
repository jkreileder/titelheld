<!-- Every setting this service reads, and why each one exists. Split out of
the README, which is about what the service does rather than how a deployment
is wired. -->

# Configuration

All configuration comes from the environment. Nothing is read from a file, so secrets have no
route into the working tree; on Cloud Run they are injected from Secret Manager.

| Variable               | Required | Default | Purpose                                      |
| ---------------------- | -------- | ------- | -------------------------------------------- |
| `STRAVA_CLIENT_ID`     | yes      | —       | Strava API application ID                    |
| `STRAVA_CLIENT_SECRET` | yes      | —       | Strava API application secret                |
| `STRAVA_VERIFY_TOKEN`  | yes      | —       | Shared secret for the subscription handshake |
| `WEBHOOK_PATH_SECRET`  | yes      | —       | Unguessable segment of the webhook path      |
| `BASE_URL`             | yes      | —       | Public base URL, used for the OAuth redirect |
| `MAX_INSTANCES`        | yes      | —       | Must be `1`; Terraform sets it — see below   |
| `STRAVA_ATHLETE_ID`    | no       | any     | Restrict processing to one athlete           |
| `PROCESS_DELAY`        | no       | `10m`   | How long to wait before naming               |
| `DRY_RUN`              | no       | on      | Set to `0` to permit writes                  |
| `LOG_PROMPT`           | no       | dry run | Log the whole prompt for each naming         |
| `PORT`                 | no       | `8080`  | Listen port; Cloud Run sets this             |

## One instance, and the binary knows it

`MAX_INSTANCES` must be present and `1`, or the service refuses to start. It is not a tuning
knob. Four pieces of state live in the process and are correct only because there is exactly one
of it:

| What | Where | What a second instance does to it |
| --- | --- | --- |
| OAuth state parameters | `server.Server.states` | rejects a callback it did not issue |
| First-bind decision | `server.Server.bind` | two callbacks both bind, to different athletes |
| Token refresh | `strava.StoredTokenSource.mu` | Strava rotates the refresh token; the two processes invalidate each other and the athlete reauthorizes |
| Sweep overlap | `sweep.Handler.running` | two sweeps read the named log for one activity before either writes it |

Each is a mutex or a map, and none of them survives a second container. Terraform sets
`max_instance_count` and passes the same number in as `MAX_INSTANCES` from one local, so the
platform limit and what the container believes cannot drift apart. The refusal reports what it
saw — unset and wrong need different fixes.

**What the check proves, and what it does not.** The ceiling is per *revision*. Two revisions
serving at once — the overlap during a rolling deploy, or a deliberate traffic split — are two
instances that each read `MAX_INSTANCES=1` and each start happily; Cloud Run also documents
running briefly above a configured maximum. So this is a deployment check, not a mutex: it
catches a service scaled past one, and a container running against infrastructure that never set
the ceiling.

The exposure that leaves is real, and unquantified: a deploy overlap is short but nothing here
measures it, and a traffic split lasts as long as it is left in place. What each of the four
states does under overlap is the table above, not a milder version of it — two revisions can
bind callbacks to different athletes and can sweep the same queue at once, as well as
invalidating each other's refresh token.

What makes it tolerable today is the deployment pattern rather than the code: releases are
single-revision, no traffic split is configured, the authorization flow is a one-time bootstrap
that has already run, and the scheduler is paused so a sweep is a manual act. None of that is
enforced. Making the states safe across instances is a compare-and-set on the token document and
a lease for the sweep, and neither is built.

**Apply the Terraform before releasing a build that carries this contract.** A revision started
against infrastructure that predates it fails readiness, `gcloud run deploy` fails with it, and
the previous revision keeps serving — visible and safe, but a failed release rather than a
deployed one.

The import (`cmd/titelheld-import`) does not require it. It is a deliberate second process that
serves no HTTP, completes no authorization flow and runs no sweep, so none of the four
assumptions apply to it. The same goes for `cmd/titelheld-config`, which seeds the athlete's
configuration document and reads only `FIRESTORE_PROJECT` and `FIRESTORE_DATABASE` — it talks to
nothing but Firestore, so even the Strava credentials would be invented values.

`LOG_PROMPT` defaults to whatever `DRY_RUN` says: prompts are logged while writes are off, which
is the observation window and the time when what the model received is the thing being judged.
The counters on the `named` line — places, achievements, facts, examples, recent titles — remain
the steady-state signal; they say how much the prompt carried and never what it was.

What is gated here is verbosity. A prompt is the athlete's own material — the ride, the gear name,
titles already used, and place names the geo layer resolved, which produces names and has nowhere
to hold a coordinate.

One value is not this service's to vouch for: a `NOTES` fact has an allow-listed *label* and a
free-text *value*, taken from a description another tool wrote. A tool that writes a coordinate
into one of those fields puts it in the prompt, and with logging on, in Cloud Logging. Allow-listing
fact values by shape would close that; it is not built.

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
| `LLM_API_KEY`            | for Anthropic| —                    | Anthropic only; Vertex is keyless             |
| `VERTEX_PROJECT`         | no           | `FIRESTORE_PROJECT`  | Project the Vertex call bills to              |
| `VERTEX_LOCATION`        | no           | `europe-west3`       | Vertex region, or `global` — see below        |
| `BANNED_WORDS`           | no           | shipped list         | Comma-separated; rejected in a title          |
| `MACHINE_TITLE_PATTERNS` | no           | Xert's pattern       | Newline-separated regexes; see below          |

`BANNED_WORDS` **replaces** the shipped list rather than adding to it, so removing a word means
naming the ones you keep. Unset means the shipped list — `Epic`, `Crushing`, `Beast` — which is
what a deployment gets, since Terraform does not set the variable. There is no environment
spelling for "ban nothing": set-but-empty and unset are the same string.

`MACHINE_TITLE_PATTERNS` is newline-separated rather than comma-separated because the entries
are regular expressions, and a comma inside `{1,3}` is not a separator.

The shipped model IDs are pinned, and each is recorded in the source next to the documentation
URL it was verified against and the date — `internal/naming/vertex.go` and
`internal/naming/anthropic.go`. They are not taken from a model's training data.

## Choosing the Vertex model and region

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

for host in aiplatform.googleapis.com \
  europe-west3-aiplatform.googleapis.com \
  europe-west4-aiplatform.googleapis.com; do
  printf '%s: ' "$host"
  curl -s -o /dev/null -w '%{http_code}\n' \
    -H "Authorization: Bearer $(gcloud auth print-access-token)" \
    -H "x-goog-user-project: ${PROJECT}" \
    "https://${host}/v1/publishers/google/models/${MODEL}"
done
```

`200` means the model is served there, `404` that it is not. The `x-goog-user-project` header is
required: without it the call returns `403`, which says nothing about the model.

## Checking the naming path for real

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
