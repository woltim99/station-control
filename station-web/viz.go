package main

import (
	"encoding/json"
	"log"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	vizAddr       = "127.0.0.1:8093"
	vizBands      = 25
	vizMagic      = 0xA5
	vizAppPkg     = "com.opencode.stationviz"
	vizAppService = "com.opencode.stationviz/.VizService"
)

var (
	vizMu       sync.Mutex
	vizLevels   [vizBands]byte
	vizAt       time.Time
	vizEnabled  = true
	vizActive   bool
	vizStyle    = "auto"
	vizServerOK bool

	vizStartMu   sync.Mutex
	vizStartedAt time.Time
	vizOwned     bool
)

func vizSnapshot() ([vizBands]byte, time.Time) {
	vizMu.Lock()
	defer vizMu.Unlock()
	return vizLevels, vizAt
}

func vizEnabledState() (bool, bool) {
	vizMu.Lock()
	defer vizMu.Unlock()
	return vizEnabled, vizActive
}

func vizStyleState() string {
	vizMu.Lock()
	defer vizMu.Unlock()
	return vizStyle
}

func vizStartServer() {
	vizMu.Lock()
	if vizServerOK {
		vizMu.Unlock()
		return
	}
	vizServerOK = true
	vizMu.Unlock()
	go func() {
		for {
			ln, err := net.Listen("tcp", vizAddr)
			if err != nil {
				log.Printf("viz: listen: %v", err)
				time.Sleep(5 * time.Second)
				continue
			}
			for {
				c, err := ln.Accept()
				if err != nil {
					break
				}
				go vizReadConn(c)
			}
			_ = ln.Close()
			time.Sleep(time.Second)
		}
	}()
}

func vizReadConn(c net.Conn) {
	defer c.Close()
	buf := make([]byte, 0, (vizBands+1)*8)
	tmp := make([]byte, 1024)
	for {
		n, err := c.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			for len(buf) >= vizBands+1 {
				if buf[0] != vizMagic {
					buf = buf[1:]
					continue
				}
				vizMu.Lock()
				copy(vizLevels[:], buf[1:vizBands+1])
				vizAt = time.Now()
				vizMu.Unlock()
				buf = buf[vizBands+1:]
			}
			if len(buf) > 4096 {
				buf = buf[len(buf)-1024:]
			}
		}
		if err != nil {
			return
		}
	}
}

func vizAppResident() bool {
	out, _ := run(3*time.Second, "/system/bin/ps", "-A", "-o", "PID,ARGS")
	return strings.Contains(out, vizAppPkg)
}

// vizEnsureHelper запускает приложение-захват AudioTap. Само приложение
// подключено к сервису Shairport и присылает полосы в 127.0.0.1:8093.
func vizEnsureHelper() {
	vizMu.Lock()
	fresh := !vizAt.IsZero() && time.Since(vizAt) < 2500*time.Millisecond
	vizMu.Unlock()
	if fresh {
		return
	}
	vizStartMu.Lock()
	defer vizStartMu.Unlock()
	if time.Since(vizStartedAt) < 5*time.Second {
		return
	}
	vizStartedAt = time.Now()
	if vizAppResident() {
		return
	}
	if _, code := run(5*time.Second, "/system/bin/am", "start-foreground-service", "-n", vizAppService); code != 0 {
		log.Printf("viz: cannot start %s", vizAppPkg)
		return
	}
	vizOwned = true
}

func vizStopHelper() {
	vizStartMu.Lock()
	defer vizStartMu.Unlock()
	if !vizOwned {
		return
	}
	_, _ = run(5*time.Second, "/system/bin/am", "force-stop", vizAppPkg)
	vizOwned = false
	vizStartedAt = time.Time{}
}

func vizWanted() bool {
	vizMu.Lock()
	enabled := vizEnabled
	vizMu.Unlock()
	if !enabled || airplaySnapshot().State != "playing" {
		return false
	}
	// Если установлено приложение-тап, визуализацию рисует штатный плеер
	// станции, а собственный рендер не должен перекрывать fb4.
	if nativeInstalledNow() {
		return false
	}
	return true
}

func vizEnsureLoop() {
	matrixMu.Lock()
	mode := matrixMode
	matrixMu.Unlock()
	if mode == "viz" {
		return
	}
	if mode != "idle" && mode != "native" {
		return
	}
	vizMu.Lock()
	vizActive = true
	vizMu.Unlock()
	matrixStartLoop("viz", "", vizLoop)
}

func vizStopLoop() {
	matrixMu.Lock()
	mode := matrixMode
	matrixMu.Unlock()
	vizMu.Lock()
	vizActive = false
	vizMu.Unlock()
	if mode != "viz" {
		return
	}
	matrixStopAnimation()
	glagolNativeClock()
	matrixMu.Lock()
	matrixMode, matrixLabel = "idle", ""
	matrixMu.Unlock()
}

func vizEffectiveStyle() string {
	vizMu.Lock()
	style := vizStyle
	vizMu.Unlock()
	if style != "auto" {
		return style
	}
	// Авто: плавно чередуем шесть штатных режимов станции.
	styles := []string{"bars", "barsCenter", "bricks", "blocks", "flame", "flameCenter"}
	return styles[(time.Now().Unix()/20)%int64(len(styles))]
}

func vizLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(40 * time.Millisecond)
	defer ticker.Stop()
	start := time.Now()
	for {
		levels, at := vizSnapshot()
		frame := matrixBlank()
		t := time.Since(start).Seconds()
		if !at.IsZero() && time.Since(at) < 2*time.Second {
			renderSpectrum(frame, levels, t, vizEffectiveStyle())
		}
		_ = matrixWrite(frame)
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
	}
}

func renderSpectrum(frame []byte, levels [vizBands]byte, t float64, style string) {
	switch style {
	case "bricks":
		renderBricksSmall(frame, levels)
	case "bars":
		renderBarsBottom(frame, levels)
	case "barsCenter":
		renderBarsCenter(frame, levels)
	case "blocks":
		renderPads(frame, levels)
	case "flameCenter":
		renderWaveCenter(frame, levels)
	default:
		renderWave(frame, levels, t)
	}
	if style == "flame" || style == "flameCenter" {
		blurFrame(frame)
	}
}

// blurFrame мягко размывает кадр — край визуализации перестаёт быть ступенькой.
func blurFrame(frame []byte) {
	src := append([]byte(nil), frame...)
	kernel := [3][3]int{{1, 2, 1}, {2, 4, 2}, {1, 2, 1}}
	for y := 0; y < matrixH; y++ {
		for x := 0; x < matrixW; x++ {
			for c := 0; c < 3; c++ {
				sum := 0
				weight := 0
				for ky := -1; ky <= 1; ky++ {
					for kx := -1; kx <= 1; kx++ {
						ny, nx := y+ky, x+kx
						if ny < 0 || ny >= matrixH || nx < 0 || nx >= matrixW {
							continue
						}
						w := kernel[ky+1][kx+1]
						sum += int(src[(ny*matrixW+nx)*3+c]) * w
						weight += w
					}
				}
				frame[(y*matrixW+x)*3+c] = byte(sum / weight)
			}
		}
	}
}

func smoothBands(levels [vizBands]byte) [vizBands]float64 {
	var s [vizBands]float64
	for i := 0; i < vizBands; i++ {
		sum := 0.0
		n := 0
		for k := -1; k <= 1; k++ {
			j := i + k
			if j >= 0 && j < vizBands {
				sum += float64(levels[j])
				n++
			}
		}
		s[i] = sum / float64(n)
	}
	return s
}

// barsBottom — тонкие столбики через один, от нижней кромки.
func renderBarsBottom(frame []byte, levels [vizBands]byte) {
	for x := 0; x < vizBands; x += 2 {
		h := int(levels[x])
		if h <= 0 {
			continue
		}
		if h > matrixH {
			h = matrixH
		}
		for y := matrixH - h; y < matrixH; y++ {
			f := 0.0
			if h > 1 {
				f = float64(y-(matrixH-h)) / float64(h-1)
			}
			bright := 0.45 + 0.55*(1-f)
			if y == matrixH-h {
				bright = 0.55
			} else if y == matrixH-1 {
				bright = 1
			}
			vizPut(frame, x, y, 255*bright, 255*bright, 255*bright)
		}
	}
}

// barsCenter — те же столбики, но симметрично вокруг центральной линии.
func renderBarsCenter(frame []byte, levels [vizBands]byte) {
	center := matrixH / 2
	// Пунктирная центральная линия по всей ширине.
	for x := 0; x < vizBands; x++ {
		vizPut(frame, x, center, 190, 190, 190)
	}
	for x := 0; x < vizBands; x += 2 {
		h := int(levels[x])
		if h <= 0 {
			continue
		}
		half := h / 2
		if half < 1 {
			half = 1
		}
		for k := -half; k <= half; k++ {
			y := center + k
			if y < 0 || y >= matrixH {
				continue
			}
			f := float64(k+half) / float64(2*half)
			bright := 0.45 + 0.55*(1-f)
			if k == 0 {
				bright = 1
			} else if k == -half || k == half {
				bright = 0.60
			}
			vizPut(frame, x, y, 255*bright, 255*bright, 255*bright)
		}
	}
}

// bricksSmall — сетка маленьких «кирпичей» 2×1 с зазорами.
func renderBricksSmall(frame []byte, levels [vizBands]byte) {
	for xi := 0; xi < 8; xi++ {
		x := xi * 3
		h := int(levels[x])
		bricks := (h + 1) / 2
		if bricks > 8 {
			bricks = 8
		}
		for k := 0; k < bricks; k++ {
			y := matrixH - 1 - 2*k
			bright := 0.50 + 0.50*(1-float64(k)/7.0)
			for dx := 0; dx < 2; dx++ {
				vizPut(frame, x+dx, y, 255*bright, 255*bright, 255*bright)
			}
		}
	}
}

// pads — крупные площадки 3×3, как в старшем режиме.
func renderPads(frame []byte, levels [vizBands]byte) {
	thresholds := [4]int{13, 9, 5, 1}
	for pi := 0; pi < 5; pi++ {
		maxL := 0
		for b := pi * 5; b < pi*5+5 && b < vizBands; b++ {
			if int(levels[b]) > maxL {
				maxL = int(levels[b])
			}
		}
		for r := 0; r < 4; r++ {
			if maxL < thresholds[r] {
				continue
			}
			x0 := pi * 5
			y0 := r * 4
			bright := 0.45 + 0.20*float64(4-r)/3.0
			for dy := 0; dy < 3; dy++ {
				for dx := 0; dx < 3; dx++ {
					vizPut(frame, x0+dx, y0+dy, 255*bright, 255*bright, 255*bright)
				}
			}
		}
	}
}

// wave — сплошная сглаженная «гора» с яркой кромкой.
func renderWave(frame []byte, levels [vizBands]byte, t float64) {
	s := smoothBands(levels)
	for x := 0; x < vizBands; x++ {
		h := int(math.Round(s[x]))
		if h <= 0 {
			continue
		}
		if h > matrixH {
			h = matrixH
		}
		for y := matrixH - h; y < matrixH; y++ {
			f := 0.0
			if h > 1 {
				f = float64(y-(matrixH-h)) / float64(h-1)
			}
			bright := 0.30 + 0.70*(1-f)
			if f > 0.82 {
				bright = 0.85
			}
			vizPut(frame, x, y, 255*bright, 255*bright, 255*bright)
		}
		vizPut(frame, x, matrixH-h, 120, 120, 120)
		vizPut(frame, x, matrixH-1, 220, 220, 220)
	}
}

// waveCenter — сплошная «гора», симметричная относительно центра.
func renderWaveCenter(frame []byte, levels [vizBands]byte) {
	s := smoothBands(levels)
	center := matrixH / 2
	for x := 0; x < vizBands; x++ {
		h := int(math.Round(s[x]))
		if h <= 0 {
			continue
		}
		half := h / 2
		if half < 1 {
			half = 1
		}
		for k := -half; k <= half; k++ {
			y := center + k
			if y < 0 || y >= matrixH {
				continue
			}
			f := math.Abs(float64(k)) / float64(half)
			bright := 0.30 + 0.70*(1-f)
			if f > 0.82 {
				bright = 0.9
			}
			vizPut(frame, x, y, 255*bright, 255*bright, 255*bright)
		}
		vizPut(frame, x, center-half, 120, 120, 120)
		vizPut(frame, x, center+half, 120, 120, 120)
	}
}

// flame — плавное «пламя» с бегущей волной и мерцанием.
func renderFlame(frame []byte, levels [vizBands]byte, t float64) {
	s := smoothBands(levels)
	for x := 0; x < vizBands; x++ {
		h := s[x] + 2.2*math.Sin(t*1.8+float64(x)*0.45)
		hi := int(math.Round(h))
		if hi <= 0 {
			continue
		}
		if hi > matrixH {
			hi = matrixH
		}
		for y := matrixH - hi; y < matrixH; y++ {
			f := float64(y-(matrixH-hi)) / float64(hi)
			flicker := 0.72 + 0.28*math.Sin(t*5.0+float64(x)*0.8+float64(y)*0.9)
			bright := (0.25 + 0.75*(1-f)) * flicker
			vizPut(frame, x, y, 255*bright, 255*bright, 255*bright)
		}
		vizPut(frame, x, matrixH-hi, 255, 255, 255)
	}
}

func vizPut(frame []byte, x, y int, r, g, b float64) {
	if x < 0 || x >= matrixW || y < 0 || y >= matrixH {
		return
	}
	i := (y*matrixW + x) * 3
	frame[i] = clampByte(b)
	frame[i+1] = clampByte(g)
	frame[i+2] = clampByte(r)
}

func clampByte(v float64) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

func vmax(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// LavaBeat — тёплые «лавоовые» столбики с медленной волной.
func renderLava(frame []byte, levels [vizBands]byte, t float64, peaks *[vizBands]float64) {
	for x := 0; x < vizBands; x++ {
		h := float64(levels[x])
		wave := 1.6 * math.Sin(t*0.9+float64(x)*0.35)
		h = math.Max(0, math.Min(matrixH, h+wave))
		hi := int(h + 0.5)
		for y := matrixH - hi; y < matrixH; y++ {
			f := float64(matrixH-y) / float64(matrixH)
			r := 130 + 125*f
			g := 18 + 210*f*f
			b := 8 + 40*f
			mod := 0.72 + 0.28*math.Sin(t*1.5+float64(x)*0.6+float64(y)*0.5)
			vizPut(frame, x, y, r*mod, g*mod, b*mod)
		}
		peaks[x] = math.Max(h, peaks[x]-0.5)
		if peaks[x] > h+0.6 {
			y := matrixH - 1 - int(peaks[x])
			vizPut(frame, x, y, 255, 240, 160)
		}
	}
}

// PolarShining — холодное «полярное сияние» с волнами и мерцанием.
func renderPolar(frame []byte, levels [vizBands]byte, t float64) {
	for x := 0; x < vizBands; x++ {
		h := float64(levels[x])
		wave := 2.5 + 2.5*math.Sin(t*1.3+float64(x)*0.55)
		top := matrixH - int(h+wave+0.5)
		if top < 0 {
			top = 0
		}
		for y := top; y < matrixH; y++ {
			f := float64(matrixH-y) / float64(matrixH)
			r := 18 + 35*f
			g := 150 + 90*(1-f)
			b := 185 + 70*(1-f)
			shimmer := 0.65 + 0.35*math.Sin(t*3.1+float64(x)*0.85+float64(y)*0.3)
			vizPut(frame, x, y, r*shimmer, g*shimmer, b*shimmer)
		}
		// верхняя кромка сияния
		if top > 0 {
			vizPut(frame, x, top, 160, 255, 235)
		}
	}
	// редкие блики по верху
	for x := 0; x < vizBands; x += 5 {
		if math.Sin(t*1.7+float64(x)) > 0.75 {
			vizPut(frame, x, 1, 120, 230, 255)
		}
	}
}

// Blink — блочные сегменты с мерцанием под бас.
func renderBlink(frame []byte, levels [vizBands]byte, t float64) {
	bass := (float64(levels[0]) + float64(levels[1]) + float64(levels[2])) / 3 / float64(matrixH)
	flash := math.Mod(t*2.4, 1.0) < 0.55 || bass > 0.62
	for x := 0; x < vizBands; x++ {
		h := (int(levels[x]) / 3) * 3
		if h <= 0 {
			continue
		}
		for y := matrixH - h; y < matrixH; y++ {
			block := y / 4
			if !flash && (block+x)%2 == 0 {
				continue
			}
			var r, g, b float64
			switch {
			case y < 5:
				r, g, b = 255, 55, 55
			case y < 10:
				r, g, b = 255, 200, 35
			default:
				r, g, b = 45, 255, 120
			}
			if bass > 0.75 {
				r, g, b = r*1.15, g*1.15, b*1.15
			}
			vizPut(frame, x, y, r, g, b)
		}
	}
}

func vizUpdate() {
	if !vizWanted() {
		vizStopLoop()
		vizStopHelper()
		return
	}
	vizStartServer()
	vizEnsureHelper()
	vizEnsureLoop()
}

func startVizSupervisor() {
	vizStartServer()
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			vizUpdate()
		}
	}()
}

func vizInfo() map[string]any {
	levels, at := vizSnapshot()
	vals := make([]int, vizBands)
	for i, v := range levels {
		vals[i] = int(v)
	}
	age := 0
	if !at.IsZero() {
		age = int(time.Since(at) / time.Millisecond)
	}
	vizMu.Lock()
	enabled, active, style := vizEnabled, vizActive, vizStyle
	vizMu.Unlock()
	return map[string]any{
		"enabled": enabled,
		"active":  active,
		"style":   style,
		"levels":  vals,
		"age_ms":  age,
	}
}

func registerVizHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/api/viz", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, vizInfo())
		case http.MethodPost:
			var req struct {
				On    *bool  `json:"on"`
				Style string `json:"style"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
				return
			}
			vizMu.Lock()
			if req.On != nil {
				vizEnabled = *req.On
			}
			if req.Style != "" {
				switch req.Style {
				case "auto", "bars", "barsCenter", "bricks", "blocks", "flame", "flameCenter":
					vizStyle = req.Style
				}
			}
			vizMu.Unlock()
			vizUpdate()
			writeJSON(w, http.StatusOK, vizInfo())
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET or POST"})
		}
	})
}
