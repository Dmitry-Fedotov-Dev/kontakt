#!/usr/bin/env bash
# «Открытое радио» — одна программа, одна страница. Порт подбирается сам (занятый пропускается).
#
#   ./scripts/radio.sh             # http://localhost:<порт> — в выводе
#   TUNNEL=1 ./scripts/radio.sh    # плюс временный Cloudflare-туннель: https://….trycloudflare.com
#   PORT=31000 ./scripts/radio.sh  # искать свободный порт, начиная с 31000
#
# Микрофон браузер даёт только по https или на localhost: чтобы вещать голосом с другого
# устройства, нужен туннель. Чужие туннели и сервисы не трогаются: у cloudflared свой пустой
# конфиг и свои журналы, при выходе гасятся только свои процессы.
set -euo pipefail
cd "$(dirname "$0")/.."

command -v go >/dev/null || { echo "Go не найден (нужен 1.24+): https://go.dev/dl/"; exit 1; }
busy() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
port="${PORT:-27620}"
while busy "$port"; do port=$((port + 1)); done

echo "Сборка…"
mkdir -p bin
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/ ./cmd/radio

pids=()
cleanup() { kill "${pids[@]}" 2>/dev/null || true; wait 2>/dev/null || true; }
trap cleanup EXIT INT TERM

./bin/radio -http "127.0.0.1:$port" &
pids+=($!)
for _ in $(seq 40); do curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && break; sleep 0.25; done

if [ "${TUNNEL:-}" = 1 ]; then
  command -v cloudflared >/dev/null || { echo "cloudflared не найден — см. подсказку в scripts/cluster-tunnel.sh"; exit 1; }
  STATE=data/radio-tunnel
  mkdir -p "$STATE"
  : >"$STATE/cloudflared.yml"   # пустой свой конфиг: ~/.cloudflared/config.yml не читается
  : >"$STATE/cloudflared.log"   # cloudflared дописывает журнал — без очистки нашёлся бы старый адрес
  cloudflared tunnel --no-autoupdate --protocol "${CF_PROTOCOL:-http2}" --config "$STATE/cloudflared.yml" \
    --metrics 127.0.0.1:0 --url "http://127.0.0.1:$port" >"$STATE/cloudflared.log" 2>&1 &
  pids+=($!)
  url=""
  for _ in $(seq 90); do # адрес выдаём только после соединения с Cloudflare, иначе на нём Error 1033
    [ -n "$url" ] || url=$(grep -ohE 'https://[a-z0-9-]+\.trycloudflare\.com' "$STATE/cloudflared.log" | head -1 || true)
    [ -n "$url" ] && grep -q "Registered tunnel connection" "$STATE/cloudflared.log" && break
    kill -0 "${pids[1]}" 2>/dev/null || { echo "cloudflared завершился:"; tail -20 "$STATE/cloudflared.log"; exit 1; }
    sleep 1
  done
  grep -q "Registered tunnel connection" "$STATE/cloudflared.log" || {
    echo "туннель не соединился за 90 с:"; grep -iE "ERR|error|fail" "$STATE/cloudflared.log" | tail -8 || true
    echo "попробуйте CF_PROTOCOL=quic"; exit 1; }
  echo
  echo "  Открытое радио в интернете: $url"
fi
echo "  Локально: http://localhost:$port   (Ctrl+C — выключить)"
wait "${pids[0]}"
