import android.app.Application;
import android.content.Context;
import android.media.AudioFormat;
import android.media.AudioRecord;

import java.io.OutputStream;
import java.lang.reflect.Field;
import java.net.InetSocketAddress;
import java.net.Socket;

/**
 * Захват общего выхода станции через REMOTE_SUBMIX и отправка 25 полос
 * визуализации в панель по TCP 127.0.0.1:8093.
 *
 * Запускается как app_process от root. Без Application у AudioRecord
 * mPackageName = null, и вендорный JNI падает в strlen(0), поэтому
 * подставляем системный контекст.
 */
public class AudioViz {
    private static final int RATE = 44100;
    private static final int BANDS = 25;
    private static final int FFT = 1024;
    private static final int PORT = 8093;
    private static final byte MAGIC = (byte) 0xA5;

    public static void main(String[] args) throws Exception {
        installFakeApp();
        AudioRecord rec = openRecord();
        if (rec == null) {
            System.err.println("AudioViz: cannot open audio record");
            System.exit(1);
        }
        System.err.println("AudioViz: capture started, source=" + rec.getAudioSource());
        rec.startRecording();

        short[] buf = new short[FFT * 2];
        double[] window = hann(FFT);
        double[] re = new double[FFT];
        double[] im = new double[FFT];
        float[] smooth = new float[BANDS];
        double agc = 1e-5;
        Socket socket = null;
        OutputStream out = null;
        long nextConnect = 0;

        while (true) {
            int read = rec.read(buf, 0, buf.length);
            if (read <= 0) {
                Thread.sleep(10);
                continue;
            }
            int frames = Math.min(FFT, read / 2);
            for (int i = 0; i < frames; i++) {
                re[i] = (buf[i * 2] / 32768.0) * window[i];
                im[i] = 0;
            }
            for (int i = frames; i < FFT; i++) {
                re[i] = 0;
                im[i] = 0;
            }
            fft(re, im);

            double[] bands = new double[BANDS];
            double frameMax = 0;
            for (int b = 0; b < BANDS; b++) {
                double f0 = 40.0 * Math.pow(16000.0 / 40.0, (double) b / BANDS);
                double f1 = 40.0 * Math.pow(16000.0 / 40.0, (double) (b + 1) / BANDS);
                int k0 = Math.max(1, (int) (f0 * FFT / RATE));
                int k1 = Math.min(FFT / 2 - 1, (int) (f1 * FFT / RATE));
                double sum = 0;
                int count = 0;
                for (int k = k0; k <= k1; k++) {
                    double p = re[k] * re[k] + im[k] * im[k];
                    sum += p;
                    count++;
                }
                bands[b] = count > 0 ? Math.sqrt(sum / count) : 0;
                if (bands[b] > frameMax) {
                    frameMax = bands[b];
                }
            }

            if (frameMax > agc) {
                agc = frameMax;
            } else {
                agc *= 0.985;
            }
            if (agc < 1e-7) {
                agc = 1e-7;
            }

            byte[] packet = new byte[1 + BANDS];
            packet[0] = MAGIC;
            boolean silent = frameMax < 5e-5;
            for (int b = 0; b < BANDS; b++) {
                float level = 0;
                if (!silent) {
                    double v = bands[b] / agc;
                    if (v < 0) {
                        v = 0;
                    }
                    if (v > 1) {
                        v = 1;
                    }
                    level = (float) (Math.sqrt(v) * 15.0);
                }
                smooth[b] = Math.max(level, smooth[b] * 0.72f);
                packet[1 + b] = (byte) Math.round(smooth[b]);
            }

            if (out == null && System.currentTimeMillis() >= nextConnect) {
                nextConnect = System.currentTimeMillis() + 1500;
                try {
                    socket = new Socket();
                    socket.connect(new InetSocketAddress("127.0.0.1", PORT), 400);
                    socket.setTcpNoDelay(true);
                    out = socket.getOutputStream();
                } catch (Exception e) {
                    closeQuietly(socket);
                    socket = null;
                    out = null;
                }
            }
            if (out != null) {
                try {
                    out.write(packet);
                    out.flush();
                } catch (Exception e) {
                    closeQuietly(socket);
                    socket = null;
                    out = null;
                }
            }
        }
    }

    private static AudioRecord openRecord() {
        int[] sources = {8, 1, 6};
        for (int src : sources) {
            AudioRecord r = null;
            try {
                int min = AudioRecord.getMinBufferSize(RATE, AudioFormat.CHANNEL_IN_STEREO, AudioFormat.ENCODING_PCM_16BIT);
                if (min <= 0) {
                    min = 4096;
                }
                r = new AudioRecord(src, RATE, AudioFormat.CHANNEL_IN_STEREO,
                        AudioFormat.ENCODING_PCM_16BIT, Math.max(min, FFT * 4));
                if (r.getState() == AudioRecord.STATE_INITIALIZED) {
                    return r;
                }
                r.release();
            } catch (Throwable t) {
                System.err.println("AudioViz: source " + src + ": " + t);
                if (r != null) {
                    try {
                        r.release();
                    } catch (Throwable ignored) {
                    }
                }
            }
        }
        return null;
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
        } catch (Throwable t) {
            System.err.println("AudioViz: fake app failed: " + t);
        }
    }

    private static class FakeApp extends Application {
        void setBase(Context c) {
            attachBaseContext(c);
        }
    }

    private static double[] hann(int n) {
        double[] w = new double[n];
        for (int i = 0; i < n; i++) {
            w[i] = 0.5 * (1 - Math.cos(2 * Math.PI * i / (n - 1)));
        }
        return w;
    }

    private static void fft(double[] re, double[] im) {
        int n = re.length;
        for (int i = 1, j = 0; i < n; i++) {
            int bit = n >> 1;
            for (; (j & bit) != 0; bit >>= 1) {
                j ^= bit;
            }
            j ^= bit;
            if (i < j) {
                double t = re[i];
                re[i] = re[j];
                re[j] = t;
                t = im[i];
                im[i] = im[j];
                im[j] = t;
            }
        }
        for (int len = 2; len <= n; len <<= 1) {
            double ang = -2 * Math.PI / len;
            double wlenR = Math.cos(ang);
            double wlenI = Math.sin(ang);
            for (int i = 0; i < n; i += len) {
                double wr = 1;
                double wi = 0;
                for (int j = 0; j < len / 2; j++) {
                    int a = i + j;
                    int b = a + len / 2;
                    double vr = re[b] * wr - im[b] * wi;
                    double vi = re[b] * wi + im[b] * wr;
                    re[b] = re[a] - vr;
                    im[b] = im[a] - vi;
                    re[a] += vr;
                    im[a] += vi;
                    double nwr = wr * wlenR - wi * wlenI;
                    wi = wr * wlenI + wi * wlenR;
                    wr = nwr;
                }
            }
        }
    }

    private static void closeQuietly(Socket s) {
        if (s != null) {
            try {
                s.close();
            } catch (Exception ignored) {
            }
        }
    }
}
