#!/usr/bin/env bash
# Prometheus и Grafana БЕЗ Docker — обычными программами на этой машине.
#
#   monitoring/local.sh up        # скачать (один раз), запустить; адрес Grafana — в выводе
#   monitoring/local.sh down      # остановить
#   monitoring/local.sh status
#
# Зачем: стек в docker-compose живёт в сети хоста, а у Docker Desktop (Windows, macOS)
# «хост» — его собственная виртуальная машина. Grafana там слушает порт, до которого не
# достаёт браузер, а Prometheus не видит станцию в WSL. Без Docker всё в одной сети: станция,
# Prometheus и Grafana на 127.0.0.1 в WSL, а Windows открывает их через localhost, как и
# любой другой порт WSL.
#
# Те же конфиги, правила, тревоги и дашборды, что и в docker-compose: пути подставляются.
# Порты: PROMETHEUS_PORT (9092) и GRAFANA_PORT (3002). Программы и данные — в
# ~/.cache/kontakt-monitoring: база Prometheus на диске Windows (/mnt/…) работала бы медленно
# и ненадёжно.
set -euo pipefail
cd "$(dirname "$0")/.."
ROOT=$PWD

PROM_V=3.5.0     # те же версии, что в docker-compose.yml
GRAFANA_V=11.5.2
CACHE=${KONTAKT_MONITORING_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/kontakt-monitoring}
RUN=data/monitoring-local
export PROMETHEUS_PORT=${PROMETHEUS_PORT:-9092}
export GRAFANA_PORT=${GRAFANA_PORT:-3002}
mkdir -p "$CACHE" "$RUN"

alive() { [ -f "$RUN/$1.pid" ] && kill -0 "$(cat "$RUN/$1.pid")" 2>/dev/null; }

case "${1:-up}" in
  down)
    for p in grafana prometheus; do
      if alive $p; then
        pid=$(cat "$RUN/$p.pid")
        kill "$pid"
        for _ in $(seq 20); do kill -0 "$pid" 2>/dev/null || break; sleep 0.5; done # Prometheus дописывает базу
        echo "остановлен $p"
      fi
      rm -f "$RUN/$p.pid"
    done
    exit 0 ;;
  status)
    for p in prometheus grafana; do
      if alive $p; then echo "$p: работает ($(cat "$RUN/$p.port"))"; else echo "$p: не запущен"; fi
    done
    exit 0 ;;
  up) ;;
  *) echo "использование: $0 up | down | status"; exit 1 ;;
esac

case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  armv7l) arch=armv7 ;;
  *) echo "архитектура $(uname -m) не поддерживается"; exit 1 ;;
esac

# fetch URL ФАЙЛ SHA256 — скачать один раз и сверить контрольную сумму с опубликованной.
fetch() {
  local url=$1 out=$2 sum=$3
  if [ ! -s "$out" ]; then
    echo "  скачиваю $(basename "$out") (один раз)…"
    curl -fL --retry 3 --progress-bar -o "$out.part" "$url"
    mv "$out.part" "$out"
  fi
  if [ -n "$sum" ] && ! echo "$sum  $out" | sha256sum -c --quiet - 2>/dev/null; then
    echo "контрольная сумма $(basename "$out") не совпала — файл удалён, запустите ещё раз"; rm -f "$out"; exit 1
  fi
}

# --- Prometheus ---
pdir=$CACHE/prometheus-$PROM_V.linux-$arch
if [ ! -x "$pdir/prometheus" ]; then
  base=https://github.com/prometheus/prometheus/releases/download/v$PROM_V
  tgz=prometheus-$PROM_V.linux-$arch.tar.gz
  sum=$(curl -fsSL "$base/sha256sums.txt" | awk -v f="$tgz" '$2 == f { print $1 }')
  [ -n "$sum" ] || { echo "не нашёл контрольную сумму $tgz"; exit 1; }
  fetch "$base/$tgz" "$CACHE/$tgz" "$sum"
  tar -xzf "$CACHE/$tgz" -C "$CACHE"
fi

# --- Grafana ---
gdir=$(ls -d "$CACHE"/grafana-v$GRAFANA_V "$CACHE"/grafana-$GRAFANA_V 2>/dev/null | head -1 || true)
if [ -z "$gdir" ] || [ ! -x "$gdir/bin/grafana" ]; then
  url=https://dl.grafana.com/oss/release/grafana-$GRAFANA_V.linux-$arch.tar.gz
  sum=$(curl -fsSL "$url.sha256" | awk '{ print $1 }')
  [ -n "$sum" ] || { echo "не нашёл контрольную сумму Grafana"; exit 1; }
  fetch "$url" "$CACHE/grafana-$GRAFANA_V.linux-$arch.tar.gz" "$sum"
  tar -xzf "$CACHE/grafana-$GRAFANA_V.linux-$arch.tar.gz" -C "$CACHE"
  gdir=$(ls -d "$CACHE"/grafana-v$GRAFANA_V "$CACHE"/grafana-$GRAFANA_V 2>/dev/null | head -1)
fi

# --- конфиги: те же файлы, пути из контейнера заменены на пути в репозитории ---
sed "s#/etc/prometheus/#$ROOT/monitoring/prometheus/#g" monitoring/prometheus/prometheus.yml >"$RUN/prometheus.yml"
rm -rf "$RUN/provisioning"
cp -r monitoring/grafana/provisioning "$RUN/provisioning"
sed -i "s#/var/lib/grafana/dashboards#$ROOT/monitoring/grafana/dashboards#g" "$RUN/provisioning/dashboards/"*.yml

if alive prometheus; then
  echo "  Prometheus уже запущен ($(cat "$RUN/prometheus.port"))"
else
  nohup "$pdir/prometheus" --config.file="$RUN/prometheus.yml" \
    --storage.tsdb.path="$CACHE/prometheus-data" --storage.tsdb.retention.time=30d \
    --web.listen-address="127.0.0.1:$PROMETHEUS_PORT" \
    --web.enable-remote-write-receiver --enable-feature=native-histograms \
    >"$RUN/prometheus.log" 2>&1 &
  echo $! >"$RUN/prometheus.pid"
  echo "$PROMETHEUS_PORT" >"$RUN/prometheus.port"
fi

export PROMETHEUS_PORT=$(cat "$RUN/prometheus.port") # источник данных Grafana — на тот Prometheus, что запущен
if alive grafana; then
  echo "  Grafana уже запущена ($(cat "$RUN/grafana.port"))"
else
  (
    cd "$gdir"
    GF_PATHS_PROVISIONING="$ROOT/$RUN/provisioning" GF_PATHS_DATA="$CACHE/grafana-data" \
    GF_PATHS_LOGS="$CACHE/grafana-logs" GF_PATHS_PLUGINS="$CACHE/grafana-plugins" \
    GF_SERVER_HTTP_ADDR=127.0.0.1 GF_SERVER_HTTP_PORT="$GRAFANA_PORT" \
    GF_AUTH_ANONYMOUS_ENABLED=true GF_AUTH_ANONYMOUS_ORG_ROLE=Admin GF_AUTH_DISABLE_LOGIN_FORM=true \
    GF_ANALYTICS_REPORTING_ENABLED=false GF_ANALYTICS_CHECK_FOR_UPDATES=false \
    GF_DASHBOARDS_DEFAULT_HOME_DASHBOARD_PATH="$ROOT/monitoring/grafana/dashboards/kontakt.json" \
      nohup ./bin/grafana server --homepath "$gdir" >"$ROOT/$RUN/grafana.log" 2>&1 &
    echo $! >"$ROOT/$RUN/grafana.pid"
  )
  echo "$GRAFANA_PORT" >"$RUN/grafana.port"
fi

# Готовность: Grafana при первом запуске создаёт базу — до минуты.
ok=""
for _ in $(seq 90); do
  curl -fsS "http://127.0.0.1:$(cat "$RUN/grafana.port")/api/health" >/dev/null 2>&1 \
    && curl -fsS "http://127.0.0.1:$(cat "$RUN/prometheus.port")/-/ready" >/dev/null 2>&1 && { ok=1; break; }
  alive prometheus || { echo "Prometheus не запустился:"; tail -5 "$RUN/prometheus.log"; exit 1; }
  alive grafana || { echo "Grafana не запустилась:"; tail -5 "$RUN/grafana.log"; exit 1; }
  sleep 1
done
[ -n "$ok" ] || { echo "мониторинг не ответил за 90 с — журналы в $RUN/"; exit 1; }
echo "  Prometheus: http://localhost:$(cat "$RUN/prometheus.port")/targets"
echo "  Grafana:    http://localhost:$(cat "$RUN/grafana.port")"
