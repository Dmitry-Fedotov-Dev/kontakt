# Контакт
[English version](README.en.md)

## Запуск и обновление

### Один раз: что поставить

Всё запускается в Linux-оболочке: **Linux**, **WSL на Windows** или **Termux на Android**.
На Windows сначала `wsl` в PowerShell: команды вида `TUNNEL=1 команда` — синтаксис bash,
PowerShell их не понимает.

```bash
# Ubuntu / WSL: Go 1.24+ — с go.dev (в apt часто слишком старый)
curl -LO https://go.dev/dl/go1.24.7.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.24.7.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin:$HOME/.local/bin' >> ~/.bashrc && source ~/.bashrc
# cloudflared — для публичной ссылки (Linux-версия; Windows-туннели она не трогает)
mkdir -p ~/.local/bin && curl -L https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64 -o ~/.local/bin/cloudflared && chmod +x ~/.local/bin/cloudflared

# Termux
pkg install golang git cloudflared curl

# сам проект
git clone https://github.com/Dmitry-Fedotov-Dev/kontakt && cd kontakt
```

Мониторинг (по желанию) — Docker или Docker Desktop; без Docker тоже работает (см. ниже).

### Запуск

Каждое — в своём окне терминала (в Termux: свайп от левого края → NEW SESSION). Остановить —
`Ctrl+C`: гасятся только свои процессы, чужие туннели и сервисы не трогаются, порты
подбираются сами.

| Что | Команда | Где открыть |
|---|---|---|
| **Контакт** со ссылкой для всех | `./scripts/cluster-tunnel.sh` | `https://….trycloudflare.com` — в выводе; радио — та же ссылка + `/radio/` |
| Контакт только на этой машине | `./scripts/run.sh` | `http://localhost:8080`, радио — `http://localhost:8080/radio/` |
| **Открытое радио** со ссылкой | `TUNNEL=1 bash scripts/radio.sh` | ссылка в выводе; локально `http://localhost:27620` |
| Радио только на этой машине | `bash scripts/radio.sh` | `http://localhost:27620` |
| Mesh-демо (Master + 5 узлов) | `./scripts/mesh-demo.sh` | граф — в Grafana |
| **С мониторингом** | добавить `MONITORING=1` перед командой Контакта или радио (только у одного из них) | Grafana — адрес в выводе (обычно `http://localhost:27630`) |
| Мониторинг отдельно | `monitoring/stack.sh up` / `down` | `http://localhost:3002` |

- Ссылка выдаётся только когда туннель уже соединился с Cloudflare (5–20 с).
- Микрофон браузер даёт только по https или на `localhost`: с других устройств — по ссылке туннеля.
- Проверить звонок: два разных браузера (или обычное окно + инкогнито) — иначе станция видит одного человека.
- Телефон в Termux: перед запуском `termux-wake-lock`, иначе Android усыпит Termux; скрипты
  запускайте через `bash scripts/…`.
- Мониторинг сам выбирает способ: Docker на Linux, отдельный compose для Docker Desktop,
  без Docker — обычными программами (`MONITORING=local`). Пустые графики — смотрите
  `http://localhost:27631/targets`: цели должны быть UP.

### Обновление

```bash
git pull
```

| Что | Как применить | Ссылка туннеля |
|---|---|---|
| Контакт | во **втором** окне: `./scripts/cluster-tunnel.sh update` (сам делает `git pull`, собирает; не собралось — работает прежняя версия). Идущие звонки оборвутся, радио переподключится само | **остаётся** |
| Радио | во **втором** окне: `bash scripts/radio.sh update` (сам делает `git pull`, собирает; не собралось — работает прежняя версия) | **остаётся**; ведущий и слушатели переподключаются сами |
| Дашборды Grafana | ничего — подхватываются сами за ~10 с | — |
| Конфиг Prometheus, правила тревог | `monitoring/stack.sh down && monitoring/stack.sh up` | — |

Открытые страницы продолжают работать со старой версией до обновления вкладки.

Постоянная ссылка (своё доменное имя) — `./scripts/tunnel.sh publish <домен>`; адрес
`trycloudflare.com` временный и меняется при каждом запуске туннеля.

### На свой сервер

Сервер — Ubuntu 24.04 или Debian 12, вход по SSH-ключу. Всё делается с вашей машины (WSL, Linux,
macOS): собирается здесь, на сервер едут готовые бинарники. Наружу у сервера открыт только SSH —
звонки и радио идут через туннель, который сервер сам открывает к Cloudflare.

```bash
scripts/deploy.sh setup    root@<IP>                    # один раз: пользователь, пакеты, ufw, Docker, cloudflared, юниты
scripts/deploy.sh env      root@<IP>                    # токен бота из .env
scripts/deploy.sh push     root@<IP>                    # выложить текущий код; не поднялось — сам откатывает
scripts/deploy.sh tunnel   root@<IP> kontakt.example.com  # свой домен (он должен быть в Cloudflare)
scripts/deploy.sh status   root@<IP>                    # сервисы, версия, ответ станции, бэкап
scripts/deploy.sh rollback root@<IP>                    # вернуть предыдущую версию
scripts/deploy.sh backup   root@<IP>                    # забрать свежий бэкап базы модерации к себе
ssh -L 3002:127.0.0.1:3002 root@<IP>                    # Grafana: http://localhost:3002
```

На сервере: `/opt/kontakt/releases/` — последние 5 версий, `current` — рабочая; `config/kontakt.json`
правится на месте и применяется на лету; `data/` (база модерации) каждый день архивируется в
`backups/`, хранится 14 дней. Сервисы — systemd (`systemctl status kontakt-*`), мониторинг —
Docker. При `push` идущие звонки обрываются, ведущий и приёмники радио переподключаются сами.

Что и как проверять — [docs/TEST_CASES.md](docs/TEST_CASES.md).

---

<img width="427" height="900" alt="image" src="https://github.com/user-attachments/assets/ee221aa9-4ef8-4bf3-8cf6-3c27cf632ed8" />

Снимаешь трубку, и говоришь со случайным человеком.

- Протокол — из браузера по WebSocket (RFC 7118). Можно подключаться с любого устройства через ваш браузер.
- Три сервиса на Go: **web**, **signal**, **media**. Media управляется по gRPC.
- Голос нигде не хранится. Модерация общая с радио: жалоба → жёлтая карточка, жалобы двух разных людей за неделю — вечный бан по куке (подробнее — «Модерация» ниже; в дальнейшем будет бан по железу).
- Сто каналов (50 разговоров) — это 0,22 ядра и 43 МБ ОЗУ, подробности в [LOAD_REPORT.md](LOAD_REPORT.md). Канал стоит сети около 69 кбит/с в каждую сторону (G.711 + RTP, замер по `kontakt_media_bytes_*_total`; с заголовками IP/UDP — около 80). Ёмкость сети **не константа**: у Cloudflare Tunnel нет официального потолка в Мбит/с, она зависит от трафика, сессий и машины, поэтому её меряют нагрузкой (`k6/load.js`) и метриками, а не делят «25 Мбит/с на канал». Quick Tunnel (`*.trycloudflare.com`) — только для разработки и демонстраций: без гарантии доступности, до 200 одновременных HTTP-запросов, временное имя.

## Открытое радио (макет)

Отдельная программа на той же основе: любой занимает свободную волну 87.5–108.0 и вещает,
любой крутит ручку и слушает. У слушателя нет списка станций — только радио и бесконечная
ручка (за 108.0 снова идёт 87.5); между станциями шорох, свист биений и треск. У ведущего —
микрофон, громкости MIC / MUSIC / AIR и очередь mp3: файлы добавляются формой, перетаскиванием
или выбором папки с поиском по ней (музыка под голос приглушается).

```bash
./scripts/radio.sh            # http://localhost:27620 (или следующий свободный порт)
TUNNEL=1 ./scripts/radio.sh   # плюс временный адрес https://….trycloudflare.com
bash scripts/radio.sh update  # из другого окна: git pull + перезапуск радио, адрес туннеля прежний
```

Поделиться волной — кнопка SHARE (у слушателя, когда пойман сигнал, и у ведущего в эфире):
ссылка вида `/w/101.7?n=станция&t=трек` хранит трек, игравший в момент нажатия. По ней
Telegram и другие мессенджеры показывают карточку с пиксельной картинкой `/og/1017.png?…`
(1200×632, рисуется сервером своим шрифтом 5×7 с кириллицей). Открывший ссылку сразу стоит
на этой волне — достаточно коснуться экрана.

Почему дёшево: кодирует **браузер ведущего** (G.711 μ-law, 8 кГц, 64 кбит/с), сервер только
раздаёт готовые кадры по 160 байт слушателям своей частоты — без декодирования и смешивания.
Слушатель между станциями трафика не получает вовсе. mp3 никуда не загружаются: играют из
браузера ведущего. Лимиты: `-max-stations`, `-max-listeners`; поток ведущего ограничен
64 кбит/с на сервере. Метрики — `/metrics` (`kontakt_radio_*`). Правила эфира — `/radio/terms.html`.

С погашенным экраном: у ведущего, пока он в эфире, экран не гаснет (Wake Lock). Приёмник играет
через `<audio>` с Media Session — Android Chrome продолжает играть в фоне, а на экране блокировки
видна станция. Safari на iPhone фоновое воспроизведение может всё равно обрывать — это
ограничение браузера.

## Модерация

Общая для рулетки и радио: `run.sh` и `cluster-tunnel.sh` отдают радио под `/radio/` того же
web, поэтому кука `kontakt_id` одна, и база одна — у signal (`data/bans.json`), радио ходит к
ней через админ-порт signal'а (`-mod`). Радио, запущенное отдельно (`radio.sh`), держит свою
базу `data/radio/bans.json`.

- **Зоны бана:** `calls` — снять трубку, `air` — выйти в эфир, `all` — и то и другое. Слушать
  радио может любой. Жалобы на звонки банят звонки, жалобы слушателей на станцию — эфир;
  `all` ставит только админ.
- **Порог:** первая жалоба — жёлтая карточка; бан — когда за `ban_window_days` (7) пожаловались
  `ban_reporters` (2) **разных** человека. Бан вечный, снимает его только админ. Повторная
  жалоба того же человека в зачёт не идёт.
- **Новички:** пока у куки меньше `trust_talks` (3) засчитанных сессий (разговор от 30 с или
  5 минут у приёмника / в эфире), её жалобы разводят пару, но в зачёт не идут: новая кука в
  инкогнито не должна банить кого угодно.
- **Приватность:** на диске — хеш куки, зоны, жалобы как хеш пары «на кого + кто» (связать с
  жалобщиком нельзя) и дата без времени. Журнал действий — `data/bans-journal.jsonl`.

Пороги — в `config/kontakt.json`, применяются на лету. Админка signal'а (только localhost):

```bash
curl -s localhost:8091/admin/journal | jq                         # журнал: yellow / ban / unban, ключ записи
curl -s -XPOST 'localhost:8091/admin/ban?key=<ключ>&zone=all'      # бан; calls/all сразу выгоняет из разговора
curl -s -XPOST 'localhost:8091/admin/unban?id=<кука kontakt_id>&zone=calls'
curl -s localhost:8091/admin/stats | jq .moderation                # баны и карточки по зонам
```

### Telegram-бот

`cmd/notify` пишет в чат владельца тревоги мониторинга (загорелась / прошла, Prometheus не
отвечает), карточки и баны — с кнопкой «Разбан», а также «стенд запущен / остановлен». Команды
принимаются только из этого чата: `/stats`, `/journal [N]`, `/ban <ключ> <зона>`,
`/unban <ключ> <зона>`. В сообщениях — только ключи записей и зоны, без кук и IP.

1. @BotFather → `/newbot`, получить токен; открыть своего бота и нажать **Start**.
2. В корне репозитория — файл `.env` (в git не попадает):
   ```bash
   TELEGRAM_BOT_TOKEN=1234567890:AA…
   TELEGRAM_CHAT_ID=<ваш id>   # https://api.telegram.org/bot<токен>/getUpdates после Start: message.chat.id
   ```
3. `./scripts/run.sh` или `./scripts/cluster-tunnel.sh` запускают бота сами; нет `.env` — бота нет,
   остальное работает. Тревоги бот берёт из Prometheus (`PROMETHEUS_PORT`, по умолчанию 9092),
   Alertmanager не нужен.

## Тесты звонков (xk6-sip)

Звонковые тесты — функциональные и нагрузочные — делает [xk6-sip](https://github.com/Dmitry-Fedotov-Dev/xk6-sip): абоненты-софтфоны по SIP/UDP, как Linphone или MicroSIP, и проверка звука `compareAudio()`. У каждого абонента своя фраза, поэтому видно, **кого** именно он слышит.

```bash
xk6 build v2.3.0 --with github.com/Dmitry-Fedotov-Dev/xk6-sip@v0.4.0 --output bin/k6
IP=127.0.0.1 SIP_UDP=:5060 ./scripts/run.sh
bin/k6 run k6/functional/pair.js          # пара: слышат друг друга, не слышат себя
bin/k6 run k6/functional/next.js          # собеседник ушёл — следующий в том же звонке
bin/k6 run k6/functional/leave-queue.js   # ушедший из очереди не соединяется с новыми
bin/k6 run -e VUS=40 -e DURATION=2m k6/load.js
```

Живая станция по её адресу (туннель или домен) — две веб-трубки звонят друг другу, ведущий
радио выходит на 87.7 и слушатель его получает:

```bash
KONTAKT_URL=https://….trycloudflare.com go test ./e2e -run TestLive -v
```

Звучание одно — G.711 с «окраской» линии 32 (выбора битрейта нет; в `config/kontakt.json` разрешена только `"32"`, другие имена в адресе станция приводит к ней). Порог качества — по замеру: на линии 32 score 0,76–0,82, порог 0,7; чужая речь < 0,4.

## Мониторинг

`/metrics` есть у signal (`127.0.0.1:8091`) и media (`127.0.0.1:8082`); `run.sh` пишет их адреса (и радио) в `monitoring/prometheus/targets/*-run-<порт>.json` и убирает при выходе. Prometheus и Grafana с готовым дашбордом:

```bash
docker compose -f monitoring/docker-compose.yml up -d   # http://localhost:3002, + --profile linux-host для машины
K6_PROMETHEUS_RW_SERVER_URL=http://127.0.0.1:9092/api/v1/write K6_FEATURES=native-histograms \
  bin/k6 run -o experimental-prometheus-rw --tag testid=run-1 k6/load.js   # сторона абонентов на том же дашборде
```

Порты заняты вашими сервисами — `GRAFANA_PORT=… PROMETHEUS_PORT=… docker compose …`, либо `MONITORING=1 ./scripts/cluster-tunnel.sh`: подберёт свободные, поднимет стек и напечатает адрес Grafana. Станцию, запущенную `cluster-tunnel.sh`, Prometheus находит сам (файлы целей в `monitoring/prometheus/targets/`).

Docker Desktop (Windows, macOS) — `monitoring/docker-compose.desktop.yml`: у него «хост» — своя виртуальная машина, поэтому там обычная сеть Docker, порты на `localhost`, а станцию Prometheus находит через `host.docker.internal`. `MONITORING=1` в скриптах выбирает файл сам (`monitoring/stack.sh`). Совсем без Docker — `MONITORING=local` (`monitoring/local.sh`: те же Prometheus, Grafana, правила и дашборды обычными программами). Явно — `MONITORING=docker | desktop | local`. Дашборд правится в `monitoring/grafana/gen_dashboard.py`, PNG за прогон — `monitoring/report.sh`.

**Радио.** Дашборд «Открытое радио» (`/d/kontakt-radio`): станции и слушатели (по волнам — только частота), поток от ведущих против нормы 50 кадров/с, полоса, потери кадров с причиной, отказы, перезапуски, ссылки на волну и картинки превью, длительность эфира. `scripts/radio.sh` сам пишет цель для Prometheus; `MONITORING=1 bash scripts/radio.sh` ещё и поднимает стек. Радио на телефоне — Prometheus на компьютере берёт метрики через туннель: `monitoring/radio-target.sh https://….trycloudflare.com`. Метрики — только счётчики, без адресов и названий; спрятать их из туннеля — флаг `-admin 127.0.0.1:27621`.

Тревоги станции (`rules/station.yml`): signal или media не отвечает, звонки падают на media. Тревоги радио (`monitoring/prometheus/rules/radio.yml`, видны в Prometheus, на дашборде и в Telegram): радио не отвечает, эфир прерывается (меньше 45 кадров/с на станцию — обычно вкладка ведущего ушла в фон), слушатели теряют больше 5% кадров, ведущий шлёт больше 64 кбит/с, упёрлись в лимит, перезапуски по кругу. Каждая проверена на синтетических рядах: `promtool test rules monitoring/prometheus/tests/{radio,station}_test.yml` (в CI).

**Граф Kontakt Mesh.** Дашборд «Kontakt Mesh»: узлы и рёбра системы глазами выбранного узла (Master видит всё), цвет — здоровье и качество связи, таблицы узлов и рёбер, RTT и потери во времени. Посмотреть на демо-стенде: `./scripts/mesh-demo.sh` (Master + 5 Worker'ов) и стек мониторинга. Подробнее о mesh — [docs/MESH.md](docs/MESH.md).

CI (`.github/workflows/ci.yml`) устроен как у xk6-sip: тесты, сборка и проверка JS страницы, функциональные сценарии с JUnit и WAV проваленных проверок звука, нагрузка с PNG дашборда, govulncheck и отчёт gosec.
