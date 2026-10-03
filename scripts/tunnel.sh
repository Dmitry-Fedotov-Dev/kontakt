#!/usr/bin/env bash
# Cloudflare Tunnel для «Контакта».
#
#   ./scripts/tunnel.sh quick
#       Временный адрес https://<случайно>.trycloudflare.com → web (:8080). Аккаунт не нужен.
#       Подходит, чтобы проверить с телефона или дать ссылку другу. Адрес меняется при каждом запуске.
#
#   ./scripts/tunnel.sh publish kontakt.example.com
#       Постоянный адрес на своём домене (домен должен быть в Cloudflare). Один раз спросит вход в аккаунт.
#       Маршруты разводятся прямо в cloudflared, мимо web-прокси (это вдвое дешевле по CPU, см. LOAD_REPORT.md):
#         /sip, /api/*  → signal :8081
#         /media        → media  :8082
#         всё остальное → web    :8080
#
#   ./scripts/tunnel.sh stop kontakt
#       Удалить именованный туннель.
#
# Через туннель идёт только HTTP/WebSocket. SIP по UDP (софтфоны) туда не попадает — он только для локалки.
set -euo pipefail
cd "$(dirname "$0")/.."

WEB="${WEB:-http://127.0.0.1:8080}"
SIGNAL="${SIGNAL:-http://127.0.0.1:8081}"
MEDIA="${MEDIA:-http://127.0.0.1:8082}"
NAME="${NAME:-kontakt}"

need_cloudflared() {
  if command -v cloudflared >/dev/null; then return; fi
  echo "cloudflared не найден. Установка:"
  echo "  Debian/Ubuntu: curl -L https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64.deb -o cf.deb && sudo dpkg -i cf.deb"
  echo "  ARM (Raspberry Pi и т.п.): …/cloudflared-linux-arm64 (или -arm), положить в /usr/local/bin/cloudflared и chmod +x"
  echo "  macOS: brew install cloudflared"
  exit 1
}

check_running() {
  if ! curl -fsS "$WEB/healthz" >/dev/null 2>&1; then
    echo "Контакт не отвечает на $WEB — сначала запустите ./scripts/run.sh"
    exit 1
  fi
}

case "${1:-}" in
  quick)
    need_cloudflared; check_running
    echo "Поднимаю временный туннель к $WEB… Адрес появится ниже (строка с trycloudflare.com)."
    exec cloudflared tunnel --no-autoupdate --url "$WEB"
    ;;

  publish)
    HOST="${2:?укажите домен: ./scripts/tunnel.sh publish kontakt.example.com}"
    need_cloudflared; check_running
    [ -f "$HOME/.cloudflared/cert.pem" ] || cloudflared tunnel login
    cloudflared tunnel info "$NAME" >/dev/null 2>&1 || cloudflared tunnel create "$NAME"
    TID="$(cloudflared tunnel list -o json | python3 -c "import sys,json;print(next(t['id'] for t in json.load(sys.stdin) if t['name']=='$NAME'))")"
    CFG="$HOME/.cloudflared/$NAME.yml"
    cat >"$CFG" <<EOF
tunnel: $TID
credentials-file: $HOME/.cloudflared/$TID.json
ingress:
  - hostname: $HOST
    path: ^/sip$
    service: $SIGNAL
  - hostname: $HOST
    path: ^/api/
    service: $SIGNAL
  - hostname: $HOST
    path: ^/media$
    service: $MEDIA
  - hostname: $HOST
    service: $WEB
  - service: http_status:404
EOF
    cloudflared tunnel route dns "$NAME" "$HOST" || true
    echo "Конфиг: $CFG"
    echo "Публикую https://$HOST …"
    exec cloudflared tunnel --no-autoupdate --config "$CFG" run "$NAME"
    ;;

  stop)
    need_cloudflared
    cloudflared tunnel cleanup "${2:-$NAME}" || true
    cloudflared tunnel delete "${2:-$NAME}"
    ;;

  *)
    sed -n '2,22p' "$0"
    exit 1
    ;;
esac
