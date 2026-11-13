#!/system/bin/sh
# Добавить/обновить только root SSH (absshd) поверх уже установленных extras.
# Запускать от root:
#   ./station-max.sh cmd 'sh /data/local/tmp/station-max-extras/install-ssh.sh'
set -eu

BASE=/data/local/tmp/station-max-extras
[ -f "$BASE/absshd" ] || { echo "нет $BASE/absshd"; exit 1; }
[ -f "$BASE/authorized_keys" ] || { echo "нет $BASE/authorized_keys"; exit 1; }
[ -f "$BASE/adbd_ssh.rc" ] || { echo "нет $BASE/adbd_ssh.rc"; exit 1; }

mount -o rw,remount /odm

cp "$BASE/absshd" /odm/bin/absshd
chown 0:0 /odm/bin/absshd
chmod 0755 /odm/bin/absshd
chcon u:object_r:shell_exec:s0 /odm/bin/absshd

mkdir -p /odm/etc/absshd
cp "$BASE/authorized_keys" /odm/etc/absshd/authorized_keys
chown 0:0 /odm/etc/absshd/authorized_keys
chmod 0600 /odm/etc/absshd/authorized_keys
chcon u:object_r:vendor_configs_file:s0 /odm/etc/absshd/authorized_keys

cp "$BASE/adbd_ssh.rc" /odm/etc/init/adbd_tcp.rc
chown 0:0 /odm/etc/init/adbd_tcp.rc
chmod 0644 /odm/etc/init/adbd_tcp.rc
chcon u:object_r:vendor_configs_file:s0 /odm/etc/init/adbd_tcp.rc

mount -o ro,remount /odm
echo "SSH установлен. Перезагрузите станцию и подключайтесь:"
echo "  ssh -p 2222 -i <ваш_ключ> root@IP_СТАНЦИИ"
