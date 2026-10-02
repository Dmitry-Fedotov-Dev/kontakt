#!/usr/bin/env bash
# Собрать и запустить все три сервиса «Контакта» на одной машине.
#
#   ./scripts/run.sh                 # http://localhost:8080
#   SIP_UDP=:5060 ./scripts/run.sh   # плюс вход для обычных SIP-софтфонов в локалке
#   HTTPS=:8443 ./scripts/run.sh     # плюс https с самоподписанным сертификатом (микрофон с телефона в локалке)
#
# Логи сервисов идут в консоль с префиксами [web] [signal] [media]. Ctrl+C останавливает всё.
set -euo pipefail
cd "$(dirname "$0")/.."

HTTP="${HTTP:-:8080}"
HTTPS="${HTTPS:-}"
SIP_UDP="${SIP_UDP:-}"
RTP="${RTP:-10000-10200}"
CONFIG="${CONFIG:-config/kontakt.json}"
BANS="${BANS:-data/bans.json}"

mkdir -p bin data
echo "Сборка…"
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/ ./cmd/web ./cmd/signal ./cmd/media

pids=()
cleanup() { kill "${pids[@]}" 2>/dev/null || true; wait 2>/dev/null || true; }
trap cleanup EXIT INT TERM

./bin/media  -grpc 127.0.0.1:7002 -ws 127.0.0.1:8082 -rtp "$RTP" &
pids+=($!)
sleep 0.3
./bin/signal -http 127.0.0.1:8081 -admin 127.0.0.1:8091 -media 127.0.0.1:7002 \
             -config "$CONFIG" -bans "$BANS" ${SIP_UDP:+-sip "$SIP_UDP"} &
pids+=($!)
./bin/web    -http "$HTTP" -signal http://127.0.0.1:8081 -media http://127.0.0.1:8082 ${HTTPS:+-https "$HTTPS"} &
pids+=($!)

echo
echo "Контакт запущен: http://localhost:${HTTP##*:}"
echo "  конфиг:  $CONFIG (правьте на лету — применится через apply_delay_ms)"
echo "  админка: curl -s localhost:8091/admin/stats | jq"
echo
wait -n
