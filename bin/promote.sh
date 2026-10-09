#!/usr/bin/env bash
# promote.sh: promote the commit of the current pipeline to main.
#
# `dev` is where work lands; `main` only ever moves by fast-forward from a green
# `dev`. Safety properties:
#   - Fails closed: if main cannot be resolved because the fetch failed (as
#     opposed to main not existing yet), nothing is promoted.
#   - No-op if main already contains the commit.
#   - Fast-forward only, never forced. A commit that is not a descendant of main's
#     tip means main was written to directly: reconcile by hand.
#   - The token is never printed, and is scrubbed from git's output.
#
# Environment:
#   GITLAB_PROMOTE_TOKEN  project access token allowed to push to main (required)
#   CI_COMMIT_SHA         commit to promote (default: HEAD)
#   CI_SERVER_HOST, CI_PROJECT_PATH  build the push URL (set by GitLab CI)
#   PROMOTE_REMOTE_URL    override the push URL (tests)
#   PROMOTE_DRY_RUN=1     stop before pushing and say what would happen
#
# Keep `set -x` off: it would print the push URL.
set -u

TARGET=main
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
git_() { git -C "$DIR" "$@"; }

SHA="${CI_COMMIT_SHA:-$(git_ rev-parse HEAD)}"
SHORT="${SHA:0:8}"

# Distinguish "main does not exist" (ls-remote exit 2) from "could not ask" (any
# other failure), so a flaky fetch cannot look like a first promotion.
git_ ls-remote --exit-code --heads origin "$TARGET" >/dev/null 2>&1
case $? in
    0) EXISTS=1 ;;
    2) EXISTS=0 ;;
    *) echo "Could not reach origin to resolve ${TARGET}: refusing to promote. Nothing was changed."; exit 1 ;;
esac

TARGET_SHA=
if [ "$EXISTS" = 1 ]; then
    if ! git_ fetch -q origin "$TARGET" 2>/dev/null; then
        echo "Could not fetch ${TARGET}: refusing to promote. Nothing was changed."; exit 1
    fi
    # `--verify` matters: a bare `rev-parse FETCH_HEAD` echoes its argument when it
    # cannot resolve it, so a failure would look like a value.
    TARGET_SHA=$(git_ rev-parse -q --verify 'FETCH_HEAD^{commit}' 2>/dev/null || echo '')
    if [ -z "$TARGET_SHA" ]; then
        echo "Could not resolve ${TARGET} after fetching it: refusing to promote. Nothing was changed."; exit 1
    fi
    if git_ merge-base --is-ancestor "$SHA" "$TARGET_SHA" 2>/dev/null; then
        echo "${TARGET} already contains ${SHORT}: nothing to promote."; exit 0
    fi
    if ! git_ merge-base --is-ancestor "$TARGET_SHA" "$SHA" 2>/dev/null; then
        echo "${TARGET} has diverged from dev; reconcile by hand. Refusing to promote ${SHORT} (never forced)."; exit 1
    fi
fi

TOKEN="${GITLAB_PROMOTE_TOKEN:-}"
if [ -z "$TOKEN" ]; then
    echo "GITLAB_PROMOTE_TOKEN is not set: cannot promote."; exit 1
fi

if [ -n "${PROMOTE_REMOTE_URL:-}" ]; then
    URL="$PROMOTE_REMOTE_URL"
elif [ -n "${CI_SERVER_HOST:-}" ] && [ -n "${CI_PROJECT_PATH:-}" ]; then
    URL="https://oauth2:${TOKEN}@${CI_SERVER_HOST}/${CI_PROJECT_PATH}.git"
else
    echo "CI_SERVER_HOST and CI_PROJECT_PATH are not set (and no PROMOTE_REMOTE_URL): cannot build the push URL."; exit 1
fi

if [ -z "$TARGET_SHA" ]; then
    echo "${TARGET} does not exist yet: promoting ${SHORT} creates it."
else
    echo "Promoting ${SHORT} to ${TARGET} (fast-forward from ${TARGET_SHA:0:8})."
fi
if [ "${PROMOTE_DRY_RUN:-0}" = 1 ]; then
    echo "Dry run: would push ${SHA} to refs/heads/${TARGET}. Nothing pushed."; exit 0
fi

# No --force: the server rejects anything that is not a fast-forward.
OUT=$(git_ push "$URL" "${SHA}:refs/heads/${TARGET}" 2>&1); RC=$?
# Scrub the token (and anything shaped like credentials in a URL) before showing git's output.
OUT=${OUT//"$TOKEN"/'***'}
OUT=$(printf '%s\n' "$OUT" | sed -E 's#(://)[^/@[:space:]]*@#\1***@#g')
if [ "$RC" = 0 ]; then
    echo "$OUT"; echo "Promoted ${SHORT} to ${TARGET}."; exit 0
fi
echo "$OUT"
case "$OUT" in
    *"401"*|*"Authentication failed"*|*"Access denied"*|*"could not read Username"*|*"HTTP Basic"*)
        reason="authentication failed: refresh GITLAB_PROMOTE_TOKEN" ;;
    *"protected branch"*|*"pre-receive hook declined"*|*"You are not allowed"*)
        reason="branch protection: the token's role must be allowed to push to ${TARGET}" ;;
    *"non-fast-forward"*|*"[rejected]"*|*"fetch first"*)
        reason="not a fast-forward: ${TARGET} moved or diverged; reconcile by hand (never forced)" ;;
    *) reason="see the output above" ;;
esac
echo "Push to ${TARGET} FAILED: ${reason}"
exit 1
