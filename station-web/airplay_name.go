package main

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

type smartHomeInfo struct {
	Name      string `json:"name"`
	Room      string `json:"room"`
	Household string `json:"household"`
	Display   string `json:"display"`
}

var (
	smartHomeMu  sync.Mutex
	smartHomeAt  time.Time
	smartHomeVal smartHomeInfo
	smartHomeErr error
)

const (
	airplayNameModeSetting   = "station_airplay_name_mode"
	airplayNameCustomSetting = "station_airplay_name_custom"
	airplayVideoNameSetting  = "station_airplay_video_name"
	airplayVideoPkg          = "io.github.jqssun.airplay"
	airplayVideoReceiver     = airplayVideoPkg + "/.SettingsReceiver"
	airplayVideoAction       = airplayVideoPkg + ".SET_SETTING"
	airplayVideoSuffix       = " · Видео"
	airplayNameMax           = 30
)

func smartHomeFetch() (smartHomeInfo, error) {
	id, platform := glagolDeviceInfo()
	if id == "" {
		return smartHomeInfo{}, errors.New("не удалось определить ID станции")
	}
	fullID := id
	if platform != "" {
		fullID = id + "." + platform
	}
	res, err := btHelper(8*time.Second, "iotname", fullID)
	if err != nil {
		return smartHomeInfo{}, err
	}
	if e, ok := res["error"].(string); ok && e != "" {
		return smartHomeInfo{}, errors.New(e)
	}
	name := strings.TrimSpace(fmt.Sprint(res["name"]))
	room := strings.TrimSpace(fmt.Sprint(res["room"]))
	house := strings.TrimSpace(fmt.Sprint(res["house"]))
	if name == "" || strings.EqualFold(name, "<nil>") {
		return smartHomeInfo{}, errors.New("в Умном доме не найдено имя станции")
	}
	display := name
	if room != "" {
		display = name + " (" + room + ")"
	}
	return smartHomeInfo{Name: name, Room: room, Household: house, Display: display}, nil
}

func smartHomeLookup() (smartHomeInfo, error) {
	smartHomeMu.Lock()
	defer smartHomeMu.Unlock()
	if !smartHomeAt.IsZero() {
		ttl := 60 * time.Second
		if smartHomeErr != nil {
			ttl = 30 * time.Second
		}
		if time.Since(smartHomeAt) < ttl {
			return smartHomeVal, smartHomeErr
		}
	}
	smartHomeVal, smartHomeErr = smartHomeFetch()
	smartHomeAt = time.Now()
	return smartHomeVal, smartHomeErr
}

// airplayShortName укладывает имя в строку списка AirPlay: убирает бренд
// «Яндекс» (он занимает место) и ограничивает длину. Комната добавляется,
// только если имя целиком помещается.
func airplayShortName(name, room string) string {
	n := strings.TrimSpace(name)
	for _, prefix := range []string{"яндекс ", "yandex "} {
		if len(n) > len(prefix) && strings.EqualFold(n[:len(prefix)], prefix) {
			n = strings.TrimSpace(n[len(prefix):])
			break
		}
	}
	full := n
	if room != "" {
		full = n + " (" + strings.TrimSpace(room) + ")"
	}
	if len([]rune(full)) > airplayNameMax {
		full = n
	}
	if r := []rune(full); len(r) > airplayNameMax {
		full = string(r[:airplayNameMax])
	}
	return full
}

func airplayNameSettingGet(key string) string {
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

func airplayNameSettingPut(key, value string) error {
	out, code := run(4*time.Second, "/system/bin/settings", "put", "global", key, value)
	if code != 0 {
		return fmt.Errorf("settings: %s", strings.TrimSpace(out))
	}
	return nil
}

func airplayNameState() map[string]any {
	mode := strings.ToLower(strings.TrimSpace(airplayNameSettingGet(airplayNameModeSetting)))
	if mode != "custom" {
		mode = "smart"
	}
	custom := airplayNameSettingGet(airplayNameCustomSetting)
	smart, err := smartHomeLookup()
	display := airplayShortName(smart.Name, smart.Room)
	current := airplayRead().Name
	if display == "" {
		display = current
	}
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	return map[string]any{
		"mode":    mode,
		"custom":  custom,
		"current": current,
		"video":   airplayNameSettingGet(airplayVideoNameSetting),
		"smart": map[string]any{
			"name":      smart.Name,
			"room":      smart.Room,
			"household": smart.Household,
			"display":   display,
			"full":      smart.Display,
		},
		"error": errText,
	}
}

func airplayNameSave(mode, custom string) (map[string]any, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	custom = strings.TrimSpace(custom)
	if mode != "smart" && mode != "custom" {
		return nil, errors.New("неизвестный режим имени")
	}
	value := ""
	if mode == "smart" {
		smart, err := smartHomeLookup()
		if err != nil {
			return nil, err
		}
		value = airplayShortName(smart.Name, smart.Room)
		if value == "" {
			return nil, errors.New("Умный дом не вернул имя станции")
		}
	} else {
		if custom == "" {
			return nil, errors.New("введите своё название")
		}
		if len([]rune(custom)) > airplayNameMax {
			return nil, errors.New("название длиннее 30 символов — не влезет в список AirPlay")
		}
		value = custom
	}
	if err := airplaySetLocked("server_name", value); err != nil {
		return nil, err
	}
	time.Sleep(1200 * time.Millisecond)
	if airplayRead().Name != value {
		// Some Shairport builds store the advertised name under "name".
		if err := airplaySetLocked("name", value); err != nil {
			return nil, err
		}
		time.Sleep(800 * time.Millisecond)
	}
	if err := airplayNameSettingPut(airplayNameModeSetting, mode); err != nil {
		return nil, err
	}
	if err := airplayNameSettingPut(airplayNameCustomSetting, custom); err != nil {
		return nil, err
	}
	airplayRefresh()
	_ = airplayVideoNameApply()
	return airplayNameState(), nil
}

// Имя видео-приёмника повторяет выбранное имя и добавляет « · Видео»,
// чтобы аудио и видео различались в списке AirPlay.
func airplayVideoNameDesired() (string, error) {
	base := ""
	mode := strings.ToLower(strings.TrimSpace(airplayNameSettingGet(airplayNameModeSetting)))
	if mode == "custom" {
		base = strings.TrimSpace(airplayNameSettingGet(airplayNameCustomSetting))
	}
	if base == "" {
		if smart, err := smartHomeLookup(); err == nil {
			// сначала пробуем имя с комнатой; если с суффиксом не влезает,
			// оставляем только имя — комнату теряем в последнюю очередь
			base = airplayShortName(smart.Name, smart.Room)
			if len([]rune(base))+len([]rune(airplayVideoSuffix)) > airplayNameMax {
				base = airplayShortName(smart.Name, "")
			}
		}
	}
	if base == "" {
		base = airplayRead().Name
	}
	if base == "" {
		return "", errors.New("нет имени для видео-приёмника")
	}
	name := base
	suffixRunes := len([]rune(airplayVideoSuffix))
	if r := []rune(name); len(r) > airplayNameMax-suffixRunes {
		name = string(r[:airplayNameMax-suffixRunes])
	}
	name += airplayVideoSuffix
	return name, nil
}

func airplayVideoDebugOverlayApply() {
	out, code := run(6*time.Second, "/system/bin/am", "broadcast",
		"--include-stopped-packages",
		"-n", airplayVideoReceiver,
		"-a", airplayVideoAction,
		"--es", "key", "debug_enabled",
		"--es", "value", "false")
	if code != 0 {
		log.Printf("airplay video debug overlay: %s", strings.TrimSpace(out))
		return
	}
	if strings.Contains(out, "data=\"invalid") || strings.Contains(out, "data=\"unknown") {
		log.Printf("airplay video debug overlay: %s", strings.TrimSpace(out))
		return
	}
	log.Printf("airplay video debug overlay: off")
}

func airplayVideoNameApply() error {
	name, err := airplayVideoNameDesired()
	if err != nil {
		return err
	}
	out, code := run(6*time.Second, "/system/bin/am", "broadcast",
		"--include-stopped-packages",
		"-n", airplayVideoReceiver,
		"-a", airplayVideoAction,
		"--es", "key", "server_name",
		"--es", "value", name)
	if code != 0 {
		return fmt.Errorf("airplay video name: %s", strings.TrimSpace(out))
	}
	if strings.Contains(out, "data=\"invalid") || strings.Contains(out, "data=\"unknown") {
		return fmt.Errorf("airplay video name: %s", strings.TrimSpace(out))
	}
	_ = airplayNameSettingPut(airplayVideoNameSetting, name)
	return nil
}

// Имя видео-приёмника применяется при старте панели и при сохранении имени.
// Периодически переотправлять нельзя: receiver перезапускает сервер на любое
// изменение server_name, даже совпадающее, и рвал бы соединения.
// В режиме «Умный дом» имя следует за станцией: если в приложении назначили
// комнату или переименовали станцию, приёмник подхватит это без ручного
// сохранения. Активную сессию не трогаем.
func airplayNameWatch() {
	time.Sleep(45 * time.Second)
	for {
		mode := strings.ToLower(strings.TrimSpace(airplayNameSettingGet(airplayNameModeSetting)))
		if mode == "smart" {
			if smart, err := smartHomeLookup(); err == nil && smart.Display != "" {
				target := airplayShortName(smart.Name, smart.Room)
				info := airplayRead()
				if !info.Connected && !info.Session && !info.Playing && info.Name != target {
					if err := airplaySetLocked("server_name", target); err != nil {
						log.Printf("airplay name: %v", err)
					} else {
						log.Printf("airplay name: %q", target)
					}
				}
			}
		}
		if video, err := airplayVideoNameDesired(); err == nil && video != airplayNameSettingGet(airplayVideoNameSetting) {
			if err := airplayVideoNameApply(); err != nil {
				log.Printf("airplay video name: %v", err)
			}
		}
		time.Sleep(60 * time.Second)
	}
}
