package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const wpaCtrlPath = "/data/vendor/wifi/wpa/sockets/wlan0"

const wifiDexPath = "/data/local/tmp/station-web/wifi.dex"

// wifiHelper запускает WifiHelper из dex через app_process: операции идут
// через WifiManager, и фреймворк сам знает о сети (DHCP, выбор сети, сохранение).
func wifiHelper(timeout time.Duration, args ...string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	full := append([]string{"/system/bin", "WifiHelper"}, args...)
	cmd := exec.CommandContext(ctx, "/system/bin/app_process", full...)
	cmd.Env = append(os.Environ(), "CLASSPATH="+wifiDexPath)
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
	return nil, errors.New("wifi helper: пустой ответ")
}

func wifiHelperError(res map[string]any, fallback string) error {
	msg := strings.TrimSpace(fmt.Sprint(res["error"]))
	if msg != "" && msg != "<nil>" {
		return errors.New(msg)
	}
	return errors.New(fallback)
}

var wifiScanMu sync.Mutex

type wpaStatusInfo struct {
	SSID  string
	State string
	IP    string
	BSSID string
	Freq  int
}

type wpaSavedNetwork struct {
	ID    int
	SSID  string
	Flags string
}

type wifiScanNetwork struct {
	SSID      string `json:"ssid"`
	Signal    int    `json:"signal"`
	Security  string `json:"security"`
	Frequency int    `json:"frequency"`
	Saved     bool   `json:"saved"`
}

// wpaRequest отправляет команду в control-сокет wpa_supplicant.
// Ответ дочитывается до паузы: простые команды возвращают OK,
// многострочные (STATUS, SCAN_RESULTS) — строки и пустую строку.
// wpaDial открывает control-сокет. На Android он обычно SOCK_DGRAM,
// поэтому сначала пробуем unixgram с локальным абстрактным адресом,
// затем — обычный unix-поток.
func wpaDialTo(path string) (net.Conn, error) {
	name := fmt.Sprintf("@stationweb-wpa-%d-%d", os.Getpid(), time.Now().UnixNano())
	conn, err := net.DialUnix("unixgram",
		&net.UnixAddr{Name: name, Net: "unixgram"},
		&net.UnixAddr{Name: path, Net: "unixgram"},
	)
	if err == nil {
		return conn, nil
	}
	return net.DialTimeout("unix", path, 2*time.Second)
}

func wpaDial() (net.Conn, error) {
	return wpaDialTo(wpaCtrlPath)
}

func wpaRequestOn(path, cmd string) (string, error) {
	conn, err := wpaDialTo(path)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(cmd)); err != nil {
		return "", err
	}
	var b strings.Builder
	buf := make([]byte, 8192)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(1200 * time.Millisecond))
		n, err := conn.Read(buf)
		if n > 0 {
			b.Write(buf[:n])
		}
		if err != nil {
			break
		}
		s := b.String()
		trimmed := strings.TrimSpace(s)
		if trimmed == "OK" || strings.HasPrefix(trimmed, "FAIL") || strings.HasSuffix(s, "\n\n") {
			break
		}
	}
	out := b.String()
	trimmed := strings.TrimSpace(out)
	if strings.HasPrefix(trimmed, "FAIL") {
		return out, errors.New(trimmed)
	}
	if trimmed == "" {
		return out, errors.New("wpa_supplicant: пустой ответ")
	}
	return out, nil
}

func wpaRequest(cmd string) (string, error) {
	return wpaRequestOn(wpaCtrlPath, cmd)
}

func parseWPAStatus(out string) wpaStatusInfo {
	var st wpaStatusInfo
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "ssid":
			st.SSID = v
		case "wpa_state":
			st.State = v
		case "ip_address":
			st.IP = v
		case "bssid":
			st.BSSID = v
		case "freq":
			st.Freq, _ = strconv.Atoi(v)
		}
	}
	return st
}

func wpaStatus() (wpaStatusInfo, error) {
	out, err := wpaRequest("STATUS")
	if err != nil {
		return wpaStatusInfo{}, err
	}
	return parseWPAStatus(out), nil
}

func wpaSignal() int {
	if out, err := wpaRequest("SIGNAL_POLL"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
			if ok && k == "RSSI" {
				if n, err := strconv.Atoi(v); err == nil {
					return n
				}
			}
		}
	}
	// Fallback: iw link.
	if out, code := run(2*time.Second, "/system/bin/iw", "dev", "wlan0", "link"); code == 0 {
		for _, line := range strings.Split(out, "\n") {
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "signal:") {
				fields := strings.Fields(t)
				if len(fields) > 1 {
					if v, err := strconv.ParseFloat(fields[1], 64); err == nil {
						return int(v)
					}
				}
			}
		}
	}
	return 0
}

func wpaListNetworks() ([]wpaSavedNetwork, error) {
	out, err := wpaRequest("LIST_NETWORKS")
	if err != nil {
		return nil, err
	}
	var list []wpaSavedNetwork
	for _, line := range strings.Split(out, "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), "\t", 4)
		if len(fields) < 4 {
			continue
		}
		id, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil {
			continue
		}
		list = append(list, wpaSavedNetwork{ID: id, SSID: fields[1], Flags: fields[3]})
	}
	return list, nil
}

func wifiSecurityFromFlags(flags string) string {
	upper := strings.ToUpper(flags)
	switch {
	case strings.Contains(upper, "WPA3"):
		return "wpa3"
	case strings.Contains(upper, "WPA2"):
		return "wpa2"
	case strings.Contains(upper, "WPA"):
		return "wpa"
	case strings.Contains(upper, "WEP"):
		return "wep"
	case strings.Contains(upper, "802.1X"):
		return "enterprise"
	default:
		return "open"
	}
}

func wifiScan() ([]wifiScanNetwork, error) {
	wifiScanMu.Lock()
	defer wifiScanMu.Unlock()

	saved := map[string]bool{}
	for _, ssid := range wifiSavedSSIDs() {
		saved[ssid] = true
	}

	// SCAN может ответить FAIL, если скан уже идёт — это не ошибка.
	_, _ = wpaRequest("SCAN")
	time.Sleep(3500 * time.Millisecond)
	out, err := wpaRequest("SCAN_RESULTS")
	if err != nil {
		return nil, err
	}

	best := map[string]wifiScanNetwork{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), "\t", 5)
		if len(fields) < 5 {
			continue
		}
		signal, err := strconv.Atoi(fields[2])
		if err != nil {
			continue
		}
		freq, _ := strconv.Atoi(fields[1])
		ssid := fields[4]
		if ssid == "" {
			continue
		}
		n := wifiScanNetwork{
			SSID: ssid, Signal: signal, Security: wifiSecurityFromFlags(fields[3]),
			Frequency: freq, Saved: saved[ssid],
		}
		if old, ok := best[ssid]; !ok || n.Signal > old.Signal {
			best[ssid] = n
		}
	}

	list := make([]wifiScanNetwork, 0, len(best))
	for _, n := range best {
		list = append(list, n)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Saved != list[j].Saved {
			return list[i].Saved
		}
		return list[i].Signal > list[j].Signal
	})
	return list, nil
}

func wpaQuote(s string) (string, error) {
	if strings.ContainsAny(s, "\r\n") {
		return "", errors.New("недопустимые символы")
	}
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s, nil
}

func wifiConnect(ssid, password, security string) (bool, error) {
	if ssid == "" || len([]byte(ssid)) > 32 {
		return false, errors.New("SSID: от 1 до 32 байт")
	}
	security = strings.ToLower(strings.TrimSpace(security))
	if security == "" {
		security = "wpa2"
	}
	switch security {
	case "open":
		password = ""
	case "wpa", "wpa2", "wpa3":
	case "wep", "enterprise":
		return false, errors.New("этот тип защиты пока не поддерживается")
	default:
		security = "wpa2"
	}
	qSSID, err := wpaQuote(ssid)
	if err != nil {
		return false, err
	}

	id := -1
	existing := false
	if list, err := wpaListNetworks(); err == nil {
		for _, n := range list {
			if n.SSID == ssid {
				id = n.ID
				existing = true
				break
			}
		}
	}
	if password != "" && (len(password) < 8 || len(password) > 63) {
		return false, errors.New("пароль WPA: от 8 до 63 символов")
	}
	if password == "" && security != "open" && !existing {
		return false, errors.New("нужен пароль")
	}

	// Основной путь — через WifiManager (dex-хелпер): сеть добавляется
	// в конфиг фреймворка, он сам делает DHCP и не откатывается на другую сеть.
	if password == "" && security != "open" {
		if res, err := wifiHelper(26*time.Second, "connect-saved", ssid); err == nil {
			if ok, _ := res["ok"].(bool); ok {
				return true, nil
			}
			return false, wifiHelperError(res, "не удалось подключиться")
		}
	}
	if res, err := wifiHelper(26*time.Second, "connect", ssid, password, security); err == nil {
		if ok, _ := res["ok"].(bool); ok {
			return true, nil
		}
		return false, wifiHelperError(res, "не удалось подключиться")
	}
	// Хелпер недоступен (нет dex) — старый путь через wpa_supplicant.

	if id < 0 {
		out, err := wpaRequest("ADD_NETWORK")
		if err != nil {
			return false, err
		}
		fields := strings.Fields(out)
		if len(fields) == 0 {
			return false, errors.New("wpa_supplicant не выдал id сети")
		}
		id, err = strconv.Atoi(fields[0])
		if err != nil {
			return false, fmt.Errorf("ADD_NETWORK: %s", strings.TrimSpace(out))
		}
	}

	// Для сохранённой сети без нового пароля оставляем её конфиг как есть.
	if !existing || password != "" || security == "open" {
		if _, err := wpaRequest(fmt.Sprintf(`SET_NETWORK %d ssid "%s"`, id, qSSID)); err != nil {
			return false, err
		}
	}
	switch security {
	case "open":
		_, _ = wpaRequest(fmt.Sprintf("SET_NETWORK %d key_mgmt NONE", id))
	case "wpa3":
		if password != "" {
			_, _ = wpaRequest(fmt.Sprintf("SET_NETWORK %d key_mgmt SAE", id))
			if _, err := wpaRequest(fmt.Sprintf(`SET_NETWORK %d sae_password "%s"`, id, password)); err != nil {
				return false, err
			}
		}
	default:
		if password != "" {
			_, _ = wpaRequest(fmt.Sprintf("SET_NETWORK %d key_mgmt WPA-PSK", id))
			if _, err := wpaRequest(fmt.Sprintf(`SET_NETWORK %d psk "%s"`, id, password)); err != nil {
				return false, err
			}
		}
	}
	_, _ = wpaRequest(fmt.Sprintf("SET_NETWORK %d priority 1", id))
	_, _ = wpaRequest(fmt.Sprintf("ENABLE_NETWORK %d", id))
	_, _ = wpaRequest(fmt.Sprintf("SELECT_NETWORK %d", id))
	_, _ = wpaRequest("RECONNECT")

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		st, err := wpaStatus()
		if err == nil && st.State == "COMPLETED" && st.SSID == ssid {
			saved := existing
			if !existing || password != "" {
				if err := wifiStoreUpsert(ssid, password, security); err != nil {
					saved = false
				} else {
					saved = true
				}
			}
			return saved, nil
		}
		time.Sleep(400 * time.Millisecond)
	}
	return false, errors.New("не удалось подключиться: проверьте пароль или сигнал")
}

func wifiForget(ssid string) error {
	if res, err := wifiHelper(12*time.Second, "forget", ssid); err == nil {
		if ok, _ := res["ok"].(bool); ok {
			return nil
		}
	}
	found := false
	if list, err := wpaListNetworks(); err == nil {
		for _, n := range list {
			if n.SSID != ssid {
				continue
			}
			found = true
			if _, err := wpaRequest(fmt.Sprintf("REMOVE_NETWORK %d", n.ID)); err != nil {
				return err
			}
		}
	}
	removedStore, err := wifiStoreRemove(ssid)
	if err != nil {
		return fmt.Errorf("сеть удалена из runtime, но не из хранилища: %v", err)
	}
	if !found && !removedStore {
		return errors.New("сеть не найдена в сохранённых")
	}
	_, _ = wpaRequest("RECONNECT")
	return nil
}

func wifiSetEnabled(on bool) error {
	arg := "disable"
	if on {
		arg = "enable"
	}
	_, code := run(10*time.Second, "/system/bin/svc", "wifi", arg)
	if code != 0 {
		return fmt.Errorf("svc wifi %s: код %d", arg, code)
	}
	time.Sleep(1500 * time.Millisecond)
	return nil
}

// ---------- persistence in WifiConfigStore.xml ----------
//
// wpa_supplicant запущен с read-only конфигом, поэтому SAVE_CONFIG не работает.
// Android Framework хранит сети в /data/misc/wifi/WifiConfigStore.xml и читает
// его при загрузке. Добавляем/удаляем Network-блок там — тогда сеть переживает
// перезагрузку.

const wifiStorePath = "/data/misc/wifi/WifiConfigStore.xml"

func xmlEscapeText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func wifiStoreSafe(s string) bool {
	return !strings.ContainsAny(s, "\"\\\r\n")
}

func wifiStoreBlock(ssid, password, security string) string {
	keyMgmt := "02"
	suffix := "WPA_PSK"
	psk := fmt.Sprintf(`<string name="PreSharedKey">&quot;%s&quot;</string>`, xmlEscapeText(password))
	switch security {
	case "open":
		keyMgmt = "01"
		suffix = "NONE"
		psk = `<null name="PreSharedKey" />`
	case "wpa3":
		keyMgmt = "08"
		suffix = "SAE"
	}
	escSSID := xmlEscapeText(ssid)
	configKey := "&quot;" + escSSID + "&quot;" + suffix
	creation := time.Now().Format("time=01-02 15:04:05.000")
	tpl := `<Network>
<WifiConfiguration>
<string name="ConfigKey">{{KEY}}</string>
<string name="SSID">&quot;{{SSID}}&quot;</string>
<null name="BSSID" />
{{PSK}}
<null name="WEPKeys" />
<int name="WEPTxKeyIndex" value="0" />
<boolean name="HiddenSSID" value="false" />
<boolean name="RequirePMF" value="false" />
<byte-array name="AllowedKeyMgmt" num="1">{{KEYMGMT}}</byte-array>
<byte-array name="AllowedProtocols" num="1">03</byte-array>
<byte-array name="AllowedAuthAlgos" num="1">01</byte-array>
<byte-array name="AllowedGroupCiphers" num="1">0f</byte-array>
<byte-array name="AllowedPairwiseCiphers" num="1">06</byte-array>
<boolean name="Shared" value="true" />
<int name="Status" value="2" />
<null name="FQDN" />
<null name="ProviderFriendlyName" />
<null name="LinkedNetworksList" />
<null name="DefaultGwMacAddress" />
<boolean name="ValidatedInternetAccess" value="false" />
<boolean name="NoInternetAccessExpected" value="false" />
<int name="UserApproved" value="0" />
<boolean name="MeteredHint" value="false" />
<int name="MeteredOverride" value="0" />
<boolean name="UseExternalScores" value="false" />
<int name="NumAssociation" value="0" />
<int name="CreatorUid" value="1000" />
<string name="CreatorName">android.uid.system:1000</string>
<string name="CreationTime">{{CREATION}}</string>
<int name="LastUpdateUid" value="1000" />
<string name="LastUpdateName">android.uid.system:1000</string>
<int name="LastConnectUid" value="1000" />
<boolean name="IsLegacyPasspointConfig" value="false" />
<long-array name="RoamingConsortiumOIs" num="0" />
<string name="RandomizedMacAddress">02:00:00:00:00:00</string>
</WifiConfiguration>
<NetworkStatus>
<string name="SelectionStatus">NETWORK_SELECTION_ENABLED</string>
<string name="DisableReason">NETWORK_SELECTION_ENABLE</string>
<null name="ConnectChoice" />
<long name="ConnectChoiceTimeStamp" value="-1" />
<boolean name="HasEverConnected" value="false" />
</NetworkStatus>
<IpConfiguration>
<string name="IpAssignment">DHCP</string>
<string name="ProxySettings">NONE</string>
</IpConfiguration>
</Network>`
	return strings.NewReplacer(
		"{{KEY}}", configKey,
		"{{SSID}}", escSSID,
		"{{PSK}}", psk,
		"{{KEYMGMT}}", keyMgmt,
		"{{CREATION}}", creation,
	).Replace(tpl)
}

type wifiStoreRange struct {
	start int
	end   int
	ssid  string
}

func wifiStoreRanges(s string) []wifiStoreRange {
	var out []wifiStoreRange
	pos := 0
	for {
		a := strings.Index(s[pos:], "<Network>")
		if a < 0 {
			break
		}
		a += pos
		b := strings.Index(s[a:], "</Network>")
		if b < 0 {
			break
		}
		b += a + len("</Network>")
		block := s[a:b]
		ssid := ""
		if i := strings.Index(block, `<string name="SSID">`); i >= 0 {
			i += len(`<string name="SSID">`)
			if j := strings.Index(block[i:], "</string>"); j >= 0 {
				v := html.UnescapeString(block[i : i+j])
				ssid = strings.Trim(strings.TrimSpace(v), `"`)
			}
		}
		out = append(out, wifiStoreRange{start: a, end: b, ssid: ssid})
		pos = b
	}
	return out
}

func wifiStoreRemoveBlocks(s, ssid string) string {
	ranges := wifiStoreRanges(s)
	var b strings.Builder
	last := 0
	for _, r := range ranges {
		if r.ssid != ssid {
			continue
		}
		b.WriteString(s[last:r.start])
		last = r.end
	}
	b.WriteString(s[last:])
	return b.String()
}

func validateWifiStoreXML(s string) error {
	dec := xml.NewDecoder(strings.NewReader(s))
	for {
		_, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func wifiStoreWrite(data []byte) error {
	if err := validateWifiStoreXML(string(data)); err != nil {
		return fmt.Errorf("XML не прошёл проверку: %v", err)
	}
	old, err := os.ReadFile(wifiStorePath)
	if err != nil {
		return err
	}
	_ = os.WriteFile(wifiStorePath+".stationweb.bak", old, 0600)
	f, err := os.OpenFile(wifiStorePath, os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func wifiStoreUpsert(ssid, password, security string) error {
	if !wifiStoreSafe(ssid) {
		return errors.New("SSID содержит символы, которые нельзя сохранить")
	}
	if security != "open" && !wifiStoreSafe(password) {
		return errors.New("пароль содержит символы, которые нельзя сохранить")
	}
	b, err := os.ReadFile(wifiStorePath)
	if err != nil {
		return err
	}
	s := wifiStoreRemoveBlocks(string(b), ssid)
	i := strings.Index(s, "</NetworkList>")
	if i < 0 {
		return errors.New("в WifiConfigStore.xml нет NetworkList")
	}
	s = s[:i] + wifiStoreBlock(ssid, password, security) + "\n" + s[i:]
	return wifiStoreWrite([]byte(s))
}

func wifiStoreRemove(ssid string) (bool, error) {
	b, err := os.ReadFile(wifiStorePath)
	if err != nil {
		return false, err
	}
	s := string(b)
	next := wifiStoreRemoveBlocks(s, ssid)
	if next == s {
		return false, nil
	}
	return true, wifiStoreWrite([]byte(next))
}

func wifiSavedSSIDs() []string {
	out, code := run(5*time.Second, "/system/bin/dumpsys", "wifi")
	seen := map[string]bool{}
	var list []string
	if code == 0 {
		for _, line := range strings.Split(out, "\n") {
			t := strings.TrimSpace(line)
			if !strings.HasPrefix(t, "ID: ") {
				continue
			}
			i := strings.Index(t, `SSID: "`)
			if i < 0 {
				continue
			}
			rest := t[i+len(`SSID: "`):]
			j := strings.Index(rest, `"`)
			if j <= 0 {
				continue
			}
			ssid := rest[:j]
			if !seen[ssid] {
				seen[ssid] = true
				list = append(list, ssid)
			}
		}
	}
	if len(list) == 0 {
		if nets, err := wpaListNetworks(); err == nil {
			for _, n := range nets {
				if n.SSID != "" && !seen[n.SSID] {
					seen[n.SSID] = true
					list = append(list, n.SSID)
				}
			}
		}
	}
	return list
}
