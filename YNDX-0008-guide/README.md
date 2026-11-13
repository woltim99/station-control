# YNDX-0008 (Яндекс Станция Макс) — ADB, root, фиксация, блокировка OTA

Версия гайда: **2.0** (05.11.2026). Часть 0 переписана под реальный путь —
стенд `lite-kit` + проксирование sync. Гайд актуален для нашей станции
Station Max `YNDX-0008` (Android, прошивка 0921); для других моделей и
сборок работоспособность не гарантируется.
Первый ADB — без телевизора и пульта.
Комплект: lite-kit + root-кит hybrid 1.2.4 + наши скрипты фиксации ADB/OTA/SSH.

> **Гайд актуален для нашей станции Яндекс Станция Макс `YNDX-0008` на Android, прошивка `5000000921`.**
> Для других станций (Station 2, Mini, первая Station) и других
> моделей/прошивок работоспособность не гарантируется: root-кит для них
> не проверялся.
>
> `PPR1.180610.011.5000000921`, Android 9, kernel 4.9.113.
> На другой прошивке бинарники root-кита не запускать: сначала
> `files/hybrid-1.2.4/OTHER-FIRMWARE.md` и проверка совместимости.

---

## Что получится

| Компонент | Куда ставится | Переживает перезагрузку |
|---|---|---|
| ADB over TCP `5555` | `service.adb.tcp.port=5555` | да (`adbd_tcp.rc`) |
| Сторож ADB `adbguard` | `/odm/bin/adbguard.sh` | да (init-сервис) |
| Root (rootd hybrid 1.2.4) | `/odm/bin/rootd`, `/odm/bin/permissive` | да (`rootd.rc`) |
| Блокировка OTA | bind-mount `/odm/etc/hosts-block` → `/system/etc/hosts` | да (`adbguard`) |
| Апдейтер Яндекса | `pm uninstall --user 0 com.yandex.launcher.updaterapp` | да |
| Root SSH `2222` (опция) | `/odm/bin/absshd` (Go, static arm64) | да (`adbd_tcp.rc`) |

После установки:

```text
adb connect IP_СТАНЦИИ:5555   # всегда, после любого ребута
adb shell id                  # uid=2000(shell)
./station-max.sh status       # rootd: HEALTHY / PROTECTED
ping ota.yandex.net           # 127.0.0.1 (внутри станции)
```

---

## Важно и опасно

- **AS IS, без гарантий.** Root-путь эксплуатирует уязвимость GPU/Mali
  (`CVE-2022-36449`), возможны panic/GPU hang. Работайте на своём устройстве.
- **Не делайте factory reset** после установки: `/odm` (rootd, absshd, adbguard,
  hosts) переживёт, но `/data/misc/adb/adb_keys` и авторизация ADB сотрутся.
- **OTA может перезаписать `/odm`.** После OTA повторите установку root/фиксации.
- **Не отключайте `com.yandex.tv.services.platform`** — это bootloop.
- Root-кит — сторонний (hybrid 1.2.4), включён без изменений. Авторы/источник
  указаны в `NOTICE.md`.
- Для первого ADB **телевизор и пульт не нужны**: конфиг с
  `adbd.remoteActivation` включает отладку, и `adb connect` обычно сразу даёт
  shell. Если `adb devices` покажет `unauthorized`, проверьте, что на станции
  авторизован ваш ADB-ключ (через root/SSH), и повторите подключение:
  экранного подтверждения у Station Max нет.
- Все IP ниже — примеры (`192.168.1.x`). Замените на свои.

---

## Что понадобится

> **Гайд актуален для нашей станции Station Max `YNDX-0008`** (Android,
> прошивка 0921). Для других Android-станций, Station 2, Mini и первой
> Station работоспособность не гарантируется: там может быть другой
> root-путь и другие бинарники.

1. Станция Макс `YNDX-0008` на прошивке 0921, подключённая к Wi-Fi/аккаунту.
2. Компьютер в той же локальной сети:
   - Windows + **Git Bash** (MINGW64), либо Linux/macOS/WSL;
   - [Android Platform Tools](https://developer.android.com/tools/releases/platform-tools)
     (`adb` в `PATH` или путь через `ADB=`).
3. ПК/мини-ПК для стенда `lite-kit`: Windows 10/11 + Python 3.8+
   (Add to PATH) + права администратора, либо Linux/macOS с
   `python3`/`dnsmasq`/`openssl`; свободные порты **53/UDP+TCP, 80, 443**.
4. Роутер, где можно выставить DNS = IP этого ПК (или DHCP-опцию 6)
   хотя бы для станции.
5. 20–40 минут и 2–3 перезагрузки.

---

## Порядок действий (кратко)

0. Поднять `lite-kit` (DNS + HTTPS + `sync_proxy`) → получить ADB.
1. Установить root (`hybrid 1.2.4`) → `INSTALL PASS`.
2. Закинуть extras → включить ADB-over-TCP + adbguard.
3. Включить блокировку OTA (hosts + updater + DNS).
4. (Опция) Root SSH.
5. Перезагрузить и проверить по чек-листу.

Подробно — ниже. Короткая версия — в `CHECKLIST.md`.

---


# Часть 0. Первый ADB (стенд lite-kit)

Мы шли именно так. На ПК/мини-ПК поднимается `lite-kit`: он притворяется
DNS+HTTPS Яндекса, проксирует запросы станции в настоящий Яндекс и
подменяет только `/get_sync_info`, добавляя ADB. Телевизор и пульт не нужны.

## 0.1. Что делает стенд

> `lite-kit` универсальный (Лайт/Мини/Миди/Макс), но в этом гайде
> используется только ветка `yandexstation_2` — Макс, 0921.

- DNS: `quasar.yandex.net`, `quasar.yandex.ru`, `scbh.yandex.net` → IP ПК.
- HTTPS с сертификатами eQuasar (лежат в `files/lite-kit/certs/`).
- `GET /get_sync_info` уходит в Яндекс, ответ возвращается станции с
  добавленными полями:
  `adbd {remoteActivation:true, network:true}` и
  `com.yandex.tv.services.platform {adbEnabled:true, developmentSettingsEnabled:true}`.
- `/check_updates` — без обновлений (прошивка в комплект не входит).
- Остальные запросы (`register`, `push_subscribe`, `update_device_state`)
  идут в Яндекс, чтобы регистрация и приложение работали.

## 0.2. Что понадобится

- Windows 10/11 + Python 3.8+ (галочка «Add python.exe to PATH») + права
  администратора, либо Linux/macOS с `python3`/`dnsmasq`/`openssl`.
- Свободные порты **53/UDP+TCP, 80, 443** на ПК.
- Роутер, где можно выставить DNS = IP ПК (или DHCP-опцию 6) хотя бы для
  станции.
- Станция в той же сети и физический доступ к ней.

## 0.3. Запуск стенда

Windows (PowerShell от администратора):

```powershell
cd files/lite-kit
New-Item -ItemType Directory -Force run | Out-Null
New-Item -ItemType File -Force run\sync_proxy | Out-Null
.\start.ps1
```

Linux/macOS:

```bash
cd files/lite-kit
mkdir -p run && touch run/sync_proxy
sudo ./start.sh
```

`run/sync_proxy` включает режим, которым мы пользовались: sync проксируется
в настоящий Яндекс, в ответ добавляется ADB. Без этого файла стенд отдаёт
локальный шаблон (для Макса он может не сработать).

Скрипт напечатает IP ПК и проверит DNS. Если порты заняты — освободите
53/80/443 (ICS, `systemd-resolved`, avahi, Pi-hole и т.п.).

## 0.4. DNS на роутере

Поставьте DNS = IP ПК (для станции). Проверка с ПК:

```bash
nslookup quasar.yandex.net IP_ПК
```

Должен ответить IP ПК. `quasar.yandex.net` **не блокируем** — это бэкенд.

## 0.5. Регистрация станции

1. Сбросьте/добавьте станцию в «Дом с Алисой» и подключите к Wi-Fi.
2. Дождитесь регистрации. В `files/lite-kit/log/local.log` (или
   `server.log`) должна появиться строка вида
   `SYNC adbd-... /get_sync_info?...platform=yandexstation_2...`
   и `>>> [SYNC-REAL→] ... adbd=on`.
3. Перезагрузите станцию 1–2 раза (питанием) и дождитесь загрузки.

## 0.6. Подключение ADB

```bash
adb connect IP_СТАНЦИИ:5555
adb devices -l
adb shell id
adb shell settings get global adb_enabled
adb shell getprop service.adb.tcp.port
```

Ожидается `uid=2000(shell)`, `adb_enabled=1`, `service.adb.tcp.port=5555`.
Подтверждение на телевизоре **не требуется**: у Station Max нет экранного
запроса ADB. Если `unauthorized` — проверьте авторизацию ADB-ключа через
root/SSH и повторите `adb connect`.

## 0.7. После ADB

- Стенд можно оставить включённым до установки root и `adbguard`.
- На Максе `adb_enabled` и `service.adb.tcp.port` переживают ребут, но
  облачный конфиг позже может выставить `adb_enabled=0` — поэтому часть 2
  (`adbguard`) обязательна.
- `install-firmware` из lite-kit в этом гайде **не используем**: прошивка
  в архив не входит, OTA блокируется в части 3.

## 0.8. Если не получилось

- Нет sync в логе: DNS на роутере не на ПК, заняты порты, firewall.
- `unknown_ca`: не те сертификаты; возьмите `certs/` из eQuasar.
- ADB нет после sync: перезагрузите станцию ещё раз; проверьте
  `adb_enabled` через любой доступный shell.
- Подробно — `files/lite-kit/README.md` и
  `files/lite-kit/docs/sync-adb.md`.

> **Альтернатива (на Максе не проверялась):** статический fake-quasar
> с фиксированным JSON `/get_sync_info` и 403 на остальное. Мы им не
> пользовались: Максу нужен родной ответ с полями модели. Шаблон Nginx
> остался в `net/nginx-quasar.conf.example` как справка.


# Часть 1. Root (hybrid 1.2.4)

Установка проверена на Windows Git Bash и WSL/Linux. Из PowerShell/cmd
скрипт не запускается — только Git Bash.

## 1.1. Узнать IP шлюза и DNS

```bash
adb -s IP_СТАНЦИИ:5555 shell 'ip route | grep default'
adb -s IP_СТАНЦИИ:5555 shell getprop net.dns1
```

- `--router-ip` — адрес шлюза (обычно роутер, например `192.168.1.1`).
- `--dns-ip` — DNS, который использует станция (часто тот же роутер или
  `192.168.1.1`/`8.8.8.8`). rootd настроит DNAT/DoT на этот адрес.

## 1.2. Запуск

**Windows (Git Bash):**

```bash
cd files/hybrid-1.2.4
ADB='C:/android/platform-tools/adb.exe' \
ANDROID_SERIAL=IP_СТАНЦИИ:5555 \
./station-max.sh install --router-ip 192.168.1.1 --dns-ip 192.168.1.1 --yes
```

**Linux/macOS/WSL:**

```bash
cd files/hybrid-1.2.4
ANDROID_SERIAL=IP_СТАНЦИИ:5555 \
./station-max.sh install --router-ip 192.168.1.1 --dns-ip 192.168.1.1 --yes
```

Установщик сам делает backup, пишет 4 файла в `/odm`, перезагружает станцию
и запускает `verify`. Ожидаемый финал:

```text
INSTALL PASS
```

Первый Mali-цикл вероятностный: установщик сам делает до 20 попыток
(`bootstrap attempt 3/20`). Прерывание до backup ничего не портит.

## 1.3. Проверка

```bash
./station-max.sh status     # HEALTHY
./station-max.sh verify     # VERIFY OK
./station-max.sh cmd 'id; getenforce'
# uid=0(root); Permissive
```

Полезные команды:

```bash
./station-max.sh diag       # zip с диагностикой для чата (без секретов)
./station-max.sh rollback   # откат root
```

Больше — в `files/hybrid-1.2.4/README.md` и `TROUBLESHOOTING.md`.

---

# Часть 2. Фиксация ADB over TCP

После ребута ADB должен подниматься сам, даже если облачный конфиг Яндекса
снова выставит `adb_enabled=0`. Это делает `adbguard` + init-сервис.

## 2.1. Закинуть файлы

```bash
adb -s IP_СТАНЦИИ:5555 shell 'rm -rf /data/local/tmp/station-max-extras'
adb -s IP_СТАНЦИИ:5555 push files/extras /data/local/tmp/station-max-extras
```

Проверка:

```bash
adb -s IP_СТАНЦИИ:5555 shell 'ls -l /data/local/tmp/station-max-extras'
```

## 2.2. Установить (от root)

```bash
cd files/hybrid-1.2.4
ADB='C:/android/platform-tools/adb.exe' \
ANDROID_SERIAL=IP_СТАНЦИИ:5555 \
./station-max.sh cmd 'sh /data/local/tmp/station-max-extras/install-extras.sh'
```

Скрипт:
- кладёт `adbguard.sh`, `hosts-block`, `adbd_tcp.rc` в `/odm`;
- ставит правильные SELinux-контексты;
- сразу включает `adb_enabled=1`, `service.adb.tcp.port=5555`;
- удаляет/отключает апдейтер Яндекса;
- (если рядом есть `absshd` + `authorized_keys`) ставит root SSH.

## 2.3. Перезагрузка и проверка

```bash
adb reboot
# подождать 60–90 секунд
adb connect IP_СТАНЦИИ:5555
adb -s IP_СТАНЦИИ:5555 shell sh /data/local/tmp/station-max-extras/verify-extras.sh
```

Ожидается:

```text
adb_enabled=1
adbd=running
adbguard=running
tcp_port=5555
updater=none
hosts=bind
```

Если `adbguard=stopped` — init не подхватил rc (проверьте, что файл лежит в
`/odm/etc/init/adbd_tcp.rc` и станция перезагружена). Если `updater` всё ещё
в списке — запустите установку ещё раз.

---

# Часть 3. Блокировка OTA

Три уровня (все уже входят в extras, кроме DNS):

1. **hosts bind-mount** — `/odm/etc/hosts-block` подменяет `/system/etc/hosts`.
   Блокируются только:
   ```text
   quasar.s3.yandex.net
   updater.mobile.yandex.net
   ota.yandex.net
   ota.yandex.ru
   ```
   `quasar.yandex.net` не блокируется — иначе отвалится Алиса/музыка.
2. **Апдейтер** — `pm uninstall --user 0 com.yandex.launcher.updaterapp`
   (или `disable-user`, если uninstall недоступен).
3. **DNS** — блок из части 0.3, если DNS-сервер остаётся в сети.

Проверка (внутри станции):

```bash
adb shell ping -c1 -W1 ota.yandex.net      # 127.0.0.1
adb shell ping -c1 -W1 quasar.yandex.net   # реальный IP, НЕ 127.0.0.1
adb shell pm list packages --user 0 | grep updater   # пусто
adb shell 'grep " /system/etc/hosts " /proc/mounts'  # есть строка
```

Ограничения: это best-effort. Уже скачанные обновления, кэшированные IP и
другие каналы могут остаться. OTA может перезаписать `/odm` — после OTA
повторите части 1–3.

---

# Часть 4. Root SSH (опция)

SSH удобен, когда ADB капризничает. Работает по ключу, порт `2222`.

## 4.1. Свой ключ

```bash
ssh-keygen -t ed25519 -f station_key
# Windows Git Bash:
cp station_key.pub files/extras/authorized_keys
# или: type station_key.pub > files\extras\authorized_keys
```

Приватный `station_key` остаётся у вас. В архив его класть нельзя.

## 4.2. Установка

```bash
adb -s IP_СТАНЦИИ:5555 push files/extras/authorized_keys /data/local/tmp/station-max-extras/
adb -s IP_СТАНЦИИ:5555 push files/extras/absshd /data/local/tmp/station-max-extras/

cd files/hybrid-1.2.4
ADB='C:/android/platform-tools/adb.exe' \
ANDROID_SERIAL=IP_СТАНЦИИ:5555 \
./station-max.sh cmd 'sh /data/local/tmp/station-max-extras/install-ssh.sh'
adb reboot
```

Подключение:

```bash
ssh -p 2222 -i station_key root@IP_СТАНЦИИ
```

Хост-ключ SSH генерируется автоматически при первом запуске в
`/data/local/tmp/absshd/host_ed25519` (поэтому он переживает ребут, но не
полный сброс `/data`).

---

# Проверка «холодной» перезагрузки

Выключите/перезагрузите станцию и подождите 60–90 секунд.

```bash
adb connect IP_СТАНЦИИ:5555
adb devices -l
adb shell getprop init.svc.adbd          # running
adb shell getprop init.svc.adbguard      # running
adb shell settings get global adb_enabled # 1
adb shell getprop service.adb.tcp.port   # 5555
```

Root:

```bash
cd files/hybrid-1.2.4
ADB='C:/android/platform-tools/adb.exe' ANDROID_SERIAL=IP_СТАНЦИИ:5555 ./station-max.sh status
# HEALTHY / PROTECTED
```

SSH (если ставили):

```bash
ssh -p 2222 -i station_key root@IP_СТАНЦИИ 'id; getenforce'
# uid=0(root); Permissive
```

---

# Откат

1. Снять extras (root обязателен):

```bash
./station-max.sh cmd 'sh /data/local/tmp/station-max-extras/uninstall-extras.sh'
adb reboot
```

2. Вернуть апдейтер (если нужно):

```bash
adb shell pm install-existing --user 0 com.yandex.launcher.updaterapp
```

3. Снять root:

```bash
./station-max.sh rollback
```

4. Остановить `lite-kit` и вернуть DNS роутера, если больше не нужны.

Полный factory reset не рекомендуется: он не «откатывает» root, но стирает
ADB-ключи и данные. Если всё же нужен — сначала `rollback`, потом сброс.

---

# Если что-то отвалилось

**ADB пропал после ребута**
- Подождите 30–60 секунд: `adbguard` проверяет раз в 10 секунд.
- Проверьте через SSH (если есть): `getprop init.svc.adbguard`,
  `settings get global adb_enabled`.
- Если extras не установлены — снова поднимите `lite-kit` и перезагрузите.

**ADB `unauthorized` / `offline`**
- `adb kill-server; adb start-server; adb connect IP:5555`.
- Экранного подтверждения у Station Max нет: проверьте, что на станции
  авторизован ваш ADB-ключ (через root/SSH), затем повторите `adb connect`.
- `adb devices -l` — проверьте, что это нужное устройство.

**SSH не поднимается**
- `adb shell ls -l /data/local/tmp/absshd/` и лог `sshd.log` (root-only).
- Проверьте `authorized_keys` (одна строка, без лишних переводов).
- Перезагрузите станцию; хост-ключ пересоздастся, если `/data` цел.

**rootd не в порядке**
- `./station-max.sh status` → смотрите состояние;
- `./station-max.sh verify` → какой пункт FAIL;
- `./station-max.sh rollback` — откат;
- подробности в `files/hybrid-1.2.4/TROUBLESHOOTING.md`.

**OTA всё равно качается**
- Проверьте hosts-mount и DNS;
- проверьте, что апдейтер действительно удалён;
- помните: OTA может перезаписать `/odm` — после обновления повторите установку.

**Станция ушла в bootloop**
- Не паниковать: подключите TV, проверьте лог;
- если недавно ставили extras — `uninstall-extras.sh` через rootd, если он жив;
- если rootd жив — `./station-max.sh rollback`;
- не отключайте `com.yandex.tv.services.platform`.

---

# Состав архива

```text
README.md                     этот гайд
CHECKLIST.md                  короткий чек-лист
NOTICE.md                     источники, авторы, дисклеймер
SHA256SUMS.txt                контрольные суммы файлов

files/lite-kit/               стенд первого ADB: DNS + HTTPS + proxy sync
  start.ps1 / start.sh         запуск (Windows / Linux/macOS)
  stop/check/diag              остановка, проверка, диагностика
  scripts/                     сервер, sync_template.json, тесты
  certs/                       публичные сертификаты eQuasar
  config/dnsmasq.conf.tpl      шаблон DNS для Linux/macOS
  docs/sync-adb.md             как включается ADB, в т.ч. на Максе
  README.md, LICENSE           описание и MIT-лицензия
  (firmware/, log/, run/ в архив не входят)

files/hybrid-1.2.4/           root-кит hybrid 1.2.4 (без изменений)
  station-max.sh              install/status/verify/rollback/diag/cmd
  hybrid/                     rootd, permissive, скрипты установки/отката
  bootstrap/root-bridge       первичный root (CVE-2022-36449)
  README.md, ADB-GUIDE.md, TROUBLESHOOTING.md, OTHER-FIRMWARE.md

files/extras/
  adbd_tcp.rc                 init: ADB TCP + adbguard
  adbd_ssh.rc                 init: ADB TCP + adbguard + absshd
  adbguard.sh                 сторож ADB + hosts
  hosts-block                 OTA-домены
  install-extras.sh           установка фиксации (root)
  install-ssh.sh              установка/обновление SSH (root)
  uninstall-extras.sh         снятие фиксации (root)
  verify-extras.sh            проверка ADB/OTA (без root)
  absshd                      root SSH-сервер (arm64, Go static)
  authorized_keys.example     куда положить свой публичный ключ

net/                          альтернатива (на Максе не проверялась)
  nginx-quasar.conf.example   статический /get_sync_info
  dnsmasq-quasar.conf.example dnsmasq: подмена + блок OTA

tools/
  scan_station.py             поиск станции по портам 5555/2222
```

Контрольные суммы:

```bash
sha256sum -c SHA256SUMS.txt        # Linux/macOS
# Windows PowerShell: Get-FileHash -Algorithm SHA256 <файл>
```

---

# Благодарности и источники

- **lite-kit** — наш стенд для первого ADB (DNS + HTTPS + проксирование
  `/get_sync_info`). Код MIT, в архив входит без `firmware/`, `log/` и `run/`.
- **eQuasar** — источник публичных сертификатов и идеи подмены
  `/get_sync_info` (commit `02f3c583a7d7335fc8f3f7d511ea795590b41341`).
  Сертификаты лежат в `files/lite-kit/certs/`.
- **hybrid 1.2.4** — сторонний root-кит для Station Max 0921, включён
  без изменений (см. `files/hybrid-1.2.4/README.md`).
- **Обвязка** (adbguard, hosts, init-сервисы, SSH, этот гайд) — собрана для
  YNDX-0008, распространяется как есть.

Если присылаете логи в чат — уберите IP, MAC, серийники, токены и ключи.
