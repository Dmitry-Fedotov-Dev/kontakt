#!/usr/bin/env bash
# Собрать и запустить все три сервиса «Контакта» на одной машине.
#
#   ./scripts/run.sh                 # http://localhost:8080
#   SIP_UDP=:5060 ./scripts/run.sh   # плюс вход для обычных SIP-софтфонов в локалке
#   IP=127.0.0.1 SIP_UDP=:5060 …     # адрес для SDP и Via/Contact софтфонов (по умолчанию — определить)
#   HTTPS=:8443 ./scripts/run.sh     # плюс https с самоподписанным сертификатом (микрофон с телефона в локалке)
#
# Логи сервисов идут в консоль с префиксами [web] [signal] [media]. Ctrl+C останавливает всё.
set -euo pipefail
cd "$(dirname "$0")/.."

HTTP="${HTTP:-:8080}"
# Внутренние порты (все — только на 127.0.0.1); переопределяются, если заняты
SIGNAL_HTTP="${SIGNAL_HTTP:-8081}"
SIGNAL_ADMIN="${SIGNAL_ADMIN:-8091}"
MEDIA_GRPC="${MEDIA_GRPC:-7002}"
MEDIA_WS="${MEDIA_WS:-8082}"
HTTPS="${HTTPS:-}"
SIP_UDP="${SIP_UDP:-}"
IP="${IP:-}"
RTP="${RTP:-10000-10200}"
CONFIG="${CONFIG:-config/kontakt.json}"
BANS="${BANS:-data/bans.json}"

mkdir -p bin data
echo "Сборка…"
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/ ./cmd/web ./cmd/signal ./cmd/media

pids=()
cleanup() { kill "${pids[@]}" 2>/dev/null || true; wait 2>/dev/null || true; }
trap cleanup EXIT INT TERM

./bin/media  -grpc "127.0.0.1:$MEDIA_GRPC" -ws "127.0.0.1:$MEDIA_WS" -rtp "$RTP" ${IP:+-ip "$IP"} &
pids+=($!)
sleep 0.3
./bin/signal -http "127.0.0.1:$SIGNAL_HTTP" -admin "127.0.0.1:$SIGNAL_ADMIN" -media "127.0.0.1:$MEDIA_GRPC" \
             -config "$CONFIG" -bans "$BANS" ${SIP_UDP:+-sip "$SIP_UDP"} ${IP:+-ip "$IP"} &
pids+=($!)
./bin/web    -http "$HTTP" -signal "http://127.0.0.1:$SIGNAL_HTTP" -media "http://127.0.0.1:$MEDIA_WS" ${HTTPS:+-https "$HTTPS"} &
pids+=($!)

echo
echo "Контакт запущен: http://localhost:${HTTP##*:}"
echo "  конфиг:  $CONFIG (правьте на лету — применится через apply_delay_ms)"
echo "  админка: curl -s localhost:$SIGNAL_ADMIN/admin/stats | jq"
echo
wait -n
