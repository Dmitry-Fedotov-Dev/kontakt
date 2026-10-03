#!/usr/bin/env bash
# Prometheus на этом компьютере будет забирать метрики радио, запущенного на другой машине
# (например, на телефоне в Termux) — через его адрес туннеля.
#
#   monitoring/radio-target.sh https://abc-def.trycloudflare.com    # добавить или заменить
#   monitoring/radio-target.sh off                                  # убрать
#
# Адрес trycloudflare меняется при перезапуске туннеля — тогда запустите снова с новым.
# Метрики отдаются на /metrics самой страницы радио; это только счётчики (станции, слушатели,
# байты) — ни адресов, ни названий. Если радио запущено с -admin, через туннель их не видно.
set -euo pipefail
cd "$(dirname "$0")"
file=prometheus/targets/radio-remote.json
case "${1:-}" in
  off) rm -f "$file"; echo "убрано: $file"; exit 0 ;;
  https://* | http://*) ;;
  *) echo "использование: $0 https://….trycloudflare.com | off"; exit 1 ;;
esac
scheme=${1%%://*}
host=${1#*://}
host=${host%%/*}
[[ "$host" =~ ^[A-Za-z0-9.-]+(:[0-9]+)?$ ]] || { echo "не похоже на адрес: $host"; exit 1; }
[[ "$host" == *:* ]] || host="$host:$([ "$scheme" = https ] && echo 443 || echo 80)"
printf '[{"targets":["%s"],"labels":{"__scheme__":"%s","instance":"radio-remote"}}]\n' "$host" "$scheme" >"$file"
echo "Prometheus подхватит за ~10 с: $file → $scheme://$host/metrics"
command -v curl >/dev/null && { curl -fsS -m 10 "$scheme://$host/metrics" | grep -q '^kontakt_radio_stations' \
  && echo "радио отвечает, метрики есть" || echo "внимание: $scheme://$host/metrics не ответил метриками радио"; }
