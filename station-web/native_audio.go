package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	nativeHTTPAddr   = "127.0.0.1:8094"
	nativePCMAddr    = "127.0.0.1:8095"
	nativeLiveURL    = "http://127.0.0.1:8094/airplay.wav"
	nativeAppPkg     = "com.opencode.stationviz"
	nativeAppService = "com.opencode.stationviz/.VizService"
	nativeUserConfig = "/data/data/com.yandex.io.sdk/files/data/data/user_config.dat"
	// nativeStreamID — маркер нашего потока в штатном плеере. Заголовок теперь
	// настоящий (для ТВ), поэтому определять свой поток по title нельзя.
	nativeStreamID = "opencode-airplay"
)

type nativeMetaInfo struct {
	Title  string
	Artist string
	Album  string
	Cover  string
}

var (
	nativePCMCh   = make(chan []byte, 512)
	nativeOnce    sync.Once
	nativeStarted bool

	nativeMu         sync.Mutex
	nativeActive     bool
	nativePCMAt      time.Time
	nativeStyle      string
	nativeLabel      string
	nativeStyleAt    time.Time
	nativePlaying    bool
	nativePlayAt     time.Time
	nativeAppOwned   bool
	nativeEnsureAt   time.Time
	nativeStopAt     time.Time
	nativeRunningAt  time.Time
	nativeLastPlay   time.Time
	nativeGlagolStop time.Time
	nativeErr        string
	nativeInstalled  bool
	nativeInstallAt  time.Time
	nativeLastLog    string

	nativeReceiverEnabled bool
	nativeAppliedStyle    string
	nativeStyleDirty      bool
	nativeStyleRestartAt  time.Time
	nativeRestarting      bool

	nativeMeta          nativeMetaInfo
	nativeMetaRefreshAt time.Time
	nativeUserPaused    bool
	nativeUserPausedAt  time.Time
	nativePauseSeenAt   time.Time

	nativeStreamMu   sync.Mutex
	nativeStreamStop chan struct{}

	nativeStyleRe = regexp.MustCompile(`"music_equalizer_visualization"\s*:\s*\{[^{}]*"style"\s*:\s*"([^"]+)"`)
)

func startNativeAudio() {
	nativeOnce.Do(func() {
		nativeStarted = true
		mux := http.NewServeMux()
		mux.HandleFunc("/airplay.wav", func(w http.ResponseWriter, r *http.Request) {
			serveNativeStream(w, r, nil)
		})
		mux.HandleFunc("/test.wav", func(w http.ResponseWriter, r *http.Request) {
			data, err := os.ReadFile("/data/local/tmp/station-web/sweep.wav")
			if err != nil || len(data) <= 44 {
				http.Error(w, "no sweep", http.StatusServiceUnavailable)
				return
			}
			pcm := data[44:]
			serveNativeStream(w, r, pcm)
		})
		go func() {
			srv := &http.Server{Addr: nativeHTTPAddr, Handler: mux}
			_ = srv.ListenAndServe()
		}()
		go func() {
			for {
				ln, err := net.Listen("tcp", nativePCMAddr)
				if err != nil {
					time.Sleep(2 * time.Second)
					continue
				}
				for {
					c, err := ln.Accept()
					if err != nil {
						break
					}
					go func(conn net.Conn) {
						defer conn.Close()
						buf := make([]byte, 8192)
						for {
							n, err := conn.Read(buf)
							if n > 0 {
								nativeMu.Lock()
								nativePCMAt = time.Now()
								nativeMu.Unlock()
								chunk := append([]byte(nil), buf[:n]...)
								select {
								case nativePCMCh <- chunk:
								default:
									// переполнение: выбрасываем старый кадр
									select {
									case <-nativePCMCh:
									default:
									}
									select {
									case nativePCMCh <- chunk:
									default:
									}
								}
							}
							if err != nil {
								return
							}
						}
					}(c)
				}
				_ = ln.Close()
				time.Sleep(time.Second)
			}
		}()
	})
}

func writeWAVHeader(w io.Writer) error {
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(0xFFFFFFFF))
	b.WriteString("WAVEfmt ")
	_ = binary.Write(&b, binary.LittleEndian, uint32(16))
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))
	_ = binary.Write(&b, binary.LittleEndian, uint16(2))
	_ = binary.Write(&b, binary.LittleEndian, uint32(44100))
	_ = binary.Write(&b, binary.LittleEndian, uint32(44100*2*2))
	_ = binary.Write(&b, binary.LittleEndian, uint16(4))
	_ = binary.Write(&b, binary.LittleEndian, uint16(16))
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(0xFFFFFFFF))
	_, err := w.Write(b.Bytes())
	return err
}

func serveNativeStream(w http.ResponseWriter, r *http.Request, testPCM []byte) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", http.StatusInternalServerError)
		return
	}
	if r.Method == http.MethodHead {
		// Плеер может сначала проверить URL методом HEAD. Отвечаем заголовками,
		// не занимая единственный слот живого потока.
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Length", "4294967295")
		w.WriteHeader(http.StatusOK)
		return
	}
	// Живой поток всегда один: новое подключение плеера вытесняет старое,
	// иначе после смены трека можно получить 409 и остаться без звука.
	var stop chan struct{}
	if testPCM == nil {
		nativeStreamMu.Lock()
		if nativeStreamStop != nil {
			close(nativeStreamStop)
		}
		stop = make(chan struct{})
		nativeStreamStop = stop
		nativeStreamMu.Unlock()
		defer func() {
			nativeStreamMu.Lock()
			if nativeStreamStop == stop {
				nativeStreamStop = nil
			}
			nativeStreamMu.Unlock()
		}()
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if err := writeWAVHeader(w); err != nil {
		return
	}
	flusher.Flush()

	if testPCM != nil {
		const chunk = 4096
		start := time.Now()
		written := 0
		for written < len(testPCM) {
			end := written + chunk
			if end > len(testPCM) {
				end = len(testPCM)
			}
			if _, err := w.Write(testPCM[written:end]); err != nil {
				return
			}
			flusher.Flush()
			written = end
			want := time.Duration(float64(written) / 176400.0 * float64(time.Second))
			if sleep := want - time.Since(start); sleep > 0 {
				time.Sleep(sleep)
			}
			select {
			case <-r.Context().Done():
				return
			default:
			}
		}
		return
	}

	// Пока AirPlay на паузе, tap не присылает PCM. Подкармливаем штатный
	// плеер тишиной, чтобы его HTTP-поток не умирал и resume был без
	// перезапуска пайплайна.
	silence := make([]byte, 3528) // 20 ms, 44.1 kHz, stereo, s16
	lastPCM := time.Now()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case chunk := <-nativePCMCh:
			if _, err := w.Write(chunk); err != nil {
				return
			}
			flusher.Flush()
			lastPCM = time.Now()
		case <-ticker.C:
			if time.Since(lastPCM) > 150*time.Millisecond {
				if _, err := w.Write(silence); err != nil {
					return
				}
				flusher.Flush()
			}
		case <-stop:
			return
		case <-r.Context().Done():
			return
		}
	}
}

// nativePlay отправляет штатному плееру URL, чтобы визуализацию рисовал
// родной SpectrogramAnimation станции.
// nativeMetaPayload готовит metadata для штатного плеера: заголовок, артист
// и обложка. Именно это показывает ТВ-экран и карточка «сейчас играет».
func nativeMetaPayload(m nativeMetaInfo) map[string]any {
	title := strings.TrimSpace(m.Title)
	artist := strings.TrimSpace(m.Artist)
	if i := strings.Index(artist, " • "); i > 0 {
		artist = strings.TrimSpace(artist[:i])
	}
	if title == "" {
		title = "AirPlay"
	}
	if artist == "" {
		artist = "Native visualizer"
	}
	meta := map[string]any{
		"title":    title,
		"subtitle": artist,
	}
	if m.Title != "" || m.Cover != "" {
		music := map[string]any{"title": title}
		if artist != "" {
			music["artists"] = artist
		}
		if m.Cover != "" {
			music["cover_uri"] = m.Cover
		}
		meta["music_metadata"] = music
	}
	return meta
}

func nativeMetaSnapshot() nativeMetaInfo {
	nativeMu.Lock()
	defer nativeMu.Unlock()
	return nativeMeta
}

// nativeStreamToken делает каждый трек AirPlay новым потоком для штатного
// плеера: так он обновляет карточку «сейчас играет» и обложку на ТВ.
func nativeStreamToken() string {
	m := nativeMetaSnapshot()
	h := fnv.New32a()
	_, _ = h.Write([]byte(m.Title))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(m.Artist))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(m.Cover))
	return fmt.Sprintf("%s-%08x", nativeStreamID, h.Sum32())
}

func nativeLanIP() string {
	c, err := net.Dial("udp", "8.8.8.8:80")
	if err == nil {
		defer c.Close()
		if a, ok := c.LocalAddr().(*net.UDPAddr); ok && a.IP != nil {
			return a.IP.String()
		}
	}
	return "127.0.0.1"
}

// nativeSetMeta обновляет метаданные AirPlay в штатном плеере. При смене
// трека плеер получает новый заголовок/артиста/обложку без участия панели.
func nativeSetMeta(info airplayInfo) {
	if strings.TrimSpace(info.Title) == "" {
		return
	}
	cover := ""
	if info.CoverAt != 0 {
		cover = "http://" + nativeLanIP() + ":8080/api/airplay/cover?v=" + fmt.Sprint(info.CoverAt)
	}
	m := nativeMetaInfo{Title: info.Title, Artist: info.Artist, Album: info.Album, Cover: cover}
	nativeMu.Lock()
	changed := m != nativeMeta
	nativeMeta = m
	playing := nativePlaying
	nativeMu.Unlock()
	if !changed {
		return
	}
	nativeLog("metadata: " + m.Title + " / " + m.Artist)
	if playing {
		nativeRefreshMetadata()
	}
}

// nativeRefreshMetadata переотправляет audio_play с тем же потоком, но новыми
// метаданными. Штатный плеер обновляет карточку «сейчас играет» и обложку.
func nativeRefreshMetadata() {
	nativeMu.Lock()
	if time.Since(nativeMetaRefreshAt) < 3*time.Second {
		nativeMu.Unlock()
		return
	}
	nativeMetaRefreshAt = time.Now()
	m := nativeMeta
	nativeMu.Unlock()
	if m.Title == "" {
		return
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		payload := map[string]any{
			"stream": map[string]any{
				"id":           nativeStreamToken(),
				"url":          nativeLiveURL,
				"format":       "MP3",
				"type":         "Track",
				"offset_ms":    0,
				"duration_sec": 0,
			},
			"set_pause": false,
			"metadata":  nativeMetaPayload(m),
		}
		if err := glagolSendCommand(glagolExternalCommand("audio_play", payload)); err != nil {
			nativeLog("metadata refresh error: " + err.Error())
			return
		}
		nativeMu.Lock()
		nativePlaying = true
		nativePlayAt = time.Now()
		nativeMu.Unlock()
	}()
}

func nativeUserPausedNow() bool {
	nativeMu.Lock()
	defer nativeMu.Unlock()
	return nativeUserPaused
}

// nativeTVControl связывает кнопку play/pause на ТВ-пульте с AirPlay:
// штатный плеер ставится на паузу локально, а панель пересылает паузу/плей
// на iPhone, чтобы источник и станция не расходились.
func nativeTVControl(info airplayInfo) {
	m := mediaSnapshot()
	ours := nativeMediaIsOurs(m)

	nativeMu.Lock()
	userPaused := nativeUserPaused
	playing := nativePlaying
	if info.Playing && userPaused && time.Since(nativeUserPausedAt) > 3*time.Second {
		// Пользователь возобновил воспроизведение на iPhone — отпускаем паузу.
		nativeUserPaused = false
		userPaused = false
	}
	nativeMu.Unlock()

	if info.Playing && ours && !m.Playing && playing {
		nativeMu.Lock()
		if nativePauseSeenAt.IsZero() {
			nativePauseSeenAt = time.Now()
		}
		seen := time.Since(nativePauseSeenAt)
		nativeMu.Unlock()
		if seen >= 2*time.Second {
			nativeMu.Lock()
			nativeUserPaused = true
			nativeUserPausedAt = time.Now()
			nativePlaying = false
			nativePauseSeenAt = time.Time{}
			nativeMu.Unlock()
			nativeLog("tv pause -> airplay pause")
			go func() {
				if err := airplayRemote("pause"); err != nil {
					nativeLog("tv pause error: " + err.Error())
				}
			}()
		}
		return
	}

	nativeMu.Lock()
	nativePauseSeenAt = time.Time{}
	nativeMu.Unlock()

	if !info.Playing && userPaused && ours && m.Playing {
		nativeMu.Lock()
		nativeUserPaused = false
		nativeUserPausedAt = time.Time{}
		nativePlaying = true
		nativePlayAt = time.Now()
		nativeMu.Unlock()
		nativeLog("tv play -> airplay play")
		go func() {
			if err := airplayRemote("play"); err != nil {
				nativeLog("tv play error: " + err.Error())
			}
		}()
		return
	}
	if !info.Playing && userPaused && !ours && m.Playing {
		// ТВ нажал play, но успела запуститься Яндекс Музыка: гасим её и
		// возвращаем AirPlay, иначе управление с пульта уводит источник.
		nativeMu.Lock()
		nativeUserPaused = false
		nativeUserPausedAt = time.Time{}
		nativePlaying = false
		nativeMu.Unlock()
		nativeLog("tv play -> airplay play (stop music)")
		go func() {
			_ = glagolSendCommand(map[string]any{"command": "stop"})
			time.Sleep(300 * time.Millisecond)
			if err := airplayRemote("play"); err != nil {
				nativeLog("tv play error: " + err.Error())
			}
		}()
	}
}

func nativePlay(mode, url string) error {
	if url == "" {
		url = nativeLiveURL
	}
	if mode == "radio" {
		payload := map[string]any{"streamUrl": url, "force_restart_player": true}
		return glagolSendCommand(glagolExternalCommand("radio_play", payload))
	}
	payload := map[string]any{
		"stream": map[string]any{
			"id":           nativeStreamToken(),
			"url":          url,
			"format":       "MP3",
			"type":         "Track",
			"offset_ms":    0,
			"duration_sec": 0,
		},
		"set_pause": false,
		"metadata":  nativeMetaPayload(nativeMetaSnapshot()),
	}
	return glagolSendCommand(glagolExternalCommand("audio_play", payload))
}

func registerNativeAudioHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/api/native/play", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		var req struct {
			Mode string `json:"mode"`
			URL  string `json:"url"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048)).Decode(&req)
		if err := nativePlay(req.Mode, req.URL); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": req.Mode, "url": req.URL})
	})
}

// ---------- native supervisor ----------

func nativeInstalledNow() bool {
	nativeMu.Lock()
	if !nativeInstallAt.IsZero() && time.Since(nativeInstallAt) < 30*time.Second {
		v := nativeInstalled
		nativeMu.Unlock()
		return v
	}
	nativeMu.Unlock()
	out, code := run(5*time.Second, "/system/bin/pm", "path", nativeAppPkg)
	ok := code == 0 && strings.Contains(out, "package:")
	nativeMu.Lock()
	nativeInstalled = ok
	nativeInstallAt = time.Now()
	nativeMu.Unlock()
	return ok
}

func nativePCMFresh() bool {
	nativeMu.Lock()
	defer nativeMu.Unlock()
	return !nativePCMAt.IsZero() && time.Since(nativePCMAt) < 2500*time.Millisecond
}

func nativeActiveState() bool {
	nativeMu.Lock()
	defer nativeMu.Unlock()
	return nativeActive
}

func nativeDrainPCM() {
	for {
		select {
		case <-nativePCMCh:
		default:
			return
		}
	}
}

func nativeAppResident() bool {
	out, _ := run(3*time.Second, "/system/bin/ps", "-A", "-o", "PID,ARGS")
	return strings.Contains(out, nativeAppPkg)
}

// nativeEnsureApp поднимает AudioTap заранее, пока AirPlay не играет.
// Регистрация тапа заставляет Shairport перезапустить движок, чтобы
// применить конфиг (lead/only_44100), поэтому делать это посреди трека нельзя.
func nativeEnsureApp() error {
	nativeMu.Lock()
	if nativeAppOwned {
		nativeMu.Unlock()
		return nil
	}
	if time.Since(nativeEnsureAt) < 5*time.Second {
		nativeMu.Unlock()
		return nil
	}
	nativeEnsureAt = time.Now()
	nativeMu.Unlock()

	if nativeAppResident() {
		nativeMu.Lock()
		nativeAppOwned = true
		nativeMu.Unlock()
		nativeLog("tap adopted")
		return nil
	}
	out, code := run(8*time.Second, "/system/bin/am", "start-foreground-service",
		"-n", nativeAppService, "--es", "mode", "native")
	if code != 0 {
		return fmt.Errorf("viz service: %s", strings.TrimSpace(out))
	}
	nativeMu.Lock()
	nativeAppOwned = true
	nativeErr = ""
	nativeMu.Unlock()
	nativeLog("tap started")
	return nil
}

func nativeStopApp() {
	nativeMu.Lock()
	if time.Since(nativeStopAt) < 3*time.Second {
		nativeMu.Unlock()
		return
	}
	// Пока приёмник включён, не убиваем tap-приложение: закрытие сокета
	// приводит к SIGPIPE в нативном движке Shairport и роняет AirPlay-сессию.
	// Оставляем сервис жить и просто перестаём его использовать.
	if nativeReceiverEnabled {
		nativeAppOwned = false
		nativeMu.Unlock()
		nativeLog("tap kept (receiver on)")
		return
	}
	nativeStopAt = time.Now()
	nativeAppOwned = false
	nativeMu.Unlock()
	_, _ = run(5*time.Second, "/system/bin/am", "force-stop", nativeAppPkg)
	nativeLog("tap stopped")
}

// nativeVendorPlaying сообщает, играет ли уже наш локальный WAV. Через
// несколько секунд после старта доверяем состоянию плеера, чтобы отличить
// живой поток от самопроизвольно завершившегося.
func nativeMediaIsOurs(m mediaState) bool {
	return strings.EqualFold(strings.TrimSpace(m.Title), "AirPlay") ||
		strings.HasPrefix(m.TrackID, nativeStreamID) ||
		strings.EqualFold(strings.TrimSpace(m.Type), nativeStreamID)
}

func nativeVendorPlaying() bool {
	nativeMu.Lock()
	playing, at := nativePlaying, nativePlayAt
	nativeMu.Unlock()
	if !playing {
		return false
	}
	if time.Since(at) < 8*time.Second {
		return true
	}
	m := mediaSnapshot()
	if m.Playing && nativeMediaIsOurs(m) {
		return true
	}
	nativeMu.Lock()
	nativePlaying = false
	nativeMu.Unlock()
	return false
}

func nativeStopVendor() {
	nativeMu.Lock()
	playing := nativePlaying
	nativePlaying = false
	nativePlayAt = time.Time{}
	nativeUserPaused = false
	nativeUserPausedAt = time.Time{}
	nativePauseSeenAt = time.Time{}
	nativeMu.Unlock()
	nativeResetStream()
	if playing {
		_ = glagolSendCommand(map[string]any{"command": "stop"})
	}
}

// nativeResetStream закрывает текущее HTTP-соединение с плеером, чтобы
// следующее audio_play могло подключиться к /airplay.wav без 409.
func nativeResetStream() {
	nativeStreamMu.Lock()
	if nativeStreamStop != nil {
		close(nativeStreamStop)
		nativeStreamStop = nil
	}
	nativeStreamMu.Unlock()
}

// nativeRecentPlaying защищает от кратковременных провалов статуса,
// пока Shairport перезапускает движок после регистрации тапа.
func nativeRecentPlaying() bool {
	nativeMu.Lock()
	defer nativeMu.Unlock()
	return !nativeLastPlay.IsZero() && time.Since(nativeLastPlay) < 5*time.Second
}

func nativeRecentRunning() bool {
	nativeMu.Lock()
	defer nativeMu.Unlock()
	return !nativeRunningAt.IsZero() && time.Since(nativeRunningAt) < 12*time.Second
}

// nativeReadStyle читает выбранный в «Умном Доме» режим эквалайзера.
func nativeReadStyle() string {
	nativeMu.Lock()
	if !nativeStyleAt.IsZero() && time.Since(nativeStyleAt) < 8*time.Second {
		s := nativeStyle
		nativeMu.Unlock()
		return s
	}
	nativeMu.Unlock()

	style := ""
	if data, err := os.ReadFile(nativeUserConfig); err == nil {
		if m := nativeStyleRe.FindSubmatch(data); len(m) > 1 {
			style = strings.TrimSpace(string(m[1]))
		}
	}
	nativeMu.Lock()
	nativeStyle = style
	nativeLabel = nativeStyleRu(style)
	nativeStyleAt = time.Now()
	nativeMu.Unlock()
	return style
}

func nativeStyleRu(style string) string {
	switch style {
	case "bricksSmall":
		return "Прямоугольники"
	case "barsBottom":
		return "Полосы"
	case "barsCenter":
		return "Полосы симметрично"
	case "pads", "blocks":
		return "Блоки"
	case "flame", "wave":
		return "Пламя"
	case "flameCenter", "fluid":
		return "Пламя симметрично"
	}
	return style
}

func nativeLabelNow() string {
	nativeMu.Lock()
	defer nativeMu.Unlock()
	if nativeLabel != "" {
		return nativeLabel
	}
	return "режим из Умного Дома"
}

// nativeObserveStyle запоминает выбранный в приложении стиль. Если стиль
// изменился во время AirPlay, помечаем его «грязным», чтобы перезапустить
// штатный плеер и заставить LED-модуль заново инициализировать спектрограмму.
func nativeObserveStyle(style string) {
	if style == "" {
		return
	}
	nativeMu.Lock()
	if nativeAppliedStyle == "" {
		nativeAppliedStyle = style
	} else if style != nativeAppliedStyle {
		nativeAppliedStyle = style
		nativeStyleDirty = true
	}
	nativeMu.Unlock()
}

func nativeStyleDirtyNow() bool {
	nativeMu.Lock()
	defer nativeMu.Unlock()
	return nativeStyleDirty
}

// nativeStyleCooldownOK не даёт перезапускать плеер чаще одного раза в 20 с,
// если приложение несколько раз подряд перезаписывает конфиг при смене стиля.
func nativeStyleCooldownOK() bool {
	nativeMu.Lock()
	defer nativeMu.Unlock()
	return nativeStyleRestartAt.IsZero() || time.Since(nativeStyleRestartAt) > 20*time.Second
}

func nativeClearStyleDirty() {
	nativeMu.Lock()
	nativeStyleDirty = false
	nativeMu.Unlock()
}

// nativeRestartPlayerForStyle перезапускает только штатный плеер (stop +
// audio_play). Tap-приложение и приёмник AirPlay не трогаем, поэтому сессия
// с iPhone не рвётся, а LED-визуализация подхватывает новый стиль.
func nativeRestartPlayerForStyle() {
	nativeMu.Lock()
	if nativeRestarting {
		nativeMu.Unlock()
		return
	}
	nativeRestarting = true
	nativeStyleRestartAt = time.Now()
	style := nativeAppliedStyle
	nativeMu.Unlock()
	go func() {
		defer func() {
			nativeMu.Lock()
			nativeRestarting = false
			nativeMu.Unlock()
		}()
		nativeLog("style changed -> " + style + ", restarting player")
		nativeStopVendor()
		time.Sleep(700 * time.Millisecond)
		nativeDrainPCM()
		if err := nativePlay("", nativeLiveURL); err != nil {
			nativeLog("style restart error: " + err.Error())
			return
		}
		nativeMu.Lock()
		nativePlaying = true
		nativePlayAt = time.Now()
		nativeActive = true
		nativeErr = ""
		nativeMu.Unlock()
		addEvent("info", "AirPlay", "Визуализация: "+nativeLabelNow())
	}()
}

func nativeError() string {
	nativeMu.Lock()
	defer nativeMu.Unlock()
	return nativeErr
}

func nativeLog(msg string) {
	nativeMu.Lock()
	if nativeLastLog == msg {
		nativeMu.Unlock()
		return
	}
	nativeLastLog = msg
	nativeMu.Unlock()
	log.Printf("native: %s", msg)
}

func nativeSetActive(v bool) {
	nativeMu.Lock()
	nativeActive = v
	nativeMu.Unlock()
}

// nativeAdoptIfResident подхватывает уже запущенный тап после перезапуска
// панели, не убивая его. Проверка процесса — не чаще раза в 5 секунд.
func nativeAdoptIfResident() bool {
	nativeMu.Lock()
	if nativeAppOwned {
		nativeMu.Unlock()
		return true
	}
	if time.Since(nativeEnsureAt) < 5*time.Second {
		nativeMu.Unlock()
		return false
	}
	nativeEnsureAt = time.Now()
	nativeMu.Unlock()
	if nativeAppResident() {
		nativeMu.Lock()
		nativeAppOwned = true
		nativeMu.Unlock()
		nativeLog("tap adopted")
		return true
	}
	return false
}

// nativeUpdate держит тап готовым заранее и не трогает его во время трека:
// регистрация/снятие тапа заставляет Shairport перезапускать движок.
func nativeUpdate(info airplayInfo) {
	if !nativeInstalledNow() {
		nativeSetActive(false)
		nativeMu.Lock()
		nativeErr = ""
		nativeMu.Unlock()
		return
	}

	// Пока watcher не прочитал статус, ничего не трогаем: иначе на старте
	// панели можно принять живой приёмник за выключенный.
	if info.At == 0 {
		return
	}

	nativeMu.Lock()
	nativeReceiverEnabled = info.Enabled
	nativeMu.Unlock()

	style := nativeReadStyle()
	nativeObserveStyle(style)
	nativeSetMeta(info)
	nativeTVControl(info)

	if info.Enabled && info.Running {
		nativeMu.Lock()
		nativeRunningAt = time.Now()
		nativeMu.Unlock()
	}
	if info.Playing {
		nativeMu.Lock()
		nativeLastPlay = time.Now()
		nativeMu.Unlock()
	}

	// Приёмник выключен. Даём запас времени: движок может перезапускаться
	// после регистрации тапа, и это не значит, что AirPlay выключили.
	if !info.Enabled {
		if nativeRecentRunning() {
			return
		}
		nativeStopVendor()
		nativeStopApp()
		nativeSetActive(false)
		nativeLog("receiver off")
		return
	}
	if !info.Running {
		// Движок перезапускается — тап не трогаем, он понадобится снова.
		return
	}

	if !info.VizEnabled {
		nativeSetActive(false)
		if info.Playing || info.Connected {
			// Не рвём активную сессию AirPlay из-за выключения визуализации.
			nativeLog("viz off, deferred")
			return
		}
		nativeStopVendor()
		nativeStopApp()
		nativeLog("viz off, idle")
		return
	}

	switch {
	case info.Playing:
		if nativeUserPausedNow() {
			nativeSetActive(false)
			return
		}
		if nativeStyleDirtyNow() && nativeVendorPlaying() && nativeStyleCooldownOK() {
			nativeClearStyleDirty()
			nativeRestartPlayerForStyle()
			return
		}
		if !nativeAdoptIfResident() {
			// Панель стартовала посреди трека — не ломаем текущую сессию,
			// визуализация включится со следующей.
			nativeSetActive(false)
			nativeLog("skip: tap not ready")
			return
		}
		if nativeVendorPlaying() {
			nativeSetActive(true)
			nativeMu.Lock()
			nativeErr = ""
			nativeMu.Unlock()
			return
		}
		if !nativePCMFresh() {
			nativeSetActive(false)
			nativeLog("waiting pcm")
			return
		}
		// AirPlay важнее Я.Музыки: если Glagol ещё играет, останавливаем его.
		m := mediaSnapshot()
		if m.Playing && !nativeMediaIsOurs(m) {
			nativeMu.Lock()
			recent := !nativeGlagolStop.IsZero() && time.Since(nativeGlagolStop) < 5*time.Second
			if !recent {
				nativeGlagolStop = time.Now()
			}
			nativeMu.Unlock()
			if !recent {
				_ = glagolSendCommand(map[string]any{"command": "stop"})
				nativeLog("stopped glagol")
			}
		}
		nativeMu.Lock()
		tooSoon := !nativePlayAt.IsZero() && time.Since(nativePlayAt) < 6*time.Second
		nativeMu.Unlock()
		if tooSoon {
			return
		}
		nativeDrainPCM()
		if err := nativePlay("", nativeLiveURL); err != nil {
			nativeMu.Lock()
			nativeErr = err.Error()
			nativePlayAt = time.Now()
			nativeMu.Unlock()
			addEvent("warn", "AirPlay", "штатная визуализация: "+err.Error())
			nativeLog("play error: " + err.Error())
			return
		}
		nativeMu.Lock()
		nativePlaying = true
		nativePlayAt = time.Now()
		nativeActive = true
		nativeErr = ""
		nativeMu.Unlock()
		nativeClearStyleDirty()
		nativeLog("play")
		addEvent("info", "AirPlay", "штатная визуализация: "+nativeLabelNow())
	default:
		// AirPlay на паузе или сессия ещё не началась.
		if nativeUserPausedNow() {
			nativeSetActive(false)
			return
		}
		if nativeStyleDirtyNow() && nativeVendorPlaying() && nativeStyleCooldownOK() {
			nativeClearStyleDirty()
			nativeRestartPlayerForStyle()
			return
		}
		if nativeRecentPlaying() && nativeVendorPlaying() {
			return
		}
		nativeStopVendor()
		nativeSetActive(false)
		if err := nativeEnsureApp(); err != nil {
			nativeMu.Lock()
			nativeErr = err.Error()
			nativeMu.Unlock()
			nativeLog("start error: " + err.Error())
			return
		}
		if info.Connected {
			nativeLog("airplay paused")
		} else {
			nativeLog("tap ready")
		}
	}
}

func startNativeSupervisor() {
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			nativeUpdate(airplaySnapshotWithViz())
		}
	}()
}
