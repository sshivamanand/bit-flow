#!/bin/bash
# End-to-end checks of the real bitflow binary on macOS (uses mkfile and BSD stat).
# Usage: scripts/e2e-macos.sh bin/bitflow /tmp/bitflow-e2e   (or: make e2e)
B="$1"; W="$2"
rm -rf "$W"; mkdir -p "$W"
pass=0; fail=0
ok()   { echo "PASS  $1"; pass=$((pass+1)); }
bad()  { echo "FAIL  $1"; fail=$((fail+1)); }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }
strip() { tr '\r' '\n' | grep -v "ETA"; }
cleanup() { pkill -f "$B" 2>/dev/null; sleep 0.4; }
trap cleanup EXIT

# --- test data: a "game" with a big file, odd sizes, many small files, empty file/dir, an executable
G="$W/src/Game"; mkdir -p "$G/data/deep" "$G/small" "$G/saves/empty"
head -c 150000000 /dev/urandom > "$G/data/big.pak"
head -c 3000001 /dev/urandom > "$G/data/deep/level.dat"
for i in $(seq 1 400); do head -c $((i*97)) /dev/urandom > "$G/small/f$i.txt"; done
: > "$G/empty.txt"; printf '#!/bin/sh\necho hi\n' > "$G/run.sh"; chmod 755 "$G/run.sh"
same() { diff -r "$1" "$2" >/dev/null 2>&1 && [ "$(stat -f %Lp "$1/run.sh")" = "$(stat -f %Lp "$2/run.sh")" ]; }
recv() { # recv NAME PORT DEST [extra flags...]
  local name=$1 port=$2 dest=$3; shift 3
  mkdir -p "$W/cfg-$name" "$dest"
  printf '{"id":"id-%s","name":"%s"}' "$name" "$name" > "$W/cfg-$name/config.json"
  (BITFLOW_CONFIG_DIR="$W/cfg-$name" "$B" recv --to "$dest" --port "$port" --code 135790 --yes "$@" > "$W/recv-$name.log" 2>&1 &)
  sleep 0.6
}
sender() { mkdir -p "$W/cfg-sender"; printf '{"id":"id-sender","name":"laptop"}' > "$W/cfg-sender/config.json"; BITFLOW_CONFIG_DIR="$W/cfg-sender" "$B" "$@"; }

echo "== CLI"
recv desk 7721 "$W/in1"
out=$(sender send "$G" --to 127.0.0.1:7721 --code 135790 2>&1 | strip)
check "send by address" "echo \"\$out\" | grep -q '✓ Sent 404 files' && same '$G' '$W/in1/Game'"

out=$(sender peers 2>&1)
check "peers lists the receiver" "echo \"\$out\" | grep -q 'desk'"

rm -rf "$W/in1/Game"
out=$(sender send "$G" --to desk --code 135790 2>&1 | strip)
check "send by device name" "echo \"\$out\" | grep -q '✓ Sent' && same '$G' '$W/in1/Game'"

out=$(sender send "$G" --to desk --code 135790 2>&1 | strip)
check "identical files skipped" "echo \"\$out\" | grep -q 'was already here'"

out=$(sender send "$G" --to desk --code 999999 2>&1 | strip)
check "wrong code rejected" "echo \"\$out\" | grep -q 'wrong pairing code'"
cleanup

recv desk 7721 "$W/in2"
mkdir -p "$W/in2/Game"; echo different > "$W/in2/Game/run.sh"
out=$(sender send "$G" --to 127.0.0.1:7721 --code 135790 2>&1 | strip)
check "differing file refused" "echo \"\$out\" | grep -q 'already exist' && grep -q different '$W/in2/Game/run.sh'"
cleanup
recv desk 7721 "$W/in2" --on-conflict=rename
out=$(sender send "$G" --to 127.0.0.1:7721 --code 135790 2>&1 | strip)
check "rename policy keeps both" "same '$G' '$W/in2/Game (1)' && grep -q different '$W/in2/Game/run.sh'"
cleanup

recv solo 7722 "$W/in3"
out=$(sender send "$G" --code 135790 2>&1 | strip)
check "auto-pick the only receiver" "echo \"\$out\" | grep -q 'Found solo' && same '$G' '$W/in3/Game'"
cleanup

recv rawbox 7723 "$W/in4" --allow-raw
out=$(sender send "$G" --to 127.0.0.1:7723 --code 135790 --raw 2>&1 | strip)
check "raw mode" "echo \"\$out\" | grep -q '✓ Sent' && same '$G' '$W/in4/Game' && grep -q 'NOT be encrypted' '$W/recv-rawbox.log'"
cleanup

# reverse: sender waits, receiver fetches by name
mkdir -p "$W/cfg-waiter" "$W/in5"; printf '{"id":"id-waiter","name":"waiter"}' > "$W/cfg-waiter/config.json"
(BITFLOW_CONFIG_DIR="$W/cfg-waiter" "$B" send "$G" --wait --port 7724 --code 246802 > "$W/waiter.log" 2>&1 &)
sleep 0.8
mkdir -p "$W/cfg-fetcher"; printf '{"id":"id-fetcher","name":"fetcher"}' > "$W/cfg-fetcher/config.json"
out=$(BITFLOW_CONFIG_DIR="$W/cfg-fetcher" "$B" recv --to "$W/in5" --from waiter --code 246802 --yes 2>&1 | strip)
check "reverse mode: fetch from waiting sender" "echo \"\$out\" | grep -q '✓ Received 404 files' && same '$G' '$W/in5/Game'"
cleanup

# resume: kill the sender mid-transfer, send again
R="$W/src/Huge"; mkdir -p "$R"; mkfile -n 10g "$R/disk.img"
recv keeper 7725 "$W/in6"
mkdir -p "$W/cfg-sender"; printf '{"id":"id-sender","name":"laptop"}' > "$W/cfg-sender/config.json"
(BITFLOW_CONFIG_DIR="$W/cfg-sender" "$B" send "$R" --to 127.0.0.1:7725 --code 135790 --streams 1 > "$W/killed.log" 2>&1 &)
sleep 1.0; pkill -f "send $R"; sleep 1
partial=$(ls "$W/in6/Huge/" 2>/dev/null)
out=$(sender send "$R" --to 127.0.0.1:7725 --code 135790 2>&1 | strip)
check "resume after the sender was killed" "echo \"\$partial\" | grep -q bitflow-partial && echo \"\$out\" | grep -q 'was already here' && cmp -s '$R/disk.img' '$W/in6/Huge/disk.img'"
cleanup

out=$("$B" bench --loopback --size 4GB 2>&1 | strip)
check "bench loopback" "echo \"\$out\" | grep -qE 'auto|streams'"
echo "$out" | grep -E 'auto \(|streams ' | tail -1

echo "== app"
mkdir -p "$W/cfg-app" "$W/app-in"; printf '{"id":"id-app","dest":"%s"}' "$W/app-in" > "$W/cfg-app/config.json"
(BITFLOW_CONFIG_DIR="$W/cfg-app" "$B" ui --no-browser --listen 127.0.0.1:7730 --port 7731 > "$W/app.log" 2>&1 &)
sleep 1.2
URL=$(grep -o 'http://[^ ]*' "$W/app.log"); BASE=${URL%%/#*}; TOK=${URL##*t=}
J=(-s -H "X-Bitflow: 1" -H "Content-Type: application/json")
code=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/api/state")
check "app API refuses anonymous callers" "[ $code = 401 ]"
curl "${J[@]}" -c "$W/cookies" -H "X-Bitflow-Token: $TOK" -X POST "$BASE/api/session/start" > /dev/null
s=$(curl "${J[@]}" -b "$W/cookies" "$BASE/api/session")
check "session cookie works; new session asks for a name" "echo '$s' | grep -q '\"needs_name\":true'"
curl "${J[@]}" -b "$W/cookies" -X POST -d '{"name":"app-box"}' "$BASE/api/session" > /dev/null
st=$(curl "${J[@]}" -b "$W/cookies" "$BASE/api/state")
check "name saved as device name" "echo '$st' | grep -q '\"name\":\"app-box\"'"
APPCODE=$(echo "$st" | python3 -c "import json,sys; print(json.load(sys.stdin)['code'])")

recv desk 7726 "$W/in7"
curl "${J[@]}" -b "$W/cookies" -X POST -d "{\"to\":\"127.0.0.1:7726\",\"peer\":\"desk\",\"paths\":[\"$G\"],\"code\":\"135790\"}" "$BASE/api/send" > /dev/null
for i in $(seq 1 30); do sleep 0.5; same "$G" "$W/in7/Game" && break; done
check "app sends to a CLI receiver" "same '$G' '$W/in7/Game'"

# CLI sends to the app; accept through the API
(BITFLOW_CONFIG_DIR="$W/cfg-sender" "$B" send "$G" --to 127.0.0.1:7731 --code "$APPCODE" > "$W/to-app.log" 2>&1 &)
for i in $(seq 1 20); do sleep 0.3; OID=$(curl "${J[@]}" -b "$W/cookies" "$BASE/api/state" | python3 -c "import json,sys; o=json.load(sys.stdin)['offers']; print(o[0]['id'] if o else '')"); [ -n "$OID" ] && break; done
curl "${J[@]}" -b "$W/cookies" -X POST -d '{"accept":true}' "$BASE/api/offers/$OID" > /dev/null
for i in $(seq 1 30); do sleep 0.5; same "$G" "$W/app-in/Game" && break; done
check "app receives after accepting in the UI" "[ -n '$OID' ] && same '$G' '$W/app-in/Game'"
hist=$(python3 -c "import json; print(len(json.load(open('$W/cfg-app/history.json'))))" 2>/dev/null)
check "transfer history saved" "[ '${hist:-0}' -ge 2 ]"

echo "== browsers, /get, steam"
recv webbox 7727 "$W/in8"
code=$(curl -s -o /dev/null -w "%{http_code}" -X POST -H "X-Bitflow: 1" -d '{"code":"000000"}' http://127.0.0.1:7727/web/login)
check "browser login refuses a wrong code" "[ $code = 401 ]"
curl -s -c "$W/webjar" -X POST -H "X-Bitflow: 1" -d '{"code":"135790"}' http://127.0.0.1:7727/web/login > /dev/null
head -c 9000000 /dev/urandom > "$W/phone.bin"
UP=$(curl -s -b "$W/webjar" -X POST -H "X-Bitflow: 1" -d '{"name":"Camera/phone.bin","size":9000000}' http://127.0.0.1:7727/web/upload | python3 -c "import json,sys; print(json.load(sys.stdin)['id'])")
dd if="$W/phone.bin" of="$W/c2" bs=1 skip=8388608 2>/dev/null; head -c 8388608 "$W/phone.bin" > "$W/c1"
curl -s -b "$W/webjar" -X PUT -H "X-Bitflow: 1" --data-binary @"$W/c2" "http://127.0.0.1:7727/web/upload/$UP?off=8388608" > /dev/null
curl -s -b "$W/webjar" -X PUT -H "X-Bitflow: 1" --data-binary @"$W/c1" "http://127.0.0.1:7727/web/upload/$UP?off=0" > /dev/null
check "browser upload (chunks out of order)" "cmp -s '$W/phone.bin' '$W/in8/Camera/phone.bin'"
curl -fsSL http://127.0.0.1:7727/get/sh | BITFLOW_DIR="$W/friend" sh > /dev/null 2>&1
check "/get installer installs a working bitflow" "'$W/friend/bitflow' version | grep -q bitflow"
cleanup

mk_steam() { # mk_steam ROOT [with-game]
  mkdir -p "$1/steamapps/common"
  [ -n "$2" ] || return 0
  printf '"AppState"\n{\n\t"appid"\t"620"\n\t"name"\t"Portal 2"\n\t"StateFlags"\t"4"\n\t"installdir"\t"Portal 2"\n\t"SizeOnDisk"\t"9000000"\n}\n' > "$1/steamapps/appmanifest_620.acf"
  mkdir -p "$1/steamapps/common/Portal 2/bin"; head -c 9000000 /dev/urandom > "$1/steamapps/common/Portal 2/portal2.pak"
  printf 'engine' > "$1/steamapps/common/Portal 2/bin/engine"
}
mk_steam "$W/steamA" yes; mk_steam "$W/steamB"
out=$(BITFLOW_STEAM_ROOT="$W/steamA" "$B" games 2>&1)
check "games lists installed Steam games" "echo \"\$out\" | grep -q 'Portal 2'"
mkdir -p "$W/cfg-gamer"; printf '{"id":"id-gamer","name":"gamer"}' > "$W/cfg-gamer/config.json"
(BITFLOW_STEAM_ROOT="$W/steamB" BITFLOW_CONFIG_DIR="$W/cfg-gamer" "$B" recv --to "$W/in9" --port 7728 --code 135790 --yes > "$W/recv-gamer.log" 2>&1 &)
mkdir -p "$W/in9"; sleep 0.6
out=$(BITFLOW_STEAM_ROOT="$W/steamA" BITFLOW_CONFIG_DIR="$W/cfg-sender" "$B" send --steam portal --to 127.0.0.1:7728 --code 135790 2>&1 | strip)
check "Steam game lands in the receiver's library" "cmp -s '$W/steamA/steamapps/common/Portal 2/portal2.pak' '$W/steamB/steamapps/common/Portal 2/portal2.pak' && [ -f '$W/steamB/steamapps/appmanifest_620.acf' ]"
cleanup

echo
echo "$pass passed, $fail failed"
cleanup
[ $fail = 0 ] && rm -rf "$W"   # keep the evidence when something failed
[ $fail = 0 ]
