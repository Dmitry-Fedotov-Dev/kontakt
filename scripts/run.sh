#!/usr/bin/env bash
# Собрать и запустить «Контакт» на одной машине: media + signal + web и Открытое радио под /radio/
# того же web — одна кука, одна модерация (база — у signal, радио ходит к нему через админ-порт).
#
#   ./scripts/run.sh                 # http://localhost:8080, радио — http://localhost:8080/radio/
#   SIP_UDP=:5060 ./scripts/run.sh   # плюс вход для обычных SIP-софтфонов в локалке
#   IP=127.0.0.1 SIP_UDP=:5060 …     # адрес для SDP и Via/Contact софтфонов (по умолчанию — определить)
#   HTTPS=:8443 ./scripts/run.sh     # плюс https с самоподписанным сертификатом (микрофон с телефона в локалке)
#
# Логи сервисов идут в консоль с префиксами [web] [signal] [media] [radio]. Ctrl+C останавливает всё.
# Цель Prometheus для радио пишется в monitoring/prometheus/targets/radio-run.json (порт этого запуска).
set -euo pipefail
cd "$(dirname "$0")/.."

HTTP="${HTTP:-:8080}"
# Внутренние порты (все — только на 127.0.0.1); переопределяются, если заняты
SIGNAL_HTTP="${SIGNAL_HTTP:-8081}"
SIGNAL_ADMIN="${SIGNAL_ADMIN:-8091}"
MEDIA_GRPC="${MEDIA_GRPC:-7002}"
MEDIA_WS="${MEDIA_WS:-8082}"
RADIO_HTTP="${RADIO_HTTP:-8083}"
RADIO_ADMIN="${RADIO_ADMIN:-8093}" # /metrics радио
HTTPS="${HTTPS:-}"
SIP_UDP="${SIP_UDP:-}"
IP="${IP:-}"
RTP="${RTP:-10000-10200}"
CONFIG="${CONFIG:-config/kontakt.json}"
BANS="${BANS:-data/bans.json}"

mkdir -p bin data
echo "Сборка…"
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/ ./cmd/web ./cmd/signal ./cmd/media ./cmd/radio

pids=()
RADIO_TARGET=monitoring/prometheus/targets/radio-run.json
cleanup() { kill "${pids[@]}" 2>/dev/null || true; wait 2>/dev/null || true; rm -f "$RADIO_TARGET"; }
trap cleanup EXIT INT TERM

./bin/media  -grpc "127.0.0.1:$MEDIA_GRPC" -ws "127.0.0.1:$MEDIA_WS" -rtp "$RTP" ${IP:+-ip "$IP"} &
pids+=($!)
sleep 0.3
./bin/signal -http "127.0.0.1:$SIGNAL_HTTP" -admin "127.0.0.1:$SIGNAL_ADMIN" -media "127.0.0.1:$MEDIA_GRPC" \
             -config "$CONFIG" -bans "$BANS" ${SIP_UDP:+-sip "$SIP_UDP"} ${IP:+-ip "$IP"} &
pids+=($!)
./bin/radio  -http "127.0.0.1:$RADIO_HTTP" -admin "127.0.0.1:$RADIO_ADMIN" -mod "http://127.0.0.1:$SIGNAL_ADMIN" &
pids+=($!)
./bin/web    -http "$HTTP" -signal "http://127.0.0.1:$SIGNAL_HTTP" -media "http://127.0.0.1:$MEDIA_WS" \
             -radio "http://127.0.0.1:$RADIO_HTTP" ${HTTPS:+-https "$HTTPS"} &
pids+=($!)
printf '[{"targets":["127.0.0.1:%s"],"labels":{"instance":"radio-run"}}]\n' "$RADIO_ADMIN" >"$RADIO_TARGET"

echo
echo "Контакт запущен: http://localhost:${HTTP##*:}, радио: http://localhost:${HTTP##*:}/radio/"
echo "  конфиг:  $CONFIG (правьте на лету — применится через apply_delay_ms)"
echo "  админка: curl -s localhost:$SIGNAL_ADMIN/admin/stats | jq"
echo "  модерация (и радио): curl -s localhost:$SIGNAL_ADMIN/admin/journal | jq; бан — POST /admin/ban?key=…&zone=calls|air|all"
echo
wait -n
