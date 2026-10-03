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
export HTTP="127.0.0.1:$WEB_PORT" SIGNAL_HTTP SIGNAL_ADMIN MEDIA_WS MEDIA_GRPC RTP

pids=()
cleanup() {
  # только свои процессы — чужие cloudflared и сервисы не трогаем
  kill "${pids[@]}" 2>/dev/null || true
  wait 2>/dev/null || true
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
cloudflared tunnel --no-autoupdate --config "$STATE/cloudflared.yml" \
  --metrics "127.0.0.1:$CF_METRICS" --logfile "$STATE/cloudflared.log" \
  --url "http://127.0.0.1:$WEB_PORT" >"$STATE/cloudflared.out" 2>&1 &
pids+=($!)

url=""
for _ in $(seq 60); do
  url=$(grep -ohE 'https://[a-z0-9-]+\.trycloudflare\.com' "$STATE/cloudflared.out" "$STATE/cloudflared.log" 2>/dev/null | head -1 || true)
  [ -n "$url" ] && break
  if ! kill -0 "${pids[1]}" 2>/dev/null; then
    echo "cloudflared завершился, журнал: $STATE/cloudflared.out"; tail -20 "$STATE/cloudflared.out"; exit 1
  fi
  sleep 1
done
[ -n "$url" ] || { echo "адрес туннеля не появился за 60 с, журнал: $STATE/cloudflared.out"; exit 1; }

echo
echo "Контакт доступен: $url"
echo "  (два разных браузера или обычное окно + инкогнито — иначе станция видит одного человека)"
echo "  метрики туннеля: http://127.0.0.1:$CF_METRICS/metrics, журналы: $STATE/"
echo "  Ctrl+C — остановить кластер и этот туннель (другие туннели не затрагиваются)"
wait -n
