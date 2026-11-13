package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- секреты ----------

// Секреты (сессии, кэш Glagol, события) лежат в отдельном каталоге 0700,
// а не в общем /data/local/tmp. Старые пути читаются для миграции.
const (
	privateDir     = "/data/local/tmp/station-web/private"
	legacyTmpDir   = "/data/local/tmp"
	sessionFile    = "sessions.json"
	eventsFile     = "events.jsonl"
	glagolFile     = "glagol.json"
	sessionLegacy  = "/data/local/tmp/station-web.sessions"
	eventsLegacy   = "/data/local/tmp/station-web-events.jsonl"
	glagolLegacy   = "/data/local/tmp/station-web.glagol"
	eventsMaxBytes = 512 * 1024
)

func ensurePrivateDir() {
	_ = os.MkdirAll(privateDir, 0700)
	_ = os.Chmod(privateDir, 0700)
}

func privatePath(name string) string {
	return filepath.Join(privateDir, name)
}

// readSecret читает файл из приватного каталога, а при первом запуске
// после обновления — со старого места, перенося данные.
func readSecret(name, legacy string) ([]byte, error) {
	ensurePrivateDir()
	if b, err := os.ReadFile(privatePath(name)); err == nil {
		return b, nil
	}
	if legacy != "" {
		if b, err := os.ReadFile(legacy); err == nil {
			_ = writeSecret(name, b)
			_ = os.Remove(legacy)
			return b, nil
		}
	}
	return nil, os.ErrNotExist
}

func writeSecret(name string, data []byte) error {
	ensurePrivateDir()
	tmp := privatePath(name) + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	_ = os.Chmod(tmp, 0600)
	return os.Rename(tmp, privatePath(name))
}

// appendSecretLine дописывает строку в файл секретов, обрезая его при росте.
func appendSecretLine(name, legacy string, line []byte, max int64) {
	ensurePrivateDir()
	path := privatePath(name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		// первый запуск: переносим старый файл, если он есть
		if legacy != "" {
			if b, rerr := os.ReadFile(legacy); rerr == nil {
				_ = writeSecret(name, append(b, line...))
				_ = os.Remove(legacy)
				return
			}
		}
		return
	}
	defer f.Close()
	_, _ = f.Write(line)
	if st, err := f.Stat(); err == nil && max > 0 && st.Size() > max {
		if data, err := os.ReadFile(path); err == nil {
			lines := strings.Split(string(data), "\n")
			if len(lines) > 2000 {
				lines = lines[len(lines)-2000:]
			}
			_ = writeSecret(name, []byte(strings.Join(lines, "\n")))
		}
	}
}

// ---------- пароль ----------

const (
	pbkdf2Iterations = 120000
	pbkdf2KeyLen     = 32
	pbkdf2Prefix     = "pbkdf2-sha256$"
)

func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	blocks := (keyLen + hashLen - 1) / hashLen
	dk := make([]byte, 0, blocks*hashLen)
	u := make([]byte, hashLen)
	var idx [4]byte
	for block := 1; block <= blocks; block++ {
		prf.Reset()
		_, _ = prf.Write(salt)
		binary.BigEndian.PutUint32(idx[:], uint32(block))
		_, _ = prf.Write(idx[:])
		u = prf.Sum(u[:0])
		t := make([]byte, hashLen)
		copy(t, u)
		for n := 2; n <= iter; n++ {
			prf.Reset()
			_, _ = prf.Write(u)
			u = prf.Sum(u[:0])
			for i := range t {
				t[i] ^= u[i]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:keyLen]
}

func hashPassword(pass string) string {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		// крайне маловероятно; лучше упасть, чем сохранить пустую соль
		salt = []byte(strconv.FormatInt(time.Now().UnixNano(), 10))
	}
	dk := pbkdf2SHA256([]byte(pass), salt, pbkdf2Iterations, pbkdf2KeyLen)
	return pbkdf2Prefix + strconv.Itoa(pbkdf2Iterations) + "$" +
		base64.RawStdEncoding.EncodeToString(salt) + "$" +
		base64.RawStdEncoding.EncodeToString(dk)
}

// verifyPassword понимает и хэш, и старый открытый пароль из конфига.
func verifyPassword(pass, stored string) bool {
	if stored == "" {
		return false
	}
	if !strings.HasPrefix(stored, pbkdf2Prefix) {
		return subtle.ConstantTimeCompare([]byte(pass), []byte(stored)) == 1
	}
	rest := strings.TrimPrefix(stored, pbkdf2Prefix)
	parts := strings.Split(rest, "$")
	if len(parts) != 3 {
		return false
	}
	iter, err := strconv.Atoi(parts[0])
	if err != nil || iter < 1000 || iter > 5000000 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil || len(want) == 0 {
		return false
	}
	got := pbkdf2SHA256([]byte(pass), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ---------- защита от перебора ----------

type loginFail struct {
	count int
	last  time.Time
	until time.Time
}

var (
	loginGuardMu sync.Mutex
	loginFails   = map[string]*loginFail{}
)

const (
	loginMaxFails = 5
	loginWindow   = 10 * time.Minute
	loginBlock    = 5 * time.Minute
)

func clientKey(r *http.Request) string {
	host, _, err := netSplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func netSplitHostPort(addr string) (string, string, error) {
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return strings.Trim(addr[:i], "[]"), addr[i+1:], nil
	}
	return addr, "", fmt.Errorf("no port")
}

// loginAllowed возвращает false и остаток блокировки, если IP перебрал пароли.
func loginAllowed(r *http.Request) (bool, time.Duration) {
	key := clientKey(r)
	loginGuardMu.Lock()
	defer loginGuardMu.Unlock()
	f, ok := loginFails[key]
	if !ok {
		return true, 0
	}
	now := time.Now()
	if now.Before(f.until) {
		return false, f.until.Sub(now)
	}
	if now.Sub(f.last) > loginWindow {
		delete(loginFails, key)
	}
	return true, 0
}

func loginFailed(r *http.Request) {
	key := clientKey(r)
	loginGuardMu.Lock()
	defer loginGuardMu.Unlock()
	f := loginFails[key]
	if f == nil {
		f = &loginFail{}
		loginFails[key] = f
	}
	f.count++
	f.last = time.Now()
	if f.count >= loginMaxFails {
		f.until = time.Now().Add(loginBlock)
		f.count = 0
	}
	// не даём карте расти бесконечно
	if len(loginFails) > 512 {
		cut := time.Now().Add(-loginWindow)
		for k, v := range loginFails {
			if v.last.Before(cut) && v.until.Before(time.Now()) {
				delete(loginFails, k)
			}
		}
	}
}

func loginSucceeded(r *http.Request) {
	key := clientKey(r)
	loginGuardMu.Lock()
	delete(loginFails, key)
	loginGuardMu.Unlock()
}

// ---------- Origin / CSRF ----------

// sameOriginOrNoOrigin пропускает GET/HEAD и запросы без Origin/Referer
// (curl, скрипты), но отклоняет межсайтовые POST из браузера.
func sameOriginOrNoOrigin(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	ref := r.Header.Get("Origin")
	if ref == "" {
		ref = r.Referer()
	}
	if ref == "" || ref == "null" {
		return ref == ""
	}
	u, err := url.Parse(ref)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func securityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "SAMEORIGIN")
	h.Set("Referrer-Policy", "same-origin")
}
