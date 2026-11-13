# Ошибки и recovery (1.2.4)

Команды — из корня распакованного архива `station-max-hybrid-1.2.4`.

## Быстрая диагностика

```sh
./station-max.sh status
./station-max.sh verify
./station-max.sh diag
```

`diag` прикладывает ZIP с безопасным набором полей, если `zip` установлен.
Без `zip` он печатает путь к каталогу с теми же файлами. Не отправляйте в
чат `rootout`, `rootstatus` целиком, `logcat` и вывод `ls -lZ`: там могут
быть адреса и пути.

## Частые ошибки

### `reader bring-up failed` / `cycle 1: no reclaim after 24 blocks`

Это **известный флейк первого Mali-цикла**, а не признак чужой или
сломанной станции. Эксплойт освобождает страницу AIO ring и ждёт, что её
заберёт свежая PTE-таблица того же процесса; на занятой станции или сразу
после загрузки это может не произойти много раз подряд. Ошибка выглядит
так:

```text
commit pinned rc=0 errno=0 value=1
commit after-release rc=0 errno=0 value=0
cycle 1: ring freed (atoms 2/3)
cycle 1: no reclaim after 24 blocks
RESULT: reader bring-up failed
```

В 1.2.4 установщик повторяет попытку сам: до 20 раз, с растущими паузами,
бюджет 600 секунд, и каждая попытка только после проверки, что нет
второго канала, живого lock, чужого in-flight маркера и `/odm` остался ro.
В логе видно `bootstrap attempt 7/20` и причину прошлой попытки.

Пока установка не дошла до backup, на устройстве **ничего не записано**,
поэтому прерывание на этом шаге безопасно.

Повторяется только полный захват, который заканчивается строкой
`RESULT: reader bring-up failed` или `RESULT: scan phase failed; NO
writes done`. Одинокая строка `cycle N: no reclaim ...` без терминальной
строки считается обрывом и не повторяется. Решение устройства — чужой файл в `/odm`, несовпадение
хеша, нехватка места — не повторяется: установка останавливается сразу и
печатает сообщение устройства.

Пустой вывод, вывод без терминальной строки, неизвестная `RESULT:`-строка,
признак обрыва транспорта в stderr, per-call timeout и вывод после
`shell: exec` автоматически не повторяются:
результат попытки неизвестен, а установщик после `exec /system/bin/sh`
называется `sh` и до device lock/записи state не виден гейту имён. В этом случае нужен ручной `status` и новый `install` только после того,
как `status` доказал отсутствие второго канала.

Если все 20 попыток не прошли:

```sh
./station-max.sh status
./station-max.sh diag
adb shell 'free -m; cat /sys/kernel/mm/transparent_hugepage/enabled'
```

Установщик сообщает то же самое и не пишет в `/odm`. Тогда причина не в
удаче: смотрите свободную память, THP и нагрузку на станцию, а не число
попыток. Не запускайте установку во время воспроизведения, установки
приложений или сразу после загрузки — дайте станции успокоиться.

Настроить иначе:

```sh
BOOTSTRAP_ATTEMPTS=20 BOOTSTRAP_BACKOFF='2 3 5 5 10 10 15 20' BOOTSTRAP_BUDGET=600 \
  ./station-max.sh install --router-ip ... --dns-ip ...
```

### `unsupported build`

Build/fingerprint/kernel не совпали. Ничего не записано. Архив только для
0921.

### `a second root/bootstrap channel is present: NAME(pid N)`

В `ps` найден процесс с именем `root-bridge`, `esc-bridge`, `esc36449`
или `cve36449`. В 1.2.4 сообщение называет **что именно** найдено, а не
только список ожидаемых имён.

Отдельный случай: `esc36449` — это имя, которое берёт себе
`/odm/bin/permissive`, в том числе когда его запускает сам rootd при
загрузке (до 8 попыток по 10 с). Если rootd жив и его состояние
`BOOTSTRAPPING`/`ROOT_ONLY`, установщик ждёт до `CHANNEL_WAIT=60` секунд
и перепроверяет, а не отказывает навсегда. Остальное — настоящий второй
канал: не запускайте install/rollback и новый bootstrap, сначала reboot,
затем `status`. Если канал остался после reboot — разбираться.

```sh
adb shell 'ps -A -o PID,PPID,NAME | grep -E "root-bridge|esc-bridge|esc36449|cve36449"'
adb shell cat /data/local/tmp/rootstatus
```

### `operation state is corrupt`

`/data/local/tmp/rootd-op.state` повреждён или не является обычным файлом.
Установщик не перезаписывает его. Сохраните файл для разбора; не удаляйте
вслепую, иначе потеряется путь к original backup.

### `BASE already exists`

Staging создаётся в новом `/data/local/tmp/station-max-hybrid.XXXXXX`.
Пользовательский `BASE` должен быть новым. Чужой каталог не удаляется.

### `invalid install.conf`

Конфиг читается как данные. Нужны ровно три строки:

```text
ROUTER_IP=192.0.2.1
DNS_IP=192.0.2.53
IPV6=none
```

Многострочные значения, пробелы, `;`, `$`, кавычки и неизвестные ключи
отклоняются. Ошибка конфига не меняет state.

### `pre-backup phase` / `install already started`

- `phase=install_started` или `backup_started`: `/odm` ещё не изменён;
  ручной повторный `install` безопасно продолжит прерванную операцию и
  переиспользует original backup. Автоматический retry внутри того же
  запуска разрешён только при неизменном state и известной строке
  Mali-флейка: если прошлая попытка успела записать фазу, установщик не
  повторяет её сам.
- `phase=backup_complete`, `writing`, `files_written`, `verified`,
  `failed` с backup: новый bootstrap не запускать; `status`, затем
  `rollback`.

```sh
adb shell cat /data/local/tmp/rootd-op.state
```

### `ADB was lost` / `attempt timed out` / `operation timed out`

- `attempt timed out` — per-call timeout (ADB не ответил за отведённое
  время), а не исчерпание `OP_TIMEOUT`. Установщик не ретраит такую
  попытку автоматически: результат неизвестен.
- пустой вывод после обрыва — тоже неизвестный результат; авторетрая нет:
  после `exec /system/bin/sh` установщик называется `sh` и не виден гейту
  имён, пока не возьмёт device lock или не запишет state.
- если ADB вернулся, установщик перечитывает `boot_id`; при смене boot
  заново проверяет identity, lock и in-flight маркер на новом boot.
  Продолжение возможно только для полного захвата с терминальной строкой
  Mali-отказа.
- если ADB не вернулся — reconnect и `status`; повторный `install` только
  после того, как `status` доказал отсутствие второго канала. Если state
  показывает запись — `rollback`. `OP_TIMEOUT` не даёт
  `VERIFY PASS`/`HEALTHY`.

### `STATE: IN-FLIGHT` / `in-flight marker from this boot remains`

Маркер означает, что bootstrap был прерван и его процесс мог остаться
живым под именем `sh`, которое гейт четырёх имён не видит. `status`
показывает `operation_id`, `boot_id`, `stage`, `created` и возраст.
Preflight не удаляет маркер текущего boot автоматически.

Проверьте `status` и `ps`; если установка точно не идёт, удалите маркер
явно:

```sh
./station-max.sh status
adb shell 'rm -f /data/local/tmp/rootd-op.inflight'
# если rootd уже поднят, то же можно сделать через очередь:
./station-max.sh cmd 'rm -f /data/local/tmp/rootd-op.inflight'
```

Маркер прошлого boot удаляется сам после проверки state: процесс не мог
пережить reboot. Если state уже прошёл backup, сначала `rollback`, а не
новый `install`.

### `/odm may still be rw`

Вернуть ro и проверить:

```sh
adb shell "mount -o ro,remount /odm"
adb shell "mount | grep ' /odm '"
```

Успешное завершение при rw не объявляется.

### `queue timeout` / `refused` / `cleanup=incomplete`

```sh
adb shell cat /data/local/tmp/rootstatus
adb shell 'tail -20 /data/local/tmp/rootout'
```

Не удаляйте `rootout` во время работы. Обрезанный блок, чужой `boot_id` и
неполный cleanup не считаются успехом.

## rootd есть, но очередь не отвечает

Не запускайте второй bootstrap. Проверьте:

```sh
adb shell cat /data/local/tmp/rootstatus
adb shell cat /proc/sys/kernel/random/boot_id
adb shell 'ps -A -o NAME | grep -E "rootd|root-bridge|esc-bridge|esc36449|cve36449"'
```

Если статус старый — reboot и `status`. Если после reboot rootd не отвечает,
recovery только после остановки rootd: `rollback --bootstrap`.

## Нет `PROTECTED` после reboot

```sh
adb shell cat /data/local/tmp/rootstatus
adb shell 'ps -A -o PID,NAME | grep -E "rootd|root-bridge|esc-bridge|esc36449|cve36449"'
```

- `ROOT_ONLY`, `bootstrap-pending` — подождать 20–40 секунд.
- `UNSUPPORTED` — откат.
- rootd нет — откат через bootstrap.
- второй канал — остановиться и разобраться.

## Откат

```sh
./station-max.sh rollback
```

Если state нет:

```sh
./station-max.sh rollback /data/local/tmp/rootd-backup-...
```

Изменённый `rootd.conf` не принимается автоматически:

```sh
./station-max.sh rollback --replace-config
```

Изменённый файл сохраняется в `/data/local/tmp/rootd-conf-replaced.*`.
Старый backup без `COMPLETE` — только с `--allow-unmarked`.

## Что не делать

- Не перепрошивать boot/vbmeta/radio.
- Не запускать `root-bridge` вручную при живом rootd.
- Не удалять `/odm/bin/rootd` при работающем rootd: можно получить EBUSY
  на remount ro и потребуется reboot.
- Не отправлять в чат полный `rootstatus`/`rootout`.
