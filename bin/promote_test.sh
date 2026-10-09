#!/usr/bin/env bash
# promote_test.sh: exercises bin/promote.sh against throwaway local repositories.
# Run by hand: bash bin/promote_test.sh
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
PROMOTE="$HERE/promote.sh"
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
TOKEN="s3cr3t-token-VALUE"
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.invalid GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.invalid
fail=0; n=0
check() { n=$((n+1)); if [ "$2" = ok ]; then echo "ok   $1"; else echo "FAIL $1"; fail=$((fail+1)); fi; }
expect() { # name, condition-exit-status
    if [ "$2" = 0 ]; then check "$1" ok; else check "$1" bad; fi; }

# fresh <name>: bare remote with dev (one commit) and a clone on dev.
fresh() {
    R="$TMP/$1.git"; C="$TMP/$1"
    git init -q --bare -b dev "$R"
    git clone -q "$R" "$C" 2>/dev/null
    git -C "$C" checkout -q -b dev
    echo 1 > "$C/f"; git -C "$C" add f; git -C "$C" commit -qm one
    git -C "$C" push -q origin dev
}
commit() { echo "$1" >> "$C/f"; git -C "$C" commit -qam "$1"; git -C "$C" push -q origin dev; }
run() { # runs promote in $C, output in $OUT, status in $RC
    OUT=$(cd "$C" && GITLAB_PROMOTE_TOKEN="${TOK-$TOKEN}" CI_COMMIT_SHA="$(git -C "$C" rev-parse HEAD)" \
        PROMOTE_REMOTE_URL="${URLOVR-$R}" bash "$C/bin/promote.sh" 2>&1); RC=$?; }
install() { mkdir -p "$C/bin"; cp "$PROMOTE" "$C/bin/promote.sh"; git -C "$C" add bin; git -C "$C" commit -qm "add promote"; git -C "$C" push -q origin dev; }
remote_main() { git -C "$R" rev-parse -q --verify refs/heads/main 2>/dev/null || echo none; }

# 1. first promotion creates main
fresh a; install; run
expect "first promotion exits 0" "$RC"
[ "$(remote_main)" = "$(git -C "$C" rev-parse HEAD)" ]; expect "first promotion creates main at dev tip" $?

# 2. normal fast-forward
commit two; run
expect "fast-forward exits 0" "$RC"
[ "$(remote_main)" = "$(git -C "$C" rev-parse HEAD)" ]; expect "main advanced to dev tip" $?

# 3. no-op when main already has the commit
before=$(remote_main); run
expect "no-op exits 0" "$RC"
case "$OUT" in *"nothing to promote"*) check "no-op says so" ok;; *) check "no-op says so" bad;; esac
[ "$(remote_main)" = "$before" ]; expect "no-op leaves main alone" $?
# main ahead of an older commit is also a no-op
git -C "$C" checkout -q -b old HEAD~1; run; git -C "$C" checkout -q dev
expect "older commit is a no-op" "$RC"

# 4. divergence is refused and the remote is unchanged
fresh b; install; run
git -C "$C" checkout -q -b other; echo x > "$C/g"; git -C "$C" add g; git -C "$C" commit -qm other
git -C "$C" push -q origin other:main
before=$(remote_main)
git -C "$C" checkout -q dev; commit three; run
[ "$RC" != 0 ]; expect "diverged exits non-zero" $?
case "$OUT" in *"main has diverged from dev; reconcile by hand"*) check "diverged message" ok;; *) check "diverged message" bad;; esac
[ "$(remote_main)" = "$before" ]; expect "diverged leaves remote unchanged" $?

# 5. missing token
fresh c; install; commit four; TOK="" run
[ "$RC" != 0 ]; expect "missing token exits non-zero" $?
case "$OUT" in *"GITLAB_PROMOTE_TOKEN is not set"*) check "missing token message" ok;; *) check "missing token message" bad;; esac
[ "$(remote_main)" = none ]; expect "missing token promotes nothing" $?

# 6. unreachable origin fails closed, even when main exists
fresh d; install; run; commit five
git -C "$C" remote set-url origin "$TMP/does-not-exist.git"
before=$(remote_main); run
[ "$RC" != 0 ]; expect "unreachable remote exits non-zero" $?
case "$OUT" in *"refusing to promote"*) check "unreachable message" ok;; *) check "unreachable message" bad;; esac
[ "$(remote_main)" = "$before" ]; expect "unreachable remote promotes nothing" $?
# and when main never existed: still not a "first promotion"
fresh e; install; git -C "$C" remote set-url origin "$TMP/does-not-exist.git"; run
[ "$RC" != 0 ] && [ "$(remote_main)" = none ]; expect "unreachable remote never creates main" $?

# 7. dry run pushes nothing
fresh f; install; PROMOTE_DRY_RUN=1 run
expect "dry run exits 0" "$RC"
case "$OUT" in *"Dry run"*) check "dry run says so" ok;; *) check "dry run says so" bad;; esac
[ "$(remote_main)" = none ]; expect "dry run pushes nothing" $?

# 8. the token never appears, including when git echoes the URL in an error
fresh g; install
raw=$(git -C "$C" push "$TMP/oauth2:${TOKEN}@missing.git" HEAD:refs/heads/main 2>&1)
case "$raw" in *"$TOKEN"*) check "control: raw git output does echo the URL" ok;; *) check "control: raw git output does echo the URL" bad;; esac
URLOVR="$TMP/oauth2:${TOKEN}@missing.git" run
[ "$RC" != 0 ]; expect "failed push exits non-zero" $?
case "$OUT" in *"$TOKEN"*) check "token absent from failed-push output" bad;; *) check "token absent from failed-push output" ok;; esac
OUT2=$(cd "$C" && GITLAB_PROMOTE_TOKEN="$TOKEN" CI_COMMIT_SHA="$(git -C "$C" rev-parse HEAD)" \
    CI_SERVER_HOST=127.0.0.1:9 CI_PROJECT_PATH=x/y bash bin/promote.sh 2>&1)
case "$OUT$OUT2" in *"$TOKEN"*) check "token absent with CI-built URL" bad;; *) check "token absent with CI-built URL" ok;; esac
URLOVR="$R" run
case "$OUT" in *"$TOKEN"*) check "token absent from success output" bad;; *) check "token absent from success output" ok;; esac

echo; echo "$((n-fail))/$n passed"
[ "$fail" = 0 ]
