#!/usr/bin/env bash
# Поднять или остановить мониторинг, выбрав способ сам:
#   - docker  — docker-compose.yml, сеть хоста: Linux, VPS, docker прямо в WSL;
#   - desktop — docker-compose.desktop.yml для Docker Desktop (Windows, macOS): его «хост» —
#     своя виртуальная машина, поэтому обычная сеть Docker, порты на localhost, станция —
#     через host.docker.internal;
#   - local   — без Docker (local.sh): если Docker нет вовсе.
#
#   monitoring/stack.sh up | down
#   monitoring/stack.sh sees 127.0.0.1:27591   # видит ли Prometheus эту цель живой
#
# Выбрать явно: MONITORING=docker | desktop | local. Порты: GRAFANA_PORT, PROMETHEUS_PORT.
set -euo pipefail
cd "$(dirname "$0")/.."
MODEFILE=data/monitoring-mode
mkdir -p data

pick_mode() {
  case "${MONITORING:-}" in local | docker | desktop) echo "$MONITORING"; return ;; esac
  local os
  if command -v docker >/dev/null && os=$(timeout 10 docker info --format '{{.OperatingSystem}}' 2>/dev/null); then
    if grep -qi "docker desktop" <<<"$os"; then echo desktop; else echo docker; fi
  else
    echo local
  fi
}

compose_file() { [ "$1" = desktop ] && echo monitoring/docker-compose.desktop.yml || echo monitoring/docker-compose.yml; }

# Конфиг Prometheus для Docker Desktop — тот же prometheus.yml, только каждое задание
# переписывает 127.0.0.1 на host.docker.internal: изнутри контейнера 127.0.0.1 — сам контейнер.
desktop_config() {
  awk '{ print }
    /^  - job_name:/ {
      print "    relabel_configs:"
      print "      - source_labels: [__address__]"
      print "        regex: \x27127\\.0\\.0\\.1:(.*)\x27"
      print "        target_label: __address__"
      print "        replacement: \x27host.docker.internal:$1\x27"
    }' monitoring/prometheus/prometheus.yml >data/prometheus.desktop.yml
}

case "${1:-}" in
  up)
    mode=$(pick_mode)
    echo "$mode" >"$MODEFILE"
    if [ "$mode" != local ]; then
      [ "$mode" = desktop ] && { desktop_config; echo "  Docker Desktop: обычная сеть Docker, станция — через host.docker.internal"; }
      echo "  Docker (первый раз скачиваются образы — пара минут)…"
      docker compose -f "$(compose_file "$mode")" up -d 2>&1 | sed 's/^/  /'
      gport=${GRAFANA_PORT:-3002}
      [ "$mode" = desktop ] && gport=3000 # внутри контейнера; наружу — GRAFANA_PORT
      for _ in $(seq 60); do
        docker exec kontakt-monitoring-grafana-1 wget -qO- "http://127.0.0.1:$gport/api/health" >/dev/null 2>&1 && break
        sleep 2
      done
      echo "  Grafana: http://localhost:${GRAFANA_PORT:-3002}"
    else
      echo "  без Docker: Prometheus и Grafana — обычные программы на этой машине"
      monitoring/local.sh up
    fi ;;
  down)
    mode=$(cat "$MODEFILE" 2>/dev/null || pick_mode)
    if [ "$mode" != local ]; then
      docker compose -f "$(compose_file "$mode")" down >/dev/null 2>&1 || true
    else
      monitoring/local.sh down >/dev/null || true
    fi
    rm -f "$MODEFILE" ;;
  sees)
    target=$2
    mode=$(cat "$MODEFILE" 2>/dev/null || pick_mode)
    pport=${PROMETHEUS_PORT:-9092}
    [ "$mode" = desktop ] && pport=9090 # спрашиваем изнутри контейнера
    url="http://127.0.0.1:$pport/api/v1/targets?state=active"
    port=${target##*:}
    for _ in $(seq 15); do
      if [ "$mode" != local ]; then
        body=$(docker exec kontakt-monitoring-prometheus-1 wget -qO- "$url" 2>/dev/null || true)
      else
        body=$(curl -fsS "$url" 2>/dev/null || true)
      fi
      # в Docker Desktop адрес цели уже переписан на host.docker.internal — сверяем по порту
      grep -Eq "\"scrapeUrl\":\"http://(127\.0\.0\.1|host\.docker\.internal):$port/metrics\"[^}]*\"health\":\"up\"" <<<"$body" && exit 0
      sleep 2
    done
    exit 1 ;;
  *) echo "использование: $0 up | down | sees host:port"; exit 1 ;;
esac
