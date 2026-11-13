package main

import (
	"encoding/hex"
	"net/http"
	"testing"
	"time"
)

func TestPBKDF2Vector(t *testing.T) {
	got := pbkdf2SHA256([]byte("password"), []byte("salt"), 1, 32)
	want := "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"
	if hex.EncodeToString(got) != want {
		t.Fatalf("pbkdf2 = %x, want %s", got, want)
	}
}

func TestPasswordRoundTrip(t *testing.T) {
	h := hashPassword("s3cret-пароль")
	if !verifyPassword("s3cret-пароль", h) {
		t.Fatal("valid password rejected")
	}
	if verifyPassword("s3cret-пароль2", h) {
		t.Fatal("wrong password accepted")
	}
	if verifyPassword("", h) {
		t.Fatal("empty password accepted")
	}
	if verifyPassword("x", "") {
		t.Fatal("empty hash accepted")
	}
	if verifyPassword("x", "pbkdf2-sha256$bad") {
		t.Fatal("malformed hash accepted")
	}
}

func TestLegacyPlaintextPassword(t *testing.T) {
	if !verifyPassword("admin", "admin") {
		t.Fatal("legacy plaintext rejected")
	}
	if verifyPassword("admin2", "admin") {
		t.Fatal("legacy plaintext accepted wrong password")
	}
}

func TestLoginLockout(t *testing.T) {
	loginGuardMu.Lock()
	loginFails = map[string]*loginFail{}
	loginGuardMu.Unlock()

	r, _ := http.NewRequest("POST", "http://x/login", nil)
	r.RemoteAddr = "10.0.0.5:1234"
	other, _ := http.NewRequest("POST", "http://x/login", nil)
	other.RemoteAddr = "10.0.0.6:9999"

	for i := 0; i < loginMaxFails; i++ {
		if ok, _ := loginAllowed(r); !ok {
			t.Fatalf("blocked too early at attempt %d", i+1)
		}
		loginFailed(r)
	}
	if ok, wait := loginAllowed(r); ok || wait <= 0 {
		t.Fatalf("expected lockout, got ok=%v wait=%v", ok, wait)
	}
	if ok, _ := loginAllowed(other); !ok {
		t.Fatal("lockout leaked to another IP")
	}
	loginSucceeded(r)
	if ok, _ := loginAllowed(r); !ok {
		t.Fatal("successful login did not clear lockout")
	}
}

func TestSameOrigin(t *testing.T) {
	mk := func(method, origin string) *http.Request {
		r, _ := http.NewRequest(method, "http://127.0.0.1:8080/api/status", nil)
		r.Host = "127.0.0.1:8080"
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return r
	}
	cases := []struct {
		name string
		r    *http.Request
		want bool
	}{
		{"get", mk("GET", "http://evil"), true},
		{"same origin", mk("POST", "http://127.0.0.1:8080"), true},
		{"foreign origin", mk("POST", "http://evil.example"), false},
		{"no origin", mk("POST", ""), true},
		{"null origin", mk("POST", "null"), false},
		{"referer only", func() *http.Request {
			r := mk("POST", "")
			r.Header.Set("Referer", "http://127.0.0.1:8080/index.html")
			return r
		}(), true},
	}
	for _, c := range cases {
		if got := sameOriginOrNoOrigin(c.r); got != c.want {
			t.Fatalf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestLockoutExpires(t *testing.T) {
	loginGuardMu.Lock()
	loginFails = map[string]*loginFail{"1.2.3.4": {until: time.Now().Add(-time.Second), last: time.Now().Add(-time.Hour)}}
	loginGuardMu.Unlock()
	r, _ := http.NewRequest("POST", "http://x/login", nil)
	r.RemoteAddr = "1.2.3.4:1"
	if ok, _ := loginAllowed(r); !ok {
		t.Fatal("expired lockout still blocks")
	}
}
