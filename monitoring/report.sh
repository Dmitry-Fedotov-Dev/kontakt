#!/usr/bin/env bash
# Сохраняет весь дашборд «Контакта» за один прогон одним высоким PNG: отчёт,
# который прикладывают к прогону CI, задаче или статье.
#
#   monitoring/report.sh <testid> <from> <to> <out.png> [grafana-url]
#
# from и to — Unix-секунды; берите запас ~30 с вокруг прогона.
# Нужен Chrome или Chromium (CHROME=путь, если его нет в PATH).
set -euo pipefail

testid=$1 from=$2 to=$3 out=$4 grafana=${5:-http://127.0.0.1:${GRAFANA_PORT:-3002}}
chrome=${CHROME:-$(command -v google-chrome || command -v chromium || command -v chromium-browser || true)}
if [ -z "$chrome" ]; then
  echo "Chrome или Chromium не найден; задайте CHROME" >&2
  exit 1
fi

# Высота страницы — из раскладки дашборда: ячейка сетки 30 px плюс отступ 8 px;
# безголовый Chrome рисует только то, что влезло в окно.
dashboard="$(dirname "$0")/grafana/dashboards/kontakt.json"
height=$(python3 -c 'import json,sys
d = json.load(open(sys.argv[1], encoding="utf-8"))
print(max(p["gridPos"]["y"] + p["gridPos"]["h"] for p in d["panels"]) * 38 + 80)' "$dashboard")

url="$grafana/d/kontakt/kontakt?orgId=1&kiosk&theme=dark&var-testid=$testid&var-window=30s&from=${from}000&to=${to}000"
"$chrome" --headless=new --disable-gpu --hide-scrollbars --no-sandbox --window-size="1600,$height" \
  --run-all-compositor-stages-before-draw --virtual-time-budget=60000 \
  --screenshot="$out" "$url" 2>/dev/null
echo "дашборд прогона $testid ($(date -u -d "@$from" +%H:%M:%S)–$(date -u -d "@$to" +%H:%M:%S) UTC) сохранён в $out"
