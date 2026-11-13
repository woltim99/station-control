package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Родные шаблоны кольца из ServicesIosdk.apk (assets/quark/ledpatterns/*.led).
// Формат текстовый: 24 цвета кадра + длительность в мс, плюс директивы
// loop / gammacorrection / smooth / gradient.
type ringAnimation struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	File string `json:"file"`
}

var ringAnimations = []ringAnimation{
	{"listen", "Слушает", "listen.led"},
	{"think", "Думает", "think.led"},
	{"speak", "Говорит", "speak.led"},
	{"activation", "Просыпается", "activation.led"},
	{"greeting", "Слышит настройку", "greeting.led"},
	{"configure", "Настройка", "configure.led"},
	{"timer", "Таймер", "timer.led"},
	{"alarm", "Будильник", "alarm.led"},
	{"volume", "Громкость", "volume.led"},
	{"error", "Ошибка", "error.led"},
	{"notify", "Уведомление", "notify.led"},
	{"mute", "Микрофон выкл", "mute.led"},
	{"call", "Звонок", "call.led"},
	{"bluetooth", "Bluetooth", "bluetooth.led"},
}

type ringAnimFrame struct {
	pixels []byte
	dur    time.Duration
}

type ringAnimData struct {
	frames []ringAnimFrame
	total  time.Duration
	loop   bool
}

var (
	ringAnimMu    sync.Mutex
	ringAnimCache = map[string]*ringAnimData{}
)

func ringAnimationByID(id string) (ringAnimation, bool) {
	for _, a := range ringAnimations {
		if a.ID == id {
			return a, true
		}
	}
	return ringAnimation{}, false
}

func hexColor(s string) ([3]byte, bool) {
	var out [3]byte
	if len(s) != 6 {
		return out, false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return out, false
	}
	out[0] = byte(v >> 16 & 0xff)
	out[1] = byte(v >> 8 & 0xff)
	out[2] = byte(v & 0xff)
	return out, true
}

func parseLED(data []byte) (*ringAnimData, error) {
	var frames []ringAnimFrame
	loop := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		low := strings.ToLower(line)
		switch {
		case low == "loop":
			loop = true
		case low == "gammacorrection":
			// вендор применяет гамму при выводе; пишем значения как есть
		case strings.HasPrefix(low, "smooth"):
			// сглаживание перехода, для fb5 не нужно
		case strings.HasPrefix(low, "gradient"):
			parts := strings.Fields(line)
			if len(parts) != 6 {
				continue
			}
			p0, err0 := strconv.ParseFloat(parts[1], 64)
			p1, err1 := strconv.ParseFloat(parts[3], 64)
			c0, ok0 := hexColor(parts[2])
			c1, ok1 := hexColor(parts[4])
			dur, _ := strconv.Atoi(parts[5])
			if err0 != nil || err1 != nil || !ok0 || !ok1 || dur <= 0 {
				continue
			}
			frame := make([]byte, 72)
			for i := 0; i < 24; i++ {
				t := float64(i) / 23
				k := 0.0
				if p1 > p0 {
					k = (t - p0) / (p1 - p0)
					if k < 0 {
						k = 0
					}
					if k > 1 {
						k = 1
					}
				}
				for j := 0; j < 3; j++ {
					frame[i*3+j] = byte(float64(c0[j]) + (float64(c1[j])-float64(c0[j]))*k)
				}
			}
			frames = append(frames, ringAnimFrame{frame, time.Duration(dur) * time.Millisecond})
		default:
			parts := strings.Fields(line)
			if len(parts) != 25 {
				continue
			}
			frame := make([]byte, 72)
			ok := true
			for i := 0; i < 24; i++ {
				c, good := hexColor(parts[i])
				if !good {
					ok = false
					break
				}
				frame[i*3] = c[0]
				frame[i*3+1] = c[1]
				frame[i*3+2] = c[2]
			}
			if !ok {
				continue
			}
			dur, _ := strconv.Atoi(parts[24])
			if dur <= 0 {
				dur = 40
			}
			frames = append(frames, ringAnimFrame{frame, time.Duration(dur) * time.Millisecond})
		}
	}
	if len(frames) == 0 {
		return nil, fmt.Errorf("no frames in pattern")
	}
	total := time.Duration(0)
	for _, f := range frames {
		total += f.dur
	}
	return &ringAnimData{frames: frames, total: total, loop: loop}, nil
}

func ringAnimGet(id string) *ringAnimData {
	ringAnimMu.Lock()
	defer ringAnimMu.Unlock()
	if d, ok := ringAnimCache[id]; ok {
		return d
	}
	a, ok := ringAnimationByID(id)
	if !ok {
		return nil
	}
	b, err := fs.ReadFile(webFiles, "web/ring-anim/"+a.File)
	if err != nil {
		return nil
	}
	d, err := parseLED(b)
	if err != nil {
		return nil
	}
	ringAnimCache[id] = d
	return d
}

// frameAt возвращает кадр для момента t. Анимация всегда зациклена: это
// фоновый эффект, а не одноразовое уведомление.
func (d *ringAnimData) frameAt(t time.Duration) []byte {
	if d == nil || len(d.frames) == 0 {
		return make([]byte, 72)
	}
	if d.total <= 0 {
		return d.frames[0].pixels
	}
	if t >= d.total {
		if !d.loop {
			// одноразовый шаблон: держим финальный кадр
			return d.frames[len(d.frames)-1].pixels
		}
		t = t % d.total
	}
	acc := time.Duration(0)
	for _, f := range d.frames {
		acc += f.dur
		if t < acc {
			return f.pixels
		}
	}
	return d.frames[len(d.frames)-1].pixels
}

func (d *ringAnimData) previewColors() []string {
	if d == nil || len(d.frames) == 0 {
		return nil
	}
	best := d.frames[0]
	bestScore := -1
	for _, f := range d.frames {
		score := 0
		for _, v := range f.pixels {
			score += int(v)
		}
		if score > bestScore {
			bestScore = score
			best = f
		}
	}
	out := make([]string, 24)
	for i := 0; i < 24; i++ {
		out[i] = fmt.Sprintf("#%02x%02x%02x", best.pixels[i*3], best.pixels[i*3+1], best.pixels[i*3+2])
	}
	return out
}

func registerRingAnimationHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/api/ring/animations", func(w http.ResponseWriter, r *http.Request) {
		type item struct {
			ringAnimation
			Preview []string `json:"preview"`
		}
		out := make([]item, 0, len(ringAnimations))
		for _, a := range ringAnimations {
			out = append(out, item{a, ringAnimGet(a.ID).previewColors()})
		}
		writeJSON(w, http.StatusOK, map[string]any{"animations": out})
	})

	mux.HandleFunc("/api/ring/animations/frames", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		d := ringAnimGet(id)
		if d == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown animation"})
			return
		}
		frames := make([][]string, 0, len(d.frames))
		delays := make([]int, 0, len(d.frames))
		for _, f := range d.frames {
			cols := make([]string, 24)
			for j := 0; j < 24; j++ {
				cols[j] = fmt.Sprintf("%02x%02x%02x",
					f.pixels[j*3], f.pixels[j*3+1], f.pixels[j*3+2])
			}
			frames = append(frames, cols)
			delays = append(delays, int(f.dur/time.Millisecond))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"frames": frames,
			"delays": delays,
			"loop":   d.loop,
			"total":  int(d.total / time.Millisecond),
		})
	})

	mux.HandleFunc("/api/ring/animation", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		var req struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		anim, ok := ringAnimationByID(req.ID)
		if !ok || ringAnimGet(req.ID) == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown animation"})
			return
		}
		applyRing(ringConfig{
			Mode:   "anim",
			Anim:   anim.ID,
			Color:  "#5c9cff",
			Color2: "#926cff",
			Level:  100,
			Speed:  50,
		})
		addEvent("info", "Кольцо: анимация", anim.Name)
		writeJSON(w, http.StatusOK, ringState())
	})
}
