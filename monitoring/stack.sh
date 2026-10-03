#!/usr/bin/env bash
# Поднять или остановить мониторинг, выбрав способ сам:
#   - Docker (docker-compose.yml) — там, где у контейнеров общая со станцией сеть: Linux,
#     docker, установленный прямо в WSL;
#   - без Docker (local.sh) — с Docker Desktop (Windows, macOS: его «хост» — своя виртуальная
#     машина, и ни браузер, ни станция до стека не достают) и там, где Docker нет вовсе.
#
#   monitoring/stack.sh up | down
#   monitoring/stack.sh sees 127.0.0.1:27591   # видит ли Prometheus эту цель живой
#
# Выбрать явно: MONITORING=local или MONITORING=docker. Порты: GRAFANA_PORT, PROMETHEUS_PORT.
set -euo pipefail
cd "$(dirname "$0")/.."
MODEFILE=data/monitoring-mode
mkdir -p data

pick_mode() {
  case "${MONITORING:-}" in local | docker) echo "$MONITORING"; return ;; esac
  local os
  if command -v docker >/dev/null && os=$(timeout 10 docker info --format '{{.OperatingSystem}}' 2>/dev/null) \
    && ! grep -qi "docker desktop" <<<"$os"; then
    echo docker
  else
    echo local
  fi
}

case "${1:-}" in
  up)
    mode=$(pick_mode)
    echo "$mode" >"$MODEFILE"
    if [ "$mode" = docker ]; then
      echo "  Docker (первый раз скачиваются образы — пара минут)…"
      docker compose -f monitoring/docker-compose.yml up -d 2>&1 | sed 's/^/  /'
      for _ in $(seq 60); do
        docker exec kontakt-monitoring-grafana-1 wget -qO- "http://127.0.0.1:${GRAFANA_PORT:-3002}/api/health" >/dev/null 2>&1 && break
        sleep 2
      done
      echo "  Grafana: http://localhost:${GRAFANA_PORT:-3002}"
    else
      echo "  без Docker: Prometheus и Grafana — обычные программы на этой машине"
      monitoring/local.sh up
    fi ;;
  down)
    mode=$(cat "$MODEFILE" 2>/dev/null || pick_mode)
    if [ "$mode" = docker ]; then
      docker compose -f monitoring/docker-compose.yml down >/dev/null 2>&1 || true
    else
      monitoring/local.sh down >/dev/null || true
    fi
    rm -f "$MODEFILE" ;;
  sees)
    target=$2
    url="http://127.0.0.1:${PROMETHEUS_PORT:-9092}/api/v1/targets?state=active"
    mode=$(cat "$MODEFILE" 2>/dev/null || pick_mode)
    for _ in $(seq 15); do
      if [ "$mode" = docker ]; then
        body=$(docker exec kontakt-monitoring-prometheus-1 wget -qO- "$url" 2>/dev/null || true)
      else
        body=$(curl -fsS "$url" 2>/dev/null || true)
      fi
      grep -q "\"scrapeUrl\":\"http://$target/metrics\"[^}]*\"health\":\"up\"" <<<"$body" && exit 0
      sleep 2
    done
    exit 1 ;;
  *) echo "использование: $0 up | down | sees host:port"; exit 1 ;;
esac
