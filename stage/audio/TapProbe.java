import android.app.Application;
import android.content.ComponentName;
import android.content.Context;
import android.content.Intent;
import android.content.ServiceConnection;
import android.os.Bundle;
import android.os.IBinder;
import android.os.Looper;
import android.os.Message;
import android.os.Messenger;
import android.os.ParcelFileDescriptor;

import java.io.FileDescriptor;
import java.io.IOException;
import java.lang.reflect.Field;
import java.lang.reflect.Method;
import java.nio.ByteBuffer;
import java.nio.ByteOrder;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;

/**
 * Проверка AudioTap приложения Shairport: получаем PCM, не забирая звук
 * у динамиков. Запускается как app_process от root.
 */
public class TapProbe {
    private static IBinder binder;

    public static void main(String[] args) throws Exception {
        installFakeApp();
        Context ctx = getContext();
        Intent intent = new Intent("com.hkfuertes.shairport.AUDIO_TAP");
        intent.setPackage("com.hkfuertes.shairport");
        final CountDownLatch latch = new CountDownLatch(1);
        ServiceConnection conn = new ServiceConnection() {
            @Override
            public void onServiceConnected(ComponentName name, IBinder service) {
                binder = service;
                latch.countDown();
            }

            @Override
            public void onServiceDisconnected(ComponentName name) {
            }
        };
        boolean ok = ctx.bindService(intent, conn, Context.BIND_AUTO_CREATE);
        System.out.println("bind=" + ok);
        if (!latch.await(5, TimeUnit.SECONDS) || binder == null) {
            System.out.println("tap service not connected");
            return;
        }
        System.out.println("connected binder=" + binder);

        FileDescriptor[] fds = socketpair();
        ParcelFileDescriptor pfd = ParcelFileDescriptor.dup(fds[0]);
        Messenger messenger = new Messenger(binder);
        Message msg = Message.obtain(null, 1);
        Bundle data = new Bundle();
        data.putParcelable("socket", pfd);
        data.putFloat("lead", 0.6f);
        data.putBoolean("silent", false);
        data.putBoolean("only_44100", false);
        msg.setData(data);
        messenger.send(msg);
        System.out.println("tap message sent");

        byte[] buf = new byte[8192];
        long end = System.currentTimeMillis() + 6000;
        int packets = 0;
        long samples = 0;
        double sum = 0;
        while (System.currentTimeMillis() < end) {
            int n;
            try {
                n = osRead(fds[1], buf, 0, buf.length);
            } catch (Exception e) {
                System.out.println("read error: " + e);
                break;
            }
            if (n <= 0) {
                continue;
            }
            packets++;
            if (n >= 16) {
                ByteBuffer bb = ByteBuffer.wrap(buf, 0, n).order(ByteOrder.LITTLE_ENDIAN);
                int type = bb.getInt();
                int value = bb.getInt();
                long time = bb.getLong();
                int frames = (n - 16) / 4;
                if (type == 0) {
                    for (int i = 0; i < frames; i++) {
                        short s = bb.getShort();
                        sum += (double) s * s;
                        samples++;
                    }
                    if (packets <= 3) {
                        System.out.println("audio packet len=" + n + " rate=" + value + " frames=" + frames + " time=" + time);
                    }
                } else if (packets <= 10) {
                    System.out.println("packet type=" + type + " value=" + value + " len=" + n);
                }
            }
            if (samples > 44100 * 2) {
                break;
            }
        }
        double rms = samples > 0 ? Math.sqrt(sum / samples) : 0;
        System.out.println("packets=" + packets + " samples=" + samples + " rms=" + Math.round(rms));
        ctx.unbindService(conn);
    }

    private static Context systemContext;

    private static Context getContext() {
        return systemContext;
    }

    private static void installFakeApp() {
        try {
            Looper.prepareMainLooper();
            Class<?> atClass = Class.forName("android.app.ActivityThread");
            Object at = atClass.getMethod("systemMain").invoke(null);
            Context ctx = (Context) atClass.getMethod("getSystemContext").invoke(at);
            FakeApp app = new FakeApp();
            app.setBase(ctx);
            Field f = atClass.getDeclaredField("mInitialApplication");
            f.setAccessible(true);
            f.set(at, app);
            systemContext = app;
            System.out.println("opPackage=" + atClass.getMethod("currentOpPackageName").invoke(null));
        } catch (Throwable t) {
            System.out.println("fake app failed: " + t);
        }
    }

    private static class FakeApp extends Application {
        void setBase(Context c) {
            attachBaseContext(c);
        }
    }

    private static FileDescriptor[] socketpair() throws Exception {
        Class<?> os = Class.forName("android.system.Os");
        Method m = os.getMethod("socketpair", int.class, int.class, int.class);
        // AF_UNIX = 1, SOCK_SEQPACKET = 5 (Linux)
        return (FileDescriptor[]) m.invoke(null, 1, 5, 0);
    }

    private static int osRead(FileDescriptor fd, byte[] buf, int off, int len) throws Exception {
        Class<?> os = Class.forName("android.system.Os");
        Method m = os.getMethod("read", FileDescriptor.class, byte[].class, int.class, int.class);
        return (Integer) m.invoke(null, fd, buf, off, len);
    }
}
