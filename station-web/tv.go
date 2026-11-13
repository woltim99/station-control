package main

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	airplayProgressFile = "/data/data/com.hkfuertes.shairport/files/airplay-progress"
	tvAppPkg            = "com.opencode.airplaytv"
	tvAppActivity       = tvAppPkg + "/.MainActivity"
	tvMusicPkg          = "com.yandex.tv.music"
	tvMusicActivity     = tvMusicPkg + "/.HostActivity"
	tvVideoPkg          = "io.github.jqssun.airplay"
	tvVideoActivity     = tvVideoPkg + "/.MainActivity"
)

var (
	tvMu           sync.Mutex
	tvLaunched     bool
	tvLaunchAt     time.Time
	tvEnsureAt     time.Time
	tvInstalled    bool
	tvMusicStopped bool
	tvDisabled     bool
	tvDisabledAt   time.Time
	tvMusicHold    time.Time
	tvLastState    string
	tvVideoAt      time.Time
	tvVideoVal     bool
	tvVideoHandled bool
)

// airplayProgress читает реальные duration/position, которые приёмник
// Shairport пишет из metadata AirPlay (миллисекунды). Файл перезаписывается
// не атомарно, поэтому последнее валидное значение кэшируем.
var (
	airplayProgMu    sync.Mutex
	airplayProgCache struct {
		Dur, Pos int64
		At       time.Time
	}
)

func airplayProgress() (int64, int64, time.Time) {
	data, err := os.ReadFile(airplayProgressFile)
	airplayProgMu.Lock()
	defer airplayProgMu.Unlock()
	if err == nil {
		line := strings.TrimSpace(string(data))
		if i := strings.IndexByte(line, '\n'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		parts := strings.Split(line, ",")
		if len(parts) == 3 {
			ts, e1 := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
			dur, e2 := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
			pos, e3 := strconv.ParseInt(strings.TrimSpace(parts[2]), 10, 64)
			// Пустые/битые поля — это недописанный файл, а не позиция 0.
			if e1 == nil && e2 == nil && e3 == nil && ts > 0 && dur > 0 && pos >= 0 && pos <= dur {
				if pos < 0 {
					pos = 0
				}
				at := time.Time{}
				if ts > 0 {
					at = time.Unix(ts/1000, (ts%1000)*int64(time.Millisecond))
				}
				// FileWriter сначала обнуляет файл, потом пишет строку.
				// Если поймали обрывок, позиция падает почти в ноль, хотя
				// трек тот же и по времени уже далеко впереди. Не принимаем.
				if !airplayProgCache.At.IsZero() && dur == airplayProgCache.Dur && pos <= 500 {
					projected := airplayProgCache.Pos + at.Sub(airplayProgCache.At).Milliseconds()
					if projected > 3000 {
						return airplayProgCache.Dur, airplayProgCache.Pos, airplayProgCache.At
					}
				}
				airplayProgCache.Dur, airplayProgCache.Pos, airplayProgCache.At = dur, pos, at
				return dur, pos, at
			}
		}
	}
	if airplayProgCache.Dur > 0 {
		return airplayProgCache.Dur, airplayProgCache.Pos, airplayProgCache.At
	}
	return 0, 0, time.Time{}
}

// tvCleanArtist убирает служебные приставки Apple/Яндекса из исполнителя.
func tvCleanArtist(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, " • "); i > 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}

func tvState() map[string]any {
	info := airplaySnapshotWithViz()
	state := info.State
	if state == "" {
		state = "idle"
	}
	return map[string]any{
		"state":       state,
		"mode":        info.Mode,
		"playing":     info.Playing,
		"title":       info.Title,
		"artist":      tvCleanArtist(info.Artist),
		"album":       info.Album,
		"cover":       info.Cover,
		"cover_at":    info.CoverAt,
		"position_ms": info.PositionMS,
		"duration_ms": info.DurationMS,
		"native":      info.Native,
		"viz":         info.VizActive,
		"name":        info.Name,
	}
}

func tvLoopback(r *http.Request) bool {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func tvAppInstalled() bool {
	tvMu.Lock()
	if !tvEnsureAt.IsZero() && time.Since(tvEnsureAt) < 30*time.Second {
		ok := tvInstalled
		tvMu.Unlock()
		return ok
	}
	tvEnsureAt = time.Now()
	tvMu.Unlock()
	out, code := run(5*time.Second, "/system/bin/pm", "path", tvAppPkg)
	ok := code == 0 && strings.Contains(out, "package:")
	tvMu.Lock()
	tvInstalled = ok
	tvMu.Unlock()
	return ok
}

// tvFocused проверяет, что наш экран действительно на переднем плане.
func tvFocused() bool {
	out, _ := run(4*time.Second, "/system/bin/dumpsys", "window")
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "mCurrentFocus") && strings.Contains(line, tvAppPkg) {
			return true
		}
	}
	return false
}

// tvVideoProbe опрашивает систему: активна ли сессия видео-приёмника.
// Тяжёлый dumpsys, поэтому вызывается только из фонового наблюдателя.
func tvVideoProbe() bool {
	active := false
	out, _ := run(4*time.Second, "/system/bin/dumpsys", "media_session")
	if strings.Contains(out, tvVideoPkg+"/AirPlay") {
		inBlock := false
		lines := 0
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "Media button session is") {
				continue
			}
			if strings.Contains(line, tvVideoPkg+"/AirPlay (userId=") {
				inBlock = true
				lines = 0
				continue
			}
			if !inBlock {
				continue
			}
			lines++
			if strings.Contains(line, "active=true") {
				active = true
				break
			}
			if strings.Contains(line, "active=false") || lines > 15 {
				break
			}
		}
	}
	if !active {
		// Запасной признак: приёмник сейчас на переднем плане.
		w, _ := run(4*time.Second, "/system/bin/dumpsys", "window")
		for _, line := range strings.Split(w, "\n") {
			if strings.Contains(line, "mCurrentFocus") && strings.Contains(line, tvVideoPkg) {
				active = true
				break
			}
		}
	}
	return active
}

// tvVideoActive возвращает последнее известное состояние без блокировки
// аудио-опроса. Значение обновляет tvVideoWatch.
func tvVideoActive() bool {
	tvMu.Lock()
	v := tvVideoVal
	tvMu.Unlock()
	return v
}

// tvVideoWatch следит за видео-приёмником и убирает аудио-экран, как только
// начинается трансляция. Проверка идёт отдельно от опроса AirPlay-аудио,
// чтобы dumpsys не задерживал обновление метаданных.
func tvVideoWatch() {
	for {
		v := tvVideoProbe()
		tvMu.Lock()
		tvVideoAt = time.Now()
		tvVideoVal = v
		tvMu.Unlock()
		if v {
			tvCloseForVideo()
		} else {
			tvVideoClear()
		}
		time.Sleep(4 * time.Second)
	}
}

// tvCloseForVideo закрывает аудио-экран и возвращает Я.Музыку в доступное
// состояние, но не запускает её. Вызывается один раз за видео-сессию.
func tvCloseForVideo() {
	tvMu.Lock()
	if tvVideoHandled {
		tvMu.Unlock()
		return
	}
	tvVideoHandled = true
	restoreMusic := tvMusicStopped || tvDisabled
	tvLaunched = false
	tvLaunchAt = time.Time{}
	tvMusicStopped = false
	tvDisabled = false
	tvDisabledAt = time.Now()
	tvMu.Unlock()

	run(6*time.Second, "/system/bin/am", "force-stop", tvAppPkg)
	if restoreMusic {
		run(6*time.Second, "/system/bin/pm", "enable", tvMusicPkg)
		run(6*time.Second, "/system/bin/pm", "enable", tvMusicActivity)
	}
	logTV("closed for video")
}

func tvVideoClear() {
	tvMu.Lock()
	tvVideoHandled = false
	tvMu.Unlock()
}

// tvEnsure открывает отдельный AirPlay-экран на ТВ при старте сессии.
func tvEnsure() {
	if tvVideoActive() {
		tvCloseForVideo()
		return
	}
	tvVideoClear()
	tvMu.Lock()
	if !tvMusicHold.IsZero() && time.Now().Before(tvMusicHold) {
		tvMu.Unlock()
		return
	}
	if tvLaunched {
		tvMu.Unlock()
		return
	}
	tvLaunched = true
	tvLaunchAt = time.Now()
	tvMu.Unlock()
	if !tvAppInstalled() {
		tvMu.Lock()
		tvLaunched = false
		tvMu.Unlock()
		return
	}
	go func() {
		// Приложение Я.Музыки — полноэкранный ТВ-клиент, который система
		// поднимает при активном звуке. Пока идёт AirPlay, отключаем только
		// его экран (HostActivity), а не весь пакет: сам плеер живёт в
		// com.yandex.io.sdk, и пакет должен оставаться доступным.
		_, _ = run(8*time.Second, "/system/bin/pm", "enable", tvMusicPkg)
		if out, code := run(8*time.Second, "/system/bin/pm", "disable", tvMusicActivity); code != 0 {
			logTV("disable music ui: " + strings.TrimSpace(out))
		}
		tvMu.Lock()
		tvMusicStopped = true
		tvDisabled = true
		tvDisabledAt = time.Now()
		tvMu.Unlock()
		time.Sleep(400 * time.Millisecond)
		for i := 0; i < 4; i++ {
			args := []string{"start", "-n", tvAppActivity}
			if i > 0 {
				args = append(args, "-f", "0x04000000")
			}
			out, code := run(8*time.Second, "/system/bin/am", args...)
			if code != 0 {
				logTV("start error: " + strings.TrimSpace(out))
			}
			time.Sleep(2 * time.Second)
			if tvFocused() {
				break
			}
		}
		logTV("opened")
	}()
}

func tvMusicDisabledNow() bool {
	tvMu.Lock()
	if !tvDisabledAt.IsZero() && time.Since(tvDisabledAt) < 30*time.Second {
		v := tvDisabled
		tvMu.Unlock()
		return v
	}
	tvDisabledAt = time.Now()
	tvMu.Unlock()
	out, _ := run(6*time.Second, "/system/bin/dumpsys", "package", tvMusicPkg)
	v := strings.Contains(out, "disabledComponents:") &&
		strings.Contains(out, tvMusicPkg+".HostActivity")
	tvMu.Lock()
	tvDisabled = v
	tvDisabledAt = time.Now()
	tvMu.Unlock()
	return v
}

func tvRestoreIfIdle() {
	if tvVideoActive() {
		tvCloseForVideo()
		return
	}
	tvVideoClear()
	tvMu.Lock()
	launched := tvLaunched
	launchAt := tvLaunchAt
	tvMu.Unlock()
	disabled := tvMusicDisabledNow()
	if !launched && !disabled {
		return
	}
	if launched && !disabled && !tvMusicStopped {
		return
	}
	if launched && !launchAt.IsZero() && time.Since(launchAt) < 20*time.Second {
		return
	}
	airplayMetaMu.Lock()
	last := airplayLastLiveAt
	airplayMetaMu.Unlock()
	// Пока телефон держит AirPlay-сессию (даже на паузе), даём запас
	// времени. Если сессия уже разорвана — возвращаем Я.Музыку быстро.
	grace := 20 * time.Second
	if !airplaySessionConnected() {
		grace = 5 * time.Second
	}
	if !last.IsZero() && time.Since(last) < grace {
		return
	}
	if launched {
		tvReset()
		return
	}
	tvEnableMusic()
}

func tvEnableMusic() {
	tvMu.Lock()
	tvMusicStopped = false
	tvDisabled = false
	tvDisabledAt = time.Now()
	tvMu.Unlock()
	logTV("restore music")
	go func() {
		run(6*time.Second, "/system/bin/pm", "enable", tvMusicPkg)
		run(6*time.Second, "/system/bin/pm", "enable", tvMusicActivity)
		time.Sleep(500 * time.Millisecond)
		if tvVideoActive() {
			logTV("restore music skipped: video active")
			return
		}
		// Если пользователь всё ещё на нашем экране (например, поставил
		// паузу), не отбираем фокус: просто возвращаем экран в лаунчер.
		if !tvFocused() {
			run(8*time.Second, "/system/bin/am", "start", "-n", tvMusicActivity)
		}
	}()
}

// tvEnableMusicForce возвращает Я.Музыку сразу — для явного выключения
// AirPlay из панели, не дожидаясь таймаута простоя.
func tvEnableMusicForce() {
	tvMu.Lock()
	tvLaunched = false
	tvLaunchAt = time.Time{}
	tvMusicStopped = false
	tvDisabled = false
	tvDisabledAt = time.Now()
	// Не даём синхронному airplayApply снова отключить экран Я.Музыки,
	// пока приложение выходит на передний план.
	tvMusicHold = time.Now().Add(8 * time.Second)
	tvMu.Unlock()
	logTV("restore music (manual)")
	go func() {
		run(6*time.Second, "/system/bin/pm", "enable", tvMusicPkg)
		run(6*time.Second, "/system/bin/pm", "enable", tvMusicActivity)
		time.Sleep(400 * time.Millisecond)
		run(8*time.Second, "/system/bin/am", "start", "-n", tvMusicActivity)
	}()
}

func tvOpen() {
	tvMu.Lock()
	tvLaunched = false
	tvLaunchAt = time.Time{}
	tvMusicHold = time.Time{}
	tvMu.Unlock()
	tvEnsure()
}

func tvReset() {
	tvMu.Lock()
	if !tvLaunched {
		tvMu.Unlock()
		return
	}
	if !tvLaunchAt.IsZero() && time.Since(tvLaunchAt) < 15*time.Second {
		tvMu.Unlock()
		return
	}
	tvLaunched = false
	restoreMusic := tvMusicStopped || tvDisabled
	tvMusicStopped = false
	tvDisabled = false
	tvDisabledAt = time.Now()
	tvMu.Unlock()
	logTV("reset")
	if restoreMusic {
		tvEnableMusic()
	}
}

func logTV(msg string) {
	log.Printf("tv: %s", msg)
}

func registerTVHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/api/tv/state", func(w http.ResponseWriter, r *http.Request) {
		if !tvLoopback(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "local only"})
			return
		}
		writeJSON(w, http.StatusOK, tvState())
	})
	mux.HandleFunc("/api/tv/action", func(w http.ResponseWriter, r *http.Request) {
		if !tvLoopback(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "local only"})
			return
		}
		var req struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		switch req.Action {
		case "play", "pause", "play_pause", "next", "prev":
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown action"})
			return
		}
		action := req.Action
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": action})
		go func() {
			if err := airplayRemote(action); err != nil {
				log.Printf("tv action %s: %v", action, err)
				return
			}
			airplayRefreshSoon()
		}()
	})
	mux.HandleFunc("/api/tv/music", func(w http.ResponseWriter, r *http.Request) {
		if !tvLoopback(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "local only"})
			return
		}
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		go func() {
			// Ставим AirPlay на паузу и даём буферу дозвучать, затем
			// возвращаем оригинальный экран Я.Музыки.
			if err := airplayRemote("pause"); err != nil {
				log.Printf("tv music: pause: %v", err)
			}
			time.Sleep(2200 * time.Millisecond)
			tvEnableMusicForce()
		}()
	})
	mux.HandleFunc("/api/airplay/tv", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		tvOpen()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("/api/airplay/video", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		tvCloseForVideo()
		out, code := run(8*time.Second, "/system/bin/am", "start", "-n", tvVideoActivity)
		if code != 0 {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": strings.TrimSpace(out)})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("/api/theme", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		var req struct {
			Accent string `json:"accent"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		accent := strings.ToLower(strings.TrimSpace(req.Accent))
		switch accent {
		case "lime", "mint", "amber", "ice", "mono":
		default:
			accent = "lime"
		}
		if out, code := run(5*time.Second, "/system/bin/settings", "put", "global", "station_accent", accent); code != 0 {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": strings.TrimSpace(out)})
			return
		}
		_, _ = run(5*time.Second, "/system/bin/cmd", "uimode", "night", "yes")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accent": accent})
	})
}
