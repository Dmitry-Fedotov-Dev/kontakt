#!/usr/bin/env bash
# Минимальный кластер «Контакта» (media + signal + web + радио под /radio/) за временным Cloudflare-туннелем.
# Не трогает то, что уже работает на машине:
#   - порты подбираются сами: занятый пропускается, берётся следующий свободный;
#   - cloudflared запускается со СВОИМ пустым конфигом, а не с ~/.cloudflared/config.yml —
#     иначе он подхватил бы чужие правила ingress (или отказался бы стартовать рядом с ним);
#   - свои порт метрик, журнал и PID; при выходе гасятся только свои процессы.
#
#   ./scripts/cluster-tunnel.sh              # адрес https://….trycloudflare.com — в выводе
#   PORT_BASE=31000 ./scripts/cluster-tunnel.sh   # искать свободные порты, начиная с 31000
#   ./scripts/cluster-tunnel.sh update       # из ДРУГОГО окна: git pull, сборка и перезапуск сервисов —
#                                            # туннель и мониторинг не трогаются, адрес остаётся прежним
#                                            # (NO_PULL=1 — без git pull). Идущие звонки оборвутся.
#   MONITORING=1 ./scripts/cluster-tunnel.sh      # плюс Prometheus и Grafana на свободных портах:
#                                                 # с Docker Desktop или без Docker — обычными программами
#                                                 # (monitoring/local.sh), иначе в Docker; см. monitoring/stack.sh
#
# Quick Tunnel — только для проверки и демонстраций: без гарантии доступности, до 200
# одновременных HTTP-запросов, адрес меняется при каждом запуске. Для постоянного адреса —
# ./scripts/tunnel.sh publish <домен>. SIP по UDP через туннель не идёт: только браузер.
set -euo pipefail
cd "$(dirname "$0")/.."

PORT_BASE="${PORT_BASE:-27580}"
STATE=data/cluster-tunnel
mkdir -p "$STATE"
build() { CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$1" ./cmd/web ./cmd/signal ./cmd/media ./cmd/radio ./cmd/notify; }

if [ "${1:-}" = update ]; then
  pid=$(cat "$STATE/run.pid" 2>/dev/null || true)
  if [ -z "$pid" ] || ! kill -0 "$pid" 2>/dev/null; then
    echo "кластер не запущен — запустите его: ./scripts/cluster-tunnel.sh"; exit 1
  fi
  [ "${NO_PULL:-}" = 1 ] || git pull --ff-only
  echo "Сборка…"
  # Собираем рядом: не собралось — работает прежняя версия, туннель и звонки не трогаются.
  rm -rf bin/.next && mkdir -p bin/.next
  build bin/.next/ || { rm -rf bin/.next; echo "сборка не удалась — работает прежняя версия"; exit 1; }
  for f in bin/.next/*; do mv -f "$f" bin/; done # переименование работает и поверх запущенного файла
  rmdir bin/.next
  kill -USR1 "$pid"
  echo "Готово: сервисы перезапускаются, адрес туннеля прежний (звонки, что шли, оборвутся)."
  exit 0
fi

command -v go >/dev/null || {
  # run.sh собирает сервисы из исходников: без Go кластер не поднимется
  echo "Go не найден (нужен 1.24+). Установка с go.dev — в apt часто слишком старая версия:"
  echo "  curl -LO https://go.dev/dl/go1.24.7.linux-amd64.tar.gz"
  echo "  sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.24.7.linux-amd64.tar.gz"
  echo "  export PATH=\$PATH:/usr/local/go/bin"
  exit 1
}

command -v cloudflared >/dev/null || {
  echo "cloudflared не найден."
  if grep -qi microsoft /proc/version 2>/dev/null; then
    # WSL: cloudflared из Windows здесь не виден, а cloudflared.exe не берём — по Ctrl+C
    # скрипт гасит свой процесс по PID в WSL, и Windows-процесс туннеля мог бы остаться жить.
    echo "  WSL: поставьте Linux-версию (Windows-туннели она не трогает):"
    echo "    mkdir -p ~/.local/bin"
    echo "    curl -L https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64 -o ~/.local/bin/cloudflared"
    echo "    chmod +x ~/.local/bin/cloudflared && export PATH=\"\$HOME/.local/bin:\$PATH\""
  else
    echo "  https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/"
    echo "  Linux: ~/.local/bin/cloudflared из releases/latest (cloudflared-linux-amd64)   Termux: pkg install cloudflared"
  fi
  exit 1
}

# busy: порт занят, если на 127.0.0.1 кто-то принимает соединения. Проверяем и 0.0.0.0-слушателей —
# они тоже отвечают на 127.0.0.1.
busy() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
# pick ПЕРЕМЕННАЯ НАЧАЛО — первый свободный порт от НАЧАЛА, ещё не выданный этим скриптом.
# Без $(…): в подоболочке список выданных терялся бы, и два сервиса получили бы один порт.
taken=" "
pick() {
  local p=$2
  while busy "$p" || [[ "$taken" == *" $p "* ]]; do p=$((p + 1)); done
  taken+="$p "
  printf -v "$1" '%s' "$p"
}
# UDP-диапазон RTP проверить снаружи нельзя без прав; берём высокий, редко занятый, и media
# сама пропустит занятые порты внутри диапазона.
RTP="${RTP:-47000-48000}" # 500 ног софтфонов; браузер через туннель портов не берёт

pick WEB_PORT "$PORT_BASE"
pick SIGNAL_HTTP $((PORT_BASE + 1))
pick SIGNAL_ADMIN $((PORT_BASE + 11))
pick MEDIA_WS $((PORT_BASE + 2))
pick MEDIA_GRPC $((PORT_BASE + 22))
pick RADIO_HTTP $((PORT_BASE + 3))
pick RADIO_ADMIN $((PORT_BASE + 13))
pick CF_METRICS $((PORT_BASE + 40))
pick NOTIFY_HTTP $((PORT_BASE + 60))
if [ -n "${MONITORING:-}" ]; then
  pick GRAFANA_PORT $((PORT_BASE + 50))
  pick PROMETHEUS_PORT $((PORT_BASE + 51))
  export GRAFANA_PORT PROMETHEUS_PORT
fi
export HTTP="127.0.0.1:$WEB_PORT" SIGNAL_HTTP SIGNAL_ADMIN MEDIA_WS MEDIA_GRPC RADIO_HTTP RADIO_ADMIN NOTIFY_HTTP RTP

run="" cf="" started=0
TARGETS=monitoring/prometheus/targets
cleanup() {
  # только свои процессы — чужие cloudflared и сервисы не трогаем
  kill $run $cf 2>/dev/null || true
  wait 2>/dev/null || true
  rm -f "$TARGETS"/cloudflared-cluster-tunnel.json "$STATE/run.pid"
  if [ -n "${MONITORING:-}" ]; then monitoring/stack.sh down; fi
}
trap cleanup EXIT
# Ctrl+C и TERM — выход. Без exit обработчик только прибрал бы, и цикл ниже принял бы
# погашенные сервисы за упавшие и поднял их снова.
trap 'exit 130' INT
trap 'exit 143' TERM
reload=0
trap 'reload=1' USR1 # update из другого окна
echo $$ >"$STATE/run.pid"

# Сборка — отдельным шагом и на экран: первая (модули, диск /mnt/… в WSL) идёт минуты, и
# без вывода скрипт выглядел зависшим, а ожидание старта съедало время сборки.
echo "Сборка (первый раз — до нескольких минут)…"
mkdir -p bin
build bin/
echo "Сборка готова, запуск…"

# start_run — сервисы станции (run.sh) на уже выбранных портах; собирает этот скрипт, не run.sh:
# при update сборка уже проверена, и неудачная не должна погасить работающее.
start_run() {
  NO_BUILD=1 ./scripts/run.sh >>"$STATE/cluster.log" 2>&1 &
  run=$!
  started=$(date +%s)
  for _ in $(seq 120); do
    curl -fsS "http://127.0.0.1:$WEB_PORT/healthz" >/dev/null 2>&1 && return 0
    kill -0 "$run" 2>/dev/null || return 1
    sleep 0.5
  done
  return 1
}
: >"$STATE/cluster.log"
echo "Кластер: web $WEB_PORT, signal $SIGNAL_HTTP (админка $SIGNAL_ADMIN), media $MEDIA_WS (gRPC $MEDIA_GRPC), радио $RADIO_HTTP (метрики $RADIO_ADMIN), RTP $RTP"
start_run || { echo "кластер не поднялся, журнал: $STATE/cluster.log"; tail -20 "$STATE/cluster.log"; exit 1; }

: >"$STATE/cloudflared.yml" # пустой собственный конфиг: ~/.cloudflared/config.yml не читается
# Журналы — с чистого листа: cloudflared ДОПИСЫВАЕТ --logfile, и без очистки скрипт находил
# адрес и «Registered tunnel connection» прошлого запуска.
: >"$STATE/cloudflared.out"
: >"$STATE/cloudflared.log"
# http2 (TCP 7844) по умолчанию: QUIC (UDP 7844) часто не проходит через NAT WSL и фаерволы,
# и туннель получал адрес без единого соединения — Cloudflare отвечал Error 1033.
cloudflared tunnel --no-autoupdate --protocol "${CF_PROTOCOL:-http2}" --config "$STATE/cloudflared.yml" \
  --metrics "127.0.0.1:$CF_METRICS" --logfile "$STATE/cloudflared.log" \
  --url "http://127.0.0.1:$WEB_PORT" >"$STATE/cloudflared.out" 2>&1 &
cf=$!

# Адрес появляется в журнале РАНЬШЕ, чем cloudflared соединится с Cloudflare: печатать его
# сразу — значит выдать ссылку, на которой Error 1033. Ждём «Registered tunnel connection».
url=""
connected=""
for _ in $(seq 90); do
  [ -n "$url" ] || url=$(grep -ohE 'https://[a-z0-9-]+\.trycloudflare\.com' "$STATE/cloudflared.out" "$STATE/cloudflared.log" 2>/dev/null | head -1 || true)
  grep -qh "Registered tunnel connection" "$STATE/cloudflared.out" "$STATE/cloudflared.log" 2>/dev/null && connected=1
  [ -n "$url" ] && [ -n "$connected" ] && break
  if ! kill -0 "$cf" 2>/dev/null; then
    echo "cloudflared завершился, журнал: $STATE/cloudflared.out"; tail -20 "$STATE/cloudflared.out"; exit 1
  fi
  sleep 1
done
if [ -z "$url" ] || [ -z "$connected" ]; then
  echo "туннель не соединился с Cloudflare за 90 с (адрес: ${url:-нет}). Последние ошибки cloudflared:"
  grep -iE "ERR|error|fail" "$STATE/cloudflared.out" "$STATE/cloudflared.log" 2>/dev/null | tail -8 || true # журнала может не быть — не повод выйти без подсказки
  echo "Попробуйте другой протокол: CF_PROTOCOL=quic ./scripts/cluster-tunnel.sh (по умолчанию http2)"
  exit 1
fi

# Цели для Prometheus: порты подобраны здесь, в prometheus.yml их нет. Файл подхватывается
# сам (file_sd) — и общим стеком мониторинга, если он уже запущен.
mkdir -p "$TARGETS"
# signal, media и радио run.sh пишет сам (*-run.json)
for t in "cloudflared:$CF_METRICS"; do
  printf '[{"targets": ["127.0.0.1:%s"], "labels": {"source": "cluster-tunnel"}}]\n' "${t#*:}" >"$TARGETS/${t%%:*}-cluster-tunnel.json"
done
echo
echo "Контакт доступен: $url"
echo "Радио:            $url/radio/  (та же кука и та же модерация)"
echo "  (два разных браузера или обычное окно + инкогнито — иначе станция видит одного человека)"
echo "  метрики туннеля: http://127.0.0.1:$CF_METRICS/metrics, журналы: $STATE/"
echo
if [ -n "${MONITORING:-}" ]; then
  echo "Мониторинг: Prometheus $PROMETHEUS_PORT, Grafana $GRAFANA_PORT…"
  monitoring/stack.sh up 2>&1 | while IFS= read -r l; do printf '%s %s\n' "$(date +%T)" "$l"; done | tee -a "$STATE/monitoring.log"
  if monitoring/stack.sh sees "127.0.0.1:$SIGNAL_ADMIN"; then
    printf '%s Prometheus видит станцию (signal 127.0.0.1:%s — up)\n' "$(date +%T)" "$SIGNAL_ADMIN" | tee -a "$STATE/monitoring.log"
  else
    printf '%s Prometheus НЕ видит станцию на 127.0.0.1:%s — графики будут пустыми.\n  Docker Desktop не достал до WSL через host.docker.internal? Перезапустите с MONITORING=local (без Docker)\n' "$(date +%T)" "$SIGNAL_ADMIN" | tee -a "$STATE/monitoring.log"
  fi
  echo "Мониторинг: http://localhost:$GRAFANA_PORT (Dashboards → Kontakt: «Контакт: станция», «Kontakt Mesh», «Открытое радио»)"
else
  echo "Мониторинг: MONITORING=1 ./scripts/cluster-tunnel.sh — или уже запущенный стек monitoring/ увидит эту станцию сам"
fi
echo "Обновить, не меняя адрес: в другом окне  ./scripts/cluster-tunnel.sh update"
echo "Ctrl+C — остановить кластер, этот туннель и его мониторинг (другие туннели не затрагиваются)"

# Туннель и мониторинг живут дольше сервисов: update и падение перезапускают только run.sh на тех
# же портах — адрес trycloudflare (он живёт, пока жив cloudflared) не меняется.
fast=0
while :; do
  wait -n "$run" "$cf" || true # USR1 тоже прерывает ожидание
  if [ "$reload" = 1 ]; then
    reload=0
    kill "$run" 2>/dev/null || true
    wait "$run" 2>/dev/null || true
    if start_run; then echo "$(date +%T) сервисы обновлены, адрес прежний: $url"; else echo "$(date +%T) после обновления сервисы не поднялись, журнал: $STATE/cluster.log"; fi
    fast=0
    continue
  fi
  if ! kill -0 "$cf" 2>/dev/null; then
    echo "cloudflared завершился — адрес больше не работает:"; tail -5 "$STATE/cloudflared.log"; exit 1
  fi
  kill -0 "$run" 2>/dev/null && continue
  # Сервисы упали сами. Поднимаем снова — туннель и адрес остаются; падают раз за разом — выходим.
  if [ $(($(date +%s) - started)) -lt 10 ]; then fast=$((fast + 1)); else fast=0; fi
  if [ "$fast" -ge 3 ]; then echo "сервисы падают сразу после запуска — выключаюсь, журнал: $STATE/cluster.log"; exit 1; fi
  echo "$(date +%T) сервисы остановились — запускаю снова"
  sleep 1
  start_run || echo "$(date +%T) сервисы не поднялись, журнал: $STATE/cluster.log"
done
