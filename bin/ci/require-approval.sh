#!/usr/bin/env bash
# require-approval.sh: fail the merge request pipeline until a human has signed off.
#
# A sign-off is an Approve, or a thumbs-up award, on the merge request. With the project
# setting "Pipelines must succeed" on, a failing run of this script blocks the merge, so a
# merge needs that sign-off even when merge-when-pipeline-succeeds was armed early.
# GitLab does not re-run a pipeline when someone approves: approve, then retry the job.
#
# Agents never approve, and never give the thumbs-up: a sign-off is a person's.
#
# Who counts:
#   - MERGE_APPROVAL_OWNERS set (usernames separated by commas, spaces or newlines; case,
#     quotes and a leading @ are ignored): only a sign-off by one of
#     them counts, including on an MR they opened themselves (logged as a self sign-off).
#   - MERGE_APPROVAL_OWNERS unset: a sign-off by anyone other than the MR's author.
#
# Reads GET .../merge_requests/:iid (author), .../approvals (approved_by; available on
# every GitLab tier, unlike approval_state) and .../award_emoji (thumbs-up).
#
# Fails closed: not a merge request pipeline, no token, an unreachable API, an HTTP error,
# or a reply that is not the expected JSON is a failure with the reason, never a pass.
# This is a guard against merging in haste, not a security boundary: a click is cheap, and
# anyone holding an owner's credentials can make it.
#
# Environment:
#   APPROVAL_CHECK_TOKEN   project access token, Reporter role, read_api scope (required)
#   MERGE_APPROVAL_OWNERS  usernames whose sign-off counts (optional)
#   CI_API_V4_URL, CI_PROJECT_ID, CI_MERGE_REQUEST_IID  set by GitLab in MR pipelines
#
# Needs bash, curl, and jq or python3 to read JSON. Keep `set -x` off: the token is sent
# on curl's stdin, never in its arguments, and must not reach the log either.
set -u

fail() {
    echo "require-approval: FAILED: $*"
    exit 1
}

API="${CI_API_V4_URL:-}"
PROJECT="${CI_PROJECT_ID:-}"
IID="${CI_MERGE_REQUEST_IID:-}"
TOKEN="${APPROVAL_CHECK_TOKEN:-}"

[ -n "$IID" ] || fail "CI_MERGE_REQUEST_IID is not set: this job only judges merge request pipelines."
[ -n "$API" ] && [ -n "$PROJECT" ] || fail "CI_API_V4_URL or CI_PROJECT_ID is not set: cannot reach the GitLab API."
[ -n "$TOKEN" ] || fail "APPROVAL_CHECK_TOKEN is not set (it must be masked but not protected, so MR branches get it). Refusing to pass unchecked."
command -v curl >/dev/null 2>&1 || fail "curl is not installed on the runner."
if command -v jq >/dev/null 2>&1; then
    JSON=jq
elif command -v python3 >/dev/null 2>&1; then
    JSON=python3
else
    fail "neither jq nor python3 is installed on the runner: cannot read the API's replies."
fi

TMP=$(mktemp -d) || fail "cannot create a temporary directory."
trap 'rm -rf "$TMP"' EXIT
BASE="${API%/}/projects/${PROJECT}/merge_requests/${IID}"

# get <url> <body-file>: GET with the token, fail closed on anything but HTTP 200.
get() {
    local code err
    code=$(printf 'PRIVATE-TOKEN: %s\n' "$TOKEN" |
        curl -sS --max-time 30 -H @- -D "$TMP/headers" -o "$2" -w '%{http_code}' "$1" 2>"$TMP/err")
    local rc=$?
    err=$(head -c 300 "$TMP/err")
    err=${err//"$TOKEN"/'***'}
    local what=${1#"${API%/}"}
    what=${what%%\?*}
    [ "$rc" = 0 ] || fail "could not reach GitLab for ${what} (curl exit ${rc}: ${err})."
    case "$code" in
        200) ;;
        401) fail "GitLab answered HTTP 401 for ${what}: APPROVAL_CHECK_TOKEN is wrong, expired or revoked." ;;
        403|404) fail "GitLab answered HTTP ${code} for ${what}: the token needs the Reporter role and read_api scope on this project." ;;
        *) fail "GitLab answered HTTP ${code} for ${what}." ;;
    esac
}

# usernames <kind> <file>: print usernames from a reply, one per line; non-zero when the
# reply is not JSON of the expected shape. Kinds: author, approvers, thumbsup.
usernames() {
    if [ "$JSON" = jq ]; then
        local prog
        case "$1" in
            author) prog='if type == "object" and (.author.username | type) == "string" then .author.username else error("shape") end' ;;
            approvers) prog='if type == "object" and (.approved_by | type) == "array" then .approved_by[] | .user.username? // empty | strings else error("shape") end' ;;
            thumbsup) prog='if type == "array" then .[] | select(.name? == "thumbsup") | .user.username? // empty | strings else error("shape") end' ;;
        esac
        jq -r "$prog" "$2" 2>/dev/null
    else
        python3 -I -c '
import json, sys
kind, path = sys.argv[1], sys.argv[2]
with open(path, encoding="utf-8") as f:
    d = json.load(f)
def name(e):
    u = e.get("user") if isinstance(e, dict) else None
    n = u.get("username") if isinstance(u, dict) else None
    return n if isinstance(n, str) else None
if kind == "author":
    a = d.get("author") if isinstance(d, dict) else None
    n = a.get("username") if isinstance(a, dict) else None
    if not isinstance(n, str):
        sys.exit(1)
    out = [n]
elif kind == "approvers":
    if not isinstance(d, dict) or not isinstance(d.get("approved_by"), list):
        sys.exit(1)
    out = [name(e) for e in d["approved_by"]]
else:
    if not isinstance(d, list):
        sys.exit(1)
    out = [name(e) for e in d if isinstance(e, dict) and e.get("name") == "thumbsup"]
for n in out:
    if n:
        print(n)
' "$1" "$2" 2>/dev/null
    fi
}

# Only plausible usernames reach the log or the comparisons.
clean() { grep -E '^[A-Za-z0-9_.-]+$' || true; }
# GitLab usernames are case-insensitive: compare, and log, them lowercased.
lower() { tr 'A-Z' 'a-z'; }
# Owner entries: commas, spaces, tabs, newlines and carriage returns all separate; each
# entry loses surrounding quotes and a leading @. Prints the non-empty entries, lowercased.
owner_entries() {
    printf '%s\n' "${MERGE_APPROVAL_OWNERS:-}" | tr '\r,\t ' '\n\n\n\n' |
        sed -E "s/^[\"']+//; s/[\"']+\$//; s/^@+//" | lower | grep -v '^$' || true
}

get "$BASE" "$TMP/mr.json"
AUTHOR=$(usernames author "$TMP/mr.json") || fail "the merge request reply is not the expected JSON."
AUTHOR=$(printf '%s\n' "$AUTHOR" | clean | lower | head -n 1)
[ -n "$AUTHOR" ] || fail "the merge request reply names no author."

get "$BASE/approvals" "$TMP/approvals.json"
APPROVERS=$(usernames approvers "$TMP/approvals.json") || fail "the approvals reply is not the expected JSON."
APPROVERS=$(printf '%s\n' "$APPROVERS" | clean | lower)

# Award emoji are paginated; follow GitLab's next-page header, within a sane bound.
THUMBS=
page=1
while [ -n "$page" ]; do
    [ "$page" -le 50 ] || fail "too many pages of award emoji to read."
    get "$BASE/award_emoji?per_page=100&page=${page}" "$TMP/awards.json"
    more=$(usernames thumbsup "$TMP/awards.json") || fail "the award emoji reply is not the expected JSON."
    THUMBS=$(printf '%s\n%s\n' "$THUMBS" "$more" | clean | lower)
    page=$(tr -d '\r' <"$TMP/headers" | awk -F': *' 'tolower($1) == "x-next-page" { print $2 }' | grep -E '^[0-9]+$' || true)
done

SIGNERS=$(printf '%s\n%s\n' "$APPROVERS" "$THUMBS" | clean | sort -u)

OWNERS=$(owner_entries | clean | sort -u)
if [ -n "$OWNERS" ]; then
    COUNTED=$(printf '%s\n' "$SIGNERS" | grep -Fxf <(printf '%s\n' "$OWNERS") || true)
    rule="a sign-off from one of MERGE_APPROVAL_OWNERS"
else
    COUNTED=$(printf '%s\n' "$SIGNERS" | grep -Fxv -- "$AUTHOR" || true)
    rule="a sign-off from someone other than the author (MERGE_APPROVAL_OWNERS is unset)"
fi

list() { if [ -n "$1" ]; then printf '%s\n' "$1" | paste -sd ',' - | sed 's/,/, /g'; else echo none; fi; }
echo "Merge request !${IID}, opened by ${AUTHOR}."
echo "Approved by: $(list "$APPROVERS")"
echo "Thumbs-up from: $(list "$THUMBS")"

if [ -z "$COUNTED" ]; then
    fail "!${IID} needs ${rule}. Approve it (or give it a thumbs-up), then retry this job: GitLab does not re-run a pipeline on approval."
fi
if printf '%s\n' "$COUNTED" | grep -Fxq -- "$AUTHOR"; then
    echo "SELF SIGN-OFF: ${AUTHOR} signed off on their own merge request (listed in MERGE_APPROVAL_OWNERS)."
fi
echo "require-approval: passed: !${IID} signed off by $(list "$COUNTED")."
