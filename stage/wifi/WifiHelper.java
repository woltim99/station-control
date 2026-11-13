import android.content.Context;
import android.net.wifi.SupplicantState;
import android.net.wifi.WifiConfiguration;
import android.net.wifi.WifiInfo;
import android.net.wifi.WifiManager;

import java.util.List;

/**
 * Wi-Fi helper: работает через фреймворк (WifiManager), а не через сокет
 * wpa_supplicant. Запускается как root через app_process.
 *
 * Команды:
 *   status
 *   list
 *   connect <ssid> <password> <open|wpa|wpa2|wpa3>
 *   forget <ssid>
 */
public class WifiHelper {
    public static void main(String[] args) {
        try {
            android.os.Looper.prepareMainLooper();
            Class<?> atClass = Class.forName("android.app.ActivityThread");
            Object at = atClass.getMethod("systemMain").invoke(null);
            Context ctx = (Context) atClass.getMethod("getSystemContext").invoke(at);
            WifiManager wm = (WifiManager) ctx.getSystemService(Context.WIFI_SERVICE);
            if (wm == null) {
                out(error("WifiManager недоступен"));
                return;
            }
            String cmd = args.length > 0 ? args[0] : "status";
            if ("status".equals(cmd)) {
                out(status(wm));
            } else if ("list".equals(cmd)) {
                out(list(wm));
            } else if ("connect".equals(cmd) && args.length >= 3) {
                String security = args.length >= 4 ? args[3] : "wpa2";
                out(connect(wm, args[1], args[2], security));
            } else if ("connect-saved".equals(cmd) && args.length >= 2) {
                out(connectSaved(wm, args[1]));
            } else if ("forget".equals(cmd) && args.length >= 2) {
                out(forget(wm, args[1]));
            } else {
                out(error("unknown command"));
            }
        } catch (Throwable t) {
            out(error(unwrap(t)));
        }
        System.exit(0);
    }

    private static String status(WifiManager wm) {
        WifiInfo i = wm.getConnectionInfo();
        return "{\"ssid\":" + jstr(i != null ? clean(i.getSSID()) : "")
                + ",\"state\":" + jstr(i != null && i.getSupplicantState() != null
                        ? i.getSupplicantState().toString() : "")
                + ",\"ip\":" + jstr(i != null ? intToIp(i.getIpAddress()) : "")
                + ",\"networkId\":" + (i != null ? i.getNetworkId() : -1) + "}";
    }

    private static String list(WifiManager wm) {
        StringBuilder b = new StringBuilder("{\"networks\":[");
        boolean first = true;
        List<WifiConfiguration> confs = wm.getConfiguredNetworks();
        if (confs != null) {
            for (WifiConfiguration c : confs) {
                if (!first) b.append(',');
                first = false;
                b.append("{\"id\":").append(c.networkId)
                        .append(",\"ssid\":").append(jstr(clean(c.SSID)))
                        .append(",\"open\":").append(c.allowedKeyManagement.get(WifiConfiguration.KeyMgmt.NONE))
                        .append('}');
            }
        }
        return b.append("]}").toString();
    }

    private static String connect(WifiManager wm, String ssid, String pass, String security) {
        String sec = security == null ? "wpa2" : security.toLowerCase();
        WifiConfiguration cfg = new WifiConfiguration();
        cfg.SSID = quote(ssid);
        if ("open".equals(sec)) {
            cfg.allowedKeyManagement.set(WifiConfiguration.KeyMgmt.NONE);
        } else {
            cfg.preSharedKey = quote(pass);
            cfg.allowedKeyManagement.set(WifiConfiguration.KeyMgmt.WPA_PSK);
        }

        int id = -1;
        List<WifiConfiguration> confs = wm.getConfiguredNetworks();
        if (confs != null) {
            for (WifiConfiguration c : confs) {
                if (ssid.equals(clean(c.SSID))) {
                    id = c.networkId;
                    break;
                }
            }
        }
        int added = wm.addNetwork(cfg);
        if (id < 0) {
            if (added < 0) {
                return error("addNetwork вернул " + added);
            }
            id = added;
        } else if (added >= 0 && added != id) {
            wm.removeNetwork(id);
            id = added;
        }

        wm.disconnect();
        wm.enableNetwork(id, true);
        wm.reconnect();
        wm.saveConfiguration();

        return waitConnected(wm, id);
    }

    private static String connectSaved(WifiManager wm, String ssid) {
        int id = -1;
        List<WifiConfiguration> confs = wm.getConfiguredNetworks();
        if (confs != null) {
            for (WifiConfiguration c : confs) {
                if (ssid.equals(clean(c.SSID))) {
                    id = c.networkId;
                    break;
                }
            }
        }
        if (id < 0) {
            return error("сеть не найдена в сохранённых");
        }
        wm.disconnect();
        wm.enableNetwork(id, true);
        wm.reconnect();
        return waitConnected(wm, id);
    }

    private static String waitConnected(WifiManager wm, int id) {
        long deadline = System.currentTimeMillis() + 20000;
        while (System.currentTimeMillis() < deadline) {
            WifiInfo i = wm.getConnectionInfo();
            if (i != null && i.getNetworkId() == id
                    && i.getSupplicantState() == SupplicantState.COMPLETED
                    && i.getIpAddress() != 0) {
                return "{\"ok\":true,\"ssid\":" + jstr(clean(i.getSSID()))
                        + ",\"ip\":" + jstr(intToIp(i.getIpAddress()))
                        + ",\"networkId\":" + id + "}";
            }
            try { Thread.sleep(400); } catch (InterruptedException ignored) { }
        }
        WifiInfo i = wm.getConnectionInfo();
        return "{\"ok\":false,\"error\":" + jstr("не удалось подключиться: "
                + (i != null ? i.getSupplicantState() : "нет состояния"))
                + ",\"ssid\":" + jstr(i != null ? clean(i.getSSID()) : "")
                + ",\"ip\":" + jstr(i != null ? intToIp(i.getIpAddress()) : "")
                + ",\"networkId\":" + id + "}";
    }

    private static String forget(WifiManager wm, String ssid) {
        List<WifiConfiguration> confs = wm.getConfiguredNetworks();
        boolean removed = false;
        if (confs != null) {
            for (WifiConfiguration c : confs) {
                if (ssid.equals(clean(c.SSID))) {
                    removed |= wm.removeNetwork(c.networkId);
                }
            }
        }
        wm.saveConfiguration();
        return removed ? "{\"ok\":true}" : error("сеть не найдена");
    }

    private static String quote(String s) {
        return "\"" + s.replace("\\", "\\\\").replace("\"", "\\\"") + "\"";
    }

    private static String clean(String s) {
        if (s == null) return "";
        if (s.length() >= 2 && s.startsWith("\"") && s.endsWith("\"")) {
            return s.substring(1, s.length() - 1);
        }
        return s;
    }

    private static String intToIp(int ip) {
        if (ip == 0) return "";
        return (ip & 0xFF) + "." + ((ip >> 8) & 0xFF) + "." + ((ip >> 16) & 0xFF) + "." + ((ip >> 24) & 0xFF);
    }

    private static String unwrap(Throwable t) {
        Throwable cur = t;
        while (cur.getCause() != null && cur.getCause() != cur) cur = cur.getCause();
        String msg = cur.getClass().getSimpleName() + ": " + cur.getMessage();
        return msg.length() > 300 ? msg.substring(0, 300) : msg;
    }

    private static String error(String msg) {
        return "{\"error\":" + jstr(msg) + "}";
    }

    private static String jstr(String s) {
        if (s == null) return "\"\"";
        StringBuilder b = new StringBuilder("\"");
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            switch (c) {
                case '"': b.append("\\\""); break;
                case '\\': b.append("\\\\"); break;
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
        return b.append('"').toString();
    }

    private static void out(String s) {
        System.out.println(s);
    }
}
