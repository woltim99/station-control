import android.app.Application;
import android.content.Context;
import android.media.AudioFormat;
import android.media.AudioRecord;

import java.lang.reflect.Field;
import java.lang.reflect.Method;

/**
 * Проверка источников звука из app_process. Без Application у AudioRecord
 * mPackageName = null, и вендорный JNI падает в strlen(0). Подставляем
 * системный контекст, чтобы currentOpPackageName() был не null.
 */
public class AudioProbe {
    public static void main(String[] args) throws Exception {
        installFakeApp();
        for (int src : new int[]{8, 1, 6, 0}) {
            probe(src);
        }
    }

    private static void installFakeApp() {
        try {
            android.os.Looper.prepareMainLooper();
            Class<?> atClass = Class.forName("android.app.ActivityThread");
            Object at = atClass.getMethod("systemMain").invoke(null);
            Context ctx = (Context) atClass.getMethod("getSystemContext").invoke(at);
            FakeApp app = new FakeApp();
            app.setBase(ctx);
            Field f = atClass.getDeclaredField("mInitialApplication");
            f.setAccessible(true);
            f.set(at, app);
            System.out.println("opPackage=" + atClass.getMethod("currentOpPackageName").invoke(null));
        } catch (Throwable t) {
            System.out.println("fake app failed: " + t);
            Throwable c = t.getCause();
            while (c != null) {
                System.out.println("  cause: " + c);
                c.printStackTrace(System.out);
                c = c.getCause();
            }
        }
    }

    private static class FakeApp extends Application {
        void setBase(Context c) {
            attachBaseContext(c);
        }
    }

    private static void probe(int src) {
        AudioRecord r = null;
        try {
            int rate = 44100;
            int min = AudioRecord.getMinBufferSize(rate, AudioFormat.CHANNEL_IN_STEREO, AudioFormat.ENCODING_PCM_16BIT);
            if (min <= 0) {
                min = 4096;
            }
            r = new AudioRecord(src, rate, AudioFormat.CHANNEL_IN_STEREO, AudioFormat.ENCODING_PCM_16BIT, Math.max(min, 16384));
            System.out.println("source " + src + " state=" + r.getState() + " min=" + min);
            if (r.getState() != AudioRecord.STATE_INITIALIZED) {
                return;
            }
            r.startRecording();
            short[] buf = new short[4096];
            long sum = 0;
            long peak = 0;
            long n = 0;
            long end = System.currentTimeMillis() + 2500;
            while (System.currentTimeMillis() < end) {
                int read = r.read(buf, 0, buf.length);
                if (read <= 0) {
                    continue;
                }
                for (int i = 0; i < read; i++) {
                    int v = buf[i];
                    sum += (long) v * v;
                    if (Math.abs(v) > peak) {
                        peak = Math.abs(v);
                    }
                    n++;
                }
            }
            double rms = n > 0 ? Math.sqrt((double) sum / n) : 0;
            System.out.println("  rms=" + Math.round(rms) + " peak=" + peak + " samples=" + n);
        } catch (Throwable t) {
            System.out.println("source " + src + " error: " + t);
        } finally {
            if (r != null) {
                try {
                    r.stop();
                } catch (Throwable ignored) {
                }
                try {
                    r.release();
                } catch (Throwable ignored) {
                }
            }
        }
    }
}
