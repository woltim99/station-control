import android.bluetooth.BluetoothAdapter;
import android.bluetooth.BluetoothDevice;
import android.content.Intent;
import android.content.IntentFilter;
import android.database.Cursor;
import android.database.sqlite.SQLiteDatabase;
import android.os.Binder;
import android.os.Looper;
import android.os.Parcel;

import java.lang.reflect.InvocationHandler;
import java.lang.reflect.Method;
import java.lang.reflect.Proxy;
import java.util.LinkedHashMap;
import java.util.Map;
import java.util.Set;

/**
 * Маленький helper для панели: запускается через app_process от root и умеет
 * то, чего нет в shell: поиск (BLE), сопряжение, подключение и удаление.
 * Вывод — одна JSON-строка в stdout.
 */
public class BtHelper {
    private static BluetoothAdapter adapter;

    public static void main(String[] args) {
        try {
            try {
                Looper.prepareMainLooper();
            } catch (Throwable ignored) {
            }
            adapter = BluetoothAdapter.getDefaultAdapter();
            if (adapter == null) {
                out("{\"error\":\"bluetooth adapter unavailable\"}");
                return;
            }
            String cmd = args.length > 0 ? args[0] : "info";
            if ("scan".equals(cmd)) {
                scan(args.length > 1 ? Integer.parseInt(args[1]) : 10);
            } else if ("classicscan".equals(cmd)) {
                classicScan(args.length > 1 ? Integer.parseInt(args[1]) : 10);
            } else if ("pair".equals(cmd) && args.length > 1) {
                pair(args[1]);
            } else if ("forget".equals(cmd) && args.length > 1) {
                forget(args[1]);
            } else if ("connect".equals(cmd) && args.length > 1) {
                connect(args[1], true);
            } else if ("disconnect".equals(cmd) && args.length > 1) {
                connect(args[1], false);
            } else if ("loc".equals(cmd)) {
                loc(args.length > 1 ? args[1] : "state");
            } else if ("uidtest".equals(cmd) && args.length > 1) {
                uidTest(Integer.parseInt(args[1]));
            } else if ("receivetest".equals(cmd)) {
                receiveTest(args.length > 1 ? Integer.parseInt(args[1]) : 8);
            } else if ("fscan".equals(cmd)) {
                filteredScan(args.length > 1 ? Integer.parseInt(args[1]) : 10);
            } else if ("iotname".equals(cmd) && args.length > 1) {
                iotName(args[1]);
            } else {
                info();
            }
        } catch (Throwable t) {
            Throwable cause = t;
            while (cause.getCause() != null) cause = cause.getCause();
            out("{\"error\":\"" + esc(cause.toString()) + "\"}");
        }
        System.exit(0);
    }

    private static void iotName(String fullID) {
        SQLiteDatabase db = null;
        Cursor c = null;
        try {
            db = SQLiteDatabase.openDatabase(
                    "/data/data/com.yandex.io.sdk/databases/iot-devices-database.db",
                    null, SQLiteDatabase.OPEN_READONLY);
            c = db.rawQuery(
                    "select d.name, ifnull(d.room_name,''), ifnull(h.name,'') " +
                            "from iot_device d left join iot_house h on h.id=d.house_id " +
                            "where d.id=? or instr(d.id, ?)=1 " +
                            "order by case when d.id=? then 0 else 1 end limit 1",
                    new String[]{fullID, fullID, fullID});
            if (c.moveToFirst()) {
                out("{\"name\":\"" + esc(c.getString(0)) + "\",\"room\":\"" + esc(c.getString(1))
                        + "\",\"house\":\"" + esc(c.getString(2)) + "\"}");
            } else {
                out("{\"error\":\"iot device not found\"}");
            }
        } catch (Throwable t) {
            out("{\"error\":\"" + esc(unwrap(t)) + "\"}");
        } finally {
            if (c != null) c.close();
            if (db != null) db.close();
        }
    }

    private static void info() {
        StringBuilder b = new StringBuilder();        b.append("{\"enabled\":").append(adapter.isEnabled());
        b.append(",\"name\":\"").append(esc(safe(adapter.getName()))).append("\"");
        b.append(",\"address\":\"").append(esc(safe(adapter.getAddress()))).append("\"");
        b.append(",\"devices\":[");
        boolean first = true;
        Set<BluetoothDevice> bonded = adapter.getBondedDevices();
        if (bonded != null) {
            for (BluetoothDevice d : bonded) {
                if (!first) b.append(',');
                first = false;
                b.append(deviceJson(d));
            }
        }
        b.append("]}");
        out(b.toString());
    }

    /**
     * BLE-поиск. В отличие от классического discovery, startLeScan не требует
     * Context и BroadcastReceiver, поэтому работает из app_process.
     * Android 9 не запускает незафильтрованный скан при выключенной
     * геолокации, поэтому на время скана включаем её и возвращаем как было.
     */
    private static void scan(int seconds) throws Exception {
        if (!adapter.isEnabled()) {
            out("{\"error\":\"bluetooth disabled\"}");
            return;
        }
        Object lm = null;
        boolean locationWasOn = true;
        String locationError = "";
        try {
            lm = locationManager();
            locationWasOn = locationEnabled(lm);
            if (!locationWasOn) {
                setLocationEnabled(lm, true);
                Thread.sleep(400);
            }
        } catch (Throwable t) {
            locationError = unwrap(t);
            lm = null;
        }
        final Object locationManager = lm;
        final boolean restoreLocation = lm != null && !locationWasOn;
        final boolean locationOnBefore = locationWasOn;
        final String locationErr = locationError;

        final LinkedHashMap<String, Map<String, Object>> found = new LinkedHashMap<String, Map<String, Object>>();
        final Object lock = new Object();

        Class<?> callbackClass = Class.forName("android.bluetooth.BluetoothAdapter$LeScanCallback");
        Object callback = Proxy.newProxyInstance(callbackClass.getClassLoader(), new Class<?>[]{callbackClass},
                new InvocationHandler() {
                    @Override
                    public Object invoke(Object proxy, Method method, Object[] args) {
                        String methodName = method.getName();
                        if ("onLeScan".equals(methodName) && args != null && args.length >= 2) {
                            BluetoothDevice d = (BluetoothDevice) args[0];
                            if (d == null) return null;
                            String mac = safe(d.getAddress());
                            if (mac.length() == 0) return null;
                            int rssi = args[1] instanceof Number ? ((Number) args[1]).intValue() : 0;
                            synchronized (lock) {
                                Map<String, Object> item = found.get(mac);
                                if (item == null) {
                                    item = new LinkedHashMap<String, Object>();
                                    item.put("mac", mac);
                                    found.put(mac, item);
                                }
                                String name = safe(d.getName());
                                if (name.length() > 0) item.put("name", name);
                                item.put("rssi", rssi);
                                item.put("type", typeOf(d));
                            }
                            return null;
                        }
                        if ("hashCode".equals(methodName)) return System.identityHashCode(proxy);
                        if ("equals".equals(methodName)) return proxy == (args != null && args.length > 0 ? args[0] : null);
                        if ("toString".equals(methodName)) return "BtLeScanCallback";
                        Class<?> rt = method.getReturnType();
                        if (rt == boolean.class) return Boolean.FALSE;
                        if (rt == int.class) return Integer.valueOf(0);
                        if (rt == long.class) return Long.valueOf(0L);
                        if (rt == short.class) return Short.valueOf((short) 0);
                        if (rt == byte.class) return Byte.valueOf((byte) 0);
                        if (rt == char.class) return Character.valueOf((char) 0);
                        if (rt == float.class) return Float.valueOf(0f);
                        if (rt == double.class) return Double.valueOf(0d);
                        return null;
                    }
                });

        Method startLeScan = BluetoothAdapter.class.getMethod("startLeScan", callbackClass);
        final Method stopLeScan = BluetoothAdapter.class.getMethod("stopLeScan", callbackClass);
        final Object cb = callback;
        startLeScan.invoke(adapter, callback);

        final int waitSeconds = Math.max(3, seconds);
        Thread worker = new Thread(new Runnable() {
            @Override
            public void run() {
                try {
                    Thread.sleep(waitSeconds * 1000L);
                } catch (InterruptedException ignored) {
                }
                try {
                    stopLeScan.invoke(adapter, cb);
                } catch (Throwable ignored) {
                }
                if (restoreLocation) {
                    try {
                        setLocationEnabled(locationManager, false);
                    } catch (Throwable ignored) {
                    }
                }
                StringBuilder b = new StringBuilder("{\"devices\":[");
                boolean first = true;
                synchronized (lock) {
                    for (Map<String, Object> item : found.values()) {
                        if (!first) b.append(',');
                        first = false;
                        b.append('{');
                        b.append("\"mac\":\"").append(esc((String) item.get("mac"))).append("\",");
                        b.append("\"name\":\"").append(esc((String) (item.get("name") != null ? item.get("name") : ""))).append("\",");
                        b.append("\"rssi\":").append(item.get("rssi") != null ? item.get("rssi") : 0).append(',');
                        b.append("\"type\":").append(item.get("type") != null ? item.get("type") : 0);
                        b.append('}');
                    }
                }
                b.append("],\"locationWasOn\":").append(locationOnBefore);
                b.append(",\"locationError\":\"").append(esc(locationErr)).append("\"}");
                out(b.toString());
                System.exit(0);
            }
        });
        worker.start();
        Looper.loop();
    }

    private static java.util.UUID uuid(String short16) {
        return java.util.UUID.fromString(short16 + "-0000-1000-8000-00805f9b34fb");
    }

    /**
     * BLE-поиск с фильтром по service UUID. В отличие от незафильтрованного
     * скана, Android 9 разрешает его при выключенной геолокации.
     */
    private static void filteredScan(int seconds) throws Exception {
        if (!adapter.isEnabled()) {
            out("{\"error\":\"bluetooth disabled\"}");
            return;
        }
        final LinkedHashMap<String, Map<String, Object>> found = new LinkedHashMap<String, Map<String, Object>>();
        final Object lock = new Object();
        Class<?> callbackClass = Class.forName("android.bluetooth.BluetoothAdapter$LeScanCallback");
        Object callback = Proxy.newProxyInstance(callbackClass.getClassLoader(), new Class<?>[]{callbackClass},
                new InvocationHandler() {
                    @Override
                    public Object invoke(Object proxy, Method method, Object[] args) {
                        if ("onLeScan".equals(method.getName()) && args != null && args.length >= 2) {
                            BluetoothDevice d = (BluetoothDevice) args[0];
                            if (d == null) return null;
                            String mac = safe(d.getAddress());
                            if (mac.length() == 0) return null;
                            int rssi = args[1] instanceof Number ? ((Number) args[1]).intValue() : 0;
                            synchronized (lock) {
                                Map<String, Object> item = found.get(mac);
                                if (item == null) {
                                    item = new LinkedHashMap<String, Object>();
                                    item.put("mac", mac);
                                    found.put(mac, item);
                                }
                                String name = safe(d.getName());
                                if (name.length() > 0) item.put("name", name);
                                item.put("rssi", rssi);
                                item.put("type", typeOf(d));
                            }
                            return null;
                        }
                        return defaultReturn(method.getReturnType());
                    }
                });
        java.util.UUID[] uuids = new java.util.UUID[]{
                uuid("0000180f"), uuid("0000180a"), uuid("00001812"), uuid("0000180d"),
                uuid("00001816"), uuid("0000181a"), uuid("00001826"),
                uuid("00001108"), uuid("0000110a"), uuid("0000110b"), uuid("0000110c"),
                uuid("0000111e"), uuid("00001112"), uuid("0000fe9f"), uuid("0000fe95"),
                uuid("0000fd6f"), uuid("0000fff0"), uuid("0000fff1")
        };
        Method start = BluetoothAdapter.class.getMethod("startLeScan", java.util.UUID[].class, callbackClass);
        final Method stop = BluetoothAdapter.class.getMethod("stopLeScan", callbackClass);
        final Object cb = callback;
        start.invoke(adapter, uuids, callback);
        final int waitSeconds = Math.max(3, seconds);
        Thread worker = new Thread(new Runnable() {
            @Override
            public void run() {
                try {
                    Thread.sleep(waitSeconds * 1000L);
                } catch (InterruptedException ignored) {
                }
                try {
                    stop.invoke(adapter, cb);
                } catch (Throwable ignored) {
                }
                StringBuilder b = new StringBuilder("{\"devices\":[");
                boolean first = true;
                synchronized (lock) {
                    for (Map<String, Object> item : found.values()) {
                        if (!first) b.append(',');
                        first = false;
                        b.append('{');
                        b.append("\"mac\":\"").append(esc((String) item.get("mac"))).append("\",");
                        b.append("\"name\":\"").append(esc((String) (item.get("name") != null ? item.get("name") : ""))).append("\",");
                        b.append("\"rssi\":").append(item.get("rssi") != null ? item.get("rssi") : 0).append(',');
                        b.append("\"type\":").append(item.get("type") != null ? item.get("type") : 0);
                        b.append('}');
                    }
                }
                b.append("]}");
                out(b.toString());
                System.exit(0);
            }
        });
        worker.start();
        Looper.loop();
    }

    /**
     * Классический поиск (BR/EDR). Регистрируем BroadcastReceiver напрямую
     * через IActivityManager с caller=null: у app_process нет записи приложения,
     * поэтому обычный Context.registerReceiver не работает.
     */
    private static void classicScan(int seconds) throws Exception {
        if (!adapter.isEnabled()) {
            out("{\"error\":\"bluetooth disabled\"}");
            return;
        }
        // UID 1002 (com.android.bluetooth) имеет BLUETOOTH_ADMIN и
        // ACCESS_COARSE_LOCATION: без них Android вырезает устройство из
        // ACTION_FOUND (STRIPPED=1), а discovery запрещён.
        dropUid(1002);
        final LinkedHashMap<String, Map<String, Object>> found = new LinkedHashMap<String, Map<String, Object>>();
        final Object lock = new Object();
        final int performReceive = intentReceiverTransaction();

        class ReceiverBinder extends Binder {
            @Override
            protected boolean onTransact(int code, Parcel data, Parcel reply, int flags) {
                if (code == performReceive) {
                    System.err.println("BtHelper: performReceive code=" + code);
                    try {
                        data.enforceInterface("android.content.IIntentReceiver");
                        Intent intent = Intent.CREATOR.createFromParcel(data);
                        data.readInt();
                        data.readString();
                        data.readBundle(BtHelper.class.getClassLoader());
                        data.readInt();
                        data.readInt();
                        data.readInt();
                        addFound(intent, found, lock);
                    } catch (Throwable ignored) {
                        System.err.println("BtHelper: recv error " + ignored);
                    }
                    return true;
                }
                try {
                    return super.onTransact(code, data, reply, flags);
                } catch (Exception e) {
                    return false;
                }
            }
        }

        final ReceiverBinder binder = new ReceiverBinder();
        Class<?> receiverClass = Class.forName("android.content.IIntentReceiver");
        Object receiver = Proxy.newProxyInstance(receiverClass.getClassLoader(), new Class<?>[]{receiverClass},
                new InvocationHandler() {
                    @Override
                    public Object invoke(Object proxy, Method method, Object[] args) {
                        if ("asBinder".equals(method.getName())) return binder;
                        return defaultReturn(method.getReturnType());
                    }
                });

        IntentFilter filter = new IntentFilter(BluetoothDevice.ACTION_FOUND);
        filter.addAction(BluetoothAdapter.ACTION_DISCOVERY_FINISHED);

        Object am = activityManager();
        Method register = null;
        for (Method m : am.getClass().getMethods()) {
            if ("registerReceiver".equals(m.getName()) && m.getParameterTypes().length == 7) {
                register = m;
                break;
            }
        }
        if (register == null) throw new Exception("registerReceiver(7) not found");
        register.invoke(am, null, "com.android.bluetooth", receiver, filter, null, Integer.valueOf(0), Integer.valueOf(0));

        adapter.startDiscovery();
        final int waitSeconds = Math.max(3, seconds);
        Thread worker = new Thread(new Runnable() {
            @Override
            public void run() {
                try {
                    Thread.sleep(waitSeconds * 1000L);
                } catch (InterruptedException ignored) {
                }
                try {
                    adapter.cancelDiscovery();
                } catch (Throwable ignored) {
                }
                StringBuilder b = new StringBuilder("{\"devices\":[");
                boolean first = true;
                synchronized (lock) {
                    for (Map<String, Object> item : found.values()) {
                        if (!first) b.append(',');
                        first = false;
                        b.append('{');
                        b.append("\"mac\":\"").append(esc((String) item.get("mac"))).append("\",");
                        b.append("\"name\":\"").append(esc((String) (item.get("name") != null ? item.get("name") : ""))).append("\",");
                        b.append("\"rssi\":").append(item.get("rssi") != null ? item.get("rssi") : 0).append(',');
                        b.append("\"type\":").append(item.get("type") != null ? item.get("type") : 0);
                        b.append('}');
                    }
                }
                b.append("],\"registered\":true}");
                out(b.toString());
                System.exit(0);
            }
        });
        worker.start();
        Looper.loop();
    }

    private static void addFound(Intent intent, Map<String, Map<String, Object>> found, Object lock) {
        if (intent == null || !BluetoothDevice.ACTION_FOUND.equals(intent.getAction())) return;
        BluetoothDevice d = (BluetoothDevice) intent.getParcelableExtra(BluetoothDevice.EXTRA_DEVICE);
        if (d == null) return;
        String mac = safe(d.getAddress());
        if (mac.length() == 0) return;
        System.err.println("BtHelper: found " + mac + " name=" + safe(d.getName()));
        short rssi = intent.getShortExtra(BluetoothDevice.EXTRA_RSSI, (short) 0);
        synchronized (lock) {
            Map<String, Object> item = found.get(mac);
            if (item == null) {
                item = new LinkedHashMap<String, Object>();
                item.put("mac", mac);
                found.put(mac, item);
            }
            String name = safe(d.getName());
            if (name.length() > 0) item.put("name", name);
            item.put("rssi", (int) rssi);
            item.put("type", typeOf(d));
        }
    }

    private static Object activityManager() throws Exception {
        Class<?> serviceManager = Class.forName("android.os.ServiceManager");
        Object binder = serviceManager.getMethod("getService", String.class).invoke(null, "activity");
        if (binder == null) throw new Exception("activity service unavailable");
        Class<?> stub = Class.forName("android.app.IActivityManager$Stub");
        Class<?> ibinder = Class.forName("android.os.IBinder");
        return stub.getMethod("asInterface", ibinder).invoke(null, binder);
    }

    private static int intentReceiverTransaction() throws Exception {
        Class<?> stub = Class.forName("android.content.IIntentReceiver$Stub");
        java.lang.reflect.Field field = stub.getDeclaredField("TRANSACTION_performReceive");
        field.setAccessible(true);
        return field.getInt(null);
    }

    private static Object defaultReturn(Class<?> rt) {
        if (rt == boolean.class) return Boolean.FALSE;
        if (rt == int.class) return Integer.valueOf(0);
        if (rt == long.class) return Long.valueOf(0L);
        if (rt == short.class) return Short.valueOf((short) 0);
        if (rt == byte.class) return Byte.valueOf((byte) 0);
        if (rt == char.class) return Character.valueOf((char) 0);
        if (rt == float.class) return Float.valueOf(0f);
        if (rt == double.class) return Double.valueOf(0d);
        return null;
    }

    private static void receiveTest(int seconds) throws Exception {
        final int performReceive = intentReceiverTransaction();
        final int[] count = new int[]{0};
        final String[] last = new String[]{""};

        class ReceiverBinder extends Binder {
            @Override
            protected boolean onTransact(int code, Parcel data, Parcel reply, int flags) {
                if (code == performReceive) {
                    try {
                        data.enforceInterface("android.content.IIntentReceiver");
                        Intent intent = Intent.CREATOR.createFromParcel(data);
                        data.readInt();
                        data.readString();
                        data.readBundle(BtHelper.class.getClassLoader());
                        data.readInt();
                        data.readInt();
                        data.readInt();
                        count[0]++;
                        last[0] = String.valueOf(intent.getAction());
                        System.err.println("BtHelper: test receive " + last[0]);
                    } catch (Throwable t) {
                        System.err.println("BtHelper: test error " + t);
                    }
                    return true;
                }
                try {
                    return super.onTransact(code, data, reply, flags);
                } catch (Exception e) {
                    return false;
                }
            }
        }

        final ReceiverBinder binder = new ReceiverBinder();
        Class<?> receiverClass = Class.forName("android.content.IIntentReceiver");
        Object receiver = Proxy.newProxyInstance(receiverClass.getClassLoader(), new Class<?>[]{receiverClass},
                new InvocationHandler() {
                    @Override
                    public Object invoke(Object proxy, Method method, Object[] args) {
                        if ("asBinder".equals(method.getName())) return binder;
                        return defaultReturn(method.getReturnType());
                    }
                });

        IntentFilter filter = new IntentFilter("com.stationweb.BT_TEST");
        Object am = activityManager();
        Method register = null;
        for (Method m : am.getClass().getMethods()) {
            if ("registerReceiver".equals(m.getName()) && m.getParameterTypes().length == 7) {
                register = m;
                break;
            }
        }
        if (register == null) throw new Exception("registerReceiver(7) not found");
        register.invoke(am, null, null, receiver, filter, null, Integer.valueOf(0), Integer.valueOf(0));

        Thread worker = new Thread(new Runnable() {
            @Override
            public void run() {
                try {
                    Thread.sleep(Math.max(3, seconds) * 1000L);
                } catch (InterruptedException ignored) {
                }
                out("{\"received\":" + count[0] + ",\"last\":\"" + esc(last[0]) + "\"}");
                System.exit(0);
            }
        });
        worker.start();
        Looper.loop();
    }

    private static void uidTest(int uid) throws Exception {
        out("{\"before\":" + android.os.Process.myUid() + "}");
        String err = "";
        try {
            dropUid(uid);
        } catch (Throwable t) {
            err = unwrap(t);
        }
        out("{\"after\":" + android.os.Process.myUid() + ",\"error\":\"" + esc(err) + "\"}");
    }

    private static void dropUid(int uid) throws Exception {
        Class<?> os = Class.forName("android.system.Os");
        try {
            os.getMethod("setgid", int.class).invoke(null, uid);
        } catch (Throwable ignored) {
        }
        os.getMethod("setuid", int.class).invoke(null, uid);
    }

    private static void loc(String mode) throws Exception {
        Object lm = locationManager();
        boolean before = locationEnabled(lm);
        if ("on".equals(mode)) {
            setLocationEnabled(lm, true);
            Thread.sleep(1200);
        } else if ("off".equals(mode)) {
            setLocationEnabled(lm, false);
            Thread.sleep(400);
        }
        boolean after = locationEnabled(lm);
        out("{\"before\":" + before + ",\"after\":" + after + "}");
    }

    private static void pair(String mac) throws Exception {
        BluetoothDevice d = adapter.getRemoteDevice(mac);
        Method createBond = method(BluetoothDevice.class, "createBond");
        Object result = createBond.invoke(d);
        int state = waitBond(d, 25000);
        out("{\"mac\":\"" + esc(mac) + "\",\"bond\":" + state + ",\"started\":" + result + "}");
    }

    private static void forget(String mac) throws Exception {
        BluetoothDevice d = adapter.getRemoteDevice(mac);
        Method removeBond = method(BluetoothDevice.class, "removeBond");
        Object result = removeBond.invoke(d);
        int state = waitBond(d, 15000);
        out("{\"mac\":\"" + esc(mac) + "\",\"bond\":" + state + ",\"removed\":" + result + "}");
    }

    private static void connect(String mac, boolean on) throws Exception {
        BluetoothDevice d = adapter.getRemoteDevice(mac);
        String name = on ? "connect" : "disconnect";
        try {
            Method m = method(BluetoothDevice.class, name);
            Object result = m.invoke(d);
            out("{\"mac\":\"" + esc(mac) + "\",\"action\":\"" + name + "\",\"result\":" + result + "}");
        } catch (Throwable t) {
            out("{\"mac\":\"" + esc(mac) + "\",\"action\":\"" + name + "\",\"error\":\"" + esc(t.toString()) + "\"}");
        }
    }

    private static int waitBond(BluetoothDevice d, long timeoutMs) throws Exception {
        long end = System.currentTimeMillis() + timeoutMs;
        int state = d.getBondState();
        while (System.currentTimeMillis() < end) {
            state = d.getBondState();
            if (state == BluetoothDevice.BOND_BONDED || state == BluetoothDevice.BOND_NONE) break;
            Thread.sleep(300);
        }
        return state;
    }

    private static Method method(Class<?> cls, String name) throws Exception {
        try {
            return cls.getMethod(name);
        } catch (NoSuchMethodException e) {
            Method m = cls.getDeclaredMethod(name);
            m.setAccessible(true);
            return m;
        }
    }

    private static int typeOf(BluetoothDevice d) {
        try {
            Method m = d.getClass().getMethod("getType");
            Object v = m.invoke(d);
            if (v instanceof Integer) return (Integer) v;
        } catch (Throwable ignored) {
        }
        return 0;
    }

    private static String deviceJson(BluetoothDevice d) {
        return "{\"name\":\"" + esc(safe(d.getName())) + "\",\"mac\":\"" + esc(safe(d.getAddress()))
                + "\",\"bond\":" + d.getBondState() + ",\"type\":" + typeOf(d)
                + ",\"connected\":" + isConnected(d) + "}";
    }

    private static boolean isConnected(BluetoothDevice d) {
        try {
            Method m = d.getClass().getMethod("isConnected");
            Object v = m.invoke(d);
            return Boolean.TRUE.equals(v);
        } catch (Throwable ignored) {
            return false;
        }
    }

    private static Object locationManager() throws Exception {
        Class<?> serviceManager = Class.forName("android.os.ServiceManager");
        Object binder = serviceManager.getMethod("getService", String.class).invoke(null, "location");
        if (binder == null) throw new Exception("location service unavailable");
        Class<?> stub = Class.forName("android.location.ILocationManager$Stub");
        Class<?> ibinder = Class.forName("android.os.IBinder");
        return stub.getMethod("asInterface", ibinder).invoke(null, binder);
    }

    private static boolean locationEnabled(Object lm) throws Exception {
        Method m = lm.getClass().getMethod("isLocationEnabledForUser", int.class);
        Object v = m.invoke(lm, 0);
        return Boolean.TRUE.equals(v);
    }

    private static void setLocationEnabled(Object lm, boolean on) throws Exception {
        Method m = lm.getClass().getMethod("setLocationEnabledForUser", boolean.class, int.class);
        m.invoke(lm, on, 0);
    }

    private static String unwrap(Throwable t) {
        while (t.getCause() != null) t = t.getCause();
        return t.toString();
    }

    private static String safe(String s) {
        return s == null ? "" : s;
    }

    private static String esc(String s) {
        StringBuilder b = new StringBuilder();
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            switch (c) {
                case '\\': b.append("\\\\"); break;
                case '"': b.append("\\\""); break;
                case '\n': b.append("\\n"); break;
                case '\r': b.append("\\r"); break;
                case '\t': b.append("\\t"); break;
                default:
                    if (c < 0x20) {
                        b.append(String.format("\\u%04x", (int) c));
                    } else {
                        b.append(c);
                    }
            }
        }
        return b.toString();
    }

    private static void out(String s) {
        System.out.println(s);
    }
}
