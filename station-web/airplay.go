package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AirPlay-приёмник — приложение com.hkfuertes.shairport (Shairport Sync + AAudio).
// Классический AirPlay работает без root; AirPlay 2 требует su/NQPTP, которого на
// станции нет. Управляем приложением через его adb-интерфейс (broadcast).
const (
	airplayPkg       = "com.hkfuertes.shairport"
	airplaySettings  = airplayPkg + "/.SettingsReceiver"
	airplayService   = airplayPkg + "/.ReceiverService"
	airplayGetStatus = airplayPkg + ".GET_STATUS"
	airplayConfigure = airplayPkg + ".CONFIGURE_SETTINGS"
)

// Apple добавляет к исполнителю служебные пометки вроде «• Доступно видео».
var airplayArtistNoise = regexp.MustCompile(`(?i)\s*[•·]\s*(доступно видео|доступно в dolby atmos|dolby atmos|lossless|apple digital master|music video|видео|explicit)\s*$`)

func airplayCleanArtist(s string) string {
	return strings.TrimSpace(airplayArtistNoise.ReplaceAllString(s, ""))
}

type airplayInfo struct {
	Installed  bool   `json:"installed"`
	Enabled    bool   `json:"enabled"`
	Running    bool   `json:"running"`
	State      string `json:"state"`
	Mode       string `json:"mode"`
	Source     string `json:"source"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	Album      string `json:"album"`
	Volume     int    `json:"volume"`
	Name       string `json:"name"`
	Error      string `json:"error,omitempty"`
	At         int64  `json:"at,omitempty"`
	VizEnabled bool   `json:"viz_enabled"`
	VizActive  bool   `json:"viz_active"`
	VizStyle   string `json:"viz_style"`
	Native     bool   `json:"native"`
	NativePCM  bool   `json:"native_pcm"`
	NativeName string `json:"native_style,omitempty"`
	NativeText string `json:"native_label,omitempty"`
	NativeErr  string `json:"native_error,omitempty"`
	Cover      string `json:"cover,omitempty"`
	CoverAt    int64  `json:"cover_at,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	PositionMS int64  `json:"position_ms,omitempty"`
	Connected  bool   `json:"connected"`
	Session    bool   `json:"session"`
	Playing    bool   `json:"playing"`
	Live       bool   `json:"-"`
}

var (
	airplayMu         sync.Mutex
	airplayCur        airplayInfo
	airplayAt         time.Time
	airplayOpMu       sync.Mutex
	airplayLastTrack  string
	airplayLastTitle  string
	airplayLastArtist string
	airplayLastAlbum  string
	airplayInstalled  bool
	airplayInstallAt  time.Time
	airplayAnnounceAt time.Time

	airplayMetaMu     sync.Mutex
	airplayLastLiveAt time.Time

	airplayConnMu  sync.Mutex
	airplayConnAt  time.Time
	airplayConnVal bool
)

// airplaySessionConnected проверяет, держит ли iPhone RTSP-соединение с
// приёмником. Соединение остаётся и на паузе, поэтому это надёжнее статуса
// движка, который на паузе очищает source/title.
func airplaySessionConnected() bool {
	airplayConnMu.Lock()
	if !airplayConnAt.IsZero() && time.Since(airplayConnAt) < 2*time.Second {
		v := airplayConnVal
		airplayConnMu.Unlock()
		return v
	}
	airplayConnMu.Unlock()

	ok := false
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if i == 0 {
				continue
			}
			f := strings.Fields(line)
			if len(f) < 4 || f[3] != "01" { // 01 = ESTABLISHED
				continue
			}
			local := f[1]
			idx := strings.LastIndex(local, ":")
			if idx < 0 {
				continue
			}
			if strings.EqualFold(local[idx+1:], "1388") { // RTSP 5000
				ok = true
				break
			}
		}
		if ok {
			break
		}
	}
	airplayConnMu.Lock()
	airplayConnVal = ok
	airplayConnAt = time.Now()
	airplayConnMu.Unlock()
	return ok
}

func airplaySnapshot() airplayInfo {
	airplayMu.Lock()
	defer airplayMu.Unlock()
	info := airplayCur
	info.Installed = airplayInstalled
	return info
}

func airplaySessionRecent() bool {
	airplayMetaMu.Lock()
	defer airplayMetaMu.Unlock()
	return !airplayLastLiveAt.IsZero() && time.Since(airplayLastLiveAt) < 20*time.Minute
}

func airplayRetained(info *airplayInfo) {
	if info.Title != "" {
		return
	}
	airplayMetaMu.Lock()
	t, a, al := airplayLastTitle, airplayLastArtist, airplayLastAlbum
	airplayMetaMu.Unlock()
	if t != "" {
		info.Title, info.Artist, info.Album = t, a, al
	}
}

func airplaySnapshotWithViz() airplayInfo {
	info := airplaySnapshot()
	info.VizEnabled, info.VizActive = vizEnabledState()
	info.VizStyle = vizStyleState()
	if nativeInstalledNow() {
		info.Native = info.VizEnabled
		info.NativeName = nativeReadStyle()
		info.NativeText = nativeLabelNow()
		info.NativePCM = nativePCMFresh()
		info.NativeErr = nativeError()
		if nativeActiveState() {
			info.VizActive = true
		}
	}
	info.Playing = info.Live || nativePCMFresh()
	info.Session = info.Playing || airplaySessionRecent()
	if dur, pos, at := airplayProgress(); dur > 0 {
		if info.Playing && !at.IsZero() && time.Since(at) < 10*time.Minute {
			pos += time.Since(at).Milliseconds()
			if pos > dur {
				pos = dur
			}
		}
		info.DurationMS, info.PositionMS = dur, pos
	}
	if info.Session {
		airplayRetained(&info)
		if u, at := airplayCover(); u != "" {
			info.Cover = u
			info.CoverAt = at
		}
	}
	return info
}

// ---------- обложка AirPlay ----------

const airplayCoverDir = "/data/data/com.hkfuertes.shairport/cache/coverart"

var (
	airplayCoverMu   sync.Mutex
	airplayCoverList time.Time
	airplayCoverPath string
	airplayCoverMod  int64
)

func airplayCoverExt(name string) bool {
	name = strings.ToLower(name)
	return strings.HasSuffix(name, ".jpg") || strings.HasSuffix(name, ".jpeg") ||
		strings.HasSuffix(name, ".png") || strings.HasSuffix(name, ".webp")
}

// airplayNewestCover отдаёт самый свежий файл из кэша обложек Shairport.
// Shairport кладёт туда artwork из AirPlay-метаданных.
func airplayNewestCover() (string, int64) {
	airplayCoverMu.Lock()
	if !airplayCoverList.IsZero() && time.Since(airplayCoverList) < 300*time.Millisecond {
		p, m := airplayCoverPath, airplayCoverMod
		airplayCoverMu.Unlock()
		return p, m
	}
	airplayCoverMu.Unlock()

	var best string
	var bestMod int64
	if entries, err := os.ReadDir(airplayCoverDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !airplayCoverExt(e.Name()) {
				continue
			}
			st, err := e.Info()
			if err != nil || st.Size() == 0 {
				continue
			}
			if mod := st.ModTime().UnixNano(); mod > bestMod {
				best, bestMod = airplayCoverDir+"/"+e.Name(), mod
			}
		}
	}
	airplayCoverMu.Lock()
	airplayCoverPath, airplayCoverMod, airplayCoverList = best, bestMod, time.Now()
	airplayCoverMu.Unlock()
	return best, bestMod
}

func airplayCover() (string, int64) {
	p, mod := airplayNewestCover()
	if p == "" {
		return "", 0
	}
	return "/api/airplay/cover?v=" + strconv.FormatInt(mod, 10), mod
}

var (
	airplayVolMu sync.Mutex
	airplayVolAt time.Time
)

// airplayPushVolume повторно рассылает VOLUME_CHANGED_ACTION, чтобы
// приложение-приёмник отправило новую громкость на iPhone. Нужно для
// физической шайбы, которая меняет громкость в обход системной рассылки.
func airplayPushVolume(prev, cur int) {
	if !airplaySessionRecent() {
		return
	}
	airplayVolMu.Lock()
	if !airplayVolAt.IsZero() && time.Since(airplayVolAt) < 400*time.Millisecond {
		airplayVolMu.Unlock()
		return
	}
	airplayVolAt = time.Now()
	airplayVolMu.Unlock()
	log.Printf("volume sync: %d -> %d", prev, cur)
	_, _ = run(3*time.Second, "/system/bin/am", "broadcast",
		"-a", "android.media.VOLUME_CHANGED_ACTION",
		"--ei", "android.media.EXTRA_VOLUME_STREAM_TYPE", "3",
		"--ei", "android.media.EXTRA_VOLUME_STREAM_VALUE", strconv.Itoa(cur),
		"--ei", "android.media.EXTRA_PREV_VOLUME_STREAM_VALUE", strconv.Itoa(prev))
}

func airplayInstalledNow() bool {
	airplayInstallAt = time.Now()
	out, code := run(5*time.Second, "/system/bin/pm", "path", airplayPkg)
	ok := code == 0 && strings.Contains(out, "package:")
	out2, _ := run(2*time.Second, "/system/bin/pm", "list", "packages", airplayPkg)
	if strings.Contains(out2, "package:"+airplayPkg) {
		ok = true
	}
	airplayInstalled = ok
	return ok
}

// airplayEngineRunning проверяет именно движок (:engine), а не процесс настроек:
// broadcast может поднять приложение, даже когда приёмник выключен.
func airplayEngineRunning() bool {
	out, _ := run(3*time.Second, "/system/bin/ps", "-A", "-o", "PID,NAME")
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, airplayPkg+":engine") || strings.Contains(line, "shairport:engine") {
			return true
		}
	}
	return false
}

func airplayReadLocked() airplayInfo {
	info := airplayInfo{Installed: true}
	out, code := run(5*time.Second, "/system/bin/cmd", "activity", "broadcast",
		"--include-stopped-packages", "-n", airplaySettings, "-a", airplayGetStatus)
	start := strings.Index(out, `data="`)
	if start < 0 {
		if code != 0 {
			info.Error = strings.TrimSpace(out)
		} else {
			info.Error = "нет ответа приложения"
		}
		return info
	}
	rest := out[start+6:]
	end := strings.Index(rest, `"`)
	if end < 0 {
		info.Error = "плохой ответ приложения"
		return info
	}
	q, err := url.ParseQuery(rest[:end])
	if err != nil {
		info.Error = err.Error()
		return info
	}
	info.State = q.Get("state")
	info.Mode = q.Get("mode")
	info.Source = q.Get("source")
	info.Title = q.Get("title")
	info.Artist = airplayCleanArtist(q.Get("artist"))
	info.Album = q.Get("album")
	info.Name = q.Get("server_name")
	info.Volume, _ = strconv.Atoi(q.Get("volume"))
	info.Enabled = q.Get("receiver_enabled") == "true"
	return info
}

func airplayRead() airplayInfo {
	airplayOpMu.Lock()
	defer airplayOpMu.Unlock()
	return airplayReadLocked()
}

func airplaySetLocked(key, value string) error {
	args := []string{"broadcast", "--include-stopped-packages", "-n", airplaySettings,
		"-a", airplayConfigure, "--es", "key", key}
	if value == "true" || value == "false" {
		args = append(args, "--ez", "value", value)
	} else {
		args = append(args, "--es", "value", value)
	}
	out, code := run(5*time.Second, "/system/bin/am", args...)
	if code != 0 {
		return fmt.Errorf("airplay: %s", strings.TrimSpace(out))
	}
	if strings.Contains(out, "result=0") && strings.Contains(out, "data=\"error") {
		return fmt.Errorf("airplay: %s", strings.TrimSpace(out))
	}
	return nil
}

func airplayStart() error {
	airplayOpMu.Lock()
	defer airplayOpMu.Unlock()
	if err := airplaySetLocked("receiver_enabled", "true"); err != nil {
		return err
	}
	out, code := run(10*time.Second, "/system/bin/am", "start-foreground-service", "-n", airplayService)
	if code != 0 {
		return fmt.Errorf("airplay: %s", strings.TrimSpace(out))
	}
	return nil
}

func airplayStop() error {
	airplayOpMu.Lock()
	defer airplayOpMu.Unlock()
	_ = airplaySetLocked("receiver_enabled", "false")
	out, code := run(10*time.Second, "/system/bin/am", "force-stop", airplayPkg)
	if code != 0 {
		return fmt.Errorf("airplay: %s", strings.TrimSpace(out))
	}
	return nil
}

func airplayApply(info airplayInfo) {
	info.Connected = airplaySessionConnected()
	info.Live = info.State == "playing" && info.Source != ""
	if info.Live || nativePCMFresh() {
		airplayMetaMu.Lock()
		airplayLastLiveAt = time.Now()
		airplayMetaMu.Unlock()
	}
	if info.Title != "" {
		airplayMetaMu.Lock()
		airplayLastTitle, airplayLastArtist, airplayLastAlbum = info.Title, info.Artist, info.Album
		airplayMetaMu.Unlock()
	} else if info.Connected || airplaySessionRecent() {
		// На паузе движок очищает title/artist/album — возвращаем последние.
		airplayRetained(&info)
		if info.Title == "" {
			// Панель перезапустилась посреди паузы: берём трек из медиасессии.
			m := mediaSnapshot()
			if m.Title != "" && !strings.EqualFold(m.Title, "AirPlay") && m.Artist != "Native visualizer" {
				info.Title, info.Artist = m.Title, m.Artist
			}
		}
	}
	info.At = time.Now().Unix()
	airplayMu.Lock()
	prev := airplayCur
	airplayCur = info
	airplayAt = time.Now()
	airplayMu.Unlock()

	// Начался AirPlay — ставим вендорную музыку на паузу, чтобы источники не смешались.
	if info.Live && !prev.Live && mediaSnapshot().Playing {
		if err := glagolSendCommand(map[string]any{"command": "stop"}); err == nil {
			addEvent("info", "AirPlay: Яндекс Музыка на паузе", "")
		}
	}

	if info.Live && info.Title != "" {
		key := strings.TrimSpace(info.Artist) + "\x00" + strings.TrimSpace(info.Title)
		airplayLastTrack = key
	}
	if info.Live {
		tvEnsure()
	} else {
		tvRestoreIfIdle()
	}
}

// airplayRefresh немедленно перечитывает статус приёмника и обновляет кэш.
func airplayRefresh() {
	info := airplayRead()
	info.Installed = true
	info.Running = airplayEngineRunning()
	airplayApply(info)
}

// airplayRefreshSoon подтягивает метаданные после команды пульта: приёмник
// обновляет title/artist не мгновенно, поэтому несколько раз опрашиваем его.
func airplayRefreshSoon() {
	go func() {
		for _, d := range []time.Duration{0, 100 * time.Millisecond, 100 * time.Millisecond, 120 * time.Millisecond, 130 * time.Millisecond, 150 * time.Millisecond, 200 * time.Millisecond, 250 * time.Millisecond} {
			if d > 0 {
				time.Sleep(d)
			}
			airplayRefresh()
		}
	}()
}

// airplayAnnounce коротко показывает трек на матрице, если она на часах.
func airplayAnnounce(info airplayInfo) {
	if time.Since(airplayAnnounceAt) < 5*time.Second {
		return
	}
	text := strings.TrimSpace(info.Artist) + " — " + strings.TrimSpace(info.Title)
	text = strings.Trim(text, " —")
	if text == "" {
		return
	}
	if len([]rune(text)) > 64 {
		text = string([]rune(text)[:64])
	}
	matrixMu.Lock()
	mode := matrixMode
	matrixMu.Unlock()
	if mode != "idle" && mode != "native" {
		return
	}
	speed := 60 * time.Millisecond
	g, err := glagolTextGIF(text, speed, 0, matrixBrightness())
	if err != nil {
		return
	}
	url, err := glagolAssetURL(g)
	if err != nil {
		return
	}
	repeat := glagolTextGIFDuration(text, speed)
	if repeat < 500*time.Millisecond {
		repeat = 500 * time.Millisecond
	}
	if repeat > 30*time.Second {
		repeat = 30 * time.Second
	}
	dur := int(repeat/time.Second) + 2
	if dur < 5 {
		dur = 5
	}
	if dur > 15 {
		dur = 15
	}
	if matrixRunGlagol(dur, "AirPlay", url, repeat) {
		airplayAnnounceAt = time.Now()
		addEvent("info", "Матрица: AirPlay", text)
	}
}

func startAirplayWatcher() {
	go func() {
		first := true
		for {
			if !airplayInstalled {
				if time.Since(airplayInstallAt) > 30*time.Second {
					if !airplayInstalledNow() {
						time.Sleep(30 * time.Second)
						continue
					}
				} else {
					time.Sleep(5 * time.Second)
					continue
				}
			}
			running := airplayEngineRunning()
			airplayMu.Lock()
			enabled := airplayCur.Enabled
			airplayMu.Unlock()

			if first || running || enabled {
				info := airplayRead()
				info.Installed = true
				info.Running = running || airplayEngineRunning()
				airplayApply(info)
				first = false
			} else {
				airplayMu.Lock()
				airplayCur.Installed = true
				airplayCur.Running = false
				airplayCur.State = "idle"
				airplayCur.Error = ""
				airplayCur.At = time.Now().Unix()
				airplayAt = time.Now()
				airplayMu.Unlock()
			}

			sleep := 15 * time.Second
			airplayMu.Lock()
			if running || airplayCur.Running || airplayCur.State == "playing" {
				sleep = 2 * time.Second
				if airplayCur.Live {
					sleep = time.Second
				}
			}
			airplayMu.Unlock()
			time.Sleep(sleep)
		}
	}()
}

// airplayRemote отправляет команду напрямую сервису приёмника — не через
// Android-медиасессию, которая после паузы может переключиться на Glagol.
func airplayRemote(action string) error {
	code := 0
	switch action {
	case "play":
		code = 0
	case "pause":
		code = 1
	case "play_pause":
		if airplaySnapshotWithViz().Playing {
			code = 1
		} else {
			code = 0
		}
	case "next":
		code = 4
	case "prev":
		code = 5
	default:
		return fmt.Errorf("unknown action")
	}
	out, rc := run(5*time.Second, "/system/bin/am", "start-service",
		"-n", airplayService, "-a", airplayPkg+".REMOTE", "--ei", "command", strconv.Itoa(code))
	if rc != 0 {
		return fmt.Errorf("airplay remote: %s", strings.TrimSpace(out))
	}
	log.Printf("airplay remote: %s (%d)", action, code)
	return nil
}

func registerAirplayHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/api/airplay", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, airplaySnapshotWithViz())
		case http.MethodPost:
			var req struct {
				On *bool `json:"on"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil || req.On == nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
				return
			}
			var err error
			if *req.On {
				err = airplayStart()
			} else {
				err = airplayStop()
				if err == nil {
					tvEnableMusicForce()
				}
			}
			if err != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
				return
			}
			time.Sleep(1200 * time.Millisecond)
			info := airplayRead()
			info.Installed = true
			info.Running = airplayEngineRunning()
			airplayApply(info)
			writeJSON(w, http.StatusOK, airplaySnapshotWithViz())
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET or POST"})
		}
	})

	mux.HandleFunc("/api/airplay/name", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, airplayNameState())
		case http.MethodPost:
			var req struct {
				Mode   string `json:"mode"`
				Custom string `json:"custom"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
				return
			}
			state, err := airplayNameSave(req.Mode, req.Custom)
			if err != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, state)
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET or POST"})
		}
	})

	mux.HandleFunc("/api/airplay/cover", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET only"})
			return
		}
		p, _ := airplayNewestCover()
		if p == "" {
			http.NotFound(w, r)
			return
		}
		f, err := os.Open(p)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "private, max-age=3600")
		http.ServeContent(w, r, st.Name(), st.ModTime(), f)
	})

	mux.HandleFunc("/api/airplay/action", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		var req struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		codes := map[string]string{"play_pause": "85", "play": "126", "pause": "127", "next": "87", "prev": "88"}
		if _, ok := codes[req.Action]; !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown action"})
			return
		}
		if err := airplayRemote(req.Action); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}
		time.Sleep(250 * time.Millisecond)
		writeJSON(w, http.StatusOK, airplaySnapshotWithViz())
	})
}
