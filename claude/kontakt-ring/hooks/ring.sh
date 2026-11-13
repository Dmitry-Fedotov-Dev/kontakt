#!/bin/sh
# Хук Claude Code → звонок на трубку «Контакта» (и/или SIP-устройство). Ноль токенов: хук — команда,
# модель не вызывается. Событие — JSON на stdin (Stop — «готово», Notification — просит разрешения).
#
# Куда звонить — из окружения (settings.json → "env") или файла ~/.config/kontakt/ring.env:
#   KONTAKT_LINE=https://openline.one/?call=<линия>   ваша ссылка из трубки (кнопка «позвонить»)
#   KONTAKT_SIP=sip:100@192.168.1.20                  SIP-устройство напрямую (нужен kontakt-ring)
#
#   sh ring.sh setup https://openline.one/?call=<линия>   — записать ссылку в ~/.config/kontakt/ring.env
#   sh ring.sh test                                       — позвонить «проверка связи»
#
# Есть kontakt-ring в PATH (go install kontakt/cmd/kontakt-ring или релиз) — звонит он; нет — curl
# (только линия). Звонок уходит в фоне: Claude Code не ждёт. Выход всегда 0 — хук ничему не мешает.

CONF="${XDG_CONFIG_HOME:-$HOME/.config}/kontakt/ring.env"

if [ "$1" = setup ]; then
  [ -n "$2" ] || { echo "sh ring.sh setup https://openline.one/?call=<линия>" >&2; exit 0; }
  mkdir -p "$(dirname "$CONF")"
  printf 'KONTAKT_LINE=%s\n' "$2" > "$CONF"
  echo "линия записана в $CONF"
  exit 0
fi

if [ -z "$KONTAKT_LINE$KONTAKT_SIP" ] && [ -f "$CONF" ]; then
  set -a; . "$CONF"; set +a
fi
[ -n "$KONTAKT_LINE$KONTAKT_SIP" ] || exit 0 # не настроено — молчим

if [ "$1" = test ]; then
  ev='{"hook_event_name":"Notification","message":"проверка связи"}'
else
  ev=$(cat)
fi

bin=$(command -v kontakt-ring 2>/dev/null)
if [ -n "$bin" ]; then
  (printf '%s' "$ev" | "$bin" -hook >/dev/null 2>&1 &)
  exit 0
fi
[ -n "$KONTAKT_LINE" ] && command -v curl >/dev/null 2>&1 || exit 0

# Без kontakt-ring: поля события — sed'ом (jq есть не везде), JSON-экранирование снимаем простое.
field() { printf '%s' "$ev" | tr '\n' ' ' | sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\"\(\([^\"\\\\]\|\\\\.\)*\)\".*/\1/p" | sed 's/\\"/"/g; s/\\n/ /g; s/\\\\/\\/g'; }
case "$(field hook_event_name)" in
  Stop | SubagentStop)
    case "$ev" in *'"stop_hook_active":true'* | *'"stop_hook_active": true'*) exit 0 ;; esac
    cwd=$(field cwd | sed 's#\\\\#/#g; s#\\#/#g')
    text="готово${cwd:+ — ${cwd##*/}}" ;;
  Notification)
    text=$(field message)
    case "$text$(field notification_type)" in *"waiting for your input"* | *idle_prompt*) exit 0 ;; esac
    [ -n "$text" ] || text="ждёт вас" ;;
  *) exit 0 ;;
esac

base="${KONTAKT_URL:-}"
case "$KONTAKT_LINE" in
  http://* | https://*) [ -n "$base" ] || base="${KONTAKT_LINE%%\?*}" ;;
esac
base="${base:-https://openline.one}"
base="${base%/}"
(curl -fsS -m 10 -A kontakt-ring "$base/api/ring" --data-urlencode "line=$KONTAKT_LINE" \
  --data-urlencode "from=${KONTAKT_FROM:-Claude Code}" --data-urlencode "text=$text" >/dev/null 2>&1 &)
exit 0
