#!/usr/bin/env bash
# require-approval_test.sh: exercises bin/ci/require-approval.sh against a fake GitLab API
# (a small python3 http.server on localhost), once with jq reading the JSON and once with
# python3, when both are installed.
# Run by hand: bash bin/ci/require-approval_test.sh
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
SCRIPT="$HERE/require-approval.sh"
TMP=$(mktemp -d)
TOKEN="glpat-s3cr3t-token-VALUE"
fail=0; n=0
check() { n=$((n+1)); if [ "$2" = ok ]; then echo "ok   $1"; else echo "FAIL $1"; fail=$((fail+1)); fi; }

command -v python3 >/dev/null 2>&1 || { echo "python3 is needed to run the fake API"; exit 1; }

# The fake API serves $TMP/api/<scenario>/{mr,approvals,awards,awards2}.json for the
# scenario named in $TMP/api/current, answers 401 to any other token, and the HTTP status
# in <scenario>/status when that file exists. awards2.json is page 2 of the award emoji.
mkdir -p "$TMP/api"
cat >"$TMP/stub.py" <<'PY'
import http.server, os, sys, urllib.parse
ROOT, TOKEN = sys.argv[1], sys.argv[2]
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        with open(os.path.join(ROOT, "current")) as f:
            d = os.path.join(ROOT, f.read().strip())
        u = urllib.parse.urlparse(self.path)
        q = urllib.parse.parse_qs(u.query)
        if self.headers.get("PRIVATE-TOKEN") != TOKEN:
            return self.reply(401, b'{"message":"401 Unauthorized"}')
        if os.path.exists(os.path.join(d, "status")):
            with open(os.path.join(d, "status")) as f:
                return self.reply(int(f.read()), b'{"message":"error"}')
        base = "/api/v4/projects/7/merge_requests/3"
        extra = {}
        if u.path == base: name = "mr.json"
        elif u.path == base + "/approvals": name = "approvals.json"
        elif u.path == base + "/award_emoji":
            page = q.get("page", ["1"])[0]
            name = "awards2.json" if page == "2" else "awards.json"
            if page == "1" and os.path.exists(os.path.join(d, "awards2.json")):
                extra["X-Next-Page"] = "2"
        else:
            return self.reply(404, b'{"message":"404 Not found"}')
        p = os.path.join(d, name)
        body = open(p, "rb").read() if os.path.exists(p) else b"[]"
        self.reply(200, body, extra)
    def reply(self, code, body, extra={}):
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        for k, v in extra.items(): self.send_header(k, v)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
s = http.server.HTTPServer(("127.0.0.1", 0), H)
with open(os.path.join(ROOT, "port"), "w") as f: f.write(str(s.server_port))
s.serve_forever()
PY
python3 -I "$TMP/stub.py" "$TMP/api" "$TOKEN" &
STUB=$!
trap 'kill "$STUB" 2>/dev/null; wait "$STUB" 2>/dev/null; rm -rf "$TMP"' EXIT
for _ in $(seq 50); do [ -s "$TMP/api/port" ] && break; sleep 0.1; done
[ -s "$TMP/api/port" ] || { echo "the fake API did not start"; exit 1; }
API="http://127.0.0.1:$(cat "$TMP/api/port")/api/v4"

# scenario <name> <author> <approvers> <thumbs-up users> [other emoji user]: write a scenario.
scenario() {
    local d="$TMP/api/$1" a="" t="" u
    mkdir -p "$d"
    printf '{"iid":3,"author":{"username":"%s"}}' "$2" >"$d/mr.json"
    for u in $3; do a="$a${a:+,}{\"user\":{\"username\":\"$u\"}}"; done
    printf '{"approved":true,"approved_by":[%s]}' "$a" >"$d/approvals.json"
    for u in $4; do t="$t${t:+,}{\"name\":\"thumbsup\",\"user\":{\"username\":\"$u\"}}"; done
    [ -n "${5:-}" ] && t="$t${t:+,}{\"name\":\"thumbsdown\",\"user\":{\"username\":\"$5\"}}"
    printf '[%s]' "$t" >"$d/awards.json"
}

scenario owner-approved bob "alice" ""
scenario self-mixed bob "Bob" ""
scenario none bob "" ""
scenario self-unset bob "bob" ""
scenario other-approved bob "carol" ""
scenario self-owner bob "bob" ""
scenario thumbs-owner bob "" "alice"
scenario thumbs-self bob "" "bob"
scenario thumbsdown bob "" "" alice
scenario non-owner bob "carol" "carol"
scenario page2 bob "" ""
printf '[{"name":"thumbsup","user":{"username":"alice"}}]' >"$TMP/api/page2/awards2.json"
scenario http500 bob "alice" ""; echo 500 >"$TMP/api/http500/status"
scenario badjson bob "alice" ""; printf '<html>not json' >"$TMP/api/badjson/approvals.json"
scenario badshape bob "alice" ""; printf '[{"user":{"username":"alice"}}]' >"$TMP/api/badshape/approvals.json"

# A PATH without jq, so the python3 reader is exercised too.
NOJQ="$TMP/nojq"; mkdir -p "$NOJQ"
for tool in bash env curl python3 grep sed awk sort tr head paste mktemp rm cat tail; do
    p=$(command -v "$tool") && ln -s "$p" "$NOJQ/$tool"
done

# run <scenario> [VAR=value ...]: run the script, output in $OUT, status in $RC.
run() {
    echo "$1" >"$TMP/api/current"; shift
    OUT=$(env -i PATH="$RUN_PATH" HOME="$TMP" CI_API_V4_URL="$API" CI_PROJECT_ID=7 CI_MERGE_REQUEST_IID=3 \
        APPROVAL_CHECK_TOKEN="$TOKEN" "$@" bash "$SCRIPT" 2>&1); RC=$?
    ALL="$ALL$OUT"
}
passes() { if [ "$RC" = 0 ]; then check "$1" ok; else check "$1" bad; echo "$OUT" | sed 's/^/     /'; fi; }
fails() { if [ "$RC" != 0 ]; then check "$1" ok; else check "$1" bad; echo "$OUT" | sed 's/^/     /'; fi; }
says() { case "$OUT" in *"$2"*) check "$1" ok ;; *) check "$1" bad; echo "$OUT" | sed 's/^/     /' ;; esac; }

suite() {
    echo "# JSON read with $1"
    ALL=
    run owner-approved MERGE_APPROVAL_OWNERS=alice; passes "approved by an allowed owner passes"
    says "the log names the approver" "signed off by alice"
    run none MERGE_APPROVAL_OWNERS=alice; fails "no approvals fails"
    says "no approvals says how to fix it" "retry this job"
    run self-unset; fails "approved only by the author, owners unset, fails"
    run other-approved; passes "approved by someone else, owners unset, passes"
    run self-owner MERGE_APPROVAL_OWNERS="alice, bob"; passes "author listed as owner approves own MR, passes"
    says "a self sign-off is logged" "SELF SIGN-OFF: bob"
    run self-owner MERGE_APPROVAL_OWNERS=$'bob\r'; passes "an owner value with a trailing CR passes"
    run self-owner MERGE_APPROVAL_OWNERS='"bob"'; passes "double-quoted owner passes"
    run self-owner MERGE_APPROVAL_OWNERS="'bob'"; passes "single-quoted owner passes"
    run self-owner MERGE_APPROVAL_OWNERS='"dave,bob"'; passes "a whole value in quotes passes"
    run self-owner MERGE_APPROVAL_OWNERS='@bob'; passes "a leading @ is ignored"
    run self-owner MERGE_APPROVAL_OWNERS='BOB'; passes "owner case is ignored"
    run self-mixed MERGE_APPROVAL_OWNERS='bob'; passes "approver case is ignored"
    run self-owner MERGE_APPROVAL_OWNERS='dave, bob'; passes "comma plus space separation passes"
    run self-owner MERGE_APPROVAL_OWNERS=$'dave\nbob'; passes "newline separation passes"
    run self-owner MERGE_APPROVAL_OWNERS=$'dave\tbob'; passes "tab separation passes"
    run self-owner MERGE_APPROVAL_OWNERS='bo'; fails "an owner is never matched as a substring (prefix)"
    run self-owner MERGE_APPROVAL_OWNERS='mbob'; fails "an owner is never matched as a substring (suffix)"
    # The three states of the owner list, each with its own message.
    run other-approved MERGE_APPROVAL_OWNERS=; passes "an empty owner list falls back"
    says "empty is reported as not set" "MERGE_APPROVAL_OWNERS is not set: using the fallback rule"
    says "the fallback logs zero owners" "Owners recognised: 0"
    run other-approved MERGE_APPROVAL_OWNERS=$' \t\r\n, '; passes "a whitespace-only owner list falls back"
    says "whitespace-only is reported as not set" "MERGE_APPROVAL_OWNERS is not set"
    run other-approved MERGE_APPROVAL_OWNERS='s3cr3t/junk!value'; fails "a junk-only owner list fails instead of falling back"
    says "junk-only says it is set, with its length" "MERGE_APPROVAL_OWNERS is set (17 characters) but holds no valid username: usernames may contain only letters, digits, '.', '_' and '-'; separate them with commas"
    run other-approved MERGE_APPROVAL_OWNERS=$'alice\xc2\xa0'; fails "a non-breaking space makes the entry invalid, and the job fails"
    says "a non-breaking space is reported as invalid" "holds no valid username"
    run other-approved MERGE_APPROVAL_OWNERS='""'; fails "a value of only quotes fails"
    says "only quotes is reported as invalid, not unset" "MERGE_APPROVAL_OWNERS is set (2 characters)"
    run owner-approved MERGE_APPROVAL_OWNERS='alice,b@d!,x/y'; passes "valid owners still work beside invalid entries"
    says "ignored entries are counted" "MERGE_APPROVAL_OWNERS: ignored 2 invalid entries"
    says "the recognised owners are counted" "Owners recognised: 1"
    run owner-approved MERGE_APPROVAL_OWNERS='alice,carol'; says "all valid logs no ignored line" "Owners recognised: 2"
    case "$OUT" in *ignored*) check "all valid owners ignore nothing" bad ;; *) check "all valid owners ignore nothing" ok ;; esac
    run owner-approved MERGE_APPROVAL_OWNERS='ALICE,alice,@alice'; says "duplicates count once" "Owners recognised: 1"
    run owner-approved MERGE_APPROVAL_OWNERS='ZZ-RAW-VALUE-ZZ!,Qq9#'; fails "a raw invalid value fails"
    case "$OUT" in *ZZ-RAW*|*Qq9*) check "the raw owner value never appears in the output" bad ;; *) check "the raw owner value never appears in the output" ok ;; esac
    run non-owner MERGE_APPROVAL_OWNERS=alice; fails "a sign-off by a non-owner does not count when owners are set"
    run thumbs-owner MERGE_APPROVAL_OWNERS=alice; passes "a thumbs-up by an owner passes"
    says "the log names the thumbs-up" "Thumbs-up from: alice"
    run thumbs-owner; passes "a thumbs-up by someone else, owners unset, passes"
    run thumbs-self; fails "a thumbs-up only by the author, owners unset, fails"
    run thumbsdown MERGE_APPROVAL_OWNERS=alice; fails "another emoji is not a sign-off"
    run page2 MERGE_APPROVAL_OWNERS=alice; passes "a thumbs-up on the second page of awards counts"
    run http500 MERGE_APPROVAL_OWNERS=alice; fails "an API error fails closed"
    says "an API error is explained" "HTTP 500"
    run owner-approved MERGE_APPROVAL_OWNERS=alice APPROVAL_CHECK_TOKEN=wrong-token; fails "HTTP 401 fails closed"
    says "401 names the token" "APPROVAL_CHECK_TOKEN is wrong"
    run badjson MERGE_APPROVAL_OWNERS=alice; fails "a reply that is not JSON fails closed"
    run badshape MERGE_APPROVAL_OWNERS=alice; fails "JSON of the wrong shape fails closed"
    run owner-approved MERGE_APPROVAL_OWNERS=alice APPROVAL_CHECK_TOKEN=; fails "a missing token fails closed"
    says "a missing token is explained" "APPROVAL_CHECK_TOKEN is not set"
    run owner-approved MERGE_APPROVAL_OWNERS=alice CI_MERGE_REQUEST_IID=; fails "outside an MR pipeline it fails closed"
    run owner-approved MERGE_APPROVAL_OWNERS=alice CI_API_V4_URL=http://127.0.0.1:1/api/v4; fails "an unreachable API fails closed"
    case "$ALL" in *"$TOKEN"*) check "the token never appears in the output" bad ;; *) check "the token never appears in the output" ok ;; esac
}

if command -v jq >/dev/null 2>&1; then RUN_PATH="$PATH"; suite jq; else echo "# jq not installed: skipping the jq run"; fi
RUN_PATH="$NOJQ"; suite python3

# Neither jq nor python3: no way to read the replies, so no pass.
NONE="$TMP/none"; mkdir -p "$NONE"
for tool in bash env curl grep sed awk sort tr head paste mktemp rm cat; do
    p=$(command -v "$tool") && ln -s "$p" "$NONE/$tool"
done
RUN_PATH="$NONE"; ALL=
run owner-approved MERGE_APPROVAL_OWNERS=alice; fails "no JSON reader on the runner fails closed"
says "a missing JSON reader is explained" "neither jq nor python3"

echo "$((n - fail))/$n passed"
[ "$fail" = 0 ]
