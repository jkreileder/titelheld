#!/usr/bin/env python3
"""Enumerate the draft-release condition over every combination of job results.

The condition decides whether a tag gets a GitHub release. It used to accept a
skipped deploy, which meant a version nobody deployed could be drafted and
published — and no test could have noticed, because a workflow `if:` is a
string that actionlint checks the syntax of and nothing evaluates.

So this reads the expression out of release.yaml and evaluates it. Not a copy:
delete the strictness from the workflow and this fails, which is the whole
point of it existing.
"""

from __future__ import annotations

import itertools
import re
import sys
from pathlib import Path

WORKFLOW = Path(__file__).resolve().parents[2] / "workflows" / "release.yaml"

RESULTS = ("success", "skipped", "failure", "cancelled")

# GitHub applies an implicit success() to a job condition that names no status
# check function of its own. Without one, a job whose dependency was skipped is
# skipped too — however the rest of the expression evaluates.
#
# Matched as a call, not as a substring, and with quoted literals blanked out
# first: `vars.X != 'always('` names no status function, and treating it as
# one would suppress the implicit success() this models.
STATUS_CALL = re.compile(r"\b(?:always|success|failure|cancelled)\s*\(")
QUOTED = re.compile(r"'[^']*'")


def names_status_function(expr: str) -> bool:
    """Report whether the expression calls a status-check function."""
    return bool(STATUS_CALL.search(QUOTED.sub("''", expr)))


def draft_condition(text: str) -> str:
    """Return the draft-release job's `if:` expression, without its ${{ }}."""
    jobs = text.split("\n  draft-release:\n", 1)
    if len(jobs) != 2:
        raise SystemExit("draft-release job not found in release.yaml")

    match = re.search(r"^    if: >-\n((?:      .*\n)+)", jobs[1], re.M)
    if not match:
        raise SystemExit("draft-release has no `if: >-` block")

    folded = " ".join(line.strip() for line in match.group(1).splitlines())
    inner = folded.strip()
    if not (inner.startswith("${{") and inner.endswith("}}")):
        raise SystemExit(f"unexpected expression shape: {inner!r}")

    return inner[3:-2].strip()


def evaluate(expr: str, needs: dict[str, str], variables: dict[str, str], ref: str) -> bool:
    """Evaluate the subset of GitHub expression syntax this condition uses.

    Deliberately narrow: booleans, comparisons, parentheses, `always()`,
    `startsWith()`, `needs.<job>.result` and `vars.<name>`. Anything else in
    the expression is an error rather than something quietly treated as true —
    a condition this test cannot read is a condition it must not vouch for.
    """
    python = expr
    python = python.replace("&&", " and ").replace("||", " or ")
    python = re.sub(r"\balways\(\)", "True", python)
    python = re.sub(
        r"startsWith\(github\.ref,\s*'([^']*)'\)",
        lambda m: repr(ref.startswith(m.group(1))),
        python,
    )
    python = re.sub(
        r"needs\.([A-Za-z0-9_-]+)\.result",
        lambda m: repr(needs[m.group(1)]),
        python,
    )
    python = re.sub(
        r"vars\.([A-Za-z0-9_]+)",
        lambda m: repr(variables.get(m.group(1), "")),
        python,
    )
    python = python.replace("!=", " != ").replace("==", " == ")

    allowed = re.compile(r"^[\sA-Za-z0-9_'\"()=!.andor]*$")
    if not allowed.match(python):
        raise SystemExit(f"expression uses syntax this test cannot evaluate: {python!r}")

    try:
        result = bool(eval(python, {"__builtins__": {}}, {}))  # noqa: S307 - vetted above
    except NameError as err:
        # A bare word the character filter let through — `true` rather than
        # `true()`, say. Reported as unevaluatable rather than raised as a
        # traceback: this test refuses to vouch for an expression it cannot
        # read, and that refusal should look like a verdict.
        raise SystemExit(f"expression uses syntax this test cannot evaluate: {python!r} ({err})")

    # The implicit success(). Deleting `always()` from the workflow does not
    # change what the expression evaluates to — it changes whether GitHub
    # evaluates it at all — so a test that ignored this would go on passing
    # while bootstrap mode quietly stopped drafting anything, its skipped
    # image and deploy jobs now skipping the draft with them.
    if not names_status_function(expr):
        result = result and all(value == "success" for value in needs.values())

    return result


def main() -> int:
    expr = draft_condition(WORKFLOW.read_text(encoding="utf-8"))
    print(f"# condition under test:\n#   {expr}\n")

    failures = 0
    rows = 0

    for image, deploy, bootstrap in itertools.product(RESULTS, RESULTS, ("", "1")):
        needs = {"check-release-version": "success", "image": image, "deploy": deploy}
        got = evaluate(expr, needs, {"RELEASE_BOOTSTRAP": bootstrap}, "refs/tags/v1.2.3")

        # What the workflow is supposed to do: draft when the image built and
        # the deploy succeeded; in bootstrap mode also when either was skipped,
        # never when either failed or was cancelled.
        strict = image == "success" and deploy == "success"
        lenient = (
            bootstrap == "1"
            and image not in ("failure", "cancelled")
            and deploy not in ("failure", "cancelled")
        )
        want = strict or lenient

        rows += 1
        if got != want:
            failures += 1
            print(f"FAIL image={image} deploy={deploy} bootstrap={bootstrap or '(unset)'}: "
                  f"drafts={got}, want {want}")

    # A tag is required whatever the job results say.
    for ref in ("refs/heads/main", "refs/pull/1/merge"):
        needs = {"check-release-version": "success", "image": "success", "deploy": "success"}
        rows += 1
        if evaluate(expr, needs, {}, ref):
            failures += 1
            print(f"FAIL ref={ref}: drafts a release off a non-tag ref")

    # Only a *successful* version check lets anything be drafted. Testing
    # `failure` alone would pass on a condition weakened to
    # `!= 'failure'` — which would then draft a release whose version check
    # was skipped or cancelled, and neither of those checked the tag or the
    # changelog.
    for check, image, deploy in itertools.product(RESULTS, RESULTS, RESULTS):
        if check == "success":
            continue

        needs = {"check-release-version": check, "image": image, "deploy": deploy}
        rows += 1
        if evaluate(expr, needs, {"RELEASE_BOOTSTRAP": "1"}, "refs/tags/v1.2.3"):
            failures += 1
            print(f"FAIL check={check} image={image} deploy={deploy}: drafts anyway")

    print(f"{rows} combinations, {failures} failures")

    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
