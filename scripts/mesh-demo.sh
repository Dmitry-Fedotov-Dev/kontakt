#!/usr/bin/env bash
# Демо Kontakt Mesh на одной машине: Master + 5 Worker'ов с именами. У части узлов мало
# слотов соседей, поэтому сетка неполная и на графе видны ретрансляторы.
#
#   ./scripts/mesh-demo.sh
#   docker compose -f monitoring/docker-compose.yml up -d   # дашборд «Kontakt Mesh»: http://localhost:3002
#
# Проверить руками: закройте Worker (kill <pid> из вывода) — через ~15 с он станет
# OFFLINE (красный), соседи перестроят маршруты; закройте Master — связи Worker'ов
# останутся. Админка узлов: 127.0.0.1:7401 (Master), 7421…7425 (Worker'ы).
set -euo pipefail
cd "$(dirname "$0")/.."
export KONTAKT_JOIN_TOKEN="${KONTAKT_JOIN_TOKEN:-demo}"
DIR=data/mesh-demo
mkdir -p bin "$DIR"
CGO_ENABLED=0 go build -trimpath -o bin/ ./cmd/kontakt-node

pids=()
cleanup() { kill "${pids[@]}" 2>/dev/null || true; wait 2>/dev/null || true; }
trap cleanup EXIT INT TERM

./bin/kontakt-node -role master -name master -identity "$DIR/master.key" -listen 127.0.0.1:7400 -admin 127.0.0.1:7401 &
pids+=($!)
sleep 1
MK=$(./bin/kontakt-node -identity "$DIR/master.key" -print-id | cut -d' ' -f2)

names=(vps-almaty home-router office phone-termux vps-bishkek)
maxpeers=(3 2 3 2 3)
capacity=(100 20 50 0 100) # Мбит/с по замеру канала; 0 — неизвестна (на графе — NaN, а не «0 свободно»)
for i in "${!names[@]}"; do
  n=$((i + 1))
  ./bin/kontakt-node -role worker -name "${names[$i]}" -identity "$DIR/w$n.key" \
    -listen "127.0.0.1:741$n" -admin "127.0.0.1:742$n" -master 127.0.0.1:7400 -master-key "$MK" \
    -max-peers "${maxpeers[$i]}" -capacity-mbps "${capacity[$i]}" &
  pids+=($!)
  echo "  ${names[$i]}: pid $!, админка 127.0.0.1:742$n"
  sleep 0.3
done
echo
echo "Mesh запущен. Граф: дашборд «Kontakt Mesh» в Grafana; состояние: curl -s 127.0.0.1:7401/mesh/status"
wait # все узлы: отказ одного (Master, Worker) — сценарий проверки, а не повод гасить остальные
