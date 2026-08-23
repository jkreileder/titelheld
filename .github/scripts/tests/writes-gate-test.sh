#!/usr/bin/env bash
#
# Runs the release's writes gate against a stubbed gcloud.
#
# The gate decides whether a release may deploy onto a service that can write
# to somebody's Strava feed. Until this existed, nothing executed it: actionlint
# and zizmor read the workflow, which is why the version of the gate that ran
# *after* the deploy was exactly as lint-clean as the one that runs before it.
# A revert of that fix would have passed every hook.
#
# The stub prints what a real `gcloud run services describe` would print, and
# the script under test is the one the workflow calls — not a copy of it.

set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
gate="${here}/../writes-gate.sh"

# A tag made at 2026-08-23T12:00:00Z. Fixed rather than derived from the clock:
# a test whose expectations move with the calendar cannot be read a year later.
#
# Lower case and not readonly, deliberately: these are passed into the script
# under test as TAG_EPOCH, and a readonly variable of that name in this shell
# would make every one of those assignments fail — which it did, loudly, the
# first time this ran.
tag_epoch=1787486400
tag_date=2026-08-23

failures=0
checks=0

stub_dir=$(mktemp -d)
trap 'rm -rf "$stub_dir"' EXIT

cat >"${stub_dir}/gcloud" <<'STUB'
#!/usr/bin/env bash
# Stands in for `gcloud run services describe`, in its two shapes: the
# existence probe (--format='value(name)') and the DRY_RUN read.
case "$*" in
  *"value(name)"*)
    if [ "${STUB_SERVICE_MISSING:-}" = "1" ]; then
      echo "ERROR: (gcloud.run.services.describe) NOT_FOUND: Resource 'titelheld' was not found" >&2
      exit 1
    fi
    echo titelheld
    ;;
  *) printf '%s\n' "${STUB_DRY_RUN-}" ;;
esac
STUB
chmod +x "${stub_dir}/gcloud"

# check <name> <want-exit> <phase> <ack> <dry-run> [service-missing] -- [expected substrings...]
check() {
  local name="$1" want="$2" phase="$3" ack="$4" dry="$5" missing="${6:-}"
  shift 6
  [ "${1:-}" = "--" ] && shift

  local output status
  set +e
  output=$(
    PATH="${stub_dir}:${PATH}" \
      PHASE="$phase" PROJECT=musterproject REGION=europe-west3 SERVICE=titelheld \
      TAG_EPOCH="$tag_epoch" WRITES_ACKNOWLEDGED="$ack" \
      STUB_DRY_RUN="$dry" STUB_SERVICE_MISSING="$missing" \
      bash "$gate" 2>&1
  )
  status=$?
  set -e

  checks=$((checks + 1))

  if [ "$status" != "$want" ]; then
    printf 'FAIL %s: exit %d, want %d\n%s\n\n' "$name" "$status" "$want" "$output"
    failures=$((failures + 1))
    return
  fi

  local expected
  for expected in "$@"; do
    case "$output" in
      *"$expected"*) ;;
      *)
        printf 'FAIL %s: output does not mention %s\n%s\n\n' "$name" "$expected" "$output"
        failures=$((failures + 1))
        return
        ;;
    esac
  done

  printf 'ok   %s\n' "$name"
}

echo "# the gate proceeds only when the service cannot write, or a fresh acknowledgement says so"

# gcloud renders a repeated field as ['1'] and a scalar as 1 depending on the
# version. Both are "writes are off".
check "dry run on, repeated rendering" 0 pre "" "['1']" "" -- "cannot write to Strava"
check "dry run on, scalar rendering" 0 pre "" "1" "" -- "cannot write to Strava"
check "dry run on, stale ack is irrelevant" 0 pre "2020-01-01" "['1']" "" -- "cannot write to Strava"

echo
echo "# writes enabled: refuse unless the acknowledgement is a date, and a fresh one"

check "no acknowledgement" 1 pre "" "['0']" "" -- "::error::" "WRITES_ACKNOWLEDGED is not set" "No revision has been deployed"
check "the old flag value" 1 pre "1" "['0']" "" -- "is not a date of the form YYYY-MM-DD"
check "a word" 1 pre "yes" "['0']" "" -- "is not a date of the form YYYY-MM-DD"
check "a timestamp" 1 pre "2026-08-23T00:00:00Z" "['0']" "" -- "is not a date of the form YYYY-MM-DD"
check "a date that does not exist" 1 pre "2026-02-30" "['0']" "" -- "is not a real calendar date"
check "a month that does not exist" 1 pre "2026-13-01" "['0']" "" -- "is not a real calendar date"
check "the tag's own date" 0 pre "$tag_date" "['0']" "" -- "::warning::" "CAN write to Strava"
check "a week before the tag" 0 pre "2026-08-16" "['0']" "" -- "CAN write to Strava"
check "eight days before the tag" 1 pre "2026-08-15" "['0']" "" -- "8 days before the tag" "$tag_date"
check "a month before the tag" 1 pre "2026-07-23" "['0']" "" -- "days before the tag"
check "one day after the tag" 0 pre "2026-08-24" "['0']" "" -- "CAN write to Strava"
check "two days after the tag" 1 pre "2026-08-25" "['0']" "" -- "days after the tag" "do not pre-authorize"

echo
echo "# an unreadable DRY_RUN fails closed, acknowledged or not"

check "empty reading" 1 pre "" "" "" -- "cannot read DRY_RUN"
check "empty reading, acknowledged" 1 pre "$tag_date" "" "" -- "cannot read DRY_RUN"
check "unexpected value, acknowledged" 1 pre "$tag_date" "None" "" -- "cannot read DRY_RUN"
check "two values, acknowledged" 1 pre "$tag_date" "['1', '0']" "" -- "cannot read DRY_RUN"

echo
echo "# the service must exist before a deploy, and the report says what to do"

check "service missing" 1 pre "" "['1']" 1 -- "cannot read the Cloud Run service" "NOT_FOUND" "apply the Terraform first"

echo
echo "# the post-deploy phase applies the same rules to what was deployed"

check "post: dry run on" 0 post "" "['1']" "" -- "the deployed service cannot write"
check "post: writes on, no ack" 1 post "" "['0']" "" -- "A writing revision was deployed" "changed underneath this job"
check "post: writes on, fresh ack" 0 post "$tag_date" "['0']" "" -- "CAN write to Strava"
check "post: writes on, stale ack" 1 post "2026-08-15" "['0']" "" -- "days before the tag"
check "post: unreadable" 1 post "$tag_date" "" "" -- "cannot read DRY_RUN" "cannot say what it can do"

# The post phase does not probe for the service: by then a deploy has either
# updated it or failed, so a missing service there is not the operator error
# the pre-deploy message describes.
check "post: no existence probe" 1 post "$tag_date" "" 1 -- "cannot read DRY_RUN"

echo
printf '%d checks, %d failures\n' "$checks" "$failures"

[ "$failures" -eq 0 ]
