# CD: выкладка на openline.one из GitHub Actions

`.github/workflows/deploy.yml` выкладывает сервер тем же `scripts/deploy.sh push`, что и руками:
новая версия рядом со старой, переключение, перезапуск; не поднялась за 30 с — откат сам. Потом
проверка снаружи через Caddy: `/healthz` и `/api/ring` (`{"listeners":0}` на случайную линию).

- **Когда:** CI на `main` прошёл зелёным (слияние PR, push в `main`). Выкладывается ровно тот коммит,
  который проверил CI. Красный CI — выкладки нет.
- **Руками:** Actions → **deploy** → Run workflow → `push` (выложить `main`), `rollback` (прошлая
  версия), `status` (сервисы и версия).
- Идущие звонки при выкладке обрываются, радио переподключается само (как и при ручном `push`).
  Не хотите выкладку на каждое слияние — включите Required reviewers (шаг 3): прогон будет ждать
  кнопки «Approve».

## Настройка — один раз

Всё в **Git Bash** на Windows (или в WSL).

### 1. Отдельный ключ для CI

Не ваш личный ключ: этот будет лежать в секретах GitHub, его можно отозвать, не трогая свой.

```bash
ssh-keygen -t ed25519 -N "" -C kontakt-cd -f ~/.ssh/kontakt-cd
```

Положить открытую часть на сервер (заходите своим ключом, как обычно):

```bash
ssh root@152.53.204.216 'cat >> ~/.ssh/authorized_keys' < ~/.ssh/kontakt-cd.pub
```

Проверить, что ключ CI пускает:

```bash
ssh -i ~/.ssh/kontakt-cd -o IdentitiesOnly=yes root@152.53.204.216 'echo ключ CI работает'
```

### 2. Ключ сервера (known_hosts)

CI пускается только к заранее известному серверу. Строка ключа:

```bash
ssh-keyscan -t ed25519 152.53.204.216 2>/dev/null
```

Сверить отпечаток с тем, что уже знает ваш компьютер (вы заходили на сервер — значит, он сверен):

```bash
ssh-keyscan -t ed25519 152.53.204.216 2>/dev/null | ssh-keygen -lf -
```
```bash
ssh-keygen -lF 152.53.204.216
```

Отпечатки `SHA256:…` должны совпасть. Не совпали — не продолжайте, сначала выясните почему.

### 3. Окружение production в GitHub

https://github.com/Dmitry-Fedotov-Dev/kontakt/settings/environments → **New environment** →
имя `production`:

- **Deployment branches and tags** → Selected branches → добавить `main`
  (из других веток выкладки не будет).
- По желанию **Required reviewers** → себя: каждая выкладка ждёт вашего «Approve».
- **Environment secrets** → Add secret:
  - `DEPLOY_SSH_KEY` — всё содержимое `~/.ssh/kontakt-cd` (закрытый ключ, с строками `-----BEGIN…`
    и `-----END…`). Показать: `cat ~/.ssh/kontakt-cd`.
  - `DEPLOY_KNOWN_HOSTS` — строка из `ssh-keyscan` шага 2 (`152.53.204.216 ssh-ed25519 AAAA…`).
- **Environment variables** → Add variable:
  - `DEPLOY_HOST` = `root@152.53.204.216`
  - `SITE_URL` = `https://openline.one`

### 4. Первая выкладка

Actions → **deploy** → Run workflow → `push`. В журнале — «версия … работает», затем
`api/ring: {"listeners":0}`. Дальше всё само: слили PR → CI → выкладка.

## Если что-то пошло не так

| В журнале | Что делать |
|---|---|
| `нет переменной DEPLOY_HOST` / `нет секрета …` | шаг 3: секреты и переменные — в окружении `production`, не в Repository secrets |
| `Host key verification failed` | шаг 2: строка `DEPLOY_KNOWN_HOSTS` не та или ключ сервера сменился |
| `Permission denied (publickey)` | шаг 1: открытая часть ключа не в `authorized_keys` |
| `версия … не поднялась за 30 с — откатываю` | сервер уже на прошлой версии; причина — в строках `journalctl` выше |
| `/api/ring отвечает не так` | выложилось, но звонков нет: проверьте Caddy (`/api/*` → signal) — `deploy.sh status` |

Отозвать доступ CI — удалить строку `kontakt-cd` из `/root/.ssh/authorized_keys` на сервере.
