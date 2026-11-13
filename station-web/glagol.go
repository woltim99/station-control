package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	glagolHost       = "127.0.0.1"
	glagolPort       = 1961
	glagolDevices    = "/data/data/com.yandex.io.sdk/files/data/devices.json"
	glagolScanBudget = 120 * time.Second
)

var (
	glagolMu       sync.Mutex
	glagolInitMu   sync.Mutex
	glagolWS       *glagolConn
	glagolToken    string
	glagolDeviceID string
	glagolPlatform string
	glagolReady    bool
	glagolLastErr  string

	glagolAssetsMu      sync.Mutex
	glagolAssetsStartMu sync.Mutex
	glagolAssetsStarted bool
	glagolAssets        = map[string]glagolAsset{}
	glagolAssetPort     = 0
	glagolAssetHits     int
	glagolAssetBytes    int
	glagolAssetAt       time.Time
)

type glagolAsset struct {
	data []byte
	exp  time.Time
}

type glagolConn struct {
	ws    *wsClient
	token string
}

// ---------- start / status ----------

func glagolStart() {
	go glagolSupervise()
}

func glagolSupervise() {
	for {
		if glagolIsReady() {
			if glagolTokenExpiring(45 * time.Minute) {
				if err := glagolRestart(); err != nil {
					glagolSetError(err)
					log.Printf("glagol: refresh: %v", err)
				}
			}
			time.Sleep(2 * time.Minute)
			continue
		}
		if err := glagolInit(); err != nil {
			glagolSetError(err)
			log.Printf("glagol: %v", err)
			time.Sleep(15 * time.Second)
			continue
		}
		log.Printf("glagol: ready device=%s platform=%s", glagolDeviceID, glagolPlatform)
		time.Sleep(30 * time.Second)
	}
}

func glagolSetError(err error) {
	glagolMu.Lock()
	glagolLastErr = err.Error()
	glagolReady = false
	glagolMu.Unlock()
}

func glagolTokenExpiring(within time.Duration) bool {
	c, err := glagolReadCache()
	if err != nil || c.Token == "" {
		return true
	}
	return time.Unix(c.Expires, 0).Before(time.Now().Add(within))
}

func glagolRestart() error {
	glagolMu.Lock()
	old := glagolWS
	glagolWS = nil
	glagolReady = false
	glagolMu.Unlock()
	if old != nil && old.ws != nil && old.ws.conn != nil {
		_ = old.ws.conn.Close()
	}
	// glagolLoadCachedToken СЃР°Рј РЅРµ РѕС‚РґР°СЃС‚ С‚РѕРєРµРЅ СЃ РёСЃС‚РµС‡РµРЅРёРµРј < 1 С‡Р°СЃР°,
	// РїРѕСЌС‚РѕРјСѓ РєСЌС€ РЅРµ СѓРґР°Р»СЏРµРј: РїСЂРё РЅРµСѓРґР°С‡Рµ РѕР±РЅРѕРІР»РµРЅРёСЏ СЃС‚Р°СЂС‹Р№ С‚РѕРєРµРЅ РѕСЃС‚Р°РЅРµС‚СЃСЏ.
	return glagolInit()
}

func glagolInit() error {
	glagolInitMu.Lock()
	defer glagolInitMu.Unlock()
	startGlagolAssetsOnce()
	id, platform := glagolDeviceInfo()
	if id == "" || platform == "" {
		return errors.New("РЅРµ СѓРґР°Р»РѕСЃСЊ РѕРїСЂРµРґРµР»РёС‚СЊ device_id/platform")
	}
	glagolMu.Lock()
	glagolDeviceID = id
	glagolPlatform = platform
	glagolMu.Unlock()

	tok := glagolLoadCachedToken()
	if tok == "" {
		oauths := glagolFindOAuthTokens()
		if len(oauths) == 0 {
			return errors.New("OAuth-С‚РѕРєРµРЅ РЅРµ РЅР°Р№РґРµРЅ РІ РїР°РјСЏС‚Рё com.yandex.io.sdk")
		}
		var lastErr error
		for _, oauth := range oauths {
			t, err := glagolFetchDeviceToken(oauth, id, platform)
			if err == nil {
				tok = t
				glagolSaveCachedToken(tok)
				break
			}
			lastErr = err
		}
		if tok == "" {
			if lastErr == nil {
				lastErr = errors.New("РЅРµ СѓРґР°Р»РѕСЃСЊ РїРѕР»СѓС‡РёС‚СЊ device-С‚РѕРєРµРЅ")
			}
			return lastErr
		}
	}

	ws, err := glagolDial()
	if err != nil {
		return err
	}
	c := &glagolConn{ws: ws, token: tok}
	if err := c.send(map[string]any{"command": "softwareVersion"}); err != nil {
		_ = ws.conn.Close()
		return err
	}

	glagolMu.Lock()
	glagolWS = c
	glagolToken = tok
	glagolDeviceID = id
	glagolPlatform = platform
	glagolReady = true
	glagolLastErr = ""
	glagolMu.Unlock()

	// Р”РµСЂР¶РёРј РѕРґРЅРѕ РїРѕСЃС‚РѕСЏРЅРЅРѕРµ С‡С‚РµРЅРёРµ: РѕРЅРѕ РѕС‚РІРµС‡Р°РµС‚ РЅР° ping РІРµРЅРґРѕСЂР° Рё
	// СЃСЂР°Р·Сѓ Р·Р°РјРµС‡Р°РµС‚ РѕР±СЂС‹РІ. РРЅР°С‡Рµ С‡РµСЂРµР· РјРёРЅСѓС‚Сѓ РїСЂРѕСЃС‚РѕСЏ WS Р·Р°РєСЂС‹РІР°РµС‚СЃСЏ,
	// Р° Р·Р°РїРёСЃСЊ РІ РјС‘СЂС‚РІС‹Р№ СЃРѕРєРµС‚ РµС‰С‘ РєР°РєРѕРµ-С‚Рѕ РІСЂРµРјСЏ В«СѓСЃРїРµС€РЅР°В».
	go func() {
		for {
			msg, err := ws.readMessage(0)
			if err != nil {
				glagolMu.Lock()
				if glagolWS == c {
					glagolWS = nil
					glagolReady = false
					glagolLastErr = err.Error()
				}
				glagolMu.Unlock()
				_ = ws.conn.Close()
				return
			}
			if msg != "" {
				glagolHandleMessage(msg)
			}
		}
	}()

	return nil
}

func glagolIsReady() bool {
	glagolMu.Lock()
	defer glagolMu.Unlock()
	return glagolReady
}

// glagolEnsureFast РїРµСЂРµРїРѕРґРєР»СЋС‡Р°РµС‚ WS, РµСЃР»Рё РѕРЅ С‚РѕР»СЊРєРѕ С‡С‚Рѕ РѕР±РѕСЂРІР°Р»СЃСЏ.
// РўРѕРєРµРЅ СѓР¶Рµ РІ РєСЌС€Рµ, РїРѕСЌС‚РѕРјСѓ СЌС‚Рѕ Р±С‹СЃС‚СЂС‹Р№ dial Р±РµР· РїРѕРІС‚РѕСЂРЅРѕРіРѕ СЃРєР°РЅРёСЂРѕРІР°РЅРёСЏ.
func glagolEnsureFast() bool {
	if glagolIsReady() {
		return true
	}
	if glagolLoadCachedToken() == "" {
		return false
	}
	if err := glagolInit(); err != nil {
		glagolSetError(err)
		return false
	}
	return true
}

func glagolStatus() map[string]any {
	glagolMu.Lock()
	ready := glagolReady
	device := glagolDeviceID
	platform := glagolPlatform
	lastErr := glagolLastErr
	glagolMu.Unlock()

	glagolAssetsMu.Lock()
	hits, bytes, at := glagolAssetHits, glagolAssetBytes, glagolAssetAt
	glagolAssetsMu.Unlock()

	out := map[string]any{
		"ready":       ready,
		"device":      device,
		"platform":    platform,
		"error":       lastErr,
		"asset_hits":  hits,
		"asset_bytes": bytes,
	}
	if !at.IsZero() {
		out["asset_at"] = at.Unix()
	}
	return out
}

// ---------- media / Alice ----------

type mediaState struct {
	Playing  bool   `json:"playing"`
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	Cover    string `json:"cover"`
	Progress int    `json:"progress"`
	Duration int    `json:"duration"`
	TrackID  string `json:"track_id"`
	Type     string `json:"type"`
	Volume   int    `json:"volume"`
	Alice    string `json:"alice,omitempty"`
	At       int64  `json:"at,omitempty"`
}

var (
	mediaMu       sync.Mutex
	mediaCur      mediaState
	mediaVolumeAt time.Time
)

func mediaSnapshot() mediaState {
	mediaMu.Lock()
	defer mediaMu.Unlock()
	return mediaCur
}

func glagolVolumeRecent() bool {
	mediaMu.Lock()
	defer mediaMu.Unlock()
	return !mediaVolumeAt.IsZero() && time.Since(mediaVolumeAt) < 700*time.Millisecond
}

func glagolCoverURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	raw = strings.TrimPrefix(raw, "https://")
	raw = strings.ReplaceAll(raw, "%%", "400x400")
	if strings.Contains(raw, ".") {
		return "https://" + raw
	}
	return ""
}

// glagolHandleMessage РѕР±РЅРѕРІР»СЏРµС‚ СЃРѕСЃС‚РѕСЏРЅРёРµ РїР»РµРµСЂР° РёР· РІС…РѕРґСЏС‰РµРіРѕ state-СЃРѕРѕР±С‰РµРЅРёСЏ.
// РЎС‚Р°РЅС†РёСЏ РїСЂРёСЃС‹Р»Р°РµС‚ РµРіРѕ РїСЂРёРјРµСЂРЅРѕ СЂР°Р· РІ СЃРµРєСѓРЅРґСѓ, РїРѕРєР° РёРіСЂР°РµС‚ РјСѓР·С‹РєР°.
func glagolHandleMessage(msg string) {
	if len(msg) > 128*1024 {
		return
	}
	if !strings.Contains(msg, `"playerState"`) && !strings.Contains(msg, `"aliceState"`) {
		return
	}
	var env struct {
		State *struct {
			Playing     *bool    `json:"playing"`
			Volume      *float64 `json:"volume"`
			AliceState  string   `json:"aliceState"`
			PlayerState *struct {
				ID       string   `json:"id"`
				Title    string   `json:"title"`
				Subtitle string   `json:"subtitle"`
				Progress *float64 `json:"progress"`
				Duration *float64 `json:"duration"`
				Type     string   `json:"type"`
				Extra    *struct {
					CoverURI string `json:"coverURI"`
				} `json:"extra"`
			} `json:"playerState"`
		} `json:"state"`
	}
	if err := json.Unmarshal([]byte(msg), &env); err != nil || env.State == nil {
		return
	}
	st := env.State
	mediaMu.Lock()
	defer mediaMu.Unlock()
	if st.Playing != nil {
		mediaCur.Playing = *st.Playing
	}
	if st.Volume != nil {
		v := int(math.Round(*st.Volume * 100))
		if v != mediaCur.Volume {
			mediaCur.Volume = v
			mediaVolumeAt = time.Now()
		}
	}
	if st.AliceState != "" {
		mediaCur.Alice = st.AliceState
	}
	if ps := st.PlayerState; ps != nil {
		if ps.Title != "" || ps.ID != "" {
			mediaCur.Title = ps.Title
			mediaCur.Artist = ps.Subtitle
			mediaCur.TrackID = ps.ID
			mediaCur.Type = ps.Type
		}
		if ps.Progress != nil {
			mediaCur.Progress = int(*ps.Progress)
		}
		if ps.Duration != nil {
			mediaCur.Duration = int(*ps.Duration)
		}
		if ps.Extra != nil {
			if u := glagolCoverURL(ps.Extra.CoverURI); u != "" {
				mediaCur.Cover = u
			}
		}
	}
	mediaCur.At = time.Now().Unix()
}

func mediaCommand(action string) error {
	switch action {
	case "play":
		return glagolSendCommand(map[string]any{"command": "play"})
	case "pause":
		return glagolSendCommand(map[string]any{"command": "stop"})
	case "play_pause":
		if mediaSnapshot().Playing {
			return glagolSendCommand(map[string]any{"command": "stop"})
		}
		return glagolSendCommand(map[string]any{"command": "play"})
	case "next":
		return glagolSendCommand(map[string]any{"command": "next"})
	case "prev":
		return glagolSendCommand(map[string]any{"command": "prev"})
	}
	return errors.New("РЅРµРёР·РІРµСЃС‚РЅРѕРµ РґРµР№СЃС‚РІРёРµ")
}

// mediaSeek РїРµСЂРµРјР°С‚С‹РІР°РµС‚ С‚РµРєСѓС‰РёР№ С‚СЂРµРє. Р’РµРЅРґРѕСЂРЅРѕРµ API РїСЂРёРЅРёРјР°РµС‚ РїРѕР·РёС†РёСЋ РІ СЃРµРєСѓРЅРґР°С….
func mediaSeek(position int) error {
	if position < 0 {
		position = 0
	}
	if position > 24*3600 {
		return errors.New("РїРѕР·РёС†РёСЏ РІРЅРµ РґРёР°РїР°Р·РѕРЅР°")
	}
	if d := mediaSnapshot().Duration; d > 0 && position > d {
		position = d
	}
	return glagolSendCommand(map[string]any{"command": "rewind", "position": position})
}

func aliceSendText(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("РїСѓСЃС‚РѕР№ С‚РµРєСЃС‚")
	}
	if len([]rune(text)) > 200 {
		return errors.New("РЅРµ Р±РѕР»СЊС€Рµ 200 СЃРёРјРІРѕР»РѕРІ")
	}
	return glagolSendCommand(map[string]any{"command": "sendText", "text": text})
}

// ---------- device id / platform ----------

var (
	glagolIDRe   = regexp.MustCompile(`"id":"(X[0-9A-Za-z_-]{6,})"`)
	glagolPlatRe = regexp.MustCompile(`"platform":"(yandexstation_2)"`)
)

func glagolDeviceInfo() (string, string) {
	b, err := os.ReadFile(glagolDevices)
	if err != nil {
		return "", ""
	}
	id := ""
	if m := glagolIDRe.FindSubmatch(b); len(m) > 1 {
		id = string(m[1])
	}
	platform := ""
	if m := glagolPlatRe.FindSubmatch(b); len(m) > 1 {
		platform = string(m[1])
	}
	if id == "" || platform == "" {
		// fallback: РєСЌС€
		if c, err := glagolReadCache(); err == nil {
			if id == "" {
				id = c.Device
			}
			if platform == "" {
				platform = c.Platform
			}
		}
	}
	return id, platform
}

// ---------- token cache ----------

type glagolCache struct {
	Token    string `json:"token"`
	Expires  int64  `json:"expires"`
	Device   string `json:"device"`
	Platform string `json:"platform"`
}

func glagolReadCache() (glagolCache, error) {
	var c glagolCache
	b, err := readSecret(glagolFile, glagolLegacy)
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(b, &c)
	return c, err
}

func glagolLoadCachedToken() string {
	c, err := glagolReadCache()
	if err != nil {
		return ""
	}
	if c.Expires <= time.Now().Unix()+3600 {
		return ""
	}
	return c.Token
}

func glagolSaveCachedToken(tok string) {
	exp := time.Now().Add(12 * time.Hour).Unix()
	// JWT exp, РµСЃР»Рё СѓРґР°Р»РѕСЃСЊ СЂР°Р·РѕР±СЂР°С‚СЊ
	if parts := strings.Split(tok, "."); len(parts) == 3 {
		if raw, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
			var p struct {
				Exp int64 `json:"exp"`
			}
			if json.Unmarshal(raw, &p) == nil && p.Exp > 0 {
				exp = p.Exp
			}
		}
	}
	c := glagolCache{Token: tok, Expires: exp, Device: glagolDeviceID, Platform: glagolPlatform}
	b, _ := json.Marshal(c)
	_ = writeSecret(glagolFile, b)
}

// ---------- OAuth token from app memory ----------

var glagolOAuthRe = regexp.MustCompile(`y0_[A-Za-z0-9_-]{40,120}`)

func glagolYandexPids() []int {
	wanted := []string{
		"com.yandex.tv.services",
		"com.yandex.io.sdk",
		"com.yandex.tv.music",
		"com.yandex.tv.alice",
		"com.yandex.tv.home",
	}
	type candid struct {
		pid  int
		prio int
	}
	var found []candid
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil {
			continue
		}
		cmd := string(b)
		for prio, w := range wanted {
			if strings.Contains(cmd, w) {
				found = append(found, candid{pid: pid, prio: prio})
				break
			}
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].prio < found[j].prio })
	pids := make([]int, 0, len(found))
	for _, f := range found {
		pids = append(pids, f.pid)
	}
	return pids
}

func glagolScanOAuth(pid int, seen map[string]bool, out *[]string, deadline time.Time) {
	mem, err := os.Open(fmt.Sprintf("/proc/%d/mem", pid))
	if err != nil {
		return
	}
	defer mem.Close()
	maps, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(maps), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		perm := fields[1]
		if !strings.HasPrefix(perm, "rw") {
			continue
		}
		path := ""
		if len(fields) >= 6 {
			path = fields[5]
		}
		if path != "" && !strings.HasPrefix(path, "[heap]") && !strings.HasPrefix(path, "[anon:") {
			continue
		}
		parts := strings.Split(fields[0], "-")
		if len(parts) != 2 {
			continue
		}
		lo, err1 := strconv.ParseUint(parts[0], 16, 64)
		hi, err2 := strconv.ParseUint(parts[1], 16, 64)
		if err1 != nil || err2 != nil || hi <= lo {
			continue
		}
		size := hi - lo
		if size > 256<<20 {
			continue
		}
		const chunk = 4 << 20
		buf := make([]byte, chunk)
		for off := uint64(0); off < size; off += chunk {
			if time.Now().After(deadline) {
				break
			}
			n, err := mem.ReadAt(buf, int64(lo+off))
			if n > 0 {
				for _, m := range glagolOAuthRe.FindAll(buf[:n], -1) {
					tok := string(m)
					if !seen[tok] {
						seen[tok] = true
						*out = append(*out, tok)
					}
				}
			}
			if err != nil {
				break
			}
		}
		if time.Now().After(deadline) {
			break
		}
	}
}

func glagolFindOAuthTokens() []string {
	deadline := time.Now().Add(glagolScanBudget)
	seen := map[string]bool{}
	var candidates []string
	for _, pid := range glagolYandexPids() {
		glagolScanOAuth(pid, seen, &candidates, deadline)
		if time.Now().After(deadline) {
			break
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool { return len(candidates[i]) > len(candidates[j]) })
	return candidates
}

// ---------- cloud token ----------

// glagolRootCAs СЃРѕР±РёСЂР°РµС‚ РґРѕРІРµСЂРµРЅРЅС‹Рµ РєРѕСЂРЅРё: СЃС‚Р°РЅРґР°СЂС‚РЅС‹Рµ РїСѓС‚Рё Go РЅР° Android
// РїСѓСЃС‚С‹, РїРѕСЌС‚РѕРјСѓ С‡РёС‚Р°РµРј СЃРёСЃС‚РµРјРЅС‹Р№ РјР°РіР°Р·РёРЅ Android РЅР°РїСЂСЏРјСѓСЋ.
func glagolRootCAs() *x509.CertPool {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	dirs := []string{
		"/system/etc/security/cacerts",
		"/apex/com.android.conscrypt/cacerts",
		"/system/etc/security/cacerts_google",
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			b, err := os.ReadFile(dir + "/" + e.Name())
			if err != nil {
				continue
			}
			pool.AppendCertsFromPEM(b)
		}
	}
	return pool
}

func glagolFetchDeviceToken(oauth, device, platform string) (string, error) {
	url := "https://quasar.yandex.net/glagol/token?device_id=" + device + "&platform=" + platform
	dns := strings.TrimSpace(prop("net.dns1"))
	if dns == "" || dns == "0.0.0.0" {
		dns = "8.8.8.8"
	}
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, "udp", net.JoinHostPort(dns, "53"))
		},
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, Resolver: resolver}
	client := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			DialContext:     dialer.DialContext,
			TLSClientConfig: &tls.Config{RootCAs: glagolRootCAs(), MinVersion: tls.VersionTLS12},
		},
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("Authorization", "OAuth "+oauth)
		req.Header.Set("User-Agent", "okhttp/4.12.0")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(5 * time.Second)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == 429 {
			lastErr = errors.New("HTTP 429 (rate limit)")
			time.Sleep(20 * time.Second)
			continue
		}
		if resp.StatusCode != 200 {
			lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
			time.Sleep(5 * time.Second)
			continue
		}
		var out struct {
			Status string `json:"status"`
			Token  string `json:"token"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			lastErr = err
			continue
		}
		if out.Status != "ok" || out.Token == "" {
			lastErr = fmt.Errorf("status=%q", out.Status)
			continue
		}
		return out.Token, nil
	}
	return "", lastErr
}

// ---------- websocket ----------

type wsClient struct {
	conn net.Conn
	br   *bufio.Reader
	mu   sync.Mutex
}

func glagolDial() (*wsClient, error) {
	addr := net.JoinHostPort(glagolHost, strconv.Itoa(glagolPort))
	raw, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if tcp, ok := raw.(*net.TCPConn); ok {
		_ = tcp.SetKeepAlive(true)
		_ = tcp.SetKeepAlivePeriod(30 * time.Second)
	}
	tlsConn := tls.Client(raw, &tls.Config{InsecureSkipVerify: true})
	if err := tlsConn.Handshake(); err != nil {
		raw.Close()
		return nil, err
	}
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	req := fmt.Sprintf("GET / HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
		addr, base64.StdEncoding.EncodeToString(key))
	if _, err := tlsConn.Write([]byte(req)); err != nil {
		tlsConn.Close()
		return nil, err
	}
	br := bufio.NewReaderSize(tlsConn, 1<<20)
	status, err := br.ReadString('\n')
	if err != nil {
		tlsConn.Close()
		return nil, err
	}
	if !strings.Contains(status, "101") {
		tlsConn.Close()
		return nil, fmt.Errorf("ws handshake: %s", strings.TrimSpace(status))
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			tlsConn.Close()
			return nil, err
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	return &wsClient{conn: tlsConn, br: br}, nil
}

func (w *wsClient) writeFrame(op byte, payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	header := []byte{0x80 | op}
	n := len(payload)
	switch {
	case n < 126:
		header = append(header, byte(n)|0x80)
	case n <= 65535:
		header = append(header, 126|0x80, byte(n>>8), byte(n))
	default:
		header = append(header, 127|0x80)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		header = append(header, b[:]...)
	}
	mask := make([]byte, 4)
	_, _ = rand.Read(mask)
	header = append(header, mask...)
	masked := make([]byte, n)
	for i := 0; i < n; i++ {
		masked[i] = payload[i] ^ mask[i%4]
	}
	_, err := w.conn.Write(append(header, masked...))
	return err
}

func (w *wsClient) readMessage(timeout time.Duration) (string, error) {
	if timeout > 0 {
		_ = w.conn.SetReadDeadline(time.Now().Add(timeout))
	} else {
		_ = w.conn.SetReadDeadline(time.Time{})
	}
	var msg []byte
	for {
		b0, err := w.br.ReadByte()
		if err != nil {
			return "", err
		}
		b1, err := w.br.ReadByte()
		if err != nil {
			return "", err
		}
		fin := b0&0x80 != 0
		op := b0 & 0x0f
		masked := b1&0x80 != 0
		n := int(b1 & 0x7f)
		if n == 126 {
			var b [2]byte
			if _, err := io.ReadFull(w.br, b[:]); err != nil {
				return "", err
			}
			n = int(binary.BigEndian.Uint16(b[:]))
		} else if n == 127 {
			var b [8]byte
			if _, err := io.ReadFull(w.br, b[:]); err != nil {
				return "", err
			}
			n = int(binary.BigEndian.Uint64(b[:]))
		}
		var mask []byte
		if masked {
			mask = make([]byte, 4)
			if _, err := io.ReadFull(w.br, mask); err != nil {
				return "", err
			}
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(w.br, payload); err != nil {
			return "", err
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}
		switch op {
		case 0x8:
			return "", io.EOF
		case 0x9:
			_ = w.writeFrame(0xA, payload)
			continue
		case 0xA:
			continue
		case 0x1, 0x0:
			msg = append(msg, payload...)
			if fin {
				return string(msg), nil
			}
		default:
			if fin {
				return string(payload), nil
			}
		}
	}
}

func glagolRandomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (g *glagolConn) send(payload map[string]any) error {
	id := glagolRandomID()
	env := map[string]any{
		"conversationToken": g.token,
		"id":                id,
		"payload":           payload,
		"sentTime":          time.Now().UnixMilli(),
	}
	b, _ := json.Marshal(env)
	if err := g.ws.writeFrame(0x1, b); err != nil {
		_ = g.ws.conn.Close()
		return err
	}
	return nil
}

func glagolSendCommand(payload map[string]any) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		glagolMu.Lock()
		c := glagolWS
		glagolMu.Unlock()

		if c == nil {
			if err := glagolInit(); err != nil {
				return err
			}
			continue
		}

		if err := c.send(payload); err == nil {
			return nil
		} else {
			lastErr = err
			glagolSetError(err)
			_ = c.ws.conn.Close()
			glagolMu.Lock()
			if glagolWS == c {
				glagolWS = nil
			}
			glagolMu.Unlock()
		}
	}
	if lastErr == nil {
		lastErr = errors.New("glagol РЅРµ РїРѕРґРєР»СЋС‡С‘РЅ")
	}
	return lastErr
}

// ---------- assets ----------

func startGlagolAssetsOnce() {
	glagolAssetsStartMu.Lock()
	defer glagolAssetsStartMu.Unlock()
	if glagolAssetsStarted {
		return
	}
	if startGlagolAssets() {
		glagolAssetsStarted = true
	}
}

func startGlagolAssets() bool {
	for p := 8091; p < 8100; p++ {
		addr := fmt.Sprintf("127.0.0.1:%d", p)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			continue
		}
		glagolAssetPort = p
		mux := http.NewServeMux()
		mux.HandleFunc("/led/", func(w http.ResponseWriter, r *http.Request) {
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/led/"), ".gif")
			glagolAssetsMu.Lock()
			a, ok := glagolAssets[id]
			glagolAssetsMu.Unlock()
			if !ok || time.Now().After(a.exp) {
				if ok {
					glagolAssetsMu.Lock()
					delete(glagolAssets, id)
					glagolAssetsMu.Unlock()
				}
				http.NotFound(w, r)
				return
			}
			glagolAssetsMu.Lock()
			glagolAssetHits++
			glagolAssetBytes = len(a.data)
			glagolAssetAt = time.Now()
			glagolAssetsMu.Unlock()
			w.Header().Set("Content-Type", "image/gif")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(a.data)
		})
		mux.HandleFunc("/audio/", func(w http.ResponseWriter, r *http.Request) {
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/audio/"), ".wav")
			glagolAssetsMu.Lock()
			a, ok := glagolAssets[id]
			glagolAssetsMu.Unlock()
			if !ok || time.Now().After(a.exp) {
				http.NotFound(w, r)
				return
			}
			glagolAssetsMu.Lock()
			glagolAssetHits++
			glagolAssetBytes = len(a.data)
			glagolAssetAt = time.Now()
			glagolAssetsMu.Unlock()
			w.Header().Set("Content-Type", "audio/wav")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Length", strconv.Itoa(len(a.data)))
			_, _ = w.Write(a.data)
		})
		go func() { _ = http.Serve(ln, mux) }()
		go func() {
			for {
				time.Sleep(time.Minute)
				now := time.Now()
				glagolAssetsMu.Lock()
				for k, v := range glagolAssets {
					if now.After(v.exp) {
						delete(glagolAssets, k)
					}
				}
				glagolAssetsMu.Unlock()
			}
		}()
		return true
	}
	return false
}

func glagolAudioURL(data []byte) (string, error) {
	glagolAssetsStartMu.Lock()
	if !glagolAssetsStarted {
		if startGlagolAssets() {
			glagolAssetsStarted = true
		}
	}
	port := glagolAssetPort
	glagolAssetsStartMu.Unlock()
	if !glagolAssetsStarted || port == 0 {
		return "", errors.New("asset server is not listening")
	}
	id := glagolRandomID()
	glagolAssetsMu.Lock()
	glagolAssets[id] = glagolAsset{data: data, exp: time.Now().Add(10 * time.Minute)}
	glagolAssetsMu.Unlock()
	return fmt.Sprintf("http://127.0.0.1:%d/audio/%s.wav", port, id), nil
}

func glagolAssetURL(data []byte) (string, error) {
	glagolAssetsStartMu.Lock()
	if !glagolAssetsStarted {
		if startGlagolAssets() {
			glagolAssetsStarted = true
		}
	}
	port := glagolAssetPort
	glagolAssetsStartMu.Unlock()
	if !glagolAssetsStarted || port == 0 {
		return "", errors.New("asset server is not listening")
	}
	id := glagolRandomID()
	glagolAssetsMu.Lock()
	glagolAssets[id] = glagolAsset{data: data, exp: time.Now().Add(10 * time.Minute)}
	for k, v := range glagolAssets {
		if time.Now().After(v.exp) {
			delete(glagolAssets, k)
		}
	}
	glagolAssetsMu.Unlock()
	return fmt.Sprintf("http://127.0.0.1:%d/led/%s.gif", port, id), nil
}

// ---------- commands ----------

func glagolDrawGIF(data []byte) error {
	url, err := glagolAssetURL(data)
	if err != nil {
		return err
	}
	return glagolDrawGIFURL(url)
}

func glagolDrawGIFURL(url string) error {
	payload := map[string]any{
		"animation_sequence": []any{
			map[string]any{"frontal_led_image": url},
		},
	}
	cmd := glagolExternalCommand("draw_led_screen", payload)
	return glagolSendCommand(cmd)
}

// glagolShowClock РїСЂРёРЅСѓРґРёС‚РµР»СЊРЅРѕ РІРѕР·РІСЂР°С‰Р°РµС‚ РјР°С‚СЂРёС†Сѓ Рє С€С‚Р°С‚РЅС‹Рј С‡Р°СЃР°Рј.
// hide_clock РіР°СЃРёС‚ СЌРєСЂР°РЅ, show_clock СЃСЂР°Р·Сѓ РїРµСЂРµСЂРёСЃРѕРІС‹РІР°РµС‚ С‡Р°СЃС‹ С‚РµРєСѓС‰РёРј РІСЂРµРјРµРЅРµРј.
func glagolShowClock() bool {
	if !glagolEnsureFast() {
		return false
	}
	_ = glagolSendCommand(glagolExternalCommand("hide_clock", "{}"))
	time.Sleep(700 * time.Millisecond)
	return glagolSendCommand(glagolExternalCommand("show_clock", "{}")) == nil
}

// glagolNativeClock РѕСЃС‚Р°РЅР°РІР»РёРІР°РµС‚ С‚РµРєСѓС‰СѓСЋ LED-Р°РЅРёРјР°С†РёСЋ РєРѕСЂРѕС‚РєРёРј РєРѕРЅРµС‡РЅС‹Рј
// С‡С‘СЂРЅС‹Рј GIF Рё С‚РѕР»СЊРєРѕ РїРѕС‚РѕРј РїРѕРєР°Р·С‹РІР°РµС‚ С‡Р°СЃС‹. Р‘РµР· СЌС‚РѕРіРѕ Р±РµСЃРєРѕРЅРµС‡РЅС‹Р№ GIF
// РїСЂРѕРґРѕР»Р¶Р°РµС‚ РёРіСЂР°С‚СЊ РїРѕРІРµСЂС… show_clock.
func glagolNativeClock() bool {
	if !glagolEnsureFast() {
		return false
	}
	if g, err := glagolStopGIF(); err == nil {
		if glagolDrawGIF(g) == nil {
			time.Sleep(500 * time.Millisecond)
		}
	}
	return glagolShowClock()
}

func glagolStopGIF() ([]byte, error) {
	pal := grayPalette()
	g := &gif.GIF{LoopCount: -1}
	seed := int(time.Now().UnixNano())
	for i := 0; i < 5; i++ {
		img := image.NewPaletted(image.Rect(0, 0, matrixW, matrixH), pal)
		for j := range img.Pix {
			img.Pix[j] = 1
		}
		img.Pix[(seed+i*37)%len(img.Pix)] = uint8(2 + (seed+i)%40)
		g.Image = append(g.Image, img)
		g.Delay = append(g.Delay, 2)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func glagolExternalCommand(name string, payload any) map[string]any {
	var s string
	switch v := payload.(type) {
	case string:
		s = v
	default:
		b, _ := json.Marshal(v)
		s = string(b)
	}
	data := appendVarintField(nil, 1, name)
	data = appendVarintField(data, 2, s)
	return map[string]any{
		"command": "externalCommandBypass",
		"data":    base64.StdEncoding.EncodeToString(data),
	}
}

func appendVarintField(b []byte, tag int, s string) []byte {
	b = append(b, byte(tag<<3|2))
	n := len(s)
	for n >= 0x80 {
		b = append(b, byte(n)|0x80)
		n >>= 7
	}
	b = append(b, byte(n))
	b = append(b, s...)
	return b
}

// ---------- matrix gifs ----------

func grayPalette() color.Palette {
	p := make(color.Palette, 256)
	for i := range p {
		p[i] = color.RGBA{uint8(i), uint8(i), uint8(i), 255}
	}
	return p
}

func glagolFrameGIF(frame []byte, duration, bright int) ([]byte, error) {
	if len(frame) < 25*16*3 {
		return nil, errors.New("bad frame")
	}
	pal := grayPalette()
	img := image.NewPaletted(image.Rect(0, 0, 25, 16), pal)
	for i := 0; i < 400; i++ {
		v := int(frame[i*3]) * bright / 100
		if v < 0 {
			v = 0
		}
		if v > 255 {
			v = 255
		}
		img.Pix[i] = uint8(v)
	}
	g := &gif.GIF{LoopCount: -1}
	g.Image = append(g.Image, img)
	g.Delay = append(g.Delay, 1)
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func glagolGIFDuration(data []byte) time.Duration {
	src, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		return 0
	}
	total := 0
	for _, d := range src.Delay {
		if d > 0 {
			total += d
		}
	}
	return time.Duration(total) * 10 * time.Millisecond
}

func glagolResizeGIF(data []byte, duration, bright int) ([]byte, error) {
	src, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if len(src.Image) == 0 {
		return nil, errors.New("empty gif")
	}
	pal := grayPalette()
	out := &gif.GIF{LoopCount: -1}
	total := 0
	limit := 0
	if duration > 0 {
		limit = duration * 100
	}
	for i, f := range src.Image {
		if i >= 600 {
			break
		}
		b := f.Bounds()
		img := image.NewPaletted(image.Rect(0, 0, 25, 16), pal)
		for y := 0; y < 16; y++ {
			for x := 0; x < 25; x++ {
				sx := b.Min.X + x*b.Dx()/25
				sy := b.Min.Y + y*b.Dy()/16
				r, g, bl, _ := f.At(sx, sy).RGBA()
				lum := int((299*r + 587*g + 114*bl) / 1000 >> 8)
				lum = lum * bright / 100
				if lum > 255 {
					lum = 255
				}
				img.Pix[y*25+x] = uint8(lum)
			}
		}
		d := 10
		if i < len(src.Delay) && src.Delay[i] > 0 {
			d = src.Delay[i]
		}
		out.Image = append(out.Image, img)
		out.Delay = append(out.Delay, d)
		total += d
		if limit > 0 && total >= limit {
			break
		}
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func glagolDrawFrame(frame []byte, duration, bright int) bool {
	if !glagolEnsureFast() {
		return false
	}
	g, err := glagolFrameGIF(frame, duration, bright)
	if err != nil {
		return false
	}
	return glagolDrawGIF(g) == nil
}

func glagolDrawUploadedGIF(data []byte, duration, bright int) bool {
	if !glagolEnsureFast() {
		return false
	}
	g, err := glagolResizeGIF(data, duration, bright)
	if err != nil {
		return false
	}
	return glagolDrawGIF(g) == nil
}

func glagolDrawVideo(frames [][]byte, fps, bright int) bool {
	if !glagolEnsureFast() || len(frames) == 0 {
		return false
	}
	if fps < 1 {
		fps = 12
	}
	if fps > 30 {
		fps = 30
	}
	delay := 100 / fps
	if delay < 2 {
		delay = 2
	}
	g, err := glagolEncodeFramesDelay(frames, delay, bright)
	if err != nil {
		return false
	}
	return glagolDrawGIF(g) == nil
}

func glagolEncodeFrames(frames [][]byte, bright int) ([]byte, error) {
	return glagolEncodeFramesDelay(frames, 2, bright)
}

func glagolEncodeFramesDelay(frames [][]byte, delay, bright int) ([]byte, error) {
	if len(frames) == 0 {
		return nil, errors.New("no frames")
	}
	if delay < 2 {
		delay = 2
	}
	pal := grayPalette()
	g := &gif.GIF{LoopCount: -1}
	for _, f := range frames {
		if len(f) < matrixW*matrixH*3 {
			return nil, errors.New("bad frame")
		}
		img := image.NewPaletted(image.Rect(0, 0, matrixW, matrixH), pal)
		for i := 0; i < matrixW*matrixH; i++ {
			v := int(f[i*3]) * bright / 100
			if v < 0 {
				v = 0
			}
			if v > 255 {
				v = 255
			}
			img.Pix[i] = uint8(v)
		}
		g.Image = append(g.Image, img)
		g.Delay = append(g.Delay, delay)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func glagolTextGIFDuration(text string, speed time.Duration) time.Duration {
	mask := renderTextMask(text)
	if mask.width <= matrixW {
		return 2 * time.Second
	}
	if speed <= 0 {
		speed = 60 * time.Millisecond
	}
	if speed < 10*time.Millisecond {
		speed = 10 * time.Millisecond
	}
	if speed > 200*time.Millisecond {
		speed = 200 * time.Millisecond
	}
	step := 20.0 / float64(speed/time.Millisecond)
	cycleFrames := int(math.Ceil(float64(matrixW+mask.width) / step))
	if cycleFrames < 2 {
		cycleFrames = 2
	}
	return time.Duration(cycleFrames) * 20 * time.Millisecond
}

func glagolTextGIF(text string, speed time.Duration, duration, bright int) ([]byte, error) {
	mask := renderTextMask(text)
	if mask.width < 1 {
		return nil, errors.New("empty text")
	}
	white := [3]int{255, 255, 255}
	y0 := (matrixH - 7) / 2

	if mask.width <= matrixW {
		frame := matrixBlank()
		drawTextMask(frame, mask, (matrixW-mask.width)/2, y0, white)
		n := 100
		if duration > 0 {
			n = duration * 50
		}
		if n < 2 {
			n = 2
		}
		if n > 3000 {
			n = 3000
		}
		frames := make([][]byte, n)
		for i := range frames {
			frames[i] = frame
		}
		return glagolEncodeFrames(frames, bright)
	}

	if speed <= 0 {
		speed = 60 * time.Millisecond
	}
	if speed < 10*time.Millisecond {
		speed = 10 * time.Millisecond
	}
	if speed > 200*time.Millisecond {
		speed = 200 * time.Millisecond
	}
	step := 20.0 / float64(speed/time.Millisecond)
	cycleFrames := int(math.Ceil(float64(matrixW+mask.width) / step))
	if cycleFrames < 2 {
		cycleFrames = 2
	}
	cycle := make([][]byte, 0, cycleFrames)
	pos := float64(matrixW)
	for i := 0; i < cycleFrames; i++ {
		frame := matrixBlank()
		drawTextMaskSub(frame, mask, pos, y0, white)
		cycle = append(cycle, frame)
		pos -= step
	}

	want := cycleFrames
	if duration > 0 {
		want = duration * 50
	}
	if want < cycleFrames {
		want = cycleFrames
	}
	if want > 3000 {
		want = 3000
	}
	frames := make([][]byte, 0, want)
	for len(frames) < want {
		frames = append(frames, cycle[len(frames)%cycleFrames])
	}
	return glagolEncodeFrames(frames, bright)
}

func glagolDrawText(text string, speed time.Duration, duration, bright int) bool {
	if !glagolEnsureFast() {
		return false
	}
	g, err := glagolTextGIF(text, speed, duration, bright)
	if err != nil {
		return false
	}
	return glagolDrawGIF(g) == nil
}
