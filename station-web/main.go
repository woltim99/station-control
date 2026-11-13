package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"io/fs"
	"log"
	"math"
	mrand "math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web
var webFiles embed.FS

// Заполняются при сборке: -X main.buildVersion=... -X main.buildTime=...
var (
	buildVersion = "dev"
	buildTime    = ""
)

//go:embed matrixfont.json
var matrixFontData []byte

const backupDir = "/data/local/tmp/station-backups"

type config struct {
	Addr string
	User string
	Pass string
	Hash string
}

var (
	credMu     sync.RWMutex
	credUser   string
	credPass   string
	credHash   string
	configPath string
	configAddr string
)

func currentCreds() (string, string) {
	credMu.RLock()
	defer credMu.RUnlock()
	return credUser, credPass
}

func setCreds(user, pass string) {
	credMu.Lock()
	credUser, credPass, credHash = user, pass, ""
	credMu.Unlock()
}

func setCredsHashed(user, pass string) {
	h := hashPassword(pass)
	credMu.Lock()
	credUser, credPass, credHash = user, "", h
	credMu.Unlock()
}

func saveConfig(addr, user, pass string) error {
	hash := hashPassword(pass)
	content := fmt.Sprintf("addr=%s\nuser=%s\npassword_hash=%s\n", addr, user, hash)
	run(10*time.Second, "/system/bin/mount", "-o", "rw,remount", "/odm")
	defer run(10*time.Second, "/system/bin/mount", "-o", "ro,remount", "/odm")
	tmp := configPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0600); err != nil {
		return err
	}
	_ = os.Chmod(tmp, 0600)
	_ = os.Chown(tmp, 0, 0)
	run(5*time.Second, "/system/bin/chcon", "u:object_r:vendor_configs_file:s0", tmp)
	if err := os.Rename(tmp, configPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	run(5*time.Second, "/system/bin/chcon", "u:object_r:vendor_configs_file:s0", configPath)
	setCredsHashed(user, pass)
	return nil
}

func loadConfig(path string) config {
	c := config{Addr: "0.0.0.0:8080", User: "admin"}
	f, err := os.Open(path)
	if err != nil {
		return c
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "addr":
			c.Addr = strings.TrimSpace(v)
		case "user":
			c.User = strings.TrimSpace(v)
		case "password":
			c.Pass = strings.TrimSpace(v)
		case "password_hash":
			c.Hash = strings.TrimSpace(v)
		}
	}
	return c
}

func run(timeout time.Duration, name string, args ...string) (string, int) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	if len(out) > 262144 {
		out = out[:262144]
	}
	return string(out), code
}

var (
	propCacheMu sync.Mutex
	propCacheAt time.Time
	propCache   map[string]string
)

func loadPropDump() map[string]string {
	out, _ := run(3*time.Second, "/system/bin/getprop")
	m := make(map[string]string, 128)
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "[") {
			continue
		}
		end := strings.Index(line, "]:")
		if end <= 0 {
			continue
		}
		key := line[1:end]
		val := strings.TrimSpace(line[end+2:])
		val = strings.TrimPrefix(val, "[")
		val = strings.TrimSuffix(val, "]")
		m[key] = val
	}
	return m
}

func prop(name string) string {
	propCacheMu.Lock()
	defer propCacheMu.Unlock()
	if propCache == nil || time.Since(propCacheAt) > 10*time.Second {
		propCache = loadPropDump()
		propCacheAt = time.Now()
	}
	if len(propCache) == 0 {
		out, _ := run(2*time.Second, "/system/bin/getprop", name)
		return strings.TrimSpace(out)
	}
	return propCache[name]
}

func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func firstIPv4() string {
	ifs, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifs {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if ok && ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return ""
}

func memInfo() (uint64, uint64) {
	var total, avail uint64
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			total = v * 1024
		case "MemAvailable:":
			avail = v * 1024
		}
	}
	return total, avail
}

type cpuTimes struct {
	total    uint64
	idle     uint64
	cores    []uint64
	coreIdle []uint64
}

var (
	cpuMu   sync.Mutex
	lastCPU *cpuTimes
)

func readCPUTimes() cpuTimes {
	var t cpuTimes
	f, err := os.Open("/proc/stat")
	if err != nil {
		return t
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 || !strings.HasPrefix(fields[0], "cpu") {
			continue
		}
		var sum, idle uint64
		for i, f := range fields[1:] {
			v, _ := strconv.ParseUint(f, 10, 64)
			sum += v
			if i == 3 || i == 4 {
				idle += v
			}
		}
		if fields[0] == "cpu" {
			t.total, t.idle = sum, idle
		} else {
			t.cores = append(t.cores, sum)
			t.coreIdle = append(t.coreIdle, idle)
		}
	}
	return t
}

func cpuUsage() (float64, []float64) {
	cur := readCPUTimes()
	cpuMu.Lock()
	defer cpuMu.Unlock()
	prev := lastCPU
	lastCPU = &cur
	pct := func(total, idle, pTotal, pIdle uint64) float64 {
		if pTotal == 0 || total <= pTotal {
			if total == 0 {
				return 0
			}
			return math.Round(1000*float64(total-idle)/float64(total)) / 10
		}
		dt := float64(total - pTotal)
		di := float64(idle - pIdle)
		if dt <= 0 {
			return 0
		}
		v := 100 * (dt - di) / dt
		if v < 0 {
			v = 0
		}
		return math.Round(v*10) / 10
	}
	var total float64
	if prev == nil {
		total = pct(cur.total, cur.idle, 0, 0)
	} else {
		total = pct(cur.total, cur.idle, prev.total, prev.idle)
	}
	cores := make([]float64, len(cur.cores))
	for i := range cur.cores {
		var pidle uint64
		var ptotal uint64
		if prev != nil && i < len(prev.cores) {
			ptotal = prev.cores[i]
			pidle = prev.coreIdle[i]
		}
		cores[i] = pct(cur.cores[i], cur.coreIdle[i], ptotal, pidle)
	}
	return total, cores
}

func diskInfo(path string) (uint64, uint64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	return st.Blocks * uint64(st.Bsize), st.Bavail * uint64(st.Bsize)
}

func temperature() float64 {
	paths, _ := filepath.Glob("/sys/class/thermal/thermal_zone*/temp")
	max := 0.0
	for _, p := range paths {
		v, err := strconv.ParseFloat(readFile(p), 64)
		if err != nil {
			continue
		}
		if v > 1000 {
			v /= 1000
		}
		if v > max && v < 200 {
			max = v
		}
	}
	return max
}

func uptimeStr() string {
	fields := strings.Fields(readFile("/proc/uptime"))
	if len(fields) == 0 {
		return ""
	}
	sec, _ := strconv.ParseFloat(fields[0], 64)
	s := int(sec)
	d, h, m := s/86400, (s%86400)/3600, (s%3600)/60
	if d > 0 {
		return fmt.Sprintf("%dд %dч %dм", d, h, m)
	}
	return fmt.Sprintf("%dч %dм", h, m)
}

// ---------- Wi-Fi / Bluetooth / volume ----------

type wifiInfo struct {
	Enabled bool   `json:"enabled"`
	SSID    string `json:"ssid"`
	IP      string `json:"ip"`
	State   string `json:"state,omitempty"`
	Signal  int    `json:"signal,omitempty"`
}

type btInfo struct {
	Enabled bool   `json:"enabled"`
	Name    string `json:"name"`
	Address string `json:"address"`
}

type volumeInfo struct {
	Current int `json:"current"`
	Max     int `json:"max"`
}

var (
	ssidRe     = regexp.MustCompile(`SSID: "([^"]+)"`)
	maxVolRe   = regexp.MustCompile(`(?m)^\s*Max: (\d+)`)
	curVolRe   = regexp.MustCompile(`\(speaker\):\s*(\d+)`)
	mediaVolRe = regexp.MustCompile(`volume is (\d+) in range \[0\.\.(\d+)\]`)
	btNameRe   = regexp.MustCompile(`(?m)^\s*name: (.+)$`)
	btAddrRe   = regexp.MustCompile(`(?m)^\s*address: (\S+)`)
	btStateRe  = regexp.MustCompile(`(?m)^\s*state: (\S+)`)
)

func wifiState() wifiInfo {
	out, _ := run(4*time.Second, "/system/bin/dumpsys", "wifi")
	info := wifiInfo{Enabled: strings.Contains(out, "Wi-Fi is enabled"), IP: firstIPv4()}
	if st, err := wpaStatus(); err == nil {
		if st.SSID != "" {
			info.SSID = st.SSID
		}
		if st.IP != "" {
			info.IP = st.IP
		}
		info.State = st.State
		if st.State == "COMPLETED" {
			info.Signal = wpaSignal()
		}
	}
	for _, m := range ssidRe.FindAllStringSubmatch(out, -1) {
		if len(m) > 1 && m[1] != "" && m[1] != "null" && info.SSID == "" {
			info.SSID = m[1]
		}
	}
	return info
}

func btState() btInfo {
	out, _ := run(3*time.Second, "/system/bin/dumpsys", "bluetooth_manager")
	info := btInfo{}
	if m := btStateRe.FindStringSubmatch(out); len(m) > 1 {
		info.Enabled = strings.Contains(m[1], "ON") || m[1] == "ON"
	}
	if strings.Contains(out, "enabled: true") {
		info.Enabled = true
	}
	if m := btNameRe.FindStringSubmatch(out); len(m) > 1 {
		info.Name = strings.TrimSpace(m[1])
	}
	if m := btAddrRe.FindStringSubmatch(out); len(m) > 1 {
		info.Address = strings.TrimSpace(m[1])
	}
	return info
}

// ---------- Bluetooth helper (app_process) ----------

const btDexPath = "/data/local/tmp/station-web/bt.dex"

var btMACRe = regexp.MustCompile(`(?i)^[0-9a-f]{2}(:[0-9a-f]{2}){5}$`)

var (
	btInfoMu  sync.Mutex
	btInfoAt  time.Time
	btInfoVal map[string]any
	btScanMu  sync.Mutex
	btScanOn  bool
)

type btScanInfo struct {
	Scanning bool   `json:"scanning"`
	Started  int64  `json:"started,omitempty"`
	Found    []any  `json:"found"`
	Error    string `json:"error,omitempty"`
}

var btScanState = btScanInfo{Found: []any{}}

// btHelper запускает dex из /data/local/tmp через app_process и читает
// последнюю JSON-строку из stdout.
func btHelper(timeout time.Duration, args ...string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	full := append([]string{"/system/bin", "BtHelper"}, args...)
	cmd := exec.CommandContext(ctx, "/system/bin/app_process", full...)
	cmd.Env = append(os.Environ(), "CLASSPATH="+btDexPath)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			return m, nil
		}
	}
	return nil, fmt.Errorf("bluetooth helper: пустой ответ")
}

func btInvalidate() {
	btInfoMu.Lock()
	btInfoVal = nil
	btInfoMu.Unlock()
}

// btInfoCached отдаёт состояние адаптера и список сопряжённых устройств.
// Ответ helper'а кэшируется на 2 секунды, чтобы опрос диалога не запускал
// app_process слишком часто.
func btInfoCached() map[string]any {
	btInfoMu.Lock()
	defer btInfoMu.Unlock()
	if btInfoVal != nil && time.Since(btInfoAt) < 2*time.Second {
		return btInfoVal
	}
	m, err := btHelper(8*time.Second, "info")
	if err != nil {
		st := btState()
		m = map[string]any{
			"enabled": st.Enabled,
			"name":    st.Name,
			"address": st.Address,
			"devices": []any{},
			"error":   err.Error(),
		}
	}
	btInfoAt = time.Now()
	btInfoVal = m
	return m
}

func btScanStatus() btScanInfo {
	btScanMu.Lock()
	defer btScanMu.Unlock()
	return btScanState
}

func btStartScan(seconds int) bool {
	if seconds < 5 {
		seconds = 5
	}
	if seconds > 30 {
		seconds = 30
	}
	btScanMu.Lock()
	if btScanOn {
		btScanMu.Unlock()
		return false
	}
	btScanOn = true
	btScanState = btScanInfo{Scanning: true, Started: time.Now().Unix(), Found: []any{}}
	btScanMu.Unlock()

	go func() {
		res, err := btHelper(time.Duration(seconds+20)*time.Second, "fscan", strconv.Itoa(seconds))
		found := []any{}
		errText := ""
		if err != nil {
			errText = err.Error()
		} else if list, ok := res["devices"].([]any); ok {
			found = list
		}
		btScanMu.Lock()
		btScanOn = false
		btScanState = btScanInfo{Scanning: false, Started: time.Now().Unix(), Found: found, Error: errText}
		btScanMu.Unlock()
	}()
	return true
}

func btRunAction(action, mac string) (map[string]any, error) {
	switch action {
	case "pair", "forget", "connect", "disconnect":
	default:
		return nil, errors.New("неизвестное действие")
	}
	mac = strings.ToUpper(strings.TrimSpace(mac))
	if !btMACRe.MatchString(mac) {
		return nil, errors.New("неверный MAC")
	}
	timeout := 25 * time.Second
	if action == "pair" {
		timeout = 40 * time.Second
	}
	return btHelper(timeout, action, mac)
}

func btOpenPicker() error {
	out, code := run(6*time.Second, "/system/bin/am", "start", "-n",
		"com.android.tv.settings/.accessories.AddAccessoryActivity")
	if code != 0 {
		return fmt.Errorf("am start: %s", strings.TrimSpace(out))
	}
	return nil
}

func volumeState() volumeInfo {
	// media показывает громкость активного устройства (speaker) и работает быстро.
	if out, code := run(2*time.Second, "/system/bin/media", "volume", "--stream", "3", "--get"); code == 0 {
		if m := mediaVolRe.FindStringSubmatch(out); len(m) > 2 {
			cur, _ := strconv.Atoi(m[1])
			max, _ := strconv.Atoi(m[2])
			if max > 0 {
				return volumeInfo{Current: cur, Max: max}
			}
		}
	}

	out, _ := run(3*time.Second, "/system/bin/dumpsys", "audio")
	info := volumeInfo{}
	idx := strings.Index(out, "STREAM_MUSIC:")
	if idx < 0 {
		return info
	}
	block := out[idx:]
	if len(block) > 2000 {
		block = block[:2000]
	}
	if m := maxVolRe.FindStringSubmatch(block); len(m) > 1 {
		info.Max, _ = strconv.Atoi(m[1])
	}
	if m := curVolRe.FindStringSubmatch(block); len(m) > 1 {
		info.Current, _ = strconv.Atoi(m[1])
	}
	return info
}

func setVolume(target int) {
	cur := volumeState()
	if cur.Max <= 0 {
		return
	}
	if target < 0 {
		target = 0
	}
	if target > cur.Max {
		target = cur.Max
	}
	if target == cur.Current {
		return
	}
	// Даём вендору показать громкость на кольце, не затирая её своим цветом.
	ringHold(3 * time.Second)

	// Сначала штатный API станции: он двигает громкость и не ломает
	// вендорную индикацию на кольце.
	if glagolIsReady() {
		vol := float64(target) / float64(cur.Max)
		if err := glagolSendCommand(map[string]any{"command": "setVolume", "volume": vol}); err == nil {
			for i := 0; i < 6; i++ {
				time.Sleep(100 * time.Millisecond)
				if got := volumeState(); got.Current == target {
					return
				}
			}
		}
		cur = volumeState()
		if target == cur.Current {
			return
		}
	}

	diff := target - cur.Current
	code := "24"
	if diff < 0 {
		code = "25"
		diff = -diff
	}
	if diff > 40 {
		diff = 40
	}
	for i := 0; i < diff; i++ {
		injectKey(code)
	}
	ringHold(2 * time.Second)
}

// ---------- ring ----------

type ringConfig struct {
	Mode   string `json:"mode"`
	Color  string `json:"color"`
	Color2 string `json:"color2"`
	Level  int    `json:"level"`
	Speed  int    `json:"speed"`
	Anim   string `json:"anim,omitempty"`
}

var (
	ringMu   sync.Mutex
	ringCfg  = ringConfig{Mode: "native", Color: "#b8f36b", Color2: "#6e62ff", Level: 70, Speed: 35}
	ringStop chan struct{}
)

func writeRingSegment(i, r, g, b int) {
	// Полный нуль выключает драйвер, после чего вендор перестаёт
	// показывать громкость на кольце до перезагрузки. 1/255 не видно.
	if r <= 0 && g <= 0 && b <= 0 {
		r, g, b = 1, 1, 1
	}
	writeLED("red", i, r)
	writeLED("green", i, g)
	writeLED("blue", i, b)
}

func lerp(a, b int, t float64) int {
	return int(float64(a) + (float64(b)-float64(a))*t)
}

func writeLED(color string, i, v int) {
	if v < 0 {
		v = 0
	}
	if v > 255 {
		v = 255
	}
	_ = os.WriteFile(fmt.Sprintf("/sys/class/leds/station2:%s:ring%d/brightness", color, i),
		[]byte(strconv.Itoa(v)), 0644)
}

func setRingRGB(r, g, b int) {
	if r <= 0 && g <= 0 && b <= 0 {
		r, g, b = 1, 1, 1
	}
	for i := 1; i <= 24; i++ {
		writeLED("red", i, r)
		writeLED("green", i, g)
		writeLED("blue", i, b)
	}
}

// setRingVendor возвращает кольцо под управление вендорного LED-драйвера:
// trigger led_ring_fb снова берёт цвета из /dev/graphics/fb5 (24×1).
func setRingVendor() {
	for i := 1; i <= 24; i++ {
		for _, c := range []string{"red", "green", "blue"} {
			base := fmt.Sprintf("/sys/class/leds/station2:%s:ring%d", c, i)
			_ = os.WriteFile(base+"/trigger", []byte("led_ring_fb"), 0644)
		}
	}
}

func parseHexColor(s string, fallback [3]int) [3]int {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) != 6 {
		return fallback
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return fallback
	}
	return [3]int{int(v >> 16 & 0xff), int(v >> 8 & 0xff), int(v & 0xff)}
}

func scaleRGB(c [3]int, k float64) (int, int, int) {
	return int(float64(c[0]) * k), int(float64(c[1]) * k), int(float64(c[2]) * k)
}

func hsv(h, s, v float64) (int, int, int) {
	c := v * s
	x := c * (1 - math.Abs(math.Mod(h/60, 2)-1))
	m := v - c
	var r, g, b float64
	switch {
	case h < 60:
		r, g, b = c, x, 0
	case h < 120:
		r, g, b = x, c, 0
	case h < 180:
		r, g, b = 0, c, x
	case h < 240:
		r, g, b = 0, x, c
	case h < 300:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}
	return int((r + m) * 255), int((g + m) * 255), int((b + m) * 255)
}

func ringSetPixel(frame []byte, i, r, g, b int) {
	if i < 0 || i >= 24 || len(frame) < 72 {
		return
	}
	if r < 0 {
		r = 0
	}
	if g < 0 {
		g = 0
	}
	if b < 0 {
		b = 0
	}
	if r > 255 {
		r = 255
	}
	if g > 255 {
		g = 255
	}
	if b > 255 {
		b = 255
	}
	frame[i*3] = byte(r)
	frame[i*3+1] = byte(g)
	frame[i*3+2] = byte(b)
}

func ringFill(frame []byte, r, g, b int) {
	for i := 0; i < 24; i++ {
		ringSetPixel(frame, i, r, g, b)
	}
}

func ringLoop(cfg ringConfig, stop chan struct{}) {
	base := parseHexColor(cfg.Color, [3]int{184, 243, 107})
	second := parseHexColor(cfg.Color2, [3]int{110, 98, 255})
	level := float64(cfg.Level) / 100
	frame := make([]byte, 72)
	ticker := time.NewTicker(33 * time.Millisecond)
	defer ticker.Stop()
	start := time.Now()
	speedFactor := 0.3 + float64(cfg.Speed)/100*2.2

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		if ringPaused() {
			continue
		}
		t := time.Since(start).Seconds()
		switch cfg.Mode {
		case "off":
			ringFill(frame, 0, 0, 0)
		case "steady":
			r, g, b := scaleRGB(base, level)
			ringFill(frame, r, g, b)
		case "breathe":
			k := 0.15 + 0.85*(0.5+0.5*math.Sin(t*1.7*speedFactor))
			r, g, b := scaleRGB(base, k*level)
			ringFill(frame, r, g, b)
		case "rainbow":
			hueBase := math.Mod(t*45*speedFactor, 360)
			for i := 0; i < 24; i++ {
				hue := math.Mod(hueBase+float64(i)*15, 360)
				r, g, b := hsv(hue, 1, level*0.85)
				ringSetPixel(frame, i, r, g, b)
			}
		case "candle":
			warm := [3]int{255, 150, 60}
			for i := 0; i < 24; i++ {
				k := 0.35 + 0.65*mrand.Float64()
				k *= 0.75 + 0.25*math.Sin(t*2.2+float64(i)*0.7)
				r, g, b := scaleRGB(warm, k*level)
				ringSetPixel(frame, i, r, g, b)
			}
		case "anim":
			// Родной .led-шаблон: 24 цвета на кадр со своей длительностью.
			// Вендорный плеер нумерует сегменты в обратную сторону, поэтому
			// отражаем индекс, сохраняя начальную точку (0 -> 0).
			src := ringAnimGet(cfg.Anim).frameAt(time.Duration(t * float64(time.Second)))
			for i := 0; i < 24; i++ {
				j := (24 - i) % 24
				frame[j*3] = src[i*3]
				frame[j*3+1] = src[i*3+1]
				frame[j*3+2] = src[i*3+2]
			}
		case "gradient":
			phase := math.Mod(t*0.12*speedFactor, 1)
			for i := 0; i < 24; i++ {
				tp := math.Mod(float64(i)/24+phase, 1)
				r := int(float64(lerp(base[0], second[0], tp)) * level)
				g := int(float64(lerp(base[1], second[1], tp)) * level)
				b := int(float64(lerp(base[2], second[2], tp)) * level)
				ringSetPixel(frame, i, r, g, b)
			}
		default:
			r, g, b := scaleRGB(base, level)
			ringFill(frame, r, g, b)
		}
		if err := ringFBWrite(frame); err != nil {
			return
		}
	}
}

func applyRing(cfg ringConfig) {
	ringMu.Lock()
	if cfg.Mode == "" || cfg.Mode == "solid" {
		cfg.Mode = "steady"
	}
	if cfg.Color == "" {
		cfg.Color = "#b8f36b"
	}
	if cfg.Color2 == "" {
		cfg.Color2 = "#6e62ff"
	}
	if cfg.Level < 0 {
		cfg.Level = 0
	}
	if cfg.Level > 100 {
		cfg.Level = 100
	}
	if cfg.Speed <= 0 {
		cfg.Speed = 35
	}
	if cfg.Speed > 100 {
		cfg.Speed = 100
	}
	ringCfg = cfg
	if ringStop != nil {
		close(ringStop)
		ringStop = nil
		// Даём старому циклу завершить текущий кадр, иначе он может
		// перезаписать fb5 уже после возврата в native/новый режим.
		time.Sleep(80 * time.Millisecond)
	}
	if cfg.Mode == "native" {
		// Чистим свою картинку и отпускаем кольцо вендору.
		ringFBClear()
		setRingVendor()
		ringMu.Unlock()
		return
	}
	// Все ручные режимы рисуются в fb5, не трогая trigger.
	setRingVendor()
	ringHold(0)
	stop := make(chan struct{})
	ringStop = stop
	ringMu.Unlock()
	startRingVolumeWatch()
	go ringLoop(cfg, stop)
}

func ringState() ringConfig {
	ringMu.Lock()
	defer ringMu.Unlock()
	return ringCfg
}

// ---------- ring profiles ----------

const ringProfilesPath = "/data/local/tmp/station-web/ring-profiles.json"

type ringProfile struct {
	Name   string `json:"name"`
	Mode   string `json:"mode"`
	Color  string `json:"color"`
	Color2 string `json:"color2"`
	Level  int    `json:"level"`
	Speed  int    `json:"speed"`
}

func ringProfileFromConfig(name string, cfg ringConfig) ringProfile {
	return ringProfile{
		Name: name, Mode: cfg.Mode, Color: cfg.Color,
		Color2: cfg.Color2, Level: cfg.Level, Speed: cfg.Speed,
	}
}

func (p ringProfile) config() ringConfig {
	return ringConfig{Mode: p.Mode, Color: p.Color, Color2: p.Color2, Level: p.Level, Speed: p.Speed}
}

func loadRingProfiles() []ringProfile {
	b, err := os.ReadFile(ringProfilesPath)
	if err != nil {
		return nil
	}
	var out []ringProfile
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}

func saveRingProfiles(list []ringProfile) error {
	if err := os.MkdirAll(filepath.Dir(ringProfilesPath), 0755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ringProfilesPath, b, 0600)
}

// ---------- ring fb5 ----------

var (
	ringFBMu       sync.Mutex
	ringPauseMu    sync.Mutex
	ringPauseUntil time.Time
	ringWatchOnce  sync.Once
	ringLastMu     sync.Mutex
	ringLastFrame  = make([]byte, 72)
	ringReadMu     sync.Mutex
	ringReadFile   *os.File
)

// ringFBWrite пишет 24 логических сегмента в /dev/graphics/fb5.
// fb5 хранит пиксели в порядке ring1..ring24 и в формате BGR.
func ringFBWrite(frame []byte) error {
	if len(frame) < 72 {
		return fmt.Errorf("ring frame must be 72 bytes")
	}
	var out [72]byte
	for i := 0; i < 24; i++ {
		src := i * 3
		dst := i * 3
		out[dst] = frame[src+2]   // B
		out[dst+1] = frame[src+1] // G
		out[dst+2] = frame[src]   // R
	}
	ringFBMu.Lock()
	defer ringFBMu.Unlock()
	f, err := os.OpenFile("/dev/graphics/fb5", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.Write(out[:]); err != nil {
		return err
	}
	ringLastMu.Lock()
	copy(ringLastFrame, frame)
	ringLastMu.Unlock()
	return nil
}

func ringFBClear() {
	_ = ringFBWrite(make([]byte, 72))
}

func ringFBRead() ([]byte, error) {
	ringReadMu.Lock()
	defer ringReadMu.Unlock()
	if ringReadFile == nil {
		f, err := os.Open("/dev/graphics/fb5")
		if err != nil {
			return nil, err
		}
		ringReadFile = f
	}
	var raw [72]byte
	if _, err := ringReadFile.Seek(0, io.SeekStart); err != nil {
		_ = ringReadFile.Close()
		ringReadFile = nil
		return nil, err
	}
	if _, err := io.ReadFull(ringReadFile, raw[:]); err != nil {
		_ = ringReadFile.Close()
		ringReadFile = nil
		return nil, err
	}
	out := make([]byte, 72)
	for i := 0; i < 24; i++ {
		src := i * 3
		dst := i * 3
		out[dst] = raw[src+2]   // R
		out[dst+1] = raw[src+1] // G
		out[dst+2] = raw[src]   // B
	}
	return out, nil
}

func ringLiveFrame() ([]byte, error) {
	ringMu.Lock()
	mode := ringCfg.Mode
	ringMu.Unlock()
	if mode != "native" {
		// Свой кадр отдаём из памяти — без чтения fb5 и без задержек.
		// Во время паузы (индикация громкости) показываем реальный fb5.
		if ringPaused() {
			return ringFBRead()
		}
		ringLastMu.Lock()
		out := append([]byte(nil), ringLastFrame...)
		ringLastMu.Unlock()
		return out, nil
	}
	return ringFBRead()
}

func ringPaused() bool {
	ringPauseMu.Lock()
	defer ringPauseMu.Unlock()
	return time.Now().Before(ringPauseUntil)
}

// ringHold приостанавливает запись в fb5, чтобы вендор показал громкость.
func ringHold(d time.Duration) {
	ringPauseMu.Lock()
	if d <= 0 {
		ringPauseUntil = time.Time{}
	} else {
		ringPauseUntil = time.Now().Add(d)
	}
	ringPauseMu.Unlock()
}

func startRingVolumeWatch() {
	ringWatchOnce.Do(func() {
		go func() {
			last := -1
			for {
				time.Sleep(500 * time.Millisecond)
				ringMu.Lock()
				mode := ringCfg.Mode
				ringMu.Unlock()
				v := volumeState()
				if v.Max <= 0 {
					continue
				}
				if last >= 0 && v.Current != last {
					if mode != "native" {
						ringHold(2500 * time.Millisecond)
					}
					// Шайба может менять громкость без системной рассылки —
					// повторяем её, чтобы приёмник отправил значение на iPhone.
					airplayPushVolume(last, v.Current)
				}
				last = v.Current
			}
		}()
	})
}

// ---------- LED matrix ----------

const (
	matrixW = 25
	matrixH = 16
)

var (
	matrixFont   map[string][]int
	matrixIOMu   sync.Mutex
	matrixFile   *os.File
	matrixMu     sync.Mutex
	matrixStop   chan struct{}
	matrixMode   = "idle"
	matrixLabel  = ""
	matrixGen    = 0
	matrixBright = 100
)

func initMatrixFont() {
	_ = json.Unmarshal(matrixFontData, &matrixFont)
	if matrixFont == nil {
		matrixFont = map[string][]int{}
	}
}

func matrixBlank() []byte { return make([]byte, matrixW*matrixH*3) }

func matrixBrightness() int {
	matrixIOMu.Lock()
	defer matrixIOMu.Unlock()
	return matrixBright
}

func matrixWrite(frame []byte) error {
	if len(frame) != matrixW*matrixH*3 {
		return fmt.Errorf("matrix frame must be %d bytes", matrixW*matrixH*3)
	}
	matrixIOMu.Lock()
	defer matrixIOMu.Unlock()
	if matrixBright != 100 {
		scaled := make([]byte, len(frame))
		for i, v := range frame {
			scaled[i] = byte(int(v) * matrixBright / 100)
		}
		frame = scaled
	}
	if matrixFile == nil {
		f, err := os.OpenFile("/dev/graphics/fb4", os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		matrixFile = f
	}
	_, err := matrixFile.WriteAt(frame, 0)
	if err != nil {
		_ = matrixFile.Close()
		matrixFile = nil
	}
	return err
}

var (
	matrixPausedMu sync.Mutex
	matrixPaused   []int
)

func matrixFB4Users() []int {
	links, _ := filepath.Glob("/proc/[0-9]*/fd/*")
	seen := map[int]bool{}
	var out []int
	for _, link := range links {
		target, err := os.Readlink(link)
		if err != nil || target != "/dev/graphics/fb4" {
			continue
		}
		parts := strings.Split(link, "/")
		if len(parts) < 4 {
			continue
		}
		pid, err := strconv.Atoi(parts[2])
		if err != nil || pid == os.Getpid() || seen[pid] {
			continue
		}
		seen[pid] = true
		out = append(out, pid)
	}
	return out
}

func matrixPauseVendor() {
	matrixPausedMu.Lock()
	defer matrixPausedMu.Unlock()
	if len(matrixPaused) > 0 {
		return
	}
	for _, pid := range matrixFB4Users() {
		if err := syscall.Kill(pid, syscall.SIGSTOP); err == nil {
			matrixPaused = append(matrixPaused, pid)
		}
	}
}

func matrixResumeVendor() {
	matrixPausedMu.Lock()
	defer matrixPausedMu.Unlock()
	for _, pid := range matrixPaused {
		_ = syscall.Kill(pid, syscall.SIGCONT)
	}
	matrixPaused = nil
}

func matrixStopAnimation() {
	matrixMu.Lock()
	if matrixStop != nil {
		close(matrixStop)
		matrixStop = nil
	}
	matrixGen++
	matrixMu.Unlock()
	matrixResumeVendor()
}

func matrixScheduleStop(seconds int) {
	if seconds <= 0 {
		return
	}
	matrixMu.Lock()
	gen := matrixGen
	matrixMu.Unlock()
	go func() {
		time.Sleep(time.Duration(seconds) * time.Second)
		matrixMu.Lock()
		same := matrixGen == gen
		matrixMu.Unlock()
		if !same {
			return
		}
		matrixStopAnimation()
		// Воспроизведение всегда идёт через Glagol, поэтому возвращаем
		// штатные часы той же командой, без записи в fb4.
		glagolNativeClock()
		matrixMu.Lock()
		matrixMode, matrixLabel = "idle", ""
		matrixMu.Unlock()
	}()
}

// matrixDrawFrameGlagol отправляет статичный кадр вендору как GIF.
// Это единственный путь показа на матрице: fb4 для воспроизведения не используется.
func matrixDrawFrameGlagol(label string, frame []byte, duration int) bool {
	g, err := glagolFrameGIF(frame, 0, matrixBrightness())
	if err != nil {
		return false
	}
	url, err := glagolAssetURL(g)
	if err != nil {
		return false
	}
	return matrixRunGlagol(duration, label, url, 0)
}

func matrixRunGlagol(duration int, label, url string, repeat time.Duration) bool {
	if err := glagolDrawGIFURL(url); err != nil {
		return false
	}
	matrixStopAnimation()
	matrixMu.Lock()
	stop := make(chan struct{})
	matrixStop = stop
	matrixMode = "glagol"
	matrixLabel = label
	gen := matrixGen
	matrixMu.Unlock()
	if duration <= 0 {
		return true
	}
	go func() {
		deadline := time.Now().Add(time.Duration(duration) * time.Second)
		if repeat > 0 {
			ticker := time.NewTicker(repeat)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					if !time.Now().Before(deadline) {
						matrixFinishGlagol(gen)
						return
					}
					_ = glagolDrawGIFURL(url)
				}
			}
		}
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		select {
		case <-stop:
			return
		case <-timer.C:
			matrixFinishGlagol(gen)
		}
	}()
	return true
}

func matrixFinishGlagol(gen int) {
	matrixMu.Lock()
	if matrixGen != gen {
		matrixMu.Unlock()
		return
	}
	if matrixStop != nil {
		close(matrixStop)
		matrixStop = nil
	}
	matrixGen++
	matrixMode, matrixLabel = "idle", ""
	matrixMu.Unlock()
	glagolNativeClock()
}

func matrixStartLoop(mode, label string, loop func(stop <-chan struct{})) {
	matrixStopAnimation()
	matrixMu.Lock()
	stop := make(chan struct{})
	matrixStop = stop
	matrixMode = mode
	matrixLabel = label
	matrixMu.Unlock()
	matrixPauseVendor()
	go loop(stop)
}

var (
	matrixLiveMu   sync.Mutex
	matrixLiveData []byte
	matrixLiveAt   time.Time
)

func matrixReadFrame() ([]byte, error) {
	f, err := os.Open("/dev/graphics/fb4")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, matrixW*matrixH*3)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func matrixLivePNG() ([]byte, error) {
	matrixLiveMu.Lock()
	defer matrixLiveMu.Unlock()
	if matrixLiveData != nil && time.Since(matrixLiveAt) < 120*time.Millisecond {
		return matrixLiveData, nil
	}
	f, err := os.Open("/dev/graphics/fb4")
	if err != nil {
		return nil, err
	}
	buf := make([]byte, matrixW*matrixH*3)
	_, err = io.ReadFull(f, buf)
	_ = f.Close()
	if err != nil {
		return nil, err
	}
	src := image.NewRGBA(image.Rect(0, 0, matrixW, matrixH))
	for i := 0; i < matrixW*matrixH; i++ {
		src.Set(i%matrixW, i/matrixW, color.RGBA{R: buf[i*3+2], G: buf[i*3+1], B: buf[i*3], A: 255})
	}
	const scale = 10
	dst := image.NewRGBA(image.Rect(0, 0, matrixW*scale, matrixH*scale))
	for y := 0; y < matrixH; y++ {
		for x := 0; x < matrixW; x++ {
			c := src.At(x, y)
			for dy := 0; dy < scale; dy++ {
				for dx := 0; dx < scale; dx++ {
					dst.Set(x*scale+dx, y*scale+dy, c)
				}
			}
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, dst); err != nil {
		return nil, err
	}
	matrixLiveData = out.Bytes()
	matrixLiveAt = time.Now()
	return matrixLiveData, nil
}

func matrixStatus() map[string]any {
	matrixMu.Lock()
	defer matrixMu.Unlock()
	return map[string]any{"mode": matrixMode, "label": matrixLabel}
}

func matrixGlyph(r rune) []int {
	s := string(r)
	if rows, ok := matrixFont[s]; ok {
		return rows
	}
	up := strings.ToUpper(s)
	if rows, ok := matrixFont[up]; ok {
		return rows
	}
	if rows, ok := matrixFont["?"]; ok {
		return rows
	}
	return []int{0, 0, 0, 0, 0, 0, 0}
}

type textMask struct {
	bits  []bool
	width int
}

func renderTextMask(text string) textMask {
	runes := []rune(text)
	width := len(runes)*6 - 1
	if width < 1 {
		width = 1
	}
	bits := make([]bool, width*7)
	x := 0
	for _, r := range runes {
		rows := matrixGlyph(r)
		for y := 0; y < 7 && y < len(rows); y++ {
			v := rows[y]
			for bx := 0; bx < 5; bx++ {
				if v&(1<<(4-bx)) != 0 {
					bits[y*width+x+bx] = true
				}
			}
		}
		x += 6
	}
	return textMask{bits: bits, width: width}
}

func drawTextMask(frame []byte, m textMask, x0, y0 int, col [3]int) {
	for y := 0; y < 7; y++ {
		for x := 0; x < m.width; x++ {
			if !m.bits[y*m.width+x] {
				continue
			}
			px, py := x0+x, y0+y
			if px < 0 || px >= matrixW || py < 0 || py >= matrixH {
				continue
			}
			idx := (py*matrixW + px) * 3
			frame[idx] = byte(col[2])
			frame[idx+1] = byte(col[1])
			frame[idx+2] = byte(col[0])
		}
	}
}

func drawTextMaskSub(frame []byte, mask textMask, pos float64, y0 int, col [3]int) {
	acc := make([]float64, matrixW*matrixH)
	for y := 0; y < 7; y++ {
		for x := 0; x < mask.width; x++ {
			if !mask.bits[y*mask.width+x] {
				continue
			}
			dst := pos + float64(x)
			lo := int(math.Floor(dst))
			frac := dst - float64(lo)
			py := y0 + y
			if py < 0 || py >= matrixH {
				continue
			}
			if lo >= 0 && lo < matrixW {
				acc[py*matrixW+lo] += 1 - frac
			}
			if lo+1 >= 0 && lo+1 < matrixW {
				acc[py*matrixW+lo+1] += frac
			}
		}
	}
	for i, v := range acc {
		if v <= 0 {
			continue
		}
		if v > 1 {
			v = 1
		}
		frame[i*3] = byte(float64(col[2]) * v)
		frame[i*3+1] = byte(float64(col[1]) * v)
		frame[i*3+2] = byte(float64(col[0]) * v)
	}
}

func scaleToMatrix(src image.Image) []byte {
	frame := matrixBlank()
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 {
		return frame
	}
	scale := math.Min(float64(matrixW)/float64(sw), float64(matrixH)/float64(sh))
	dw := int(float64(sw) * scale)
	dh := int(float64(sh) * scale)
	if dw < 1 {
		dw = 1
	}
	if dh < 1 {
		dh = 1
	}
	x0, y0 := (matrixW-dw)/2, (matrixH-dh)/2
	for y := 0; y < dh; y++ {
		sy := b.Min.Y + y*sh/dh
		for x := 0; x < dw; x++ {
			sx := b.Min.X + x*sw/dw
			r, g, bl, _ := src.At(sx, sy).RGBA()
			lum := byte(((299*r + 587*g + 114*bl) / 1000) >> 8)
			idx := ((y0+y)*matrixW + (x0 + x)) * 3
			frame[idx] = lum
			frame[idx+1] = lum
			frame[idx+2] = lum
		}
	}
	return frame
}

func matrixTestPattern() []byte {
	frame := matrixBlank()
	for y := 0; y < matrixH; y++ {
		for x := 0; x < matrixW; x++ {
			idx := (y*matrixW + x) * 3
			var v byte
			switch {
			case x < matrixW/3:
				v = 60
			case x < 2*matrixW/3:
				v = 150
			default:
				v = 255
			}
			frame[idx], frame[idx+1], frame[idx+2] = v, v, v
		}
	}
	return frame
}

// ---------- status ----------

type status struct {
	Model      string            `json:"model"`
	Build      string            `json:"build"`
	Android    string            `json:"android"`
	SELinux    string            `json:"selinux"`
	IP         string            `json:"ip"`
	Hostname   string            `json:"hostname"`
	Uptime     string            `json:"uptime"`
	Load       string            `json:"load"`
	MemTotal   uint64            `json:"mem_total"`
	MemAvail   uint64            `json:"mem_avail"`
	DiskTot    uint64            `json:"disk_total"`
	DiskFree   uint64            `json:"disk_free"`
	TempC      float64           `json:"temp_c"`
	CPU        float64           `json:"cpu_pct"`
	CPUCores   []float64         `json:"cpu_cores"`
	DNS        string            `json:"dns"`
	AuthUser   string            `json:"auth_user"`
	Services   map[string]string `json:"services"`
	WiFi       wifiInfo          `json:"wifi"`
	BT         btInfo            `json:"bt"`
	Volume     volumeInfo        `json:"volume"`
	Media      mediaState        `json:"media"`
	Ring       ringConfig        `json:"ring"`
	Updater    bool              `json:"updater_installed"`
	ADBKeys    int               `json:"adb_keys"`
	Hosts      bool              `json:"hosts_blocked"`
	Version    string            `json:"panel_version"`
	Built      string            `json:"panel_build"`
	DefaultPwd bool              `json:"default_password"`
	ADBAllowed bool              `json:"adb_allowed"`
	ADBRunning bool              `json:"adb_running"`
}

func defaultPasswordInUse() bool {
	user, _ := currentCreds()
	return strings.EqualFold(user, "admin") && passwordOK("admin")
}

func invalidateStatusCache() {
	statusCacheMu.Lock()
	statusCacheAt = time.Time{}
	statusCacheMu.Unlock()
}

func collectStatusCached() status {
	statusCacheMu.Lock()
	if !statusCacheAt.IsZero() && time.Since(statusCacheAt) < 3*time.Second {
		v := statusCacheVal
		statusCacheMu.Unlock()
		return v
	}
	statusCacheMu.Unlock()

	v := collectStatus()
	statusCacheMu.Lock()
	statusCacheVal = v
	statusCacheAt = time.Now()
	statusCacheMu.Unlock()
	return v
}

var (
	statusCacheMu  sync.Mutex
	statusCacheAt  time.Time
	statusCacheVal status
	updaterMu      sync.Mutex
	updaterAt      time.Time
	updaterVal     bool
	wifiStatMu     sync.Mutex
	wifiStatAt     time.Time
	wifiStatVal    wifiInfo
	btStatMu       sync.Mutex
	btStatAt       time.Time
	btStatVal      btInfo
)

func wifiStateCached() wifiInfo {
	wifiStatMu.Lock()
	defer wifiStatMu.Unlock()
	if time.Since(wifiStatAt) < 10*time.Second {
		return wifiStatVal
	}
	wifiStatVal = wifiState()
	wifiStatAt = time.Now()
	return wifiStatVal
}

func btStateCached() btInfo {
	btStatMu.Lock()
	defer btStatMu.Unlock()
	if time.Since(btStatAt) < 10*time.Second {
		return btStatVal
	}
	btStatVal = btState()
	btStatAt = time.Now()
	return btStatVal
}

func invalidateWifiStatus() {
	wifiStatMu.Lock()
	wifiStatAt = time.Time{}
	wifiStatMu.Unlock()
}

func invalidateBtStatus() {
	btStatMu.Lock()
	btStatAt = time.Time{}
	btStatMu.Unlock()
}

func updaterInstalled() bool {
	updaterMu.Lock()
	defer updaterMu.Unlock()
	if time.Since(updaterAt) < 60*time.Second {
		return updaterVal
	}
	out, _ := run(4*time.Second, "/system/bin/pm", "list", "packages", "--user", "0")
	updaterVal = strings.Contains(out, "com.yandex.launcher.updaterapp")
	updaterAt = time.Now()
	return updaterVal
}

func volumeStateFast() volumeInfo {
	m := mediaSnapshot()
	if m.Volume > 0 && m.At > 0 && time.Now().Unix()-m.At < 15 {
		return volumeInfo{Current: m.Volume, Max: 100}
	}
	return volumeState()
}

func collectStatus() status {
	total, avail := memInfo()
	dt, df := diskInfo("/data")
	cpuPct, cpuCores := cpuUsage()
	load := readFile("/proc/loadavg")
	if fields := strings.Fields(load); len(fields) >= 3 {
		load = strings.Join(fields[:3], " ")
	}
	svc := map[string]string{}
	for _, name := range []string{"rootd", "adbd", "absshd", "adbguard", "stationweb"} {
		svc[name] = prop("init.svc." + name)
	}
	// Панель после обновления запускается через setsid, и init.svc
	// остаётся "stopped", хотя процесс работает.
	if out, _ := run(2*time.Second, "/system/bin/pidof", "station-web"); strings.TrimSpace(out) != "" {
		svc["stationweb"] = "running"
	}
	selinux, _ := run(2*time.Second, "/system/bin/getenforce")
	keys := 0
	for _, line := range strings.Split(readFile("/data/misc/adb/adb_keys"), "\n") {
		if strings.TrimSpace(line) != "" {
			keys++
		}
	}
	mounts := readFile("/proc/mounts")
	return status{
		Model:      prop("ro.product.model"),
		Build:      prop("ro.build.version.incremental"),
		Android:    prop("ro.build.version.release"),
		SELinux:    strings.TrimSpace(selinux),
		IP:         firstIPv4(),
		Hostname:   prop("net.hostname"),
		Uptime:     uptimeStr(),
		Load:       load,
		MemTotal:   total,
		MemAvail:   avail,
		DiskTot:    dt,
		DiskFree:   df,
		TempC:      temperature(),
		CPU:        cpuPct,
		CPUCores:   cpuCores,
		DNS:        prop("net.dns1"),
		AuthUser:   func() string { u, _ := currentCreds(); return u }(),
		Services:   svc,
		WiFi:       wifiStateCached(),
		BT:         btStateCached(),
		Volume:     volumeStateFast(),
		Media:      mediaSnapshot(),
		Ring:       ringState(),
		Updater:    updaterInstalled(),
		ADBKeys:    keys,
		Hosts:      strings.Contains(mounts, " /system/etc/hosts "),
		Version:    buildVersion,
		Built:      buildTime,
		DefaultPwd: defaultPasswordInUse(),
		ADBAllowed: adbAllowed(),
		ADBRunning: adbServiceRunning(),
	}
}

// ---------- backups ----------

func addTarFile(tw *tar.Writer, path, name string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() || st.Size() > 32*1024*1024 {
		return
	}
	hdr := &tar.Header{Name: name, Mode: 0600, Size: st.Size(), ModTime: st.ModTime()}
	if err := tw.WriteHeader(hdr); err != nil {
		return
	}
	_, _ = io.Copy(tw, f)
}

func createBackup() (string, error) {
	if err := os.MkdirAll(backupDir, 0700); err != nil {
		return "", err
	}
	name := "backup-" + time.Now().Format("20060102-150405") + ".tar.gz"
	full := filepath.Join(backupDir, name)
	f, err := os.Create(full)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	_ = filepath.Walk("/odm/etc", func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		rel := strings.TrimPrefix(path, "/")
		addTarFile(tw, path, rel)
		return nil
	})
	addTarFile(tw, "/data/misc/adb/adb_keys", "data/misc/adb/adb_keys")
	addTarFile(tw, "/data/local/tmp/station-web.log", "logs/station-web.log")
	addTarFile(tw, "/data/local/tmp/absshd/sshd.log", "logs/absshd.log")
	if err := tw.Close(); err != nil {
		return "", err
	}
	if err := gw.Close(); err != nil {
		return "", err
	}
	return name, nil
}

type event struct {
	Time   string `json:"time"`
	Level  string `json:"level"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

var eventsMu sync.Mutex

func addEvent(level, title, detail string) {
	eventsMu.Lock()
	defer eventsMu.Unlock()
	line, err := json.Marshal(event{Time: time.Now().Format(time.RFC3339), Level: level, Title: title, Detail: detail})
	if err != nil {
		return
	}
	appendSecretLine(eventsFile, eventsLegacy, append(line, '\n'), eventsMaxBytes)
}

func readEvents(limit int) []event {
	data, err := readSecret(eventsFile, eventsLegacy)
	if err != nil {
		return nil
	}
	out := make([]event, 0, limit)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e event
		if json.Unmarshal([]byte(line), &e) == nil {
			out = append(out, e)
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func restoreBackup(name string) (int, error) {
	if !regexp.MustCompile(`^[A-Za-z0-9._-]+\.tar\.gz$`).MatchString(name) {
		return 0, fmt.Errorf("bad backup name")
	}
	f, err := os.Open(filepath.Join(backupDir, name))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		return 0, err
	}
	defer gr.Close()
	tr := tar.NewReader(gr)

	type item struct {
		name string
		mode os.FileMode
		data []byte
	}
	var items []item
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
		if hdr.Typeflag != tar.TypeReg || hdr.Size > 32*1024*1024 {
			continue
		}
		clean := path.Clean(strings.TrimPrefix(hdr.Name, "/"))
		if strings.Contains(clean, "..") {
			continue
		}
		allowed := strings.HasPrefix(clean, "odm/etc/") || clean == "data/misc/adb/adb_keys"
		if !allowed || clean == "odm/etc/station-web.conf" {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, 32*1024*1024))
		if err != nil {
			return 0, err
		}
		items = append(items, item{name: clean, mode: os.FileMode(hdr.Mode) & 0777, data: data})
	}
	if len(items) == 0 {
		return 0, fmt.Errorf("в копии нет подходящих файлов")
	}
	run(10*time.Second, "/system/bin/mount", "-o", "rw,remount", "/odm")
	defer run(10*time.Second, "/system/bin/mount", "-o", "ro,remount", "/odm")
	count := 0
	for _, it := range items {
		dest := "/" + it.name
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			continue
		}
		mode := it.mode
		if mode == 0 {
			mode = 0600
		}
		if err := os.WriteFile(dest, it.data, mode); err != nil {
			continue
		}
		_ = os.Chmod(dest, mode)
		_ = os.Chown(dest, 0, 0)
		if it.name == "data/misc/adb/adb_keys" {
			_ = os.Chown(dest, 1000, 2000)
			_ = os.Chmod(dest, 0640)
			run(5*time.Second, "/system/bin/chcon", "u:object_r:adb_keys_file:s0", dest)
		} else {
			run(5*time.Second, "/system/bin/chcon", "u:object_r:vendor_configs_file:s0", dest)
		}
		count++
	}
	return count, nil
}

var (
	sshdLineRe = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}\.\d+)\s+(.*)$`)
	sshdUserRe = regexp.MustCompile(`login user=(\S+) from=(\S+)`)
	sshdHandRe = regexp.MustCompile(`handshake from (\S+):\s*(.*)`)
	logcatRe   = regexp.MustCompile(`^(\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d+)\s+\d+\s+\d+\s+([EWF])\s+(.+?)\s*:\s*(.*)$`)
)

func normalizeTime(layout, value string) string {
	t, err := time.ParseInLocation(layout, value, time.Local)
	if err != nil {
		return time.Now().Format(time.RFC3339)
	}
	if t.Year() == 0 {
		t = t.AddDate(time.Now().Year(), 0, 0)
	}
	return t.Format(time.RFC3339)
}

func humanSSHD() []event {
	data := tailFile("/data/local/tmp/absshd/sshd.log", 256*1024)
	var out []event
	for _, line := range strings.Split(data, "\n") {
		m := sshdLineRe.FindStringSubmatch(strings.TrimSpace(line))
		if len(m) != 3 {
			continue
		}
		when := normalizeTime("2006/01/02 15:04:05.000000", m[1])
		msg := m[2]
		e := event{Time: when, Level: "info", Title: "SSH"}
		switch {
		case strings.Contains(msg, "listening on"):
			e.Level, e.Title = "ok", "SSH-сервер запущен"
			e.Detail = strings.TrimSpace(strings.SplitN(msg, "listening on", 2)[1])
		case strings.Contains(msg, "login user="):
			if u := sshdUserRe.FindStringSubmatch(msg); len(u) == 3 {
				e.Level, e.Title = "ok", "SSH-вход"
				e.Detail = "пользователь " + u[1] + " · " + u[2]
			}
		case strings.Contains(msg, "handshake from"):
			e.Level, e.Title = "warn", "Неудачное SSH-подключение"
			if u := sshdHandRe.FindStringSubmatch(msg); len(u) == 3 {
				e.Detail = u[1] + " В· " + u[2]
			}
		case strings.Contains(msg, "listen attempt"), strings.Contains(msg, "socket"):
			e.Level, e.Title = "warn", "SSH не смог занять порт"
			e.Detail = msg
		case strings.Contains(msg, "authorized_keys"), strings.Contains(msg, "host key"):
			e.Level, e.Title = "error", "Ошибка SSH-ключей"
			e.Detail = msg
		default:
			e.Title = "SSH"
			e.Detail = msg
		}
		out = append(out, e)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}

func humanLogcat() []event {
	data, _ := run(10*time.Second, "/system/bin/logcat", "-d", "-t", "500", "-v", "time")
	var out []event
	for _, line := range strings.Split(data, "\n") {
		m := logcatRe.FindStringSubmatch(strings.TrimSpace(line))
		if len(m) != 5 {
			continue
		}
		level := "warn"
		if m[2] == "E" || m[2] == "F" {
			level = "error"
		}
		out = append(out, event{
			Time:   normalizeTime("01-02 15:04:05.000", m[1]),
			Level:  level,
			Title:  strings.TrimSpace(m[3]),
			Detail: strings.TrimSpace(m[4]),
		})
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if len(out) > 200 {
		out = out[:200]
	}
	if len(out) == 0 {
		out = append(out, event{Time: time.Now().Format(time.RFC3339), Level: "ok", Title: "Ошибок и предупреждений нет", Detail: "logcat чистый"})
	}
	return out
}

func trashBackup(name string) (string, error) {
	if !regexp.MustCompile(`^[A-Za-z0-9._-]+\.tar\.gz$`).MatchString(name) {
		return "", fmt.Errorf("bad name")
	}
	trashDir := filepath.Join(backupDir, ".trash")
	if err := os.MkdirAll(trashDir, 0700); err != nil {
		return "", err
	}
	token := name + ".undo." + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.Rename(filepath.Join(backupDir, name), filepath.Join(trashDir, token)); err != nil {
		return "", err
	}
	go func() {
		time.Sleep(30 * time.Second)
		_ = os.Remove(filepath.Join(trashDir, token))
	}()
	return token, nil
}

func untrashBackup(token string) error {
	if strings.Contains(token, "/") || strings.Contains(token, "..") {
		return fmt.Errorf("bad token")
	}
	idx := strings.LastIndex(token, ".undo.")
	if idx <= 0 {
		return fmt.Errorf("bad token")
	}
	name := token[:idx]
	if !regexp.MustCompile(`^[A-Za-z0-9._-]+\.tar\.gz$`).MatchString(name) {
		return fmt.Errorf("bad name")
	}
	return os.Rename(filepath.Join(backupDir, ".trash", token), filepath.Join(backupDir, name))
}

type backupInfo struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	Time string `json:"time"`
}

func listBackups() []backupInfo {
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		return nil
	}
	out := make([]backupInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tar.gz") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, backupInfo{Name: e.Name(), Size: info.Size(), Time: info.ModTime().Format("02.01.2006 15:04")})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out
}

// ---------- auth ----------

var (
	sessionsMu sync.Mutex
	sessions   = map[string]time.Time{}
)

// Сессии переживают перезапуск панели: иначе после каждого обновления
// открытая страница остаётся без авторизации, а live-view молча умирает.
func saveSessionsLocked() {
	data, err := json.Marshal(sessions)
	if err != nil {
		return
	}
	_ = writeSecret(sessionFile, data)
}

func saveSessions() {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	saveSessionsLocked()
}

func loadSessions() {
	b, err := readSecret(sessionFile, sessionLegacy)
	if err != nil {
		return
	}
	saved := map[string]time.Time{}
	if json.Unmarshal(b, &saved) != nil {
		return
	}
	now := time.Now()
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	for token, until := range saved {
		if token != "" && now.Before(until) {
			sessions[token] = until
		}
	}
}

func newSession() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

func sessionValid(r *http.Request) bool {
	c, err := r.Cookie("sw_session")
	if err != nil || c.Value == "" {
		return false
	}
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	until, ok := sessions[c.Value]
	if !ok || time.Now().After(until) {
		delete(sessions, c.Value)
		saveSessionsLocked()
		return false
	}
	return true
}

func credsOK(u, p string) bool {
	credMu.RLock()
	user, pass, hash := credUser, credPass, credHash
	credMu.RUnlock()
	if subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 {
		return false
	}
	if hash != "" {
		return verifyPassword(p, hash)
	}
	return subtle.ConstantTimeCompare([]byte(p), []byte(pass)) == 1
}

func passwordOK(p string) bool {
	credMu.RLock()
	pass, hash := credPass, credHash
	credMu.RUnlock()
	if hash != "" {
		return verifyPassword(p, hash)
	}
	return subtle.ConstantTimeCompare([]byte(p), []byte(pass)) == 1
}

func basicAuthOK(r *http.Request) bool {
	u, p, ok := r.BasicAuth()
	if !ok {
		return false
	}
	if allowed, _ := loginAllowed(r); !allowed {
		return false
	}
	if credsOK(u, p) {
		loginSucceeded(r)
		return true
	}
	loginFailed(r)
	return false
}

func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w)
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/login" || r.URL.Path == "/logout" {
			if !sameOriginOrNoOrigin(r) {
				if strings.HasPrefix(r.URL.Path, "/api/") {
					writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request blocked"})
					return
				}
				http.Error(w, "cross-origin request blocked", http.StatusForbidden)
				return
			}
		}
		switch r.URL.Path {
		case "/login", "/logout", "/themes.css", "/api/airplay/cover":
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/tv/") {
			next.ServeHTTP(w, r)
			return
		}
		if basicAuthOK(r) {
			next.ServeHTTP(w, r)
			return
		}
		if sessionValid(r) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

func loginHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if ok, wait := loginAllowed(r); !ok {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, loginPage(`<p class="err">Слишком много попыток. Подождите `+strconv.Itoa(int(wait.Minutes())+1)+` мин.</p>`))
				return
			}
			_ = r.ParseForm()
			if credsOK(r.FormValue("user"), r.FormValue("password")) {
				user, _ := currentCreds()
				loginSucceeded(r)
				token := newSession()
				sessionsMu.Lock()
				sessions[token] = time.Now().Add(7 * 24 * time.Hour)
				saveSessionsLocked()
				sessionsMu.Unlock()
				addEvent("ok", "Вход в панель", "пользователь "+user)
				http.SetCookie(w, &http.Cookie{
					Name: "sw_session", Value: token, Path: "/",
					HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 604800,
				})
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
			time.Sleep(400 * time.Millisecond)
			loginFailed(r)
			addEvent("warn", "Неудачный вход", "неверный логин или пароль")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, loginPage(`<p class="err">Неверный логин или пароль</p>`))
			return
		}
		if sessionValid(r) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, loginPage(""))
	}
}

func loginPage(errHTML string) string {
	page := strings.Replace(loginHTML, "{{ERROR}}", errHTML, 1)
	return strings.Replace(page, "{{VERSION}}", buildVersion, 1)
}

const loginHTML = `<!doctype html>
<html lang="ru"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Вход · Станция</title>
<link rel="stylesheet" href="/themes.css?v=5">
<script>
(function(){
  var accent = localStorage.getItem('sw-accent');
  var legacy = localStorage.getItem('sw-theme');
  if (!accent && legacy) accent = legacy === 'light' ? 'lime' : legacy;
  document.documentElement.dataset.accent = accent || 'lime';
  document.documentElement.removeAttribute('data-mode');
  var color = getComputedStyle(document.documentElement).getPropertyValue('--accent').trim() || '#b8f36b';
  var svg = "<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'><rect width='32' height='32' rx='9' fill='#121a1c'/><path d='M9 22V10h14v12M13 16h6' fill='none' stroke='" + color + "' stroke-width='3'/></svg>";
  var icon = document.createElement('link');
  icon.rel = 'icon';
  icon.href = 'data:image/svg+xml,' + encodeURIComponent(svg);
  document.head.appendChild(icon);
  var theme = document.createElement('meta');
  theme.name = 'theme-color';
  theme.content = color;
  document.head.appendChild(theme);
})();
</script>
<style>
body{margin:0;min-height:100vh;display:grid;place-items:center;background:radial-gradient(1200px 600px at 70% -10%,var(--surface) 0%,var(--bg) 60%);color:var(--text);font:15px/1.5 "Segoe UI",system-ui,sans-serif}
form{width:min(360px,90vw);background:var(--panel);border:1px solid var(--line);border-radius:18px;padding:26px}
h1{font-size:20px;margin:0 0 18px;color:var(--text)}
label{display:block;font-size:12px;color:var(--muted);margin:12px 0 6px}
input{width:100%;box-sizing:border-box;background:var(--surface2);border:1px solid var(--line);border-radius:10px;color:var(--text);padding:10px 12px;font:14px Consolas,monospace}
button{width:100%;margin-top:18px;background:var(--accent);color:var(--accent-ink);border:0;border-radius:10px;padding:11px;font-weight:700;cursor:pointer}
.err{color:var(--bad);font-size:13px;margin:12px 0 0}
small{display:block;color:var(--muted);margin-top:14px;font-size:11px}
</style></head><body>
<form method="post" action="/login">
<h1>Станция · вход</h1>
<label>Логин</label><input name="user" autocomplete="username" autofocus>
<label>Пароль</label><input name="password" type="password" autocomplete="current-password">
{{ERROR}}
<button type="submit">Войти</button>
<small>Сервер работает на станции · <span id="srv">…</span> · v{{VERSION}}</small>
<script>document.getElementById('srv').textContent = location.host;</script>
</form></body></html>`

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ---------- screen/logs ----------

var (
	screenMu   sync.Mutex
	screenData []byte
	screenAt   time.Time
)

func captureScreen() ([]byte, error) {
	screenMu.Lock()
	defer screenMu.Unlock()
	if len(screenData) > 0 && time.Since(screenAt) < 3*time.Second {
		return screenData, nil
	}
	path := "/data/local/tmp/sw-screen.png"
	if out, code := run(15*time.Second, "/system/bin/screencap", "-p", path); code != 0 {
		return nil, fmt.Errorf("screencap: %s", strings.TrimSpace(out))
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	screenData = b
	screenAt = time.Now()
	return b, nil
}

func tailFile(path string, max int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(b) > max {
		b = b[len(b)-max:]
	}
	return string(b)
}

// ---------- main ----------

func main() {
	cfgPath := flag.String("config", "/odm/etc/station-web.conf", "config file")
	logPath := flag.String("log", "/data/local/tmp/station-web.log", "log file")
	flag.Parse()

	if f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
		log.SetOutput(f)
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	cfg := loadConfig(*cfgPath)
	if cfg.Pass == "" && cfg.Hash == "" {
		log.Fatal("password is empty in ", *cfgPath)
	}
	configPath = *cfgPath
	configAddr = cfg.Addr
	if cfg.Hash != "" {
		credMu.Lock()
		credUser, credPass, credHash = cfg.User, "", cfg.Hash
		credMu.Unlock()
	} else {
		// старый конфиг: открытый пароль, при первой смене станет хэшем
		setCreds(cfg.User, cfg.Pass)
		// сразу миграция: в /odm больше не должно лежать открытого пароля
		if err := saveConfig(cfg.Addr, cfg.User, cfg.Pass); err != nil {
			log.Printf("config: не удалось заменить пароль на хэш: %v", err)
		}
	}
	loadSessions()
	initMatrixFont()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		matrixResumeVendor()
		os.Exit(0)
	}()
	addEvent("ok", "Панель запущена", "station-web · "+cfg.Addr)

	// Restore ring state after boot.
	applyRing(ringCfg)

	// Локальный Glagol: матрица через вендорный API (если доступен).
	glagolStart()

	// Держим имя видео-приёмника в соответствии с выбором в панели.
	go airplayNameWatch()

	// Отладочный слой видео-приёмника перекрывает трансляцию — гасим его.
	go func() {
		time.Sleep(15 * time.Second)
		airplayVideoDebugOverlayApply()
	}()

	// Следим за видео-приёмником отдельно от аудио-опроса.
	go tvVideoWatch()

	sub, err := fs.Sub(webFiles, "web")
	if err != nil {
		log.Fatal(err)
	}
	static := http.FileServer(http.FS(sub))
	serveIndex := func(w http.ResponseWriter, r *http.Request) {
		b, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		page := strings.ReplaceAll(string(b), "{{VERSION}}", buildVersion)
		page = strings.ReplaceAll(page, "{{BUILD}}", strings.ReplaceAll(buildTime, "T", " "))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, page)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, collectStatusCached())
	})
	mux.HandleFunc("/api/debug/mem", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET only"})
			return
		}
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		rss := 0
		if b, err := os.ReadFile("/proc/self/status"); err == nil {
			if mm := regexp.MustCompile(`(?m)^VmRSS:\s+(\d+)`).FindSubmatch(b); len(mm) > 1 {
				rss, _ = strconv.Atoi(string(mm[1]))
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"heap_alloc": m.HeapAlloc,
			"heap_inuse": m.HeapInuse,
			"heap_sys":   m.HeapSys,
			"sys":        m.Sys,
			"goroutines": runtime.NumGoroutine(),
			"num_gc":     m.NumGC,
			"rss_kb":     rss,
		})
	})
	mux.HandleFunc("/api/glagol/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, glagolStatus())
	})
	mux.HandleFunc("/api/volume", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		var req struct {
			Level int `json:"level"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		setVolume(req.Level)
		writeJSON(w, http.StatusOK, volumeState())
	})
	mux.HandleFunc("/api/media", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET only"})
			return
		}
		writeJSON(w, http.StatusOK, mediaSnapshot())
	})
	mux.HandleFunc("/api/media/action", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		var req struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		if err := mediaCommand(req.Action); err != nil {
			codes := map[string]string{"play": "126", "pause": "127", "play_pause": "85", "next": "87", "prev": "88"}
			code, ok := codes[req.Action]
			if !ok {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			out, rc := injectKey(code)
			if rc != 0 {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": strings.TrimSpace(out)})
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "media": mediaSnapshot()})
	})
	mux.HandleFunc("/api/media/seek", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		var req struct {
			Position int `json:"position"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		if err := mediaSeek(req.Position); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}
		time.Sleep(300 * time.Millisecond)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "media": mediaSnapshot()})
	})
	mux.HandleFunc("/api/alice/text", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		var req struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		if err := aliceSendText(req.Text); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("/api/bluetooth", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, map[string]any{
				"info": btInfoCached(),
				"scan": btScanStatus(),
			})
			return
		}
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET or POST"})
			return
		}
		var req struct {
			On bool `json:"on"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		arg := "disable"
		if req.On {
			arg = "enable"
		}
		run(10*time.Second, "/system/bin/svc", "bluetooth", arg)
		time.Sleep(800 * time.Millisecond)
		invalidateBtStatus()
		if !req.On {
			btScanMu.Lock()
			btScanState = btScanInfo{Found: []any{}}
			btScanOn = false
			btScanMu.Unlock()
		}
		btInvalidate()
		addEvent("info", "Bluetooth: "+arg, "")
		writeJSON(w, http.StatusOK, btState())
	})
	mux.HandleFunc("/api/bluetooth/scan", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		var req struct {
			Seconds int `json:"seconds"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048)).Decode(&req)
		if req.Seconds <= 0 {
			req.Seconds = 10
		}
		if !btStartScan(req.Seconds) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "поиск уже идёт"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "seconds": req.Seconds})
	})
	mux.HandleFunc("/api/bluetooth/action", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		var req struct {
			Action string `json:"action"`
			MAC    string `json:"mac"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		res, err := btRunAction(req.Action, req.MAC)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}
		btInvalidate()
		invalidateBtStatus()
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("/api/bluetooth/pick", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		if err := btOpenPicker(); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/wifi", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET only"})
			return
		}
		writeJSON(w, http.StatusOK, wifiState())
	})
	mux.HandleFunc("/api/wifi/saved", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"networks": wifiSavedSSIDs()})
	})
	mux.HandleFunc("/api/wifi/scan", func(w http.ResponseWriter, r *http.Request) {
		networks, err := wifiScan()
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"networks": networks})
	})
	mux.HandleFunc("/api/wifi/connect", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		var req struct {
			SSID     string `json:"ssid"`
			Password string `json:"password"`
			Security string `json:"security"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		saved, err := wifiConnect(strings.TrimSpace(req.SSID), req.Password, req.Security)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "saved": saved})
			return
		}
		addEvent("info", "Wi-Fi: подключение", req.SSID)
		invalidateWifiStatus()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved": saved, "wifi": wifiState()})
	})
	mux.HandleFunc("/api/wifi/forget", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		var req struct {
			SSID string `json:"ssid"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		if err := wifiForget(strings.TrimSpace(req.SSID)); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		addEvent("info", "Wi-Fi: сеть забыта", req.SSID)
		invalidateWifiStatus()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "wifi": wifiState()})
	})
	mux.HandleFunc("/api/wifi/enable", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		var req struct {
			On bool `json:"on"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		if err := wifiSetEnabled(req.On); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		addEvent("info", "Wi-Fi: включение/выключение", fmt.Sprintf("%v", req.On))
		invalidateWifiStatus()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "wifi": wifiState()})
	})
	mux.HandleFunc("/api/ring", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, ringState())
			return
		}
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET or POST"})
			return
		}
		var req ringConfig
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		applyRing(req)
		addEvent("info", "Кольцо: "+req.Mode, fmt.Sprintf("%s · %d%%", req.Color, req.Level))
		writeJSON(w, http.StatusOK, ringState())
	})
	mux.HandleFunc("/api/ring/profiles", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, loadRingProfiles())
		case http.MethodPost:
			var p ringProfile
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&p); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
				return
			}
			p.Name = strings.TrimSpace(p.Name)
			if p.Name == "" || len([]rune(p.Name)) > 32 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "имя профиля: 1–32 символа"})
				return
			}
			if p.Color == "" {
				p.Color = "#b8f36b"
			}
			if p.Color2 == "" {
				p.Color2 = "#6e62ff"
			}
			if p.Level < 0 {
				p.Level = 0
			}
			if p.Level > 100 {
				p.Level = 100
			}
			if p.Speed <= 0 {
				p.Speed = 35
			}
			if p.Speed > 100 {
				p.Speed = 100
			}
			list := loadRingProfiles()
			replaced := false
			for i := range list {
				if list[i].Name == p.Name {
					list[i] = p
					replaced = true
					break
				}
			}
			if !replaced {
				list = append(list, p)
			}
			if err := saveRingProfiles(list); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, list)
		case http.MethodDelete:
			name := strings.TrimSpace(r.URL.Query().Get("name"))
			if name == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
				return
			}
			list := loadRingProfiles()
			next := list[:0]
			for _, p := range list {
				if p.Name != name {
					next = append(next, p)
				}
			}
			if err := saveRingProfiles(next); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, next)
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET, POST or DELETE"})
		}
	})
	mux.HandleFunc("/api/ring/frame", func(w http.ResponseWriter, r *http.Request) {
		data, err := ringLiveFrame()
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"pixels": base64.StdEncoding.EncodeToString(data)})
	})
	mux.HandleFunc("/api/ring/live/stream", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "stream unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Connection", "keep-alive")
		flusher.Flush()
		var last []byte
		interval := 50 * time.Millisecond
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(interval):
			}
			frame, err := ringLiveFrame()
			if err != nil {
				continue
			}
			if last != nil && string(frame) == string(last) {
				// ничего не изменилось — читаем реже, чтобы не мешать станции
				if interval < 250*time.Millisecond {
					interval += 40 * time.Millisecond
				}
				continue
			}
			interval = 80 * time.Millisecond
			last = frame
			_, _ = io.WriteString(w, "data: "+base64.StdEncoding.EncodeToString(frame)+"\n\n")
			flusher.Flush()
		}
	})
	mux.HandleFunc("/api/matrix/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, matrixStatus())
	})
	mux.HandleFunc("/api/matrix/brightness", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		var req struct {
			Value int `json:"value"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		if req.Value < 0 {
			req.Value = 0
		}
		if req.Value > 100 {
			req.Value = 100
		}
		matrixIOMu.Lock()
		matrixBright = req.Value
		matrixIOMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]int{"brightness": req.Value})
	})
	mux.HandleFunc("/api/matrix/live/stream", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "stream unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Connection", "keep-alive")
		flusher.Flush()
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				frame, err := matrixReadFrame()
				if err != nil {
					continue
				}
				_, _ = io.WriteString(w, "data: "+base64.StdEncoding.EncodeToString(frame)+"\n\n")
				flusher.Flush()
			}
		}
	})
	mux.HandleFunc("/api/matrix/live", func(w http.ResponseWriter, r *http.Request) {
		data, err := matrixLivePNG()
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(data)
	})
	mux.HandleFunc("/api/live/stream", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "stream unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Connection", "keep-alive")
		flusher.Flush()
		ticker := time.NewTicker(125 * time.Millisecond)
		defer ticker.Stop()
		var lastM, lastR string
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
			}
			payload := map[string]string{}
			if m, err := matrixReadFrame(); err == nil {
				s := base64.StdEncoding.EncodeToString(m)
				if s != lastM {
					payload["m"] = s
					lastM = s
				}
			}
			if rf, err := ringLiveFrame(); err == nil {
				s := base64.StdEncoding.EncodeToString(rf)
				if s != lastR {
					payload["r"] = s
					lastR = s
				}
			}
			if len(payload) == 0 {
				continue
			}
			b, _ := json.Marshal(payload)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
	})
	mux.HandleFunc("/api/matrix/frame", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			data, err := matrixReadFrame()
			if err != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"pixels": base64.StdEncoding.EncodeToString(data)})
			return
		}
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET or POST"})
			return
		}
		var req struct {
			Pixels   string `json:"pixels"`
			Duration int    `json:"duration"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		frame, err := base64.StdEncoding.DecodeString(req.Pixels)
		if err != nil || len(frame) != matrixW*matrixH*3 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad frame"})
			return
		}
		if g, err := glagolFrameGIF(frame, 0, matrixBrightness()); err == nil {
			if url, err := glagolAssetURL(g); err == nil && matrixRunGlagol(req.Duration, "рисунок", url, 0) {
				addEvent("info", "Матрица: рисунок (Glagol)", "")
				writeJSON(w, http.StatusOK, map[string]any{"ok": true, "engine": "glagol"})
				return
			}
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Glagol недоступен"})
	})
	mux.HandleFunc("/api/matrix/text", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		var req struct {
			Text     string `json:"text"`
			Color    string `json:"color"`
			Speed    int    `json:"speed"`
			Duration int    `json:"duration"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		text := strings.TrimSpace(req.Text)
		if text == "" || len([]rune(text)) > 64 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "текст: от 1 до 64 символов"})
			return
		}
		speed := time.Duration(req.Speed) * time.Millisecond
		if g, err := glagolTextGIF(text, speed, 0, matrixBrightness()); err == nil {
			repeat := glagolTextGIFDuration(text, speed)
			if repeat < 500*time.Millisecond {
				repeat = 500 * time.Millisecond
			}
			if repeat > 30*time.Second {
				repeat = 30 * time.Second
			}
			if url, err := glagolAssetURL(g); err == nil && matrixRunGlagol(req.Duration, text, url, repeat) {
				addEvent("info", "Матрица: текст (Glagol)", text)
				writeJSON(w, http.StatusOK, map[string]any{"ok": true, "engine": "glagol", "mode": "glagol", "label": text})
				return
			}
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Glagol недоступен"})
	})
	mux.HandleFunc("/api/matrix/gif", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "не удалось прочитать файл"})
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "файл не передан"})
			return
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, 8<<20))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		duration, _ := strconv.Atoi(r.FormValue("duration"))
		if g, err := glagolResizeGIF(data, 0, matrixBrightness()); err == nil {
			repeat := glagolGIFDuration(data)
			if repeat <= 0 {
				repeat = 2 * time.Second
			}
			if repeat < 500*time.Millisecond {
				repeat = 500 * time.Millisecond
			}
			if repeat > 30*time.Second {
				repeat = 30 * time.Second
			}
			if url, err := glagolAssetURL(g); err == nil && matrixRunGlagol(duration, "GIF", url, repeat) {
				addEvent("info", "Матрица: GIF (Glagol)", "")
				writeJSON(w, http.StatusOK, map[string]any{"ok": true, "engine": "glagol"})
				return
			}
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Glagol недоступен"})
	})
	mux.HandleFunc("/api/matrix/video", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		var req struct {
			Frames   string `json:"frames"`
			FPS      int    `json:"fps"`
			Duration int    `json:"duration"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		raw, err := base64.StdEncoding.DecodeString(req.Frames)
		if err != nil || len(raw) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad frames"})
			return
		}
		const frameSize = matrixW * matrixH * 3
		count := len(raw) / frameSize
		if count < 2 || count > 900 || len(raw)%frameSize != 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "нужно от 2 до 900 кадров 25×16"})
			return
		}
		frames := make([][]byte, count)
		for i := 0; i < count; i++ {
			frames[i] = append([]byte(nil), raw[i*frameSize:(i+1)*frameSize]...)
		}
		if glagolDrawVideo(frames, req.FPS, matrixBrightness()) {
			matrixMu.Lock()
			matrixMode, matrixLabel = "glagol", "видео"
			matrixMu.Unlock()
			if req.Duration > 0 {
				matrixScheduleStop(req.Duration)
			}
			addEvent("info", "Матрица: видео (Glagol)", fmt.Sprintf("%d кадров", count))
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "engine": "glagol", "frames": count})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Glagol недоступен"})
	})
	mux.HandleFunc("/api/matrix/raw", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		var req struct {
			Command string          `json:"command"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		if strings.TrimSpace(req.Command) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty command"})
			return
		}
		var payload any = "{}"
		if len(req.Payload) > 0 {
			if err := json.Unmarshal(req.Payload, &payload); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad payload"})
				return
			}
		}
		if err := glagolSendCommand(glagolExternalCommand(req.Command, payload)); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/matrix/clear", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		if !matrixDrawFrameGlagol("выключено", matrixBlank(), 0) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Glagol недоступен"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/matrix/test", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		if !matrixDrawFrameGlagol("тест цветов", matrixTestPattern(), 0) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Glagol недоступен"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/matrix/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		matrixStopAnimation()
		matrixMu.Lock()
		matrixMode, matrixLabel = "stopped", ""
		matrixMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/matrix/native", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		matrixStopAnimation()
		clock := glagolNativeClock()
		matrixResumeVendor()
		matrixMu.Lock()
		matrixMode, matrixLabel = "native", ""
		matrixMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "clock": clock})
	})
	mux.HandleFunc("/api/exec", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		var req struct {
			Cmd string `json:"cmd"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		req.Cmd = strings.TrimSpace(req.Cmd)
		if req.Cmd == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty command"})
			return
		}
		out, code := run(30*time.Second, "/system/bin/sh", "-c", req.Cmd)
		writeJSON(w, http.StatusOK, map[string]any{"out": out, "code": code})
	})
	mux.HandleFunc("/api/key", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		var req struct {
			Key string `json:"key"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		codes := map[string]string{
			"volume_up": "24", "volume_down": "25", "mute": "164",
			"play_pause": "85", "next": "87", "prev": "88",
			"home": "3", "back": "4", "power": "26",
			"wakeup": "224", "sleep": "223",
			"dpad_up": "19", "dpad_down": "20", "dpad_left": "21", "dpad_right": "22",
			"dpad_center": "23", "enter": "66", "delete": "67",
			"menu": "82", "app_switch": "187", "search": "84",
		}
		code, ok := codes[req.Key]
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown key"})
			return
		}
		out, rc := injectKey(code)
		writeJSON(w, http.StatusOK, map[string]any{"out": out, "code": rc})
	})
	mux.HandleFunc("/api/reboot", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		addEvent("warn", "Запрошена перезагрузка", "")
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		go func() {
			time.Sleep(300 * time.Millisecond)
			run(10*time.Second, "/system/bin/reboot")
		}()
	})
	mux.HandleFunc("/api/screen", func(w http.ResponseWriter, r *http.Request) {
		data, err := captureScreen()
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(data)
	})
	mux.HandleFunc("/api/screen/info", func(w http.ResponseWriter, r *http.Request) {
		sw, sh := screenSize()
		writeJSON(w, http.StatusOK, map[string]int{"width": sw, "height": sh})
	})
	mux.HandleFunc("/api/screen/live", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "stream unsupported", http.StatusInternalServerError)
			return
		}
		screenLiveAcquire()
		defer screenLiveRelease()
		log.Printf("screen: sse start clients=%d", screenLiveCount())
		w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
		w.Header().Set("Cache-Control", "no-store, no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher.Flush()
		var lastSend int64
		var lastWrite time.Time
		for {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			screenLiveMu.Lock()
			seq := screenLiveSeq
			frame := screenLiveFrame
			screenLiveMu.Unlock()
			// Покадрово — при изменениях; раз в 0,8 с повторяем последний кадр,
			// чтобы картинка гарантированно появилась даже если первый кадр
			// пришёл раньше, чем браузер успел начать декодирование.
			if len(frame) > 0 && (seq != lastSend || time.Since(lastWrite) > 800*time.Millisecond) {
				lastSend = seq
				lastWrite = time.Now()
				_, _ = fmt.Fprintf(w, "--frame\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(frame))
				_, werr := w.Write(frame)
				_, _ = io.WriteString(w, "\r\n")
				flusher.Flush()
				if werr != nil {
					log.Printf("screen: sse write error: %v", werr)
					return
				}
				time.Sleep(3 * time.Millisecond)
			} else {
				time.Sleep(2 * time.Millisecond)
			}
		}
	})
	mux.HandleFunc("/api/input/tap", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		var req struct {
			X int `json:"x"`
			Y int `json:"y"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		if err := screenTap(req.X, req.Y); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/input/swipe", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		var req struct {
			X1       int `json:"x1"`
			Y1       int `json:"y1"`
			X2       int `json:"x2"`
			Y2       int `json:"y2"`
			Duration int `json:"duration"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		if err := screenSwipe(req.X1, req.Y1, req.X2, req.Y2, req.Duration); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/input/text", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		var req struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		if err := screenText(req.Text); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		addEvent("info", "Экран: текст", req.Text)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/logs", func(w http.ResponseWriter, r *http.Request) {
		src := r.URL.Query().Get("src")
		var body string
		switch src {
		case "logcat":
			body, _ = run(10*time.Second, "/system/bin/logcat", "-d", "-t", "250", "-v", "time")
		case "sshd":
			body = tailFile("/data/local/tmp/absshd/sshd.log", 128*1024)
		default:
			body = tailFile("/data/local/tmp/station-web.log", 128*1024)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, body)
	})
	mux.HandleFunc("/api/backups", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			name, err := createBackup()
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			addEvent("ok", "Создана резервная копия", name)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name, "items": listBackups()})
			return
		}
		writeJSON(w, http.StatusOK, listBackups())
	})
	mux.HandleFunc("/api/backups/restore", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		var req struct {
			Name             string `json:"name"`
			ResetCredentials bool   `json:"reset_credentials"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		count, err := restoreBackup(req.Name)
		if err != nil {
			addEvent("error", "Ошибка восстановления", req.Name+": "+err.Error())
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		addEvent("ok", "Восстановлено из копии", req.Name+": "+strconv.Itoa(count)+" файлов")
		relogin := false
		if req.ResetCredentials {
			if err := saveConfig(configAddr, "admin", "admin"); err == nil {
				relogin = true
				addEvent("warn", "Доступ панели сброшен", "логин/пароль admin/admin")
			}
			sessionsMu.Lock()
			sessions = map[string]time.Time{}
			saveSessionsLocked()
			sessionsMu.Unlock()
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "files": count, "reboot": true, "relogin": relogin})
	})
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, readEvents(300))
	})
	mux.HandleFunc("/api/journal", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("src") {
		case "sshd":
			writeJSON(w, http.StatusOK, humanSSHD())
		case "logcat":
			writeJSON(w, http.StatusOK, humanLogcat())
		default:
			writeJSON(w, http.StatusOK, readEvents(300))
		}
	})
	mux.HandleFunc("/api/backups/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "не удалось прочитать файл: " + err.Error()})
			return
		}
		file, hdr, err := r.FormFile("file")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "файл не передан"})
			return
		}
		defer file.Close()
		name := filepath.Base(hdr.Filename)
		if !regexp.MustCompile(`^[A-Za-z0-9._-]+\.tar\.gz$`).MatchString(name) {
			name = "upload-" + time.Now().Format("20060102-150405") + ".tar.gz"
		}
		dest := filepath.Join(backupDir, name)
		if _, err := os.Stat(dest); err == nil {
			name = "upload-" + time.Now().Format("20060102-150405") + "-" + name
			dest = filepath.Join(backupDir, name)
		}
		if err := os.MkdirAll(backupDir, 0700); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		_, err = io.Copy(out, file)
		_ = out.Close()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		addEvent("ok", "Загружена резервная копия", name)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name, "items": listBackups()})
	})
	mux.HandleFunc("/api/backups/delete", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		token, err := trashBackup(req.Name)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		addEvent("warn", "Копия удалена", req.Name+" (можно отменить 30 секунд)")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "token": token, "items": listBackups()})
	})
	mux.HandleFunc("/api/backups/undo", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		var req struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		if err := untrashBackup(req.Token); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		addEvent("ok", "Удаление отменено", "")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "items": listBackups()})
	})
	mux.HandleFunc("/api/backups/download", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if !regexp.MustCompile(`^[A-Za-z0-9._-]+\.tar\.gz$`).MatchString(name) {
			http.Error(w, "bad name", http.StatusBadRequest)
			return
		}
		http.ServeFile(w, r, filepath.Join(backupDir, name))
	})
	mux.HandleFunc("/api/features", func(w http.ResponseWriter, r *http.Request) {
		st := collectStatus()
		feats := []map[string]any{
			{"name": "Root (rootd)", "state": st.Services["rootd"], "ok": st.Services["rootd"] == "running"},
			{"name": "ADB 5555", "state": st.Services["adbd"], "ok": st.Services["adbd"] == "running"},
			{"name": "SSH 2222", "state": st.Services["absshd"], "ok": st.Services["absshd"] == "running"},
			{"name": "ADB guard", "state": st.Services["adbguard"], "ok": st.Services["adbguard"] == "running"},
			{"name": "OTA hosts", "state": map[bool]string{true: "включено", false: "выключено"}[st.Hosts], "ok": st.Hosts},
			{"name": "Апдейтер", "state": map[bool]string{true: "установлен", false: "удалён для user 0"}[st.Updater], "ok": !st.Updater},
			{"name": "Веб-панель", "state": "эта страница", "ok": true},
			{"name": "AirPlay-аудио", "state": "план", "ok": false},
			{"name": "AirPlay-видео", "state": "план", "ok": false},
		}
		writeJSON(w, http.StatusOK, feats)
	})
	mux.HandleFunc("/api/credentials", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
			return
		}
		var req struct {
			CurrentPassword string `json:"current_password"`
			User            string `json:"user"`
			Password        string `json:"password"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		if !passwordOK(req.CurrentPassword) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "неверный текущий пароль"})
			return
		}
		newUser := strings.TrimSpace(req.User)
		if !regexp.MustCompile(`^[A-Za-z0-9_.-]{3,32}$`).MatchString(newUser) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "логин: 3–32 символа, латиница/цифры/._-"})
			return
		}
		if len(req.Password) < 4 || len(req.Password) > 128 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "пароль: от 4 до 128 символов"})
			return
		}
		if err := saveConfig(configAddr, newUser, req.Password); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "не удалось сохранить конфиг: " + err.Error()})
			return
		}
		addEvent("ok", "Данные доступа изменены", "логин: "+newUser)
		sessionsMu.Lock()
		sessions = map[string]time.Time{}
		token := newSession()
		sessions[token] = time.Now().Add(7 * 24 * time.Hour)
		saveSessionsLocked()
		sessionsMu.Unlock()
		http.SetCookie(w, &http.Cookie{
			Name: "sw_session", Value: token, Path: "/",
			HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 604800,
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": newUser})
	})
	mux.HandleFunc("/login", loginHandler())
	mux.HandleFunc("/logout", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("sw_session"); err == nil {
			sessionsMu.Lock()
			delete(sessions, c.Value)
			saveSessionsLocked()
			sessionsMu.Unlock()
		}
		http.SetCookie(w, &http.Cookie{Name: "sw_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			serveIndex(w, r)
			return
		}
		static.ServeHTTP(w, r)
	}))

	registerAirplayHandlers(mux)
	registerMatrixAnimationHandlers(mux)
	registerRingAnimationHandlers(mux)
	registerADBHandlers(mux)
	registerVizHandlers(mux)
	registerNativeAudioHandlers(mux)
	registerTVHandlers(mux)

	handler := authMiddleware(mux)
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	var ln net.Listener
	for attempt := 1; ; attempt++ {
		ln, err = net.Listen("tcp", cfg.Addr)
		if err == nil {
			break
		}
		log.Printf("listen attempt %d failed: %v", attempt, err)
		if attempt >= 60 {
			log.Fatalf("cannot listen on %s: %v", cfg.Addr, err)
		}
		time.Sleep(2 * time.Second)
	}
	log.Printf("station-web listening on %s", cfg.Addr)

	// Прогреваем резидентный помощник ввода, чтобы первое нажатие после
	// запуска панели не платило за холодный старт app_process (~0,8 с).
	go func() {
		if err := inputHelperEnsure(); err != nil {
			log.Printf("input helper: %v", err)
		}
	}()

	// AirPlay-приёмник: следим за состоянием приложения и переводим на паузу
	// Яндекс Музыку, когда начинается воспроизведение с телефона.
	startAirplayWatcher()
	// Следим за громкостью и синхронизируем её с iPhone.
	startRingVolumeWatch()
	// Визуализация звука на матрице, пока что-то играет.
	startVizSupervisor()
	// Локальный WAV-поток для штатного плеера (native-визуализация).
	startNativeAudio()
	// Следим за AirPlay и включаем штатную визуализацию станции.
	startNativeSupervisor()
	// Автоподъём AirPlay 2: NQPTP, shm, su и перезапуск приёмника.
	startAirplay2Manager()

	if err := srv.Serve(ln); err != nil {
		log.Fatal(err)
	}
}
