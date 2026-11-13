package com.opencode.airplaytv;

import android.app.Activity;
import android.graphics.Bitmap;
import android.graphics.BitmapFactory;
import android.graphics.Color;
import android.graphics.Outline;
import android.graphics.Typeface;
import android.graphics.drawable.ClipDrawable;
import android.graphics.drawable.Drawable;
import android.graphics.drawable.GradientDrawable;
import android.graphics.drawable.LayerDrawable;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.os.SystemClock;
import android.renderscript.Allocation;
import android.renderscript.Element;
import android.renderscript.RenderScript;
import android.renderscript.ScriptIntrinsicBlur;
import android.view.Gravity;
import android.view.KeyEvent;
import android.view.View;
import android.view.ViewOutlineProvider;
import android.view.WindowManager;
import android.widget.FrameLayout;
import android.widget.ImageView;
import android.widget.LinearLayout;
import android.widget.ProgressBar;
import android.widget.TextView;

import org.json.JSONObject;

import java.io.BufferedReader;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.nio.charset.StandardCharsets;

public class MainActivity extends Activity {
    private static final String BASE = "http://127.0.0.1:8080";
    private static final String STATE_URL = BASE + "/api/tv/state";
    private static final String ACTION_URL = BASE + "/api/tv/action";

    private ImageView bgCover;
    private ImageView cover;
    private TextView artist;
    private TextView title;
    private TextView label;
    private TextView musicTab;
    private TextView time;
    private ProgressBar progress;
    private View progressThumb;
    private int progressWidthPx;
    private boolean playingNow;
    private long fastPollUntil;
    private String lastTitle = "";
    private boolean switching;
    private long switchUntil;
    private boolean playingOverride;
    private long playingOverrideUntil;
    private FrameLayout rootView;
    private boolean musicSwitching;
    private long backDownAt;
    private boolean backIgnore;
    private long uiPos;
    private long uiDur;
    private long uiAt;
    private boolean uiPlaying;

    private final Runnable uiTick = new Runnable() {
        @Override
        public void run() {
            if (!running) {
                return;
            }
            renderProgress();
            handler.postDelayed(uiTick, 200);
        }
    };

    private final Handler handler = new Handler(Looper.getMainLooper());
    private boolean running;
    private String lastCover = "";
    private int pollToken;

    private final Runnable poll = new Runnable() {
        @Override
        public void run() {
            if (!running) {
                return;
            }
            final int token = pollToken;
            new Thread(new Runnable() {
                @Override
                public void run() {
                    final JSONObject j = fetchState();
                    handler.post(new Runnable() {
                        @Override
                        public void run() {
                            if (!running || token != pollToken) {
                                return;
                            }
                            if (j != null) {
                                apply(j);
                            }
                            if (running) {
                                if (switching && System.currentTimeMillis() > switchUntil) {
                                    endSwitch();
                                }
                                long delay = System.currentTimeMillis() < fastPollUntil ? 150 : 800;
                                handler.postDelayed(poll, delay);
                            }
                        }
                    });
                }
            }).start();
        }
    };

    private int dp(float v) {
        return Math.round(v * getResources().getDisplayMetrics().density);
    }

    private GradientDrawable rounded(int color, int radius) {
        GradientDrawable d = new GradientDrawable();
        d.setColor(color);
        d.setCornerRadius(dp(radius));
        return d;
    }

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        getWindow().addFlags(WindowManager.LayoutParams.FLAG_FULLSCREEN
                | WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON);
        getWindow().getDecorView().setSystemUiVisibility(
                View.SYSTEM_UI_FLAG_FULLSCREEN
                        | View.SYSTEM_UI_FLAG_HIDE_NAVIGATION
                        | View.SYSTEM_UI_FLAG_IMMERSIVE_STICKY
                        | View.SYSTEM_UI_FLAG_LAYOUT_FULLSCREEN
                        | View.SYSTEM_UI_FLAG_LAYOUT_HIDE_NAVIGATION);

        int screenW = getResources().getDisplayMetrics().widthPixels;
        int screenH = getResources().getDisplayMetrics().heightPixels;

        FrameLayout root = new FrameLayout(this);
        root.setBackgroundColor(Color.rgb(21, 21, 23));
        root.setFocusable(true);
        root.setFocusableInTouchMode(true);
        rootView = root;

        bgCover = new ImageView(this);
        bgCover.setScaleType(ImageView.ScaleType.CENTER_CROP);
        root.addView(bgCover, new FrameLayout.LayoutParams(
                FrameLayout.LayoutParams.MATCH_PARENT,
                FrameLayout.LayoutParams.MATCH_PARENT));

        View shadow = new View(this);
        shadow.setBackgroundColor(0x4D352020);
        root.addView(shadow, new FrameLayout.LayoutParams(
                FrameLayout.LayoutParams.MATCH_PARENT,
                FrameLayout.LayoutParams.MATCH_PARENT));

        View fade = new View(this);
        fade.setBackground(new GradientDrawable(GradientDrawable.Orientation.TOP_BOTTOM,
                new int[]{0x00000000, 0x33000000, 0xB3000000}));
        root.addView(fade, new FrameLayout.LayoutParams(
                FrameLayout.LayoutParams.MATCH_PARENT,
                FrameLayout.LayoutParams.MATCH_PARENT));

        label = new TextView(this);
        label.setTextColor(Color.WHITE);
        label.setTextSize(20);
        label.setText("AirPlay");
        label.setGravity(Gravity.CENTER);
        label.setSingleLine(true);
        label.setTypeface(Typeface.create("sans-serif-medium", Typeface.NORMAL));

        musicTab = new TextView(this);
        musicTab.setText("\u042F.\u041C\u0443\u0437\u044B\u043A\u0430");
        musicTab.setTextColor(0xFF9C9C9F);
        musicTab.setTextSize(20);
        musicTab.setGravity(Gravity.CENTER);
        musicTab.setSingleLine(true);
        musicTab.setTypeface(Typeface.create("sans-serif-medium", Typeface.NORMAL));
        musicTab.setFocusable(true);
        musicTab.setClickable(true);
        musicTab.setBackground(rounded(0x00000000, 19));
        musicTab.setOnClickListener(new View.OnClickListener() {
            @Override
            public void onClick(View v) {
                switchToMusic();
            }
        });
        musicTab.setOnFocusChangeListener(new View.OnFocusChangeListener() {
            @Override
            public void onFocusChange(View v, boolean has) {
                v.setBackground(rounded(has ? 0x1FFFFFFF : 0x00000000, 19));
                ((TextView) v).setTextColor(has ? Color.WHITE : 0xFF9C9C9F);
            }
        });

        LinearLayout tabs = new LinearLayout(this);
        tabs.setOrientation(LinearLayout.HORIZONTAL);
        tabs.setGravity(Gravity.CENTER_VERTICAL);
        tabs.setBackground(rounded(0x11FFFFFF, 22));
        tabs.setPadding(dp(3), dp(3), dp(3), dp(3));
        tabs.addView(label, new LinearLayout.LayoutParams(0,
                LinearLayout.LayoutParams.MATCH_PARENT, 1f));
        tabs.addView(musicTab, new LinearLayout.LayoutParams(0,
                LinearLayout.LayoutParams.MATCH_PARENT, 1f));

        FrameLayout.LayoutParams lp = new FrameLayout.LayoutParams(
                dp(248), dp(45));
        lp.gravity = Gravity.TOP | Gravity.CENTER_HORIZONTAL;
        lp.topMargin = dp(26);
        root.addView(tabs, lp);

        LinearLayout head = new LinearLayout(this);
        head.setOrientation(LinearLayout.VERTICAL);
        head.setGravity(Gravity.CENTER_HORIZONTAL);
        FrameLayout.LayoutParams hp = new FrameLayout.LayoutParams(
                FrameLayout.LayoutParams.MATCH_PARENT,
                FrameLayout.LayoutParams.WRAP_CONTENT);
        hp.gravity = Gravity.TOP;
        hp.topMargin = dp(84);
        root.addView(head, hp);

        artist = new TextView(this);
        artist.setTextColor(0x80FFFFFF);
        artist.setTextSize(17);
        artist.setGravity(Gravity.CENTER);
        artist.setSingleLine(true);
        artist.setTypeface(Typeface.create("sans-serif", Typeface.NORMAL));
        head.addView(artist);

        title = new TextView(this);
        title.setTextColor(Color.WHITE);
        title.setTextSize(28);
        title.setGravity(Gravity.CENTER);
        title.setSingleLine(true);
        title.setTypeface(Typeface.create("sans-serif-medium", Typeface.NORMAL));
        LinearLayout.LayoutParams tp = new LinearLayout.LayoutParams(
                LinearLayout.LayoutParams.MATCH_PARENT,
                LinearLayout.LayoutParams.WRAP_CONTENT);
        tp.topMargin = dp(2);
        head.addView(title, tp);

        int coverSize = Math.min(dp(318), Math.min(screenW - dp(120), screenH - dp(265)));
        if (coverSize < dp(180)) {
            coverSize = Math.min(screenW - dp(80), screenH - dp(220));
        }
        FrameLayout coverBox = new FrameLayout(this);
        FrameLayout.LayoutParams cbp = new FrameLayout.LayoutParams(
                coverSize + dp(96), coverSize);
        cbp.gravity = Gravity.TOP | Gravity.CENTER_HORIZONTAL;
        cbp.topMargin = dp(160);
        root.addView(coverBox, cbp);

        cover = new ImageView(this);
        cover.setScaleType(ImageView.ScaleType.CENTER_CROP);
        cover.setBackground(rounded(0x00000000, 8));
        cover.setClipToOutline(true);
        cover.setOutlineProvider(new ViewOutlineProvider() {
            @Override
            public void getOutline(View v, Outline o) {
                o.setRoundRect(0, 0, v.getWidth(), v.getHeight(), dp(8));
            }
        });
        FrameLayout.LayoutParams cp = new FrameLayout.LayoutParams(coverSize, coverSize);
        cp.gravity = Gravity.CENTER_HORIZONTAL | Gravity.TOP;
        coverBox.addView(cover, cp);

        TextView left = arrow("\u2039");
        FrameLayout.LayoutParams alp = new FrameLayout.LayoutParams(
                FrameLayout.LayoutParams.WRAP_CONTENT,
                FrameLayout.LayoutParams.WRAP_CONTENT);
        alp.gravity = Gravity.LEFT | Gravity.CENTER_VERTICAL;
        coverBox.addView(left, alp);

        TextView right = arrow("\u203A");
        FrameLayout.LayoutParams arp = new FrameLayout.LayoutParams(
                FrameLayout.LayoutParams.WRAP_CONTENT,
                FrameLayout.LayoutParams.WRAP_CONTENT);
        arp.gravity = Gravity.RIGHT | Gravity.CENTER_VERTICAL;
        coverBox.addView(right, arp);

        FrameLayout progressBox = new FrameLayout(this);
        FrameLayout.LayoutParams pbp = new FrameLayout.LayoutParams(
                coverSize, dp(18));
        pbp.gravity = Gravity.BOTTOM | Gravity.CENTER_HORIZONTAL;
        pbp.bottomMargin = dp(74);
        root.addView(progressBox, pbp);

        progress = new ProgressBar(this, null, android.R.attr.progressBarStyleHorizontal);
        progress.setMax(1000);
        progress.setProgress(0);
        GradientDrawable track = rounded(0x4CFFFFFF, 2);
        GradientDrawable bar = rounded(0xFFFFFFFF, 2);
        ClipDrawable clip = new ClipDrawable(bar, Gravity.LEFT, ClipDrawable.HORIZONTAL);
        LayerDrawable ld = new LayerDrawable(new Drawable[]{track, clip});
        ld.setId(0, android.R.id.background);
        ld.setId(1, android.R.id.progress);
        progress.setProgressDrawable(ld);
        FrameLayout.LayoutParams pp = new FrameLayout.LayoutParams(
                FrameLayout.LayoutParams.MATCH_PARENT, dp(4));
        pp.gravity = Gravity.CENTER_VERTICAL;
        progressBox.addView(progress, pp);

        progressThumb = new View(this);
        progressThumb.setBackground(rounded(0xFFFFFFFF, 1));
        FrameLayout.LayoutParams thp = new FrameLayout.LayoutParams(dp(2), dp(15));
        thp.gravity = Gravity.LEFT | Gravity.CENTER_VERTICAL;
        progressBox.addView(progressThumb, thp);
        progressWidthPx = coverSize;

        time = new TextView(this);
        time.setTextColor(0x99FFFFFF);
        time.setTextSize(16);
        time.setGravity(Gravity.CENTER);
        time.setText("0:00 / --:--");
        FrameLayout.LayoutParams tmp = new FrameLayout.LayoutParams(
                FrameLayout.LayoutParams.MATCH_PARENT,
                FrameLayout.LayoutParams.WRAP_CONTENT);
        tmp.gravity = Gravity.BOTTOM | Gravity.CENTER_HORIZONTAL;
        tmp.bottomMargin = dp(42);
        root.addView(time, tmp);

        setContentView(root);
        root.requestFocus();

        running = true;
        handler.post(poll);
        handler.post(uiTick);
    }

    private TextView arrow(String s) {
        TextView v = new TextView(this);
        v.setText(s);
        v.setTextColor(0x59FFFFFF);
        v.setTextSize(46);
        v.setTypeface(Typeface.create("sans-serif-light", Typeface.NORMAL));
        v.setFocusable(false);
        return v;
    }

    private void beginSwitch() {
        switching = true;
        switchUntil = System.currentTimeMillis() + 4000;
        title.animate().alpha(0.3f).setDuration(120).start();
        artist.animate().alpha(0.22f).setDuration(120).start();
        cover.animate().alpha(0.45f).setDuration(120).start();
        bgCover.animate().alpha(0.55f).setDuration(120).start();
    }

    private void endSwitch() {
        switching = false;
        switchUntil = 0;
        title.animate().alpha(1f).setDuration(220).start();
        artist.animate().alpha(1f).setDuration(220).start();
        cover.animate().alpha(1f).setDuration(220).start();
        bgCover.animate().alpha(1f).setDuration(220).start();
    }

    private void renderProgress() {
        long dur = uiDur;
        long pos = uiPos;
        if (uiPlaying && uiAt > 0) {
            long dt = SystemClock.elapsedRealtime() - uiAt;
            if (dt > 0) {
                pos += dt;
            }
        }
        if (dur > 0) {
            if (pos < 0) pos = 0;
            if (pos > dur) pos = dur;
            float frac = (float) pos / (float) dur;
            progress.setMax(1000);
            progress.setProgress((int) (frac * 1000));
            if (progressThumb != null) {
                progressThumb.setTranslationX(frac * Math.max(0, progressWidthPx - dp(2)));
            }
            time.setText(fmt(pos) + " / " + fmt(dur));
        } else {
            progress.setProgress(0);
            if (progressThumb != null) {
                progressThumb.setTranslationX(0);
            }
            time.setText("0:00 / --:--");
        }
    }

    private void apply(JSONObject j) {
        String t = j.optString("title", "").trim();
        String a = j.optString("artist", "").trim();
        boolean playing = j.optBoolean("playing", false);
        long dur = j.optLong("duration_ms", 0);
        long pos = j.optLong("position_ms", 0);

        if (t.isEmpty()) {
            t = "AirPlay";
        }
        // Пауза/плей в AirPlay 2 применяется с задержкой (дренаж буфера),
        // поэтому несколько секунд доверяем нажатию, а не статусу приёмника.
        if (playingOverride) {
            if (playing == playingNow || System.currentTimeMillis() > playingOverrideUntil) {
                playingOverride = false;
            }
        } else {
            playingNow = playing;
        }
        title.setText(t);
        artist.setText(a);
        if (!t.equals(lastTitle)) {
            lastTitle = t;
            if (switching) {
                endSwitch();
            }
        }
        uiPos = pos;
        uiDur = dur;
        uiAt = SystemClock.elapsedRealtime();
        uiPlaying = playingNow;
        renderProgress();

        String c = j.optString("cover", "").trim();
        if (!c.equals(lastCover)) {
            lastCover = c;
            if (c.isEmpty()) {
                cover.setImageDrawable(null);
                bgCover.setImageDrawable(null);
            } else {
                loadCover(BASE + c);
            }
        }
    }

    private void loadCover(final String url) {
        new Thread(new Runnable() {
            @Override
            public void run() {
                try {
                    HttpURLConnection c = (HttpURLConnection) new URL(url).openConnection();
                    c.setConnectTimeout(3000);
                    c.setReadTimeout(3000);
                    c.setUseCaches(false);
                    InputStream in = c.getInputStream();
                    final Bitmap b = BitmapFactory.decodeStream(in);
                    in.close();
                    c.disconnect();
                    if (b == null) {
                        return;
                    }
                    final Bitmap blurred = blur(b);
                    handler.post(new Runnable() {
                        @Override
                        public void run() {
                            if (url.equals(BASE + lastCover)) {
                                cover.setImageBitmap(b);
                                if (blurred != null) {
                                    bgCover.setImageBitmap(blurred);
                                } else {
                                    bgCover.setImageBitmap(b);
                                }
                            }
                        }
                    });
                } catch (Throwable ignored) {
                }
            }
        }).start();
    }

    private Bitmap blur(Bitmap src) {
        try {
            int w = 120;
            int h = Math.max(1, src.getHeight() * w / Math.max(1, src.getWidth()));
            Bitmap small = Bitmap.createScaledBitmap(src, w, h, true);
            RenderScript rs = RenderScript.create(this);
            Allocation in = Allocation.createFromBitmap(rs, small);
            Allocation out = Allocation.createFromBitmap(rs, small);
            ScriptIntrinsicBlur b = ScriptIntrinsicBlur.create(rs, Element.U8_4(rs));
            b.setRadius(8f);
            b.setInput(in);
            b.forEach(out);
            out.copyTo(small);
            rs.destroy();
            return Bitmap.createScaledBitmap(small, 640, 640, true);
        } catch (Throwable e) {
            return null;
        }
    }

    private JSONObject fetchState() {
        HttpURLConnection c = null;
        try {
            c = (HttpURLConnection) new URL(STATE_URL).openConnection();
            c.setConnectTimeout(2500);
            c.setReadTimeout(2500);
            c.setUseCaches(false);
            BufferedReader r = new BufferedReader(new InputStreamReader(c.getInputStream(), StandardCharsets.UTF_8));
            StringBuilder sb = new StringBuilder();
            String line;
            while ((line = r.readLine()) != null) {
                sb.append(line);
            }
            r.close();
            return new JSONObject(sb.toString());
        } catch (Throwable e) {
            return null;
        } finally {
            if (c != null) {
                c.disconnect();
            }
        }
    }

    private void sendAction(final String action) {
        new Thread(new Runnable() {
            @Override
            public void run() {
                HttpURLConnection c = null;
                try {
                    c = (HttpURLConnection) new URL(ACTION_URL).openConnection();
                    c.setConnectTimeout(3000);
                    c.setReadTimeout(3000);
                    c.setRequestMethod("POST");
                    c.setDoOutput(true);
                    c.setRequestProperty("Content-Type", "application/json");
                    byte[] body = ("{\"action\":\"" + action + "\"}").getBytes(StandardCharsets.UTF_8);
                    OutputStream out = c.getOutputStream();
                    out.write(body);
                    out.close();
                    c.getResponseCode();
                } catch (Throwable ignored) {
                } finally {
                    if (c != null) {
                        c.disconnect();
                    }
                }
                handler.post(new Runnable() {
                    @Override
                    public void run() {
                        pollToken++;
                        fastPollUntil = System.currentTimeMillis() + 3000;
                        handler.removeCallbacks(poll);
                        handler.post(poll);
                    }
                });
            }
        }).start();
    }

    private String fmt(long ms) {
        long total = Math.max(0, ms / 1000);
        long h = total / 3600;
        long m = (total % 3600) / 60;
        long s = total % 60;
        if (h > 0) {
            return String.format("%d:%02d:%02d", h, m, s);
        }
        return String.format("%d:%02d", m, s);
    }

    private void switchToMusic() {
        if (musicSwitching) {
            return;
        }
        musicSwitching = true;
        new Thread(new Runnable() {
            @Override
            public void run() {
                HttpURLConnection c = null;
                try {
                    c = (HttpURLConnection) new URL(BASE + "/api/tv/music").openConnection();
                    c.setConnectTimeout(2500);
                    c.setReadTimeout(2500);
                    c.setRequestMethod("POST");
                    c.setDoOutput(true);
                    c.setRequestProperty("Content-Type", "application/json");
                    OutputStream out = c.getOutputStream();
                    out.write("{}".getBytes(StandardCharsets.UTF_8));
                    out.close();
                    c.getResponseCode();
                } catch (Throwable ignored) {
                } finally {
                    if (c != null) {
                        c.disconnect();
                    }
                }
                handler.postDelayed(new Runnable() {
                    @Override
                    public void run() {
                        musicSwitching = false;
                    }
                }, 4000);
            }
        }).start();
    }

    @Override
    public boolean onKeyDown(int keyCode, KeyEvent event) {
        if (musicTab != null && musicTab.isFocused()) {
            if (keyCode == KeyEvent.KEYCODE_DPAD_CENTER || keyCode == KeyEvent.KEYCODE_ENTER) {
                switchToMusic();
                return true;
            }
            if (keyCode == KeyEvent.KEYCODE_BACK) {
                backIgnore = true;
                if (rootView != null) {
                    rootView.requestFocus();
                }
                return true;
            }
            if (keyCode == KeyEvent.KEYCODE_DPAD_DOWN) {
                if (rootView != null) {
                    rootView.requestFocus();
                }
                return true;
            }
            if (keyCode == KeyEvent.KEYCODE_DPAD_LEFT
                    || keyCode == KeyEvent.KEYCODE_DPAD_RIGHT
                    || keyCode == KeyEvent.KEYCODE_DPAD_UP) {
                return true;
            }
        }
        if (keyCode == KeyEvent.KEYCODE_DPAD_UP && musicTab != null) {
            musicTab.requestFocus();
            return true;
        }
        if (keyCode == KeyEvent.KEYCODE_DPAD_RIGHT || keyCode == KeyEvent.KEYCODE_MEDIA_NEXT) {
            beginSwitch();
            sendAction("next");
            return true;
        }
        if (keyCode == KeyEvent.KEYCODE_DPAD_LEFT || keyCode == KeyEvent.KEYCODE_MEDIA_PREVIOUS) {
            beginSwitch();
            sendAction("prev");
            return true;
        }
        if (keyCode == KeyEvent.KEYCODE_DPAD_CENTER
                || keyCode == KeyEvent.KEYCODE_ENTER
                || keyCode == KeyEvent.KEYCODE_MEDIA_PLAY_PAUSE
                || keyCode == KeyEvent.KEYCODE_HEADSETHOOK) {
            boolean next = !playingNow;
            playingNow = next;
            uiPlaying = next;
            playingOverride = true;
            playingOverrideUntil = System.currentTimeMillis() + 4000;
            sendAction(next ? "play" : "pause");
            return true;
        }
        if (keyCode == KeyEvent.KEYCODE_BACK) {
            if (event.getRepeatCount() > 0) {
                switchToMusic();
                backIgnore = true;
                return true;
            }
            if (event.getRepeatCount() == 0) {
                backDownAt = SystemClock.elapsedRealtime();
                backIgnore = false;
            }
            return true;
        }
        return super.onKeyDown(keyCode, event);
    }

    @Override
    public boolean onKeyUp(int keyCode, KeyEvent event) {
        if (keyCode == KeyEvent.KEYCODE_BACK) {
            if (backIgnore) {
                backIgnore = false;
                return true;
            }
            long down = backDownAt;
            backDownAt = 0;
            if (down == 0) {
                finish();
                return true;
            }
            if (SystemClock.elapsedRealtime() - down >= 700) {
                switchToMusic();
            } else {
                finish();
            }
            return true;
        }
        return super.onKeyUp(keyCode, event);
    }

    @Override
    protected void onResume() {
        super.onResume();
        running = true;
        if (rootView != null) {
            rootView.requestFocus();
        }
        pollToken++;
        handler.removeCallbacks(poll);
        handler.post(poll);
        handler.removeCallbacks(uiTick);
        handler.post(uiTick);
    }

    @Override
    protected void onPause() {
        running = false;
        pollToken++;
        handler.removeCallbacks(poll);
        handler.removeCallbacks(uiTick);
        super.onPause();
    }
}
