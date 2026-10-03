#!/usr/bin/env bash
# Минимальный кластер «Контакта» (media + signal + web) за временным Cloudflare-туннелем.
# Не трогает то, что уже работает на машине:
#   - порты подбираются сами: занятый пропускается, берётся следующий свободный;
#   - cloudflared запускается со СВОИМ пустым конфигом, а не с ~/.cloudflared/config.yml —
#     иначе он подхватил бы чужие правила ingress (или отказался бы стартовать рядом с ним);
#   - свои порт метрик, журнал и PID; при выходе гасятся только свои процессы.
#
#   ./scripts/cluster-tunnel.sh              # адрес https://….trycloudflare.com — в выводе
#   PORT_BASE=31000 ./scripts/cluster-tunnel.sh   # искать свободные порты, начиная с 31000
#   MONITORING=1 ./scripts/cluster-tunnel.sh      # плюс Prometheus и Grafana (Docker) на свободных портах
#
# Quick Tunnel — только для проверки и демонстраций: без гарантии доступности, до 200
# одновременных HTTP-запросов, адрес меняется при каждом запуске. Для постоянного адреса —
# ./scripts/tunnel.sh publish <домен>. SIP по UDP через туннель не идёт: только браузер.
set -euo pipefail
cd "$(dirname "$0")/.."

PORT_BASE="${PORT_BASE:-27580}"
STATE=data/cluster-tunnel
mkdir -p "$STATE"

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
RTP="${RTP:-47000-47200}"

pick WEB_PORT "$PORT_BASE"
pick SIGNAL_HTTP $((PORT_BASE + 1))
pick SIGNAL_ADMIN $((PORT_BASE + 11))
pick MEDIA_WS $((PORT_BASE + 2))
pick MEDIA_GRPC $((PORT_BASE + 22))
pick CF_METRICS $((PORT_BASE + 40))
if [ "${MONITORING:-}" = 1 ]; then
  command -v docker >/dev/null || { echo "MONITORING=1: нужен Docker (в WSL — Docker Desktop с интеграцией WSL или docker в WSL)"; exit 1; }
  if docker info --format '{{.OperatingSystem}}' 2>/dev/null | grep -qi "docker desktop"; then
    # Стек живёт в сети хоста (метрики станции слушают только 127.0.0.1). У Docker Desktop
    # «хост» — его виртуальная машина: без host networking Prometheus не видит станцию в WSL.
    echo "Docker Desktop: включите Settings → Resources → Network → Enable host networking (4.34+),"
    echo "  иначе мониторинг не увидит станцию. Либо docker прямо в WSL."
  fi
  pick GRAFANA_PORT $((PORT_BASE + 50))
  pick PROMETHEUS_PORT $((PORT_BASE + 51))
  export GRAFANA_PORT PROMETHEUS_PORT
fi
export HTTP="127.0.0.1:$WEB_PORT" SIGNAL_HTTP SIGNAL_ADMIN MEDIA_WS MEDIA_GRPC RTP

pids=()
TARGETS=monitoring/prometheus/targets
cleanup() {
  # только свои процессы — чужие cloudflared и сервисы не трогаем
  kill "${pids[@]}" 2>/dev/null || true
  wait 2>/dev/null || true
  rm -f "$TARGETS"/{signal,media,cloudflared}-cluster-tunnel.json
  if [ "${MONITORING:-}" = 1 ]; then docker compose -f monitoring/docker-compose.yml down >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT INT TERM

# Сборка — отдельным шагом и на экран: первая (модули, диск /mnt/… в WSL) идёт минуты, и
# без вывода скрипт выглядел зависшим, а ожидание старта съедало время сборки.
echo "Сборка (первый раз — до нескольких минут)…"
mkdir -p bin
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/ ./cmd/web ./cmd/signal ./cmd/media
echo "Сборка готова, запуск…"

./scripts/run.sh >"$STATE/cluster.log" 2>&1 & # run.sh соберёт ещё раз — из кеша, за секунды
pids+=($!)
echo "Кластер: web $WEB_PORT, signal $SIGNAL_HTTP (админка $SIGNAL_ADMIN), media $MEDIA_WS (gRPC $MEDIA_GRPC), RTP $RTP"
for _ in $(seq 120); do
  curl -fsS "http://127.0.0.1:$WEB_PORT/healthz" >/dev/null 2>&1 && break
  if ! kill -0 "${pids[0]}" 2>/dev/null; then
    echo "кластер не поднялся, журнал: $STATE/cluster.log"; tail -20 "$STATE/cluster.log"; exit 1
  fi
  sleep 0.5
done
curl -fsS "http://127.0.0.1:$WEB_PORT/healthz" >/dev/null || { echo "web не отвечает, журнал: $STATE/cluster.log"; exit 1; }

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
pids+=($!)

# Адрес появляется в журнале РАНЬШЕ, чем cloudflared соединится с Cloudflare: печатать его
# сразу — значит выдать ссылку, на которой Error 1033. Ждём «Registered tunnel connection».
url=""
connected=""
for _ in $(seq 90); do
  [ -n "$url" ] || url=$(grep -ohE 'https://[a-z0-9-]+\.trycloudflare\.com' "$STATE/cloudflared.out" "$STATE/cloudflared.log" 2>/dev/null | head -1 || true)
  grep -qh "Registered tunnel connection" "$STATE/cloudflared.out" "$STATE/cloudflared.log" 2>/dev/null && connected=1
  [ -n "$url" ] && [ -n "$connected" ] && break
  if ! kill -0 "${pids[1]}" 2>/dev/null; then
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
for t in "signal:$SIGNAL_ADMIN" "media:$MEDIA_WS" "cloudflared:$CF_METRICS"; do
  printf '[{"targets": ["127.0.0.1:%s"], "labels": {"source": "cluster-tunnel"}}]\n' "${t#*:}" >"$TARGETS/${t%%:*}-cluster-tunnel.json"
done
echo
echo "Контакт доступен: $url"
echo "  (два разных браузера или обычное окно + инкогнито — иначе станция видит одного человека)"
echo "  метрики туннеля: http://127.0.0.1:$CF_METRICS/metrics, журналы: $STATE/"
echo
grafana=""
if [ "${MONITORING:-}" = 1 ]; then
  echo "Мониторинг: Prometheus $PROMETHEUS_PORT, Grafana $GRAFANA_PORT…"
  # на экран и в журнал, каждая строка со временем: первый запуск качает образы (~400 МБ),
  # и молча это выглядело зависанием
  echo "  (первый раз Docker скачивает образы Prometheus и Grafana — пара минут)"
  docker compose -f monitoring/docker-compose.yml up -d 2>&1 | while IFS= read -r l; do printf '%s %s\n' "$(date +%T)" "$l"; done | tee -a "$STATE/monitoring.log"
  # Проверка изнутри контейнеров: с Docker Desktop «хост» контейнера — не WSL, и запрос из WSL
  # на 127.0.0.1 не дошёл бы, хотя Grafana в Windows-браузере открывается.
  in_ct() { docker exec "kontakt-monitoring-$1-1" wget -qO- "$2" 2>/dev/null; }
  for _ in $(seq 60); do in_ct grafana "http://127.0.0.1:$GRAFANA_PORT/api/health" >/dev/null && { grafana="http://localhost:$GRAFANA_PORT"; break; }; sleep 2; done
  [ -n "$grafana" ] || printf '%s Grafana не ответила за 2 мин — docker compose -f monitoring/docker-compose.yml logs grafana\n' "$(date +%T)" | tee -a "$STATE/monitoring.log"
  # Видит ли Prometheus станцию: самая частая беда с Docker Desktop без host networking
  seen=""
  for _ in $(seq 15); do
    in_ct prometheus "http://127.0.0.1:$PROMETHEUS_PORT/api/v1/targets?state=active" | grep -q "\"scrapeUrl\":\"http://127.0.0.1:$SIGNAL_ADMIN/metrics\"[^}]*\"health\":\"up\"" && { seen=1; break; }
    sleep 2
  done
  if [ -n "$seen" ]; then
    printf '%s Prometheus видит станцию (signal 127.0.0.1:%s — up)\n' "$(date +%T)" "$SIGNAL_ADMIN" | tee -a "$STATE/monitoring.log"
  else
    printf '%s Prometheus НЕ видит станцию на 127.0.0.1:%s — графики будут пустыми.\n' "$(date +%T)" "$SIGNAL_ADMIN" | tee -a "$STATE/monitoring.log"
    echo "  Docker Desktop: Settings → Resources → Network → Enable host networking, затем перезапуск скрипта;"
    echo "  либо docker прямо в WSL (sudo apt install docker.io) вместо Docker Desktop."
  fi
fi
if [ -n "$grafana" ]; then
  echo "Мониторинг: $grafana (дашборды «Контакт: станция» и «Kontakt Mesh»)"
elif [ "${MONITORING:-}" != 1 ]; then
  echo "Мониторинг: MONITORING=1 ./scripts/cluster-tunnel.sh — или уже запущенный стек monitoring/ увидит эту станцию сам"
fi
echo "Ctrl+C — остановить кластер, этот туннель и его мониторинг (другие туннели не затрагиваются)"
wait -n
