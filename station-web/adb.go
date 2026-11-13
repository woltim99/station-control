package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ADB-over-TCP можно выключить из панели. Желаемое состояние хранится в
// settings global station_adb_enabled (1 — включён, 0 — выключен) и
// применяется adbguard'ом и сразу обработчиком.

func adbSettingGet(key string) string {
	out, code := run(3*time.Second, "/system/bin/settings", "get", "global", key)
	if code != 0 {
		return ""
	}
	v := strings.TrimSpace(out)
	if strings.EqualFold(v, "null") {
		return ""
	}
	return v
}

func adbAllowed() bool {
	return adbSettingGet("station_adb_enabled") != "0"
}

func adbServiceRunning() bool {
	out, _ := run(2*time.Second, "/system/bin/getprop", "init.svc.adbd")
	return strings.TrimSpace(out) == "running"
}

func adbState() map[string]any {
	return map[string]any{
		"allowed": adbAllowed(),
		"enabled": adbSettingGet("adb_enabled") == "1",
		"running": adbServiceRunning(),
	}
}

func adbApply(on bool) error {
	val := "0"
	if on {
		val = "1"
	}
	if out, code := run(4*time.Second, "/system/bin/settings", "put", "global", "station_adb_enabled", val); code != 0 {
		return fmt.Errorf("settings: %s", strings.TrimSpace(out))
	}
	if on {
		_, _ = run(3*time.Second, "/system/bin/settings", "put", "global", "adb_enabled", "1")
		_, _ = run(3*time.Second, "/system/bin/settings", "put", "global", "development_settings_enabled", "1")
		_, _ = run(3*time.Second, "/system/bin/setprop", "service.adb.tcp.port", "5555")
	} else {
		_, _ = run(3*time.Second, "/system/bin/settings", "put", "global", "adb_enabled", "0")
	}
	return nil
}

func registerADBHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/api/adb", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, adbState())
		case http.MethodPost:
			var req struct {
				Enabled *bool `json:"enabled"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil || req.Enabled == nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
				return
			}
			if err := adbApply(*req.Enabled); err != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
				return
			}
			invalidateStatusCache()
			addEvent("info", "ADB по сети", map[bool]string{true: "включён", false: "выключен"}[*req.Enabled])
			time.Sleep(1500 * time.Millisecond)
			writeJSON(w, http.StatusOK, adbState())
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET or POST"})
		}
	})
}
