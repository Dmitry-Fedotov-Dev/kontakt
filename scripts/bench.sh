#!/usr/bin/env bash
# Нагрузочный прогон: для каждого числа каналов поднимает чистый стек, гоняет loadgen,
# складывает JSON в bench/. Использование:
#   ./scripts/bench.sh                 # 0 2 100 200 каналов по 30 с
#   DUR=60s ./scripts/bench.sh 2 100   # свои значения
#   MODE=direct ./scripts/bench.sh 100  # /sip и /media мимо web-прокси (как с маршрутами в cloudflared)
set -euo pipefail
cd "$(dirname "$0")/.."
DUR="${DUR:-30s}"
MODE="${MODE:-proxy}"
EXTRA=()
[ "$MODE" = direct ] && EXTRA=(-sip-url http://127.0.0.1:8081 -media-url http://127.0.0.1:8082)
LIST=("$@"); [ ${#LIST[@]} -eq 0 ] && LIST=(0 2 100 200)

mkdir -p bin bench data
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/ ./cmd/web ./cmd/signal ./cmd/media ./cmd/loadgen

for n in "${LIST[@]}"; do
  pkill -f 'bin/(web|signal|media) ' 2>/dev/null || true; sleep 1
  ./bin/media  -grpc 127.0.0.1:7002 -ws 127.0.0.1:8082 -rtp 10000-12000 >bench/media-$n.log 2>&1 &
  sleep 0.3
  ./bin/signal -http 127.0.0.1:8081 -admin 127.0.0.1:8091 -media 127.0.0.1:7002 \
               -config config/kontakt.json -bans "$(mktemp -d)/bans.json" >bench/signal-$n.log 2>&1 &
  ./bin/web    -http 127.0.0.1:8080 >bench/web-$n.log 2>&1 &
  sleep 1
  echo "=== $n каналов, $MODE ==="
  ./bin/loadgen -url http://127.0.0.1:8080 "${EXTRA[@]}" -channels "$n" -dur "$DUR" -json "bench/result-$MODE-$n.json" | tail -40
done
pkill -f 'bin/(web|signal|media) ' 2>/dev/null || true
