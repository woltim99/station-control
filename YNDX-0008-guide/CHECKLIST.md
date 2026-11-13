# Чек-лист YNDX-0008: ADB + root + фиксация + OTA

> **Только для Station Max `YNDX-0008`** (Android, прошивка 0921).
> Не для Linux-станций и не для других моделей/прошивок.

Короткая версия. Подробности — в `README.md`.

## 0. Первый ADB (lite-kit)
- [ ] `files/lite-kit`: создан `run/sync_proxy`
- [ ] Запущен `start.ps1` / `sudo ./start.sh`
- [ ] DNS роутера → IP ПК (для станции)
- [ ] Станция зарегистрирована; в логе `SYNC ... platform=yandexstation_2` и `[SYNC-REAL→] ... adbd=on`
- [ ] 1–2 перезагрузки станции
- [ ] `adb connect IP:5555` → `adb shell id` = `uid=2000(shell)`
- [ ] `settings get global adb_enabled` = `1`, `service.adb.tcp.port` = `5555`
- [ ] Если `unauthorized` — проверить авторизацию ADB-ключа и подключение (экранного подтверждения у Station Max нет)

## 1. Root
- [ ] `adb shell 'ip route | grep default'` и `getprop net.dns1`
- [ ] Git Bash: `station-max.sh install --router-ip ... --dns-ip ... --yes`
- [ ] `INSTALL PASS`
- [ ] `./station-max.sh status` = `HEALTHY`
- [ ] `./station-max.sh cmd 'id; getenforce'` = `uid=0(root)`, `Permissive`

## 2. Фиксация ADB
- [ ] `adb push files/extras /data/local/tmp/station-max-extras`
- [ ] `station-max.sh cmd 'sh /data/local/tmp/station-max-extras/install-extras.sh'`
- [ ] `adb reboot`
- [ ] Через 60–90 с: `adb connect IP:5555`
- [ ] `adb shell sh /data/local/tmp/station-max-extras/verify-extras.sh`
  - `adb_enabled=1`
  - `adbd=running`
  - `adbguard=running`
  - `tcp_port=5555`
  - `updater=none`
  - `hosts=bind`

## 3. OTA
- [ ] `ping ota.yandex.net` → `127.0.0.1`
- [ ] `ping quasar.yandex.net` → реальный IP (не 127.0.0.1)
- [ ] `pm list packages --user 0 | grep updater` → пусто

## 4. SSH (опция)
- [ ] `ssh-keygen -t ed25519 -f station_key`
- [ ] `authorized_keys` = `station_key.pub`
- [ ] `install-ssh.sh` через `station-max.sh cmd`
- [ ] `adb reboot`
- [ ] `ssh -p 2222 -i station_key root@IP` → `uid=0(root)`

## 5. Холодная проверка
- [ ] `adb shell getprop init.svc.adbguard` = `running`
- [ ] `./station-max.sh status` = `HEALTHY`
- [ ] `getenforce` = `Permissive`

## Откат
- [ ] `station-max.sh cmd 'sh /data/local/tmp/station-max-extras/uninstall-extras.sh'`
- [ ] `adb reboot`
- [ ] `adb shell pm install-existing --user 0 com.yandex.launcher.updaterapp`
- [ ] `./station-max.sh rollback`

## Не делать
- Не блокировать `quasar.yandex.net` — отвалится Алиса.
- Не отключать `com.yandex.tv.services.platform` — bootloop.
- Не делать factory reset после установки — потеряете ADB-ключи.
- Не запускать root-кит на другой прошивке.
