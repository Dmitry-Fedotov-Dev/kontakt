#!/usr/bin/env bash
# «Открытое радио» — одна программа, одна страница. Порт подбирается сам (занятый пропускается).
#
#   bash scripts/radio.sh             # http://localhost:<порт> — в выводе
#   TUNNEL=1 bash scripts/radio.sh    # плюс временный Cloudflare-туннель: https://….trycloudflare.com
#   PORT=31000 bash scripts/radio.sh  # искать свободный порт, начиная с 31000
#   bash scripts/radio.sh update      # из ДРУГОГО окна: git pull, сборка и перезапуск радио —
#                                     # туннель не трогается, адрес остаётся прежним
#   MONITORING=1 bash scripts/radio.sh  # плюс Prometheus и Grafana — дашборд «Открытое радио»
#                                       # (Docker или без него — сам выберет monitoring/stack.sh)
#
# Мониторинг: скрипт всегда пишет monitoring/prometheus/targets/radio.json с портом этого
# запуска, и уже работающий стек monitoring/ видит радио сам. Радио на телефоне (Termux,
# без Docker) — Prometheus на компьютере забирает метрики через туннель:
#   monitoring/radio-target.sh https://….trycloudflare.com
#
# Адрес trycloudflare живёт, пока жив процесс cloudflared, поэтому при обновлении перезапускается
# только сервер радио, на том же порту. Слушатели и ведущий переподключаются сами за пару секунд.
# Упал сервер сам — тоже поднимается заново, туннель остаётся.
#
# Микрофон браузер даёт только по https или на localhost: чтобы вещать голосом с другого
# устройства, нужен туннель. Чужие туннели и сервисы не трогаются: у cloudflared свой пустой
# конфиг и свои журналы, при выходе гасятся только свои процессы.
set -euo pipefail
cd "$(dirname "$0")/.."

STATE=data/radio
mkdir -p "$STATE" bin
build() { CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$1" ./cmd/radio; }

if [ "${1:-}" = update ]; then
  pid=$(cat "$STATE/run.pid" 2>/dev/null || true)
  if [ -z "$pid" ] || ! kill -0 "$pid" 2>/dev/null; then
    echo "радио не запущено — запустите его: TUNNEL=1 bash scripts/radio.sh"; exit 1
  fi
  [ "${NO_PULL:-}" = 1 ] || git pull --ff-only
  echo "Сборка…"
  # Собираем рядом: не собралось — работает прежняя версия, эфир не прерывается.
  build bin/radio.new || { echo "сборка не удалась — работает прежняя версия"; exit 1; }
  mv bin/radio.new bin/radio # переименование работает и поверх запущенного файла
  kill -USR1 "$pid"          # не HUP: его шлёт закрытие окна Termux, и это было бы не обновление
  echo "Готово: радио перезапускается, адрес туннеля прежний."
  exit 0
fi

command -v go >/dev/null || { echo "Go не найден (нужен 1.24+): https://go.dev/dl/"; exit 1; }
busy() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
port="${PORT:-27620}"
while busy "$port"; do port=$((port + 1)); done

echo "Сборка…"
build bin/radio

srv="" cf="" started=0
TARGET=monitoring/prometheus/targets/radio.json
cleanup() {
  kill $srv $cf 2>/dev/null || true
  wait 2>/dev/null || true
  rm -f "$STATE/run.pid" "$TARGET"
  if [ -n "${MONITORING:-}" ]; then monitoring/stack.sh down; fi
}
trap cleanup EXIT
# Ctrl+C и TERM — выход. Без exit обработчик только прибрал бы, и цикл ниже принял бы
# погашенный сервер за упавший и поднял его снова: так и было без туннеля.
trap 'exit 130' INT
trap 'exit 143' TERM
reload=0
trap 'reload=1' USR1
echo $$ >"$STATE/run.pid"

start_server() {
  ./bin/radio -http "127.0.0.1:$port" &
  srv=$!
  started=$(date +%s)
  for _ in $(seq 40); do curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && return 0; sleep 0.25; done
  echo "сервер радио не ответил за 10 с"
}
start_server
# Цель для Prometheus: файл подхватывается без его перезапуска (file_sd), порт — этого запуска.
printf '[{"targets":["127.0.0.1:%s"],"labels":{"instance":"radio-local"}}]\n' "$port" >"$TARGET"

if [ "${TUNNEL:-}" = 1 ]; then
  command -v cloudflared >/dev/null || { echo "cloudflared не найден — см. подсказку в scripts/cluster-tunnel.sh"; exit 1; }
  : >"$STATE/cloudflared.yml" # пустой свой конфиг: ~/.cloudflared/config.yml не читается
  : >"$STATE/cloudflared.log" # cloudflared дописывает журнал — без очистки нашёлся бы старый адрес
  cloudflared tunnel --no-autoupdate --protocol "${CF_PROTOCOL:-http2}" --config "$STATE/cloudflared.yml" \
    --metrics 127.0.0.1:0 --url "http://127.0.0.1:$port" >"$STATE/cloudflared.log" 2>&1 &
  cf=$!
  url=""
  for _ in $(seq 90); do # адрес выдаём только после соединения с Cloudflare, иначе на нём Error 1033
    [ -n "$url" ] || url=$(grep -ohE 'https://[a-z0-9-]+\.trycloudflare\.com' "$STATE/cloudflared.log" | head -1 || true)
    [ -n "$url" ] && grep -q "Registered tunnel connection" "$STATE/cloudflared.log" && break
    kill -0 "$cf" 2>/dev/null || { echo "cloudflared завершился:"; tail -20 "$STATE/cloudflared.log"; exit 1; }
    sleep 1
  done
  grep -q "Registered tunnel connection" "$STATE/cloudflared.log" || {
    echo "туннель не соединился за 90 с:"; grep -iE "ERR|error|fail" "$STATE/cloudflared.log" | tail -8 || true
    echo "попробуйте CF_PROTOCOL=quic"; exit 1; }
  echo
  echo "  Открытое радио в интернете: $url"
fi
echo "  Локально: http://localhost:$port   (Ctrl+C — выключить)"
if [ -n "${MONITORING:-}" ]; then
  pick() { local p=$1; while busy "$p"; do p=$((p + 1)); done; echo "$p"; }
  export GRAFANA_PORT="${GRAFANA_PORT:-$(pick $((port + 50)))}"
  export PROMETHEUS_PORT="${PROMETHEUS_PORT:-$(pick $((port + 51)))}"
  [ "$PROMETHEUS_PORT" != "$GRAFANA_PORT" ] || PROMETHEUS_PORT=$(pick $((GRAFANA_PORT + 1)))
  echo "  Мониторинг: Prometheus $PROMETHEUS_PORT, Grafana $GRAFANA_PORT…"
  monitoring/stack.sh up
  if monitoring/stack.sh sees "127.0.0.1:$port"; then echo "  Prometheus видит радио"; else echo "  Prometheus НЕ видит радио — графики будут пустыми (с Docker Desktop попробуйте MONITORING=local)"; fi
  echo "  Дашборд: http://localhost:$GRAFANA_PORT/d/kontakt-radio"
fi
echo "  Обновить, не меняя адрес: в другом окне  bash scripts/radio.sh update"

fast=0
while :; do
  wait "$srv" || true # сигнал USR1 прерывает ожидание
  if [ "$reload" = 1 ]; then
    reload=0
    kill "$srv" 2>/dev/null || true
    wait "$srv" 2>/dev/null || true
    start_server
    echo "  $(date +%H:%M:%S) радио обновлено, адрес прежний"
    fast=0
    continue
  fi
  if [ -n "$cf" ] && ! kill -0 "$cf" 2>/dev/null; then
    echo "cloudflared завершился — адрес больше не работает:"; tail -5 "$STATE/cloudflared.log"; exit 1
  fi
  # Сервер упал сам. Поднимаем снова — туннель и адрес остаются; падает раз за разом — выходим.
  if [ $(($(date +%s) - started)) -lt 10 ]; then fast=$((fast + 1)); else fast=0; fi
  if [ "$fast" -ge 3 ]; then echo "радио падает сразу после запуска — выключаюсь"; exit 1; fi
  echo "  $(date +%H:%M:%S) радио остановилось — запускаю снова"
  sleep 1
  start_server
done
