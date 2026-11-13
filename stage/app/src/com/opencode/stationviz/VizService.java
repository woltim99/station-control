package com.opencode.stationviz;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.Service;
import android.content.ComponentName;
import android.content.Context;
import android.content.Intent;
import android.content.ServiceConnection;
import android.os.Bundle;
import android.os.IBinder;
import android.os.Message;
import android.os.Messenger;
import android.os.ParcelFileDescriptor;
import android.util.Log;

import java.io.FileDescriptor;
import java.io.OutputStream;
import java.lang.reflect.Method;
import java.net.InetSocketAddress;
import java.net.Socket;
import java.nio.ByteBuffer;
import java.nio.ByteOrder;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;

/**
 * Захват PCM из AudioTap приложения Shairport и отправка 25 полос
 * визуализации в панель по TCP 127.0.0.1:8093.
 */
public class VizService extends Service {
    private static final String TAG = "StationViz";
    private static final int BANDS = 25;
    private static final int FFT = 1024;
    private static final int PORT = 8093;
    private static final byte MAGIC = (byte) 0xA5;

    private volatile boolean running;
    private volatile boolean nativeMode;
    private volatile int epoch;
    private Thread worker;
    private Thread watchdog;
    private Socket panel;
    private OutputStream panelOut;
    private Socket pcm;
    private OutputStream pcmOut;
    private volatile FileDescriptor tapFd;
    private long pcmTry;
    private volatile long lastPacketAt;
    private volatile long packets;

    @Override
    public void onCreate() {
        super.onCreate();
        if (android.os.Build.VERSION.SDK_INT >= 26) {
            NotificationManager nm = getSystemService(NotificationManager.class);
            nm.createNotificationChannel(new NotificationChannel("viz", "Визуализация", NotificationManager.IMPORTANCE_LOW));
        }
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        boolean explicit = intent != null && intent.hasExtra("mode");
        boolean wantNative;
        if (explicit) {
            wantNative = "native".equals(intent.getStringExtra("mode"));
            getSharedPreferences("viz", MODE_PRIVATE).edit().putBoolean("native", wantNative).apply();
        } else {
            // После перезапуска процесса Android приходит с null-intent.
            wantNative = getSharedPreferences("viz", MODE_PRIVATE).getBoolean("native", false);
        }
        if (worker != null && nativeMode != wantNative) {
            // Старый поток не должен дожить до нового режима: он может
            // перехватить AudioTap и снова включить не тот маршрут.
            running = false;
            epoch++;
            closeTapFd();
            closePanel();
            closePCM();
            worker = null;
        }
        nativeMode = wantNative;
        Notification.Builder b;
        if (android.os.Build.VERSION.SDK_INT >= 26) {
            b = new Notification.Builder(this, "viz");
        } else {
            b = new Notification.Builder(this);
        }
        b.setContentTitle("Визуализация звука").setSmallIcon(android.R.drawable.ic_media_play);
        startForeground(1, b.build());
        if (worker == null) {
            running = true;
            final int myEpoch = epoch;
            worker = new Thread(() -> runLoop(myEpoch), "viz-tap");
            worker.start();
            if (watchdog == null) {
                watchdog = new Thread(() -> {
                    while (running) {
                        sleep(5000);
                        Log.i(TAG, "watchdog packets=" + packets + " since=" + (System.currentTimeMillis() - lastPacketAt));
                    }
                }, "viz-watch");
                watchdog.start();
            }
        }
        return START_STICKY;
    }

    @Override
    public void onDestroy() {
        running = false;
        epoch++;
        closeTapFd();
        closePanel();
        closePCM();
        if (worker != null) {
            worker.interrupt();
            worker = null;
        }
        super.onDestroy();
    }

    @Override
    public IBinder onBind(Intent intent) {
        return null;
    }

    private void runLoop(int myEpoch) {
        while (running && epoch == myEpoch) {
            IBinder binder = bindTap();
            if (binder == null) {
                sleep(1000);
                continue;
            }
            try {
                streamTap(binder, myEpoch);
            } catch (Throwable t) {
                Log.w(TAG, "tap error: " + t);
            }
            sleep(500);
        }
    }

    private IBinder bindTap() {
        Intent intent = new Intent("com.hkfuertes.shairport.AUDIO_TAP");
        intent.setPackage("com.hkfuertes.shairport");
        final CountDownLatch latch = new CountDownLatch(1);
        final IBinder[] out = new IBinder[1];
        ServiceConnection conn = new ServiceConnection() {
            @Override
            public void onServiceConnected(ComponentName name, IBinder service) {
                out[0] = service;
                latch.countDown();
            }

            @Override
            public void onServiceDisconnected(ComponentName name) {
            }
        };
        try {
            if (!bindService(intent, conn, Context.BIND_AUTO_CREATE)) {
                return null;
            }
            if (!latch.await(5, TimeUnit.SECONDS)) {
                unbindService(conn);
                Log.w(TAG, "tap bind timeout");
                return null;
            }
            Log.i(TAG, "tap bound");
            return out[0];
        } catch (Throwable t) {
            Log.w(TAG, "bind error: " + t);
            return null;
        }
    }

    private void streamTap(IBinder binder, int myEpoch) throws Exception {
        FileDescriptor[] fds = socketpair();
        tapFd = fds[1];
        ParcelFileDescriptor pfd = ParcelFileDescriptor.dup(fds[0]);
        Messenger messenger = new Messenger(binder);
        Message msg = Message.obtain(null, 1);
        Bundle data = new Bundle();
        data.putParcelable("socket", pfd);
        data.putFloat("lead", 0.5f);
        data.putBoolean("silent", nativeMode);
        data.putBoolean("only_44100", nativeMode);
        msg.setData(data);
        messenger.send(msg);
        Log.i(TAG, "tap message sent");

        double[] window = hann(FFT);
        double[] re = new double[FFT];
        double[] im = new double[FFT];
        float[] smooth = new float[BANDS];
        int pos = 0;
        int fftCount = 0;
        byte[] buf = new byte[16384];
        long lastPanelTry = 0;

        while (running && epoch == myEpoch) {
            int n = osRead(fds[1], buf, 0, buf.length);
            if (n <= 0) {
                // Пир закрыл сокет: пересоздаём тап, а не крутимся вхолостую.
                throw new Exception("tap socket closed");
            }
            if (n < 16) {
                continue;
            }
            lastPacketAt = System.currentTimeMillis();
            packets++;
            if (packets <= 3 || packets % 200 == 0) {
                Log.i(TAG, "tap packet n=" + n + " packets=" + packets);
            }
            ByteBuffer bb = ByteBuffer.wrap(buf, 0, n).order(ByteOrder.LITTLE_ENDIAN);
            int type = bb.getInt();
            int value = bb.getInt();
            bb.getLong();
            if (packets <= 5) {
                Log.i(TAG, "pkt type=" + type + " value=" + value + " n=" + n);
            }
            if (type != 0) {
                continue;
            }
            if (nativeMode) {
                if (pcmOut == null && System.currentTimeMillis() - pcmTry > 1500) {
                    pcmTry = System.currentTimeMillis();
                    openPCM();
                }
                if (pcmOut != null && n > 16) {
                    try {
                        pcmOut.write(buf, 16, n - 16);
                        pcmOut.flush();
                    } catch (Exception e) {
                        closePCM();
                    }
                }
                continue;
            }
            int rate = value > 0 ? value : 44100;
            int frames = (n - 16) / 4;
            for (int i = 0; i < frames; i++) {
                short s = bb.getShort();
                bb.getShort();
                re[pos] = (s / 32768.0) * window[pos];
                im[pos] = 0;
                pos++;
                if (pos < FFT) {
                    continue;
                }
                pos = 0;
                fftCount++;
                fft(re, im);
                double[] bands = new double[BANDS];
                double frameMax = 0;
                for (int b = 0; b < BANDS; b++) {
                    double f0 = 40.0 * Math.pow(16000.0 / 40.0, (double) b / BANDS);
                    double f1 = 40.0 * Math.pow(16000.0 / 40.0, (double) (b + 1) / BANDS);
                    int k0 = Math.max(1, (int) (f0 * FFT / rate));
                    int k1 = Math.min(FFT / 2 - 1, (int) (f1 * FFT / rate));
                    double sum = 0;
                    int count = 0;
                    for (int k = k0; k <= k1; k++) {
                        sum += re[k] * re[k] + im[k] * im[k];
                        count++;
                    }
                    bands[b] = count > 0 ? Math.sqrt(sum / count) / (FFT / 2.0) : 0;
                    if (bands[b] > frameMax) {
                        frameMax = bands[b];
                    }
                }
                byte[] packet = new byte[1 + BANDS];
                packet[0] = MAGIC;
                boolean silent = frameMax < 1e-4;
                double[] dbBands = new double[BANDS];
                for (int b = 0; b < BANDS; b++) {
                    dbBands[b] = 20.0 * Math.log10(bands[b] + 1e-12);
                }
                double lastDb = -100;
                for (int b = 0; b < BANDS; b++) {
                    float level = 0;
                    if (!silent) {
                        // дробнооктавное сглаживание: соседние полосы подмешиваются,
                        // поэтому край спектра получается мягким, а не ступенькой
                        double sum = 0;
                        double wsum = 0;
                        for (int k = -2; k <= 2; k++) {
                            int j = b + k;
                            if (j < 0 || j >= BANDS) {
                                continue;
                            }
                            double w = 1.0 / (1.0 + Math.abs(k));
                            sum += dbBands[j] * w;
                            wsum += w;
                        }
                        double db = sum / wsum;
                        if (b == 5) {
                            lastDb = db;
                        }
                        double v = (db + 70.0) / 70.0 * 15.0;
                        if (v < 0) v = 0;
                        if (v > 15) v = 15;
                        level = (float) v;
                    }
                    // быстрая атака, плавный спад — как фейдинг в оригинале
                    if (level >= smooth[b]) {
                        smooth[b] = level;
                    } else {
                        smooth[b] = smooth[b] * 0.90f + level * 0.10f;
                    }
                    packet[1 + b] = (byte) Math.round(smooth[b]);
                }
                if (fftCount <= 3 || fftCount % 100 == 0) {
                    Log.i(TAG, "fft=" + fftCount + " max=" + frameMax + " db5=" + Math.round(lastDb)
                            + " lv=" + packet[1] + "," + packet[2] + "," + packet[3] + "," + packet[4] + "," + packet[5]);
                }
                if (panelOut == null && System.currentTimeMillis() - lastPanelTry > 1500) {
                    lastPanelTry = System.currentTimeMillis();
                    openPanel();
                }
                if (panelOut != null) {
                    try {
                        panelOut.write(packet);
                        panelOut.flush();
                    } catch (Exception e) {
                        closePanel();
                    }
                }
            }
        }
    }

    private void openPanel() {
        try {
            panel = new Socket();
            panel.connect(new InetSocketAddress("127.0.0.1", PORT), 400);
            panel.setTcpNoDelay(true);
            panelOut = panel.getOutputStream();
            Log.i(TAG, "panel connected");
        } catch (Exception e) {
            Log.w(TAG, "panel connect: " + e);
            closePanel();
        }
    }

    private void closePanel() {
        try {
            if (panel != null) panel.close();
        } catch (Exception ignored) {
        }
        panel = null;
        panelOut = null;
    }

    private void closeTapFd() {
        FileDescriptor fd = tapFd;
        tapFd = null;
        if (fd == null) {
            return;
        }
        try {
            android.system.Os.close(fd);
        } catch (Throwable ignored) {
        }
    }

    private void openPCM() {
        try {
            pcm = new Socket();
            pcm.connect(new InetSocketAddress("127.0.0.1", 8095), 400);
            pcm.setTcpNoDelay(true);
            pcmOut = pcm.getOutputStream();
            Log.i(TAG, "pcm connected");
        } catch (Exception e) {
            closePCM();
        }
    }

    private void closePCM() {
        try {
            if (pcm != null) pcm.close();
        } catch (Exception ignored) {
        }
        pcm = null;
        pcmOut = null;
    }

    private static void sleep(long ms) {
        try {
            Thread.sleep(ms);
        } catch (InterruptedException ignored) {
        }
    }

    private static FileDescriptor[] socketpair() throws Exception {
        Class<?> os = Class.forName("android.system.Os");
        FileDescriptor a = new FileDescriptor();
        FileDescriptor b = new FileDescriptor();
        Method m = os.getMethod("socketpair", int.class, int.class, int.class, FileDescriptor.class, FileDescriptor.class);
        m.invoke(null, 1, 5, 0, a, b);
        return new FileDescriptor[]{a, b};
    }

    private static int osRead(FileDescriptor fd, byte[] buf, int off, int len) throws Exception {
        Class<?> os = Class.forName("android.system.Os");
        Method m = os.getMethod("read", FileDescriptor.class, byte[].class, int.class, int.class);
        return (Integer) m.invoke(null, fd, buf, off, len);
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
}
