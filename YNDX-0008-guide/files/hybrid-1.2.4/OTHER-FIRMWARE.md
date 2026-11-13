# Другая прошивка: проверка и портирование

Этот архив поддерживает **только**:

```text
PPR1.180610.011.5000000921
Yandex/yandexstation_2/yandexstation_2:9/PPR1.180610.011/5000000921:user/release-keys
Linux 4.9.113
```

Любая другая сборка, включая более старые, — **не поддерживается**.
Готовых бинарников для неё здесь нет и быть не может: `root-bridge` и
`permissive` привязаны к layout, адресам и kernel конкретной сборки.

## 1. Сначала проверить

Из корня распакованного архива:

```sh
adb shell getprop ro.build.display.id
adb shell getprop ro.build.fingerprint
adb shell uname -r
adb shell getenforce
adb shell id
adb shell cat /proc/stat | grep btime
```

Если build/fingerprint/kernel не совпали с профилем `0921` — **стоп**.
Не запускать `bootstrap/root-bridge`, `hybrid/permissive` и
`station-max.sh install`: на чужой сборке `root-bridge` может упасть в
panic, а установщик всё равно откажется по build gate, но уже после
лишнего риска. Установщик читает точную сборку, fingerprint и kernel до
локальных хешей и до любой доставки; ошибка чтения свойства означает
«проверка не выполнена», а не успех.

`rootd` сам умеет fail closed по build gate, но это защита **после** первого
root. Она не делает чужие бинарники безопасными.

## 2. Что собрать для портирования

Если есть желание адаптировать метод под свою сборку, нужен не «попробовать
наш бинарник», а новый профиль. Минимальный набор данных:

```sh
adb shell getprop ro.build.display.id
adb shell getprop ro.build.fingerprint
adb shell cat /proc/version
adb shell getprop ro.product.device
adb shell getprop ro.build.version.release
adb shell getenforce
adb shell mount | grep ' /odm '
adb shell ls -lZ /odm/bin /odm/etc 2>/dev/null
```

Read-only наблюдения можно снимать обычным `adb shell`.

Runtime ZIP не содержит исходников и готового установщика для других
сборок. Для портирования нужен отдельный source-архив проекта.

## 3. Порядок портирования

Это лабораторная работа, а не установка «в один клик».

1. **Первый root для своей сборки.** Нужен отдельный проверенный путь.
   Нельзя использовать 0921-бинарники и нельзя писать в boot/vbmeta.
2. **Read-only probe.** Проверить, применим ли 36449 к вашему kernel/Mali:
   версия драйвера, доступ к `/dev/mali0`, поведение ioctl. Только чтение.
3. **Dump/kallsyms.** Снять образ, разобрать `init_task`, `init_cred`,
   `selinux_enforcing`, проверить markers/token table. Адреса boot-specific.
4. **Свой permissive.** Собрать инструмент под свою сборку, добавить точный
   build gate: display id + fingerprint + kernel. Проверить `check` и
   read-back до записи.
5. **Профиль rootd.** Добавить в исходник rootd запись профиля с точными
   значениями и пересобрать. Без профиля rootd вернёт `UNSUPPORTED`.
6. **Поэтапный тест.** Сначала root-only boot, затем один `permissive`,
   затем B2, DNS/DoT, очередь.
7. **5 холодных reboot.** Каждый boot: `PROTECTED`, UID 0, B2, DNS/DoT,
   `/odm ro`, один rootd, без второго канала.
8. **Откат.** Проверить backup/restore до того, как считать профиль готовым.

## 4. Чего не будет в этом архиве

- готового `permissive` под неизвестную сборку;
- готового `root-bridge` под неизвестную сборку;
- разблокировки загрузчика;
- записи в boot/vbmeta/radio;
- обхода AVB/verity;
- «универсального» бинарника, который сам подберёт адреса.

Если для вашей сборки нет проверенного первого root — архив не поможет.
Лучше остаться на stock, чем запускать чужие Mali-бинарники вслепую.

## 5. Если сборка 0921, но что-то не сходится

Проверьте:

- `ro.build.fingerprint` совпадает полностью, а не только `5000000921`;
- kernel именно `4.9.113`;
- ADB shell именно UID 2000, а не root;
- `/odm` существует и смонтирован;
- SHA256 скачанного ZIP совпадает с опубликованным `.sha256`.

Если после установки `rootstatus` показывает `UNSUPPORTED` — не продолжать,
выполнить откат и разобрать, какой из трёх параметров не совпал.
