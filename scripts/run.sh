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
# Цели Prometheus (signal, media, радио) пишутся в monitoring/prometheus/targets/<сервис>-run-<порт>.json
# с портами этого запуска (два стенда рядом друг другу не мешают) и убираются при выходе: тревога «станция не отвечает» горит, только
# пока стенд должен работать.
#
# Telegram-бот (cmd/notify): если в .env есть TELEGRAM_BOT_TOKEN и TELEGRAM_CHAT_ID — тревоги,
# карточки и баны приходят в чат. Упавший бот стенд не останавливает.
set -euo pipefail
cd "$(dirname "$0")/.."

HTTP="${HTTP:-:8080}"
# Внутренние порты (все — только на 127.0.0.1); переопределяются, если заняты
SIGNAL_HTTP="${SIGNAL_HTTP:-8081}"
SIGNAL_ADMIN="${SIGNAL_ADMIN:-8091}"
MEDIA_GRPC="${MEDIA_GRPC:-7002}"
MEDIA_WS="${MEDIA_WS:-8082}"
NOTIFY_HTTP="${NOTIFY_HTTP:-27640}"
RADIO_HTTP="${RADIO_HTTP:-8083}"
RADIO_ADMIN="${RADIO_ADMIN:-8093}" # /metrics радио
HTTPS="${HTTPS:-}"
SIP_UDP="${SIP_UDP:-}"
IP="${IP:-}"
RTP="${RTP:-10000-11000}" # UDP RTP софтфонов: нога занимает чётный порт — 500 ног; браузер портов не берёт
CONFIG="${CONFIG:-config/kontakt.json}"
BANS="${BANS:-data/bans.json}"

# токен бота — из .env (в git не попадает); переменные окружения важнее файла
if [ -f .env ] && [ -z "${TELEGRAM_BOT_TOKEN:-}" ]; then set -a; . ./.env; set +a; fi

mkdir -p bin data
if [ "${NO_BUILD:-}" != 1 ]; then # cluster-tunnel.sh собирает сам — и при update проверяет сборку заранее
  echo "Сборка…"
  CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/ ./cmd/web ./cmd/signal ./cmd/media ./cmd/radio ./cmd/notify
fi

pids=() core=()
TARGETS=monitoring/prometheus/targets
cleanup() {
  kill "${pids[@]}" 2>/dev/null || true
  wait 2>/dev/null || true
  rm -f "$TARGETS/signal-run-$SIGNAL_ADMIN.json" "$TARGETS/media-run-$MEDIA_WS.json" "$TARGETS/radio-run-$RADIO_ADMIN.json"
}
trap cleanup EXIT INT TERM

notify=""
# NOTIFY=0 — без бота: второй стенд рядом (Telegram отдаёт обновления только одному боту с токеном)
if [ "${NOTIFY:-1}" != 0 ] && [ -n "${TELEGRAM_BOT_TOKEN:-}" ] && [ -n "${TELEGRAM_CHAT_ID:-}" ]; then
  ./bin/notify -http "127.0.0.1:$NOTIFY_HTTP" -admin "http://127.0.0.1:$SIGNAL_ADMIN" \
               -prometheus "http://127.0.0.1:${PROMETHEUS_PORT:-9092}" &
  pids+=($!)
  notify="http://127.0.0.1:$NOTIFY_HTTP"
fi

./bin/media  -grpc "127.0.0.1:$MEDIA_GRPC" -ws "127.0.0.1:$MEDIA_WS" -rtp "$RTP" ${IP:+-ip "$IP"} &
pids+=($!) core+=($!)
sleep 0.3
./bin/signal -http "127.0.0.1:$SIGNAL_HTTP" -admin "127.0.0.1:$SIGNAL_ADMIN" -media "127.0.0.1:$MEDIA_GRPC" \
             -config "$CONFIG" -bans "$BANS" ${SIP_UDP:+-sip "$SIP_UDP"} ${IP:+-ip "$IP"} ${notify:+-notify "$notify"} &
pids+=($!) core+=($!)
./bin/radio  -http "127.0.0.1:$RADIO_HTTP" -admin "127.0.0.1:$RADIO_ADMIN" -mod "http://127.0.0.1:$SIGNAL_ADMIN" &
pids+=($!) core+=($!)
./bin/web    -http "$HTTP" -signal "http://127.0.0.1:$SIGNAL_HTTP" -media "http://127.0.0.1:$MEDIA_WS" \
             -radio "http://127.0.0.1:$RADIO_HTTP" ${HTTPS:+-https "$HTTPS"} &
pids+=($!) core+=($!)
mkdir -p "$TARGETS"
# без своей метки instance: её роль играет адрес. С общей меткой (была «radio-run») два стенда
# рядом склеивались в один ряд, и время старта, прыгая между ними, зажигало RadioRestartLoop.
for t in "signal:$SIGNAL_ADMIN" "media:$MEDIA_WS" "radio:$RADIO_ADMIN"; do
  printf '[{"targets":["127.0.0.1:%s"]}]\n' "${t#*:}" >"$TARGETS/${t%%:*}-run-${t#*:}.json"
done

echo
echo "Контакт запущен: http://localhost:${HTTP##*:}, радио: http://localhost:${HTTP##*:}/radio/"
echo "  конфиг:  $CONFIG (правьте на лету — применится через apply_delay_ms)"
echo "  админка: curl -s localhost:$SIGNAL_ADMIN/admin/stats | jq"
echo "  модерация (и радио): curl -s localhost:$SIGNAL_ADMIN/admin/journal | jq; бан — POST /admin/ban?key=…&zone=calls|air|all"
echo "  Telegram-бот: ${notify:-выключен (NOTIFY=0 или нет TELEGRAM_BOT_TOKEN / TELEGRAM_CHAT_ID в .env)}"
echo
wait -n "${core[@]}" # упал любой сервис станции — гасим всё; бот сюда не входит
