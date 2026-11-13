package main

import (
	"bytes"
	"encoding/json"
	"image/gif"
	"io/fs"
	"net/http"
	"time"
)

// buildLoopGIF собирает один длинный GIF из повторов анимации, чтобы вендор
// проигрывал его без чёрных пауз между переотправками. Хвостовые тёмные
// кадры (фейд в чёрный) отрезаются.
func buildLoopGIF(data []byte, want time.Duration) ([]byte, time.Duration, error) {
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil || len(g.Image) == 0 {
		return nil, 0, err
	}
	if want <= 0 {
		want = 10 * time.Second
	}
	if want > 120*time.Second {
		want = 120 * time.Second
	}

	bright := make([]uint64, len(g.Image))
	var maxBright uint64
	for i, fr := range g.Image {
		var sum uint64
		for _, idx := range fr.Pix {
			r, gg, b, _ := fr.Palette[idx].RGBA()
			sum += uint64(r + gg + b)
		}
		bright[i] = sum
		if sum > maxBright {
			maxBright = sum
		}
	}
	keep := len(g.Image)
	if maxBright > 0 {
		for keep > 4 && bright[keep-1]*100 < maxBright*12 {
			keep--
		}
	}

	frames := g.Image[:keep]
	delays := g.Delay[:keep]
	var seq time.Duration
	for _, d := range delays {
		seq += time.Duration(d) * 10 * time.Millisecond
	}
	if seq <= 0 {
		seq = time.Second
	}

	out := &gif.GIF{LoopCount: 0}
	total := time.Duration(0)
	for total < want {
		for i, fr := range frames {
			out.Image = append(out.Image, fr)
			d := delays[i]
			if d <= 0 {
				d = 2
			}
			out.Delay = append(out.Delay, d)
			out.Disposal = append(out.Disposal, gif.DisposalNone)
		}
		total += seq
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, out); err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), total, nil
}

// Родные анимации матрицы из ServicesIosdk.apk (assets/quark/frontal_animation).
// GIF-файлы лежат в web/matrix-anim и отдаются как статика: браузер сам
// проигрывает их в превью, а панель — на матрице через Glagol.
type matrixAnimation struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	File string `json:"file"`
}

var matrixAnimations = []matrixAnimation{
	{"eyes", "Глаза", "eyes.gif"},
	{"wink", "Подмигивает", "wink.gif"},
	{"heart", "Сердце", "heart.gif"},
	{"love", "Глаза-сердечки", "love.gif"},
	{"heart_broken", "Разбитое сердце", "heart_broken.gif"},
	{"joy", "Радость", "joy.gif"},
	{"surprise", "Удивление", "surprise.gif"},
	{"listen", "Слушает", "listen.gif"},
	{"listen_half", "Слушает (ждёт)", "listen_half.gif"},
	{"activation", "Просыпается", "activation.gif"},
	{"confirmation", "Подтверждение", "confirmation.gif"},
	{"success", "Успех", "success.gif"},
	{"think", "Думает", "think.gif"},
	{"answer", "Говорит", "answer.gif"},
	{"timer", "Таймер", "timer.gif"},
	{"alarm", "Будильник", "alarm.gif"},
	{"volume", "Громкость", "volume.gif"},
	{"mute", "Без звука", "mute.gif"},
	{"mic_on", "Микрофон включён", "mic_on.gif"},
	{"mic_off", "Микрофон выключен", "mic_off.gif"},
	{"progress", "Загрузка", "progress.gif"},
	{"error", "Ошибка", "error.gif"},
}

func matrixAnimationByID(id string) (matrixAnimation, bool) {
	for _, a := range matrixAnimations {
		if a.ID == id {
			return a, true
		}
	}
	return matrixAnimation{}, false
}

func registerMatrixAnimationHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/api/matrix/animations", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"animations": matrixAnimations})
	})

	mux.HandleFunc("/api/matrix/animation", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		var req struct {
			ID       string `json:"id"`
			Duration int    `json:"duration"`
			Once     bool   `json:"once"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		anim, ok := matrixAnimationByID(req.ID)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown animation"})
			return
		}
		data, err := fs.ReadFile(webFiles, "web/matrix-anim/"+anim.File)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		duration := req.Duration
		if duration <= 0 {
			duration = 300
		}
		// Собираем один длинный GIF из повторов: вендор проигрывает его
		// непрерывно, без чёрной паузы между переотправками.
		playData := data
		repeat := time.Duration(0)
		if !req.Once {
			want := time.Duration(duration) * time.Second
			if want > 120*time.Second {
				want = 120 * time.Second
			}
			if long, dur, err := buildLoopGIF(data, want); err == nil {
				playData = long
				repeat = dur
			} else {
				repeat = glagolGIFDuration(data)
				if repeat <= 0 {
					repeat = 2 * time.Second
				}
				if repeat < 700*time.Millisecond {
					repeat = 700 * time.Millisecond
				}
			}
		}
		// Только Glagol: отдаём GIF локальным сервером и просим вендорный
		// draw_led_screen его показать. fb4 для воспроизведения не используем.
		url, err := glagolAssetURL(playData)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}
		if !matrixRunGlagol(duration, anim.Name, url, repeat) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Glagol недоступен"})
			return
		}
		addEvent("info", "Матрица: анимация (Glagol)", anim.Name)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "engine": "glagol", "mode": "glagol", "label": anim.Name,
		})
	})
}
