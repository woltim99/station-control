# Источники, авторство и дисклеймер

## Root-кит
`files/hybrid-1.2.4/` — сторонний root-кит **Station Max Hybrid 1.2.4**
для `yandexstation_2`, прошивка `5000000921`. Включён без изменений,
со всеми оригинальными документами:

- `README.md` — установка, команды, recovery;
- `ADB-GUIDE.md` — первичное включение ADB через eQuasar;
- `TROUBLESHOOTING.md` — ошибки и восстановление;
- `OTHER-FIRMWARE.md` — проверка и портирование других сборок.

Если вы автор кита и хотите изменить условия распространения — напишите,
файлы будут убраны/заменены ссылкой.

## lite-kit
`files/lite-kit/` — наш стенд для первого ADB: DNS + HTTPS + проксирование
`/get_sync_info` с добавлением ADB. Код распространяется по MIT
(см. `files/lite-kit/LICENSE`). В архив не входят `firmware/`, `log/`,
`run/` и `.git`.

В `files/lite-kit/certs/` лежат **публичные** сертификаты eQuasar
(`fullchain.pem`, `quasar-all.key`, `root-aaa.pem`). Это не ваши личные
ключи; они давно публичны. Если не доверяете — замените на свои.

## eQuasar
Идея и сертификаты — из публичного проекта **eQuasar**
(commit `02f3c583a7d7335fc8f3f7d511ea795590b41341`).
В `files/lite-kit/certs/` включён публичный набор; исходный репозиторий:
`copymetoroot/opt/fakequasar/certs/`.

## AirPlay

- `airplay-video-0.0.38.apk` и `airplay-receiver-upstream.apk` — форк
  `dypaul/android-airplay-server` (GPLv3). Исходники и лицензия:
  `stage/airplay_video/src/`, файл `LICENSE`.
- `shairport-0.3.0.apk` — сторонний Shairport Sync для Android
  (`com.hkfuertes.shairport`). APK включён как есть; исходники и лицензия —
  у автора приложения.

## Наша обвязка
`files/lite-kit/`, `files/extras/`, `net/`, `tools/`, `README.md`,
`CHECKLIST.md` — собраны для YNDX-0008. Можно использовать и изменять
свободно, «как есть».

## Безопасность
В архиве **нет**:
- ваших приватных SSH-ключей;
- ADB-ключей (`adb_keys.pub` вашей станции);
- паролей, токенов, MAC/серийников;
- логов с device_id/IP.

В `files/lite-kit/certs/` есть публичные сертификаты eQuasar — это не
секрет, но и не «ваши» ключи. Не публикуйте в чатах:
- `station_key` (приватный SSH-ключ);
- `~/.android/adbkey`;
- логи с IP/MAC/токенами.

## Дисклеймер
Гайд актуален для нашей станции **Яндекс Станция Макс `YNDX-0008`**
(Android, прошивка 0921). Для других станций, Station 2, Mini, первой
Station и других Android-моделей/прошивок работоспособность не
гарантируется: там может быть другой root-путь и другие бинарники.
Установка root эксплуатирует уязвимость и может привести к зависанию,
потере данных или bootloop. Авторы гайда не несут ответственности.
Делайте backup, читайте `TROUBLESHOOTING.md`, не запускайте бинарники
на непроверенной прошивке.
