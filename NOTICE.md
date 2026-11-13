# Station Control — лицензии и сторонние компоненты

## Наш код

`station-web`, ТВ-приложение, скрипты обслуживания и `absshd` распространяются
по лицензии **MIT** (см. `LICENSE`), если для конкретного каталога ниже не
указано иное.

## AirPlay-видео

`stage/airplay_video/` — форк проекта
`dypaul/android-airplay-server` (база `20e026b`, PR #86), лицензия
**GPLv3**. Текст лицензии: `stage/airplay_video/src/LICENSE`.

APK `airplay-video-*.apk` распространяется на условиях GPLv3; исходники и
наши патчи находятся в `stage/airplay_video/`.

## lite-kit

`YNDX-0008-guide/files/lite-kit/` — MIT, текст: `LICENSE` внутри каталога.
В `certs/` лежат **публичные** сертификаты eQuasar, а не личные ключи
пользователя; это описано в `YNDX-0008-guide/NOTICE.md`.

## Root-кит

`YNDX-0008-guide/files/hybrid-1.2.4/` — сторонний root-кит
Station Max Hybrid 1.2.4, включён без изменений. Авторство и условия —
в `YNDX-0008-guide/NOTICE.md` и документах внутри каталога.

## AirPlay-аудио

`shairport-0.3.0.apk` — сторонний Shairport Sync для Android
(`com.hkfuertes.shairport`). APK включён как есть; исходники и лицензия —
у автора приложения.

## Сторонние библиотеки

Лицензии библиотек внутри `stage/airplay_video/src` (FFmpeg, UxPlay,
OpenSSL, libplist и другие) находятся в их исходниках и каталогах
`third_party`/`LICENSE`/`NOTICE`.
