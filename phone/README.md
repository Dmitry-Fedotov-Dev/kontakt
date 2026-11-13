# Приложение «Контакт» (трубка) — Android и iOS

Обёртка Capacitor 8 вокруг `https://openline.one/`: страница та же, что на сайте, правки трубки видны
в приложении после `deploy.sh push` без новой версии. Устроено как приложение радио (`mobile/`), только
без фонового сервиса эфира.

| | Android | iOS |
|---|---|---|
| Идентификатор | `one.openline.kontakt` | `one.openline.kontakt` |
| Название | «Контакт» (ru, uk, be, kk, ky), иначе «Kontakt» | «Контакт» (ru), иначе «Kontakt» |
| Нативное | микрофон, вибрация (Capacitor Haptics), звук без касания | микрофон, вибрация (Haptics), `playAndRecord` + фоновый `audio` |

**Входящий звонок.** Трубка на рычаге слушает свою линию (`/api/ring`, `internal/signal/ring.go`):
звонит, когда друг открыл вашу ссылку или пришло оповещение (`cmd/kontakt-ring`, хуки Claude Code —
`docs/CLAUDE_RING.md`). Трубка качается, телефон вибрирует, играет трель. В приложении звук идёт без
касания экрана (Capacitor снимает требование жеста), вибрация есть и на iPhone (в браузере iPhone её нет).

**Ограничение:** звонит, пока приложение открыто или недавно свёрнуто — система со временем усыпляет
WebView. Push (FCM/APNs) при закрытом приложении — следующий шаг.

## Сборка и релиз

Только в CI (`.github/workflows/phone.yml`), как радио: Android SDK и Xcode на машину владельца не ставим.

- push в `main` с изменениями в `phone/` — APK/AAB и сборка для симулятора iOS в Artifacts прогона;
- **релиз:** `git tag phone-v1.0.0 && git push origin phone-v1.0.0` — GitHub Release с
  `kontakt-1.0.0.apk` (поставить на Android напрямую), `.aab` (Google Play) и сборкой для симулятора.

Подпись — тем же ключом загрузки, что радио (секреты `UPLOAD_KEYSTORE_*`); TestFlight — с секретами
`ASC_*`, как в `mobile/STORES.md` (в App Store Connect нужен свой Bundle ID `one.openline.kontakt`).

Иконки и заставки — `python3 make_icons.py` (из той же пиксельной трубки, что рисует страница).
