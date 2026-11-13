# Station Max Hybrid Root — 0921 (1.2.4)

Root + Permissive для **Яндекс Станции Макс** `yandexstation_2`, сборка
`PPR1.180610.011.5000000921`, kernel `4.9.113`. После холодного boot rootd
из `/odm` сам поднимает Permissive, B2 и DNS/DoT. Роутер и Mac для этого
не нужны.

Это не boot-патч, не `adb root`, не разблокировка vbmeta и не отключение
OTA. Другие сборки не поддерживаются; путь их проверки и портирования
описан в [OTHER-FIRMWARE.md](OTHER-FIRMWARE.md).

## Проверка ZIP

Рядом с архивом лежит `station-max-hybrid-1.2.4.zip.sha256`:

```sh
shasum -a 256 station-max-hybrid-1.2.4.zip
```

Сверьте строку с файлом `.sha256`. Это проверяет, что ZIP передан без
повреждений и не был подменён.

## Установка

Нужны установленный `adb` и уже работающий ADB shell станции. Включение
ADB установщик не выполняет.

Установка 1.2.4 проверена live на Windows Git Bash и WSL Linux; на macOS
прогнан полный офлайн-набор. ADB-клиент на Windows работал напрямую с
`192.168.0.54:5555`: `adb connect`, `status` и `verify` прошли без Mac и
промежуточного туннеля.

**macOS, Linux, WSL.** Нужен POSIX-терминал и `adb` в `PATH`:

```sh
adb shell id
# ожидается uid=2000(shell)
./station-max.sh install --router-ip ... --dns-ip ... --yes
```

WSL — это обычный Linux-путь; отдельный Windows-ADB не нужен, если `adb`
доступен внутри WSL.

**Windows.** Только **Git Bash** (MINGW64); из PowerShell и `cmd.exe`
скрипт не запускается. Нужен `adb.exe`; если его нет в `PATH`:

```sh
ADB='C:/android/platform-tools/adb.exe' ./station-max.sh status
ADB='C:/android/platform-tools/adb.exe' ./station-max.sh install --router-ip ... --dns-ip ... --yes
```

```sh
unzip station-max-hybrid-1.2.4.zip -d station-max-hybrid-1.2.4
cd station-max-hybrid-1.2.4
./station-max.sh install --router-ip 192.168.1.1 --dns-ip 192.168.1.53
```

`192.168.1.1` — роутер, `192.168.1.53` — адрес, DNS-запросы к которому
нужно перенаправить на роутер. Примеры заменить на свои. `--ipv6 none`
по умолчанию; `--ipv6 drop` добавляет блокировку IPv6 DoT.

Установщик:

1. проверяет build / fingerprint / kernel;
2. проверяет хеши `root-bridge`, `rootd`, `permissive`, `rootd.rc`;
3. проверяет, что нет второго root-канала (`root-bridge`, `esc-bridge`,
   `esc36449`, `cve36449`), живого lock и чужого in-flight маркера;
4. поднимает первый root: `root-bridge esc-shell` → Permissive; попыток
   до 20, с паузами, и каждая только после повторной проверки пункта 3;
   повтор — только при терминальной строке отказа и без признаков обрыва
   транспорта в stderr;
5. делает backup четырёх файлов в `/data/local/tmp/rootd-backup-*`;
6. пишет четыре файла в `/odm`, проверяет их и возвращает `/odm` в ro;
7. перезагружает станцию и ждёт `PROTECTED`, затем запускает `verify`.

Ожидаемый финал:

```text
INSTALL PASS
```

Первый Mali-цикл (`CVE-2022-36449`) вероятностный: на занятой или только
что загруженной станции он может не пройти с первого раза. Поэтому
установщик сам повторяет попытку до 20 раз с растущими паузами и печатает
`bootstrap attempt 3/20`; до backup на устройстве ничего не пишется, так
что прерванная на этом шаге установка не портит станцию. Настройки, если
нужно иначе:

```sh
BOOTSTRAP_ATTEMPTS=20 BOOTSTRAP_BACKOFF='2 3 5 5 10 10 15 20' BOOTSTRAP_BUDGET=600 \
  ./station-max.sh install --router-ip ... --dns-ip ...
```

Если не нужен автоматический reboot:

```sh
./station-max.sh install --router-ip ... --dns-ip ... --no-reboot
adb reboot
./station-max.sh verify
```

## Команды

```sh
./station-max.sh status
./station-max.sh verify
./station-max.sh rollback
./station-max.sh diag
./station-max.sh cmd 'id; getenforce'
```

`status` различает `HEALTHY`, `BOOTING`, `BOOTSTRAPPING`,
`STUCK-ROOT-ONLY`, `DEGRADED`, `UNSUPPORTED`, `UNSUPPORTED-BUILD`,
`LEGACY-SELECTED`, `NO-ROOTD`, `SECOND-CHANNEL`, `ODM-RW`,
`NOT-INSTALLED`, `NO-STATUS`, `STALE-STATUS`, `IN-FLIGHT`, `UNKNOWN`.
`HEALTHY` выдаётся только после текущих проверок, общих с `verify`, и
только при отсутствии in-flight маркера; `status` показывает его
`operation_id`, `boot_id`, `stage`, `created` и возраст.

`verify` проверяет `boot_id`, `PROTECTED`, `getenforce`, ровно один rootd
без второго канала, `/odm ro`, хеши и metadata четырёх файлов, B2,
DNS DNAT и DoT DROP для TCP/UDP, IPv6 и очередь. Любой FAIL даёт
`VERIFY FAIL` и ненулевой код.

`diag` собирает ZIP для отправки в чат: только build/kernel, `boot_id`,
известные поля `rootstatus`, состояние mount, количество процессов, хеши
и metadata четырёх файлов. Свободный `reason`, адреса, токены и `rootout`
не попадают. Если команды `zip` нет, как в чистом Git Bash, остаётся
каталог с теми же файлами и печатается его путь. На Unix каталог 0700,
файлы 0600; на Windows действуют ACL пользовательского temp-каталога.

## Что меняется

```text
/odm/bin/rootd
/odm/bin/permissive
/odm/etc/init/rootd.rc
/odm/etc/rootd.conf
```

Backup и состояние операции:

```text
/data/local/tmp/rootd-backup-<timestamp>/
/data/local/tmp/rootd-op.state
/data/local/tmp/rootd-op.lock
```

Путь backup и фаза хранятся в state. Повтор использует тот же backup и не
создаёт новый «исходный» из уже изменённого `/odm`. Ошибка конфига,
build gate, `ps`, mount или занятый lock не перезаписывают state.
Повреждённый state останавливает работу, а не сбрасывается.

Установка через bootstrap делает до 20 попыток (по умолчанию), и каждая
начинается только после повторной проверки каналов, lock, in-flight
маркера и state. Перед первой попыткой публикуется
`/data/local/tmp/rootd-op.inflight`; маркер уникален парой
`operation_id` + `staging_nonce` и не перезаписывает чужой. Автоматически
повторяется только чистый ненулевой rc с известной строкой Mali-флейка;
пустой, неизвестный вывод или вывод после `shell: exec` не ретраится
(после `exec /system/bin/sh`
установщик называется `sh` и не виден гейту четырёх имён), а per-call
timeout печатается как «attempt timed out», не как `OP_TIMEOUT`.

## Откат

```sh
./station-max.sh rollback
```

С завершённой установкой путь backup берётся из state. Если state нет,
укажите путь явно:

```sh
./station-max.sh rollback /data/local/tmp/rootd-backup-...
```

Перед remount rw проверяются manifest и **точный** текущий набор из
четырёх хешей. Изменённый `rootd.conf` останавливает откат. Если изменение
намеренное:

```sh
./station-max.sh rollback --replace-config
```

Скрипт сначала сохранит изменённый файл в
`/data/local/tmp/rootd-conf-replaced.XXXXXX`, затем восстановит backup.
Старый backup без `COMPLETE` принимается только с `--allow-unmarked`.

Если rootd не работает:

```sh
./station-max.sh rollback --bootstrap /data/local/tmp/rootd-backup-...
```

`--bootstrap` разрешён только когда rootd не запущен. После reboot
проверяется ожидаемое состояние: предыдущий гибрид или stock.

## Recovery

- Оборвалось до backup: `/odm` ещё не изменён, повторный `install`
  безопасно создаст original backup. Если прошлая попытка вернула непустой
  известный Mali-флейк, установщик повторяет её сам; после пустого вывода
  или timeout авторетрая нет — сначала `status`, затем ручной `install`.
- Оборвалось после backup: новый bootstrap не запускать; `status`, затем
  `rollback`.
- Остался `rootd-op.inflight`: следующий `install` удалит его только после
  `ps`, доказавшего отсутствие второго канала.
- Появился `esc36449`/`esc-bridge`/`cve36449`/`root-bridge`: остановиться,
  reboot, `status`; не запускать новый bootstrap.

Подробности — [TROUBLESHOOTING.md](TROUBLESHOOTING.md).

## Ограничения

- Только 0921. На другой сборке бинарники не запускать.
- OTA может перезаписать `/odm`; после OTA установку повторить.
- Permissive не переживает reboot сам по себе — его поднимает rootd.
- Bootloader, vbmeta, radio/modem, custom kernel/ROM не затрагиваются.
- SSH/SFTP, сетевые порты и постоянный SELinux-off не входят.
- Возможны panic/GPU-hang на Mali-пути. Только свой стенд.
- Не выдёргивайте питание во время установки. Прерванную операцию
  восстанавливайте по разделу Recovery.

**AS IS, no warranty.**

## Состав

```text
station-max.sh             install/status/verify/rollback/diag/cmd
bootstrap/root-bridge      первый root, 0921
hybrid/common.sh           общие проверки, state, lock, backup
hybrid/rootd               постоянный демон
hybrid/permissive          Permissive helper, 0921
hybrid/rootd.rc            init-сервис
hybrid/rootd.conf.example  пример конфига
hybrid/install-run.sh      bootstrap-обёртка install
hybrid/install-device.sh   установка на станции
hybrid/rollback-run.sh     bootstrap-обёртка rollback
hybrid/rollback-device.sh  откат на станции
README.md                  установка и команды
TROUBLESHOOTING.md         ошибки и recovery
ADB-GUIDE.md               как получить ADB на станции (DNS/Nginx)
OTHER-FIRMWARE.md          проверка и чек-лист портирования другой сборки
```
