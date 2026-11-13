import android.os.SystemClock;
import android.view.InputDevice;
import android.view.InputEvent;
import android.view.KeyEvent;
import android.view.MotionEvent;

import java.io.BufferedReader;
import java.io.InputStreamReader;
import java.io.PrintWriter;
import java.lang.reflect.Method;
import java.net.InetAddress;
import java.net.ServerSocket;
import java.net.Socket;

/**
 * Резидентный помощник ввода: app_process + InputManager.injectInputEvent.
 * Обычный `input` на этой станции стартует ~0,8 с; здесь процесс живёт
 * постоянно и ввод занимает десятки миллисекунд.
 *
 * Протокол: одна строка на команду, ответ "ok" или "err ...".
 *   key <code>
 *   tap <x> <y>
 *   swipe <x1> <y1> <x2> <y2> <duration_ms>
 *   ping
 */
public class InputHelper {
    private static Object inputManager;
    private static Method injectInputEvent;
    private static final int MODE_ASYNC = 0; // INJECT_INPUT_EVENT_MODE_ASYNC

    public static void main(String[] args) {
        try {
            Class<?> cls = Class.forName("android.hardware.input.InputManager");
            inputManager = cls.getMethod("getInstance").invoke(null);
            if (inputManager == null) {
                System.err.println("InputHelper: InputManager is null");
                System.exit(2);
            }
            injectInputEvent = cls.getMethod("injectInputEvent", InputEvent.class, int.class);
            ServerSocket server = new ServerSocket(8092, 4, InetAddress.getByName("127.0.0.1"));
            System.err.println("InputHelper ready on 127.0.0.1:8092");
            while (true) {
                Socket socket = server.accept();
                Thread t = new Thread(new Client(socket));
                t.setDaemon(true);
                t.start();
            }
        } catch (Throwable t) {
            t.printStackTrace();
            System.exit(1);
        }
    }

    private static class Client implements Runnable {
        private final Socket socket;

        Client(Socket socket) {
            this.socket = socket;
        }

        @Override
        public void run() {
            try {
                BufferedReader in = new BufferedReader(new InputStreamReader(socket.getInputStream(), "UTF-8"));
                PrintWriter out = new PrintWriter(socket.getOutputStream(), true);
                String line;
                while ((line = in.readLine()) != null) {
                    String cmd = line.trim();
                    if (cmd.isEmpty()) {
                        continue;
                    }
                    try {
                        out.println(execute(cmd));
                    } catch (Throwable t) {
                        out.println("err " + t);
                    }
                }
            } catch (Throwable ignored) {
            } finally {
                try {
                    socket.close();
                } catch (Throwable ignored) {
                }
            }
        }
    }

    private static String execute(String cmd) throws Exception {
        String[] parts = cmd.split("\\s+");
        long now = SystemClock.uptimeMillis();
        if ("ping".equals(parts[0])) {
            return "ok";
        }
        if ("key".equals(parts[0]) && parts.length >= 2) {
            int code = Integer.parseInt(parts[1]);
            sendKey(code, now);
            return "ok";
        }
        if ("tap".equals(parts[0]) && parts.length >= 3) {
            float x = Float.parseFloat(parts[1]);
            float y = Float.parseFloat(parts[2]);
            sendTap(x, y, now);
            return "ok";
        }
        if ("swipe".equals(parts[0]) && parts.length >= 6) {
            float x1 = Float.parseFloat(parts[1]);
            float y1 = Float.parseFloat(parts[2]);
            float x2 = Float.parseFloat(parts[3]);
            float y2 = Float.parseFloat(parts[4]);
            int duration = Integer.parseInt(parts[5]);
            sendSwipe(x1, y1, x2, y2, duration, now);
            return "ok";
        }
        return "err unknown";
    }

    private static void sendKey(int code, long now) throws Exception {
        inject(new KeyEvent(now, now, KeyEvent.ACTION_DOWN, code, 0));
        Thread.sleep(8);
        inject(new KeyEvent(now, now + 8, KeyEvent.ACTION_UP, code, 0));
    }

    private static void sendTap(float x, float y, long now) throws Exception {
        MotionEvent down = MotionEvent.obtain(now, now, MotionEvent.ACTION_DOWN, x, y, 0);
        down.setSource(InputDevice.SOURCE_TOUCHSCREEN);
        inject(down);
        Thread.sleep(24);
        MotionEvent up = MotionEvent.obtain(now, now + 24, MotionEvent.ACTION_UP, x, y, 0);
        up.setSource(InputDevice.SOURCE_TOUCHSCREEN);
        inject(up);
    }

    private static void sendSwipe(float x1, float y1, float x2, float y2, int duration, long now) throws Exception {
        if (duration < 50) {
            duration = 50;
        }
        if (duration > 2000) {
            duration = 2000;
        }
        MotionEvent down = MotionEvent.obtain(now, now, MotionEvent.ACTION_DOWN, x1, y1, 0);
        down.setSource(InputDevice.SOURCE_TOUCHSCREEN);
        inject(down);
        int steps = Math.max(4, duration / 30);
        long stepMs = Math.max(1, duration / steps);
        for (int i = 1; i <= steps; i++) {
            float k = (float) i / steps;
            float x = x1 + (x2 - x1) * k;
            float y = y1 + (y2 - y1) * k;
            Thread.sleep(stepMs);
            long et = now + (long) stepMs * i;
            MotionEvent move = MotionEvent.obtain(now, et, MotionEvent.ACTION_MOVE, x, y, 0);
            move.setSource(InputDevice.SOURCE_TOUCHSCREEN);
            inject(move);
        }
        Thread.sleep(16);
        MotionEvent up = MotionEvent.obtain(now, now + duration + 16, MotionEvent.ACTION_UP, x2, y2, 0);
        up.setSource(InputDevice.SOURCE_TOUCHSCREEN);
        inject(up);
    }

    private static void inject(InputEvent event) throws Exception {
        Object result = injectInputEvent.invoke(inputManager, event, MODE_ASYNC);
        if (result instanceof Boolean && !((Boolean) result)) {
            throw new IllegalStateException("injectInputEvent returned false");
        }
    }
}
