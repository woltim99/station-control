package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	screenWidth  = 1920
	screenHeight = 1080
)

// ---------- screen live ----------

// minicap — нативный захват через SurfaceFlinger. Один процесс сам кодирует
// JPEG и отдаёт кадры через abstract unix socket, поэтому не нужно запускать
// screencap на каждый кадр и кодировать его в Go.
const (
	minicapDir  = "/data/local/tmp/minicap"
	minicapSock = "@minicap"
)

var (
	screenLiveMu      sync.Mutex
	screenLiveClients int
	screenLiveStop    chan struct{}
	screenLiveFrame   []byte
	screenLiveSeq     int64
	screenLiveMode    string
	screenMinicapMu   sync.Mutex
)

func screenLiveAcquire() {
	screenLiveMu.Lock()
	screenLiveClients++
	if screenLiveClients == 1 {
		stop := make(chan struct{})
		screenLiveStop = stop
		go screenLiveLoop(stop)
		go screenLivePrime(stop)
	}
	screenLiveMu.Unlock()
}

// screenLivePrime страхует первый кадр: minicap отдаёт кадр только при
// изменении экрана, поэтому на статичной картинке сразу после подключения
// может быть пусто. Через 600 мс, если кадров ещё нет, снимаем один снимок.
func screenLivePrime(stop chan struct{}) {
	time.Sleep(300 * time.Millisecond)
	screenLiveMu.Lock()
	seq := screenLiveSeq
	screenLiveMu.Unlock()
	if seq > 0 {
		return
	}
	jpg, w, h, err := captureMinicapShot()
	if err != nil {
		jpg, w, h, err = captureScreenJPEG()
	}
	if err != nil {
		return
	}
	select {
	case <-stop:
		return
	default:
	}
	screenLiveMu.Lock()
	if screenLiveSeq == 0 {
		screenLiveFrame = jpg
		screenLiveSeq++
		screenWidth, screenHeight = w, h
	}
	screenLiveMu.Unlock()
}

// captureMinicapShot — одиночный снимок нативным minicap (JPEG сразу).
func captureMinicapShot() ([]byte, int, int, error) {
	bin := minicapDir + "/minicap"
	if _, err := os.Stat(bin); err != nil {
		return nil, 0, 0, err
	}
	cmd := exec.Command(bin, "-P", "1920x1080@800x450/0", "-Q", "50", "-s")
	cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH="+minicapDir)
	raw, err := cmd.Output()
	if err != nil {
		return nil, 0, 0, err
	}
	if len(raw) < 2 || raw[0] != 0xFF || raw[1] != 0xD8 {
		return nil, 0, 0, errors.New("minicap: не JPEG")
	}
	return raw, 1920, 1080, nil
}

func screenLiveRelease() {
	screenLiveMu.Lock()
	screenLiveClients--
	if screenLiveClients <= 0 && screenLiveStop != nil {
		close(screenLiveStop)
		screenLiveStop = nil
		screenLiveClients = 0
	}
	n := screenLiveClients
	screenLiveMu.Unlock()
	log.Printf("screen: sse release clients=%d", n)
}

func screenLiveCount() int {
	screenLiveMu.Lock()
	defer screenLiveMu.Unlock()
	return screenLiveClients
}

func screenLiveLoop(stop chan struct{}) {
	if screenMinicapLoop(stop) {
		return
	}
	select {
	case <-stop:
		return
	default:
	}
	screenScreencapLoop(stop)
}

// screenMinicapLoop поднимает minicap и читает кадры, пока не закроют
// live-view или процесс не умрёт. false — minicap не завёлся, и вызывающий
// переключится на резервный screencap.
func screenMinicapLoop(stop chan struct{}) bool {
	screenMinicapMu.Lock()
	defer screenMinicapMu.Unlock()

	bin := minicapDir + "/minicap"
	if _, err := os.Stat(bin); err != nil {
		return false
	}
	// Подчищаем процесс, оставшийся от предыдущего запуска панели.
	_ = exec.Command("/system/bin/pkill", "-f", bin).Run()
	time.Sleep(150 * time.Millisecond)

	cmd := exec.Command(bin, "-P", "1920x1080@800x450/0", "-Q", "50", "-S")
	cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH="+minicapDir)
	if err := cmd.Start(); err != nil {
		log.Printf("screen: minicap start: %v", err)
		return false
	}
	procDone := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(procDone)
	}()
	defer func() {
		_ = cmd.Process.Kill()
		<-procDone
	}()

	var conn net.Conn
	deadline := time.Now().Add(3 * time.Second)
	for {
		c, err := net.Dial("unix", minicapSock)
		if err == nil {
			conn = c
			break
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-stop:
			return false
		case <-time.After(80 * time.Millisecond):
		}
	}
	defer conn.Close()

	var banner [24]byte
	if _, err := io.ReadFull(conn, banner[:]); err != nil {
		log.Printf("screen: minicap banner: %v", err)
		return false
	}
	w := int(binary.LittleEndian.Uint32(banner[6:10]))
	h := int(binary.LittleEndian.Uint32(banner[10:14]))
	if w <= 0 || h <= 0 {
		w, h = 1920, 1080
	}
	log.Printf("screen: minicap connected %dx%d", w, h)

	closed := make(chan struct{})
	go func() {
		select {
		case <-stop:
			_ = conn.Close()
		case <-closed:
		}
	}()
	defer close(closed)

	screenLiveMu.Lock()
	screenLiveMode = "minicap"
	screenLiveMu.Unlock()

	var head [4]byte
	first := true
	for {
		if _, err := io.ReadFull(conn, head[:]); err != nil {
			log.Printf("screen: minicap read: %v", err)
			return false
		}
		n := int(binary.LittleEndian.Uint32(head[:]))
		if n <= 0 || n > 4<<20 {
			log.Printf("screen: minicap bad frame size %d", n)
			return false
		}
		jpg := make([]byte, n)
		if _, err := io.ReadFull(conn, jpg); err != nil {
			log.Printf("screen: minicap frame: %v", err)
			return false
		}
		if first {
			first = false
			log.Printf("screen: minicap first frame %d bytes", n)
		}
		screenLiveMu.Lock()
		screenLiveFrame = jpg
		screenLiveSeq++
		screenWidth, screenHeight = w, h
		screenLiveMu.Unlock()
	}
}

func screenScreencapLoop(stop chan struct{}) {
	log.Printf("screen: fallback to screencap")
	screenLiveMu.Lock()
	screenLiveMode = "screencap"
	screenLiveMu.Unlock()
	for i := 0; i < 3; i++ {
		go screenScreencapWorker(stop)
	}
	<-stop
}

func screenScreencapWorker(stop chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		jpg, w, h, err := captureScreenJPEG()
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		screenLiveMu.Lock()
		screenLiveFrame = jpg
		screenLiveSeq++
		screenWidth, screenHeight = w, h
		screenLiveMu.Unlock()
	}
}

// captureScreenJPEG — резервный путь, если minicap недоступен.
func captureScreenJPEG() ([]byte, int, int, error) {
	raw, err := exec.Command("/system/bin/screencap").Output()
	if err != nil {
		return nil, 0, 0, err
	}
	if len(raw) < 16 {
		return nil, 0, 0, errors.New("screencap: короткий заголовок")
	}
	w := int(binary.LittleEndian.Uint32(raw[0:4]))
	h := int(binary.LittleEndian.Uint32(raw[4:8]))
	if w <= 0 || h <= 0 || len(raw) < 16+w*h*4 {
		return nil, 0, 0, fmt.Errorf("screencap: %dx%d, %d байт", w, h, len(raw))
	}
	src := &image.RGBA{
		Pix:    raw[16 : 16+w*h*4],
		Stride: w * 4,
		Rect:   image.Rect(0, 0, w, h),
	}
	tw, th := 640, 360
	if th*w > tw*h {
		th = tw * h / w
	} else {
		tw = th * w / h
	}
	if tw < 2 {
		tw = 2
	}
	if th < 2 {
		th = 2
	}
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	for y := 0; y < th; y++ {
		sy := y * h / th
		srow := src.Pix[sy*src.Stride:]
		drow := dst.Pix[y*dst.Stride:]
		for x := 0; x < tw; x++ {
			sx := x * w / tw
			copy(drow[x*4:x*4+4], srow[sx*4:sx*4+4])
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 55}); err != nil {
		return nil, 0, 0, err
	}
	return buf.Bytes(), w, h, nil
}

func screenSize() (int, int) {
	out, code := run(2*time.Second, "/system/bin/wm", "size")
	if code == 0 {
		for _, line := range strings.Split(out, "\n") {
			if !strings.Contains(line, "size:") {
				continue
			}
			part := strings.TrimSpace(strings.SplitN(line, "size:", 2)[1])
			wh := strings.SplitN(part, "x", 2)
			if len(wh) != 2 {
				continue
			}
			w, err1 := strconv.Atoi(strings.TrimSpace(wh[0]))
			h, err2 := strconv.Atoi(strings.TrimSpace(wh[1]))
			if err1 == nil && err2 == nil && w > 0 && h > 0 {
				screenWidth, screenHeight = w, h
				return w, h
			}
		}
	}
	screenLiveMu.Lock()
	w, h := screenWidth, screenHeight
	screenLiveMu.Unlock()
	return w, h
}

func screenTap(x, y int) error {
	w, h := screenSize()
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if x >= w {
		x = w - 1
	}
	if y >= h {
		y = h - 1
	}
	out, code := injectTap(x, y)
	if code != 0 {
		return fmt.Errorf("input tap: %s", strings.TrimSpace(out))
	}
	return nil
}

func screenSwipe(x1, y1, x2, y2, duration int) error {
	w, h := screenSize()
	clamp := func(v, max int) int {
		if v < 0 {
			return 0
		}
		if v >= max {
			return max - 1
		}
		return v
	}
	x1, y1 = clamp(x1, w), clamp(y1, h)
	x2, y2 = clamp(x2, w), clamp(y2, h)
	if duration < 50 {
		duration = 50
	}
	if duration > 2000 {
		duration = 2000
	}
	out, code := injectSwipe(x1, y1, x2, y2, duration)
	if code != 0 {
		return fmt.Errorf("input swipe: %s", strings.TrimSpace(out))
	}
	return nil
}

func screenText(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("пустой текст")
	}
	if len([]rune(text)) > 120 {
		return errors.New("текст: до 120 символов")
	}
	if strings.ContainsAny(text, "\r\n") {
		return errors.New("переводы строк не поддерживаются")
	}
	out, code := run(5*time.Second, "/system/bin/input", "text", text)
	if code != 0 {
		return fmt.Errorf("input text: %s", strings.TrimSpace(out))
	}
	return nil
}
