#!/usr/bin/env bash
# Выкладка «Контакта» на свой сервер (Ubuntu 24.04 / Debian 12) по SSH. Запускать с машины
# разработчика (WSL, Linux, macOS): собирает здесь, на сервер везёт готовые бинарники.
#
#   scripts/deploy.sh setup  root@1.2.3.4            # один раз: пользователь kontakt, пакеты, ufw,
#                                                    # Docker для мониторинга, cloudflared, юниты
#   scripts/deploy.sh harden root@1.2.3.4            # вход по SSH только по ключу (после того как ключ работает)
#   scripts/deploy.sh env    root@1.2.3.4            # токен бота: локальный .env → /opt/kontakt/.env
#   scripts/deploy.sh push   root@1.2.3.4            # новая версия рядом со старой, переключение,
#                                                    # перезапуск; не поднялась — сам откатывает
#   scripts/deploy.sh rollback root@1.2.3.4          # вернуть предыдущую версию
#   scripts/deploy.sh https  root@1.2.3.4 openline.one   # прямой HTTPS (Caddy, Let's Encrypt), без Cloudflare;
#                                                    # открывает 80/443; DNS домена уже должен вести на сервер
#   scripts/deploy.sh tunnel root@1.2.3.4 kontakt.example.com   # или туннель Cloudflare на свой домен
#   scripts/deploy.sh status root@1.2.3.4            # сервисы, версия, ответ станции
#   scripts/deploy.sh backup root@1.2.3.4            # забрать свежий бэкап data/ к себе (data/backups/)
#
# На сервере: /opt/kontakt/releases/<версия>/ (bin, monitoring), current → рабочая версия;
# monitoring/ — копия конфига мониторинга текущей версии (её монтирует docker compose);
# config/, data/ (база модерации), .env, cloudflared/ — общие для всех версий. Наружу открыт только
# SSH: звонки и радио идут через туннель, который сервер сам открывает к Cloudflare.
# Grafana — через SSH: ssh -L 3002:127.0.0.1:3002 root@1.2.3.4 → http://localhost:3002
#
# Идущие звонки при push оборвутся (сервисы перезапускаются), ведущий и приёмники радио
# переподключатся сами.
set -euo pipefail
cd "$(dirname "$0")/.."

cmd=${1:-}
host=${2:-}
[ -n "$cmd" ] && [ -n "$host" ] || { sed -n '2,22p' "$0"; exit 1; }

# ssh с проверкой ключа хоста при первом подключении (accept-new) и без пароля;
# DEPLOY_SSH_OPTS="-p 2222 -i ~/.ssh/kontakt" — нестандартный порт или ключ
read -ra extra <<<"${DEPLOY_SSH_OPTS:-}"
SSH=(ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 "${extra[@]}" "$host")
remote() { "${SSH[@]}" "$@"; }
# root — как есть, иначе через sudo
sudo_remote() { "${SSH[@]}" 'if [ "$(id -u)" = 0 ]; then bash -s; else sudo bash -s; fi'; }

SERVICES=(kontakt-media kontakt-signal kontakt-radio kontakt-web kontakt-notify)

case "$cmd" in
setup)
  echo "Настраиваю $host…"
  { cat <<'EOS'
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
apt-get update -q
# compose: в Ubuntu — docker-compose-v2, в Debian — docker-compose (тоже v2)
compose=docker-compose-v2
apt-cache policy docker-compose-v2 2>/dev/null | grep -q 'Candidate: [0-9]' || compose=docker-compose
apt-get install -yq curl rsync ufw unattended-upgrades ffmpeg docker.io "$compose" >/dev/null # ffmpeg — станции с сервера
if ! command -v cloudflared >/dev/null; then
  arch=$(dpkg --print-architecture)
  curl -fsSL "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-$arch.deb" -o /tmp/cf.deb
  dpkg -i /tmp/cf.deb >/dev/null && rm -f /tmp/cf.deb
fi
id kontakt >/dev/null 2>&1 || useradd --system --home /opt/kontakt --shell /usr/sbin/nologin kontakt
mkdir -p /opt/kontakt/releases /opt/kontakt/config /opt/kontakt/data /opt/kontakt/backups /opt/kontakt/cloudflared
chown -R kontakt:kontakt /opt/kontakt/data /opt/kontakt/backups /opt/kontakt/cloudflared
# наружу — только SSH: туннель исходящий, входящие порты не нужны
ufw allow OpenSSH >/dev/null && ufw --force enable >/dev/null || echo "ВНИМАНИЕ: ufw не включился — проверьте фаервол вручную"
systemctl enable --now unattended-upgrades >/dev/null 2>&1 || true
systemctl enable --now docker >/dev/null 2>&1 || echo "ВНИМАНИЕ: Docker не запустился — мониторинга не будет"
echo "пакеты, пользователь kontakt, ufw — готово"
EOS
  } | sudo_remote
  echo "Юниты…"
  tar -C deploy -czf - . | remote 'cat > /tmp/kontakt-deploy.tgz'
  sudo_remote <<'EOS'
set -euo pipefail
mkdir -p /tmp/kontakt-deploy && tar -xzf /tmp/kontakt-deploy.tgz -C /tmp/kontakt-deploy
install -m 644 /tmp/kontakt-deploy/*.service /tmp/kontakt-deploy/*.target /tmp/kontakt-deploy/*.timer /etc/systemd/system/
rm -rf /tmp/kontakt-deploy /tmp/kontakt-deploy.tgz
systemctl daemon-reload
systemctl enable kontakt.target kontakt-backup.timer >/dev/null
systemctl start kontakt-backup.timer
echo "юниты установлены"
EOS
  echo "Готово. Дальше: scripts/deploy.sh push $host"
  ;;

harden)
  # Вход по SSH — только по ключу (root тоже). Скрипт сам вошёл по ключу — значит, доступ не пропадёт;
  # на крайний случай остаётся консоль в панели хостинга.
  sudo_remote <<'EOS'
set -euo pipefail
cat > /etc/ssh/sshd_config.d/10-kontakt.conf <<CFG
# scripts/deploy.sh harden: только ключи
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin prohibit-password
CFG
# в sshd_config первое значение побеждает: прямое «PasswordAuthentication yes» в нём перекрыло бы файл
sed -i -E 's/^(PasswordAuthentication|PermitRootLogin|KbdInteractiveAuthentication)\b/#&/' /etc/ssh/sshd_config
sshd -t
systemctl reload ssh 2>/dev/null || systemctl reload sshd
sshd -T | grep -E '^(passwordauthentication|permitrootlogin|kbdinteractiveauthentication) '
EOS
  ;;

env)
  [ -f .env ] || { echo "нет .env в корне репозитория (TELEGRAM_BOT_TOKEN, TELEGRAM_CHAT_ID)"; exit 1; }
  remote 'umask 077; cat > /tmp/kontakt.env' <.env
  sudo_remote <<'EOS'
install -m 600 -o kontakt -g kontakt /tmp/kontakt.env /opt/kontakt/.env && rm -f /tmp/kontakt.env
systemctl restart kontakt-notify 2>/dev/null || true
echo ".env на месте (600, kontakt), бот перезапущен"
EOS
  ;;

push)
  ver="$(date -u +%Y%m%d-%H%M%S)-$(git rev-parse --short HEAD)$(git diff --quiet || echo -dirty)"
  out="dist/$ver"
  echo "Сборка $ver (linux/amd64)…"
  rm -rf "$out" && mkdir -p "$out/bin"
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$out/bin/" \
    ./cmd/web ./cmd/signal ./cmd/media ./cmd/radio ./cmd/notify
  cp -r monitoring "$out/monitoring"
  cp -r deploy "$out/deploy" # юниты — вместе с версией: флаги сервисов меняются вместе с кодом
  rm -f "$out"/monitoring/prometheus/targets/*.json # цели стенда разработчика серверу не нужны
  # цели сервера: порты из юнитов deploy/
  printf '[{"targets":["127.0.0.1:8091"]}]\n'>"$out/monitoring/prometheus/targets/signal-server.json"
  printf '[{"targets":["127.0.0.1:8082"]}]\n' >"$out/monitoring/prometheus/targets/media-server.json"
  printf '[{"targets":["127.0.0.1:8093"]}]\n' >"$out/monitoring/prometheus/targets/radio-server.json"
  printf '[{"targets":["127.0.0.1:20241"]}]\n' >"$out/monitoring/prometheus/targets/cloudflared-server.json"
  echo "Отправляю…"
  tar -C dist -czf - "$ver" | remote "cat > /tmp/kontakt-$ver.tgz"
  # конфиг поведения — только если на сервере его ещё нет: там он правится на месте
  [ -f config/kontakt.json ] && remote 'cat > /tmp/kontakt-config.json' <config/kontakt.json
  sudo_remote <<EOS
set -euo pipefail
R=/opt/kontakt
tar --no-same-owner -xzf /tmp/kontakt-$ver.tgz -C \$R/releases && rm -f /tmp/kontakt-$ver.tgz
# собрано на Windows — бит исполнения в архив не попадает; права выставляем сами
chmod -R u=rwX,go=rX \$R/releases/$ver && chmod 755 \$R/releases/$ver/bin/*
[ -f \$R/config/kontakt.json ] || install -m 644 /tmp/kontakt-config.json \$R/config/kontakt.json
rm -f /tmp/kontakt-config.json
prev=\$(readlink \$R/current || true)
ln -sfn \$R/releases/$ver \$R/current.new && mv -T \$R/current.new \$R/current
install -m 644 \$R/current/deploy/*.service \$R/current/deploy/*.target \$R/current/deploy/*.timer /etc/systemd/system/
systemctl daemon-reload
systemctl restart ${SERVICES[*]} 2>/dev/null || systemctl restart kontakt.target
ok=0
for i in \$(seq 30); do curl -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1 && { ok=1; break; }; sleep 1; done
if [ \$ok != 1 ]; then
  echo "версия $ver не поднялась за 30 с — откатываю на \$prev"
  journalctl -u kontakt-web -u kontakt-signal -u kontakt-media -n 20 --no-pager || true
  [ -n "\$prev" ] && ln -sfn "\$prev" \$R/current.new && mv -T \$R/current.new \$R/current && systemctl restart ${SERVICES[*]}
  exit 1
fi
# мониторинг: конфиг и дашборды текущей версии — в постоянную папку \$R/monitoring, на месте
# (--inplace: смонтированный в контейнер файл должен остаться тем же). Из current/ монтировать
# нельзя: Docker раскрывает ссылку при создании контейнера, и после чистки старых версий
# Prometheus терял цели — графики обрывались. Стек поднимается один раз и живёт сам.
dc="docker compose"; docker compose version >/dev/null 2>&1 || dc=docker-compose
mkdir -p \$R/monitoring && rsync -a --delete --inplace \$R/current/monitoring/ \$R/monitoring/
cd \$R/monitoring && \$dc -f docker-compose.yml --profile linux-host up -d --remove-orphans >/dev/null 2>&1 \
  && docker kill -s HUP kontakt-monitoring-prometheus-1 >/dev/null 2>&1 || echo "мониторинг не поднялся — docker compose up вручную"
# храним 5 последних версий
ls -1dt \$R/releases/*/ | tail -n +6 | xargs -r rm -rf
echo "версия $ver работает (было: \${prev##*/})"
EOS
  rm -rf "$out"
  ;;

rollback)
  sudo_remote <<EOS
set -euo pipefail
R=/opt/kontakt
cur=\$(readlink \$R/current)
prev=\$(ls -1dt \$R/releases/*/ | sed 's#/\$##' | grep -vx "\$cur" | head -1)
[ -n "\$prev" ] || { echo "откатываться некуда — версия одна"; exit 1; }
ln -sfn "\$prev" \$R/current.new && mv -T \$R/current.new \$R/current
systemctl restart ${SERVICES[*]}
echo "откат: \${cur##*/} → \${prev##*/}"
EOS
  ;;

https)
  # Прямой HTTPS на сервере, без Cloudflare: Caddy сам получает и продлевает сертификат Let's
  # Encrypt. DNS домена (A-записи домена и www) должен уже указывать на сервер.
  domain=${3:?укажите домен: scripts/deploy.sh https $host openline.one}
  sudo_remote <<EOS
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
command -v caddy >/dev/null || { apt-get update -q >/dev/null; apt-get install -yq caddy >/dev/null; }
cat > /etc/caddy/Caddyfile <<'CFG'
# scripts/deploy.sh https — разводка путей как у туннеля: /sip, /api → signal, /media → media,
# сокеты радио /radio/ws/* → радио; остальное (и страницы /radio/) → web. Журнал запросов Caddy
# не ведёт: IP посетителей никуда не пишутся.
# Без HTTP/3: UDP 443 — эфир радио по WebRTC (docs/UDP_BROADCAST.md); QUIC к серверу из РФ и так режется.
{
	servers {
		protocols h1 h2
	}
}
$domain {
	@signal path /sip /api/*
	reverse_proxy @signal 127.0.0.1:8081
	@media path /media
	reverse_proxy @media 127.0.0.1:8082
	@radiows path /radio/ws/*
	reverse_proxy @radiows 127.0.0.1:8083
	reverse_proxy 127.0.0.1:8080
	header {
		Strict-Transport-Security "max-age=31536000"
		-Server
	}
}
www.$domain {
	redir https://$domain{uri} permanent
}
CFG
caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null 2>&1 || { caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile; exit 1; }
ufw allow 80/tcp >/dev/null && ufw allow 443/tcp >/dev/null && ufw allow 443/udp >/dev/null
# Прямой эфир радио (порт 8443, мимо Caddy): копия сертификата Caddy для радио — при каждом
# продлении (path-юнит следит за файлом), владелец root, читать может группа kontakt.
CERTS=/var/lib/caddy/.local/share/caddy/certificates/acme-v02.api.letsencrypt.org-directory/$domain
install -d -m 750 -o root -g kontakt /opt/kontakt/tls
cat > /etc/systemd/system/kontakt-tls-sync.service <<UNIT
[Unit]
Description=Kontakt: копия сертификата Caddy для прямого эфира радио
[Service]
Type=oneshot
ExecStart=/bin/sh -c 'install -m 640 -o root -g kontakt \$CERTS/$domain.crt /opt/kontakt/tls/cert.pem && install -m 640 -o root -g kontakt \$CERTS/$domain.key /opt/kontakt/tls/key.pem'
UNIT
cat > /etc/systemd/system/kontakt-tls-sync.path <<UNIT
[Unit]
Description=Kontakt: следить за продлением сертификата Caddy
[Path]
PathChanged=\$CERTS/$domain.crt
[Install]
WantedBy=multi-user.target
UNIT
ufw allow 8443/tcp >/dev/null
systemctl enable caddy >/dev/null 2>&1
systemctl reload caddy 2>/dev/null || systemctl restart caddy
systemctl disable --now kontakt-tunnel >/dev/null 2>&1 || true   # туннель Cloudflare больше не нужен
systemctl daemon-reload
systemctl enable --now kontakt-tls-sync.path >/dev/null 2>&1
# первая копия — когда Caddy уже получил сертификат (ниже ждём ответа по https)
for i in \$(seq 40); do curl -fsS -m 5 https://$domain/healthz >/dev/null 2>&1 && {
  systemctl start kontakt-tls-sync.service && systemctl restart kontakt-radio
  echo "https://$domain отвечает, сертификат получен; прямой эфир радио — https://$domain:8443"; exit 0; }; sleep 3; done
echo "Caddy запущен, но https://$domain пока не отвечает: DNS ещё не разошёлся или порт 80 закрыт у провайдера. journalctl -u caddy -n 30"
EOS
  ;;

tunnel)
  domain=${3:?укажите домен: scripts/deploy.sh tunnel $host kontakt.example.com}
  echo "Вход в Cloudflare: откройте в браузере ссылку, которую напечатает cloudflared, и выберите домен."
  sudo_remote <<EOS
set -euo pipefail
C=/opt/kontakt/cloudflared
export HOME=\$C   # cloudflared кладёт cert.pem и ключ туннеля в ~/.cloudflared
mkdir -p \$C/.cloudflared
[ -f \$C/.cloudflared/cert.pem ] || cloudflared tunnel login
cloudflared tunnel info kontakt >/dev/null 2>&1 || cloudflared tunnel create kontakt
tid=\$(cloudflared tunnel list -o json | python3 -c "import sys,json;print(next(t['id'] for t in json.load(sys.stdin) if t['name']=='kontakt'))")
cat > \$C/config.yml <<CFG
tunnel: \$tid
credentials-file: \$C/.cloudflared/\$tid.json
# /sip, /api, /media — прямо в signal и media, сокеты радио — в радио, мимо web (вдвое дешевле
# по CPU, LOAD_REPORT.md); остальное, включая страницы /radio/, — в web
ingress:
  - hostname: $domain
    path: ^/sip\\\$
    service: http://127.0.0.1:8081
  - hostname: $domain
    path: ^/api/
    service: http://127.0.0.1:8081
  - hostname: $domain
    path: ^/media\\\$
    service: http://127.0.0.1:8082
  - hostname: $domain
    path: ^/radio/ws/
    service: http://127.0.0.1:8083
  - hostname: $domain
    service: http://127.0.0.1:8080
  - service: http_status:404
CFG
cloudflared tunnel route dns kontakt $domain || true
chown -R kontakt:kontakt \$C
systemctl enable --now kontakt-tunnel >/dev/null
systemctl restart kontakt-tunnel
for i in \$(seq 30); do curl -fsS -m 5 https://$domain/healthz >/dev/null 2>&1 && { echo "https://$domain отвечает"; exit 0; }; sleep 2; done
echo "туннель запущен, но https://$domain пока не отвечает — journalctl -u kontakt-tunnel"
EOS
  ;;

status)
  sudo_remote <<'EOS'
echo "версия: $(basename "$(readlink /opt/kontakt/current 2>/dev/null)" 2>/dev/null || echo нет)"
for s in kontakt-media kontakt-signal kontakt-radio kontakt-web kontakt-notify kontakt-tunnel caddy; do
  printf '  %-16s %s\n' "$s" "$(systemctl is-active $s)"
done
printf '  станция:         %s\n' "$(curl -fsS -m 3 http://127.0.0.1:8080/healthz 2>/dev/null || echo 'не отвечает')"
printf '  мониторинг:      %s\n' "$(docker ps --format '{{.Names}}' 2>/dev/null | grep -c kontakt-monitoring) контейнеров"
printf '  последний бэкап: %s\n' "$(ls -1t /opt/kontakt/backups 2>/dev/null | head -1 || echo нет)"
EOS
  ;;

backup)
  mkdir -p data/backups
  f=$(remote 'ls -1t /opt/kontakt/backups/data-*.tgz 2>/dev/null | head -1')
  [ -n "$f" ] || { echo "на сервере бэкапов пока нет (таймер — раз в сутки; сейчас: systemctl start kontakt-backup)"; exit 1; }
  remote "cat $f" >"data/backups/$(basename "$f")"
  echo "data/backups/$(basename "$f")"
  ;;

*)
  sed -n '2,22p' "$0"; exit 1 ;;
esac
