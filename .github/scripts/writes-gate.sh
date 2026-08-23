#!/usr/bin/env bash
#
# Decides whether a release may deploy onto the Cloud Run service, and whether
# what it deployed is what it expected.
#
# A script rather than a `run:` block so that something can execute it. The
# workflow linters read this logic; they cannot run it, which is why the
# version of this gate that ran *after* the deploy was as lint-clean as the one
# that runs before it. The tests beside this file run it against a stubbed
# gcloud.
#
# Environment:
#   PHASE                pre | post — before the deploy, or checking afterwards
#   PROJECT, REGION      the Cloud Run service's project and region
#   SERVICE              the service name
#   WRITES_ACKNOWLEDGED  a date, YYYY-MM-DD; see below
#   TAG_EPOCH            the release tag's own timestamp, seconds since epoch
#
# Exit 0 to proceed, 1 to refuse.

set -euo pipefail

: "${PHASE:?PHASE must be pre or post}"
: "${PROJECT:?PROJECT is required}"
: "${REGION:?REGION is required}"
: "${SERVICE:?SERVICE is required}"
: "${TAG_EPOCH:?TAG_EPOCH is required}"

# Absent is empty, never unset: an unset variable under `set -u` would abort
# with a shell error instead of the refusal this is here to produce.
WRITES_ACKNOWLEDGED="${WRITES_ACKNOWLEDGED:-}"

# How long an acknowledgement stays good. A week is long enough to tag a
# release you decided on, and short enough that a variable nobody removed
# stops mattering on its own.
readonly ACK_MAX_AGE_DAYS=7

# read_dry_run prints the service's DRY_RUN as gcloud renders it.
read_dry_run() {
  gcloud run services describe "$SERVICE" \
    --project="$PROJECT" --region="$REGION" \
    --format='value(spec.template.spec.containers[0].env.filter("name:DRY_RUN").extract("value"))'
}

# classify_dry_run maps gcloud's rendering onto what it means.
#
# A repeated field comes back as ['1'] and a scalar as 1 depending on the
# version, so both spellings are read. Everything else — including the empty
# string — is "unreadable", which is not a synonym for "writes are off".
classify_dry_run() {
  case "$1" in
    "['1']" | 1) printf 'off\n' ;;
    "['0']" | 0) printf 'on\n' ;;
    *) printf 'unreadable\n' ;;
  esac
}

# epoch_from_date converts YYYY-MM-DD to seconds since the epoch, UTC.
#
# Arithmetic rather than `date -d`, which is a GNU extension: the runner has
# it and a developer's macOS does not, and a gate nobody can execute locally
# is how this logic ended up untested in the first place. The algorithm is the
# standard days-from-civil one.
#
# Fails on a date that does not exist, which is what makes 2026-02-30 a
# refusal rather than a silently shifted timestamp.
epoch_from_date() {
  local value="$1"
  local year month day

  year=$((10#${value:0:4}))
  month=$((10#${value:5:2}))
  day=$((10#${value:8:2}))

  if [ "$month" -lt 1 ] || [ "$month" -gt 12 ] || [ "$day" -lt 1 ] || [ "$day" -gt 31 ]; then
    return 1
  fi

  local y=$year era yoe doy doe days
  if [ "$month" -le 2 ]; then
    y=$((year - 1))
  fi

  era=$((y / 400))
  yoe=$((y - era * 400))

  if [ "$month" -gt 2 ]; then
    doy=$(((153 * (month - 3) + 2) / 5 + day - 1))
  else
    doy=$(((153 * (month + 9) + 2) / 5 + day - 1))
  fi

  doe=$((yoe * 365 + yoe / 4 - yoe / 100 + doy))
  days=$((era * 146097 + doe - 719468))

  # Round-trip, so a day that does not exist in that month is rejected rather
  # than rolling into the next one.
  if [ "$(date_from_epoch $((days * 86400)))" != "$value" ]; then
    return 1
  fi

  printf '%s\n' $((days * 86400))
}

# date_from_epoch converts seconds since the epoch to YYYY-MM-DD, UTC.
date_from_epoch() {
  local days=$(($1 / 86400))
  local z=$((days + 719468))
  local era=$((z / 146097))
  local doe=$((z - era * 146097))
  local yoe=$(((doe - doe / 1460 + doe / 36524 - doe / 146096) / 365))
  local y=$((yoe + era * 400))
  local doy=$((doe - (365 * yoe + yoe / 4 - yoe / 100)))
  local mp=$(((5 * doy + 2) / 153))
  local d=$((doy - (153 * mp + 2) / 5 + 1))
  local m=$((mp + 3))

  if [ "$mp" -ge 10 ]; then
    m=$((mp - 9))
    y=$((y + 1))
  fi

  printf '%04d-%02d-%02d\n' "$y" "$m" "$d"
}

# acknowledgement_error prints why an acknowledgement is not usable, and prints
# nothing when it is.
#
# The value is a date rather than a flag because a flag never expires. A
# variable set for one flip and left behind would still be there the day an
# accidental Terraform change reintroduces DRY_RUN=0 — and the release that
# followed would ship a writing revision on a warning. A date invalidates
# itself whether or not anybody remembers it.
acknowledgement_error() {
  local value="$1" tag_epoch="$2"

  if [ -z "$value" ]; then
    printf 'WRITES_ACKNOWLEDGED is not set\n'
    return
  fi

  # Shape first, so "1" is refused for what it is rather than for failing to
  # parse as a date.
  if ! printf '%s' "$value" | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}$'; then
    printf "WRITES_ACKNOWLEDGED is '%s', which is not a date of the form YYYY-MM-DD\\n" "$value"
    return
  fi

  local ack_epoch
  if ! ack_epoch=$(epoch_from_date "$value"); then
    printf "WRITES_ACKNOWLEDGED is '%s', which is not a real calendar date\\n" "$value"
    return
  fi

  # Both sides reduced to a UTC midnight before subtracting, so the result is
  # a whole number of days rather than a rounded fraction of one. The obvious
  # (tag_epoch - ack_epoch) / 86400 truncates toward zero, which made an
  # acknowledgement dated the day *after* the tag look like zero days old.
  local tag_date tag_midnight age_days
  tag_date=$(date_from_epoch "$tag_epoch")
  tag_midnight=$(epoch_from_date "$tag_date")
  age_days=$(((tag_midnight - ack_epoch) / 86400))

  # One day of slack in the future, and no more. A tag pushed late in the UTC
  # evening is already tomorrow for the person pushing it, and refusing their
  # own date would be a puzzle rather than a guard. A week ahead is not slack,
  # it is pre-authorization.
  if [ "$age_days" -lt -1 ]; then
    printf "WRITES_ACKNOWLEDGED is '%s', %d days after the tag (%s); acknowledge a release, do not pre-authorize one\\n" \
      "$value" "$((-age_days))" "$tag_date"
    return
  fi

  if [ "$age_days" -gt "$ACK_MAX_AGE_DAYS" ]; then
    printf "WRITES_ACKNOWLEDGED is '%s', %d days before the tag (%s); it goes stale after %d\\n" \
      "$value" "$age_days" "$tag_date" "$ACK_MAX_AGE_DAYS"
    return
  fi
}

# require_service refuses unless the managed service is there to be updated.
#
# `gcloud run deploy` creates a service when it cannot find one, so a renamed
# service or a wrong region would quietly produce a second, Terraform-less
# service with the default compute identity and no DRY_RUN.
#
# Reports what gcloud said rather than naming a cause: "missing" and "not
# allowed to look" fail identically, and the first time this fired the service
# existed and the permission did not.
require_service() {
  local found
  if ! found=$(gcloud run services describe "$SERVICE" \
    --project="$PROJECT" --region="$REGION" --format='value(name)' 2>&1); then
    echo "::error::cannot read the Cloud Run service '${SERVICE}' in ${PROJECT}/${REGION}; gcloud reported:"
    printf '%s\n' "$found"
    echo "::error::If the service is genuinely absent, apply the Terraform first - this job updates a service, it does not create one."
    return 1
  fi
}

main() {
  if [ "$PHASE" = "pre" ]; then
    require_service
  fi

  local dry_run state ack_error
  dry_run=$(read_dry_run)
  state=$(classify_dry_run "$dry_run")

  if [ "$state" = "off" ]; then
    if [ "$PHASE" = "pre" ]; then
      echo "DRY_RUN is on; the revision this deploys cannot write to Strava."
    else
      echo "DRY_RUN is on; the deployed service cannot write to Strava."
    fi

    return 0
  fi

  if [ "$state" = "unreadable" ]; then
    echo "::error::cannot read DRY_RUN off the service: expected 1 or 0, got '${dry_run}'."

    if [ "$PHASE" = "pre" ]; then
      echo "::error::No revision has been deployed. This job cannot show whether the deploy would be able to write."
    else
      echo "::error::A revision has been deployed and this job cannot say what it can do."
    fi

    return 1
  fi

  # Writes are legibly on. Only a fresh, dated acknowledgement gets past here.
  ack_error=$(acknowledgement_error "$WRITES_ACKNOWLEDGED" "$TAG_EPOCH")

  if [ -n "$ack_error" ]; then
    echo "::error::the service has writes enabled (DRY_RUN='${dry_run}') and ${ack_error}."

    if [ "$PHASE" = "pre" ]; then
      echo "::error::No revision has been deployed. Enabling writes is a Terraform change plus WRITES_ACKNOWLEDGED set to today's date; releasing is not how writes get turned on."
    else
      echo "::error::A writing revision was deployed - the pre-deploy check disagreed, so the service changed underneath this job."
    fi

    return 1
  fi

  echo "::warning::the service has DRY_RUN='${dry_run}' and WRITES_ACKNOWLEDGED='${WRITES_ACKNOWLEDGED}': this release involves a revision that CAN write to Strava."

  return 0
}

main "$@"
