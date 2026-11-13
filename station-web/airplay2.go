package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	airplay2Pkg    = "com.hkfuertes.shairport"
	airplay2Prefs  = "/data/data/com.hkfuertes.shairport/shared_prefs/com.hkfuertes.shairport_preferences.xml"
	airplay2ShmApp = "/storage/emulated/0/Android/data/com.hkfuertes.shairport/files/nqptp-shm"
	airplay2Shm    = "/data/media/0/Android/data/com.hkfuertes.shairport/files/nqptp-shm"
	airplay2Conf   = "/data/data/com.hkfuertes.shairport/files/shairport-sync.conf"
	airplay2Su     = "/odm/bin/su"
	airplay2DefUID = 10049
	airplay2DefGID = 10049
)

const airplay2SuScript = `#!/system/bin/sh
echo "@nqptp up"
echo "@root ready"
exec sleep 2147483647
`

var (
	airplay2Mu     sync.Mutex
	airplay2LastAt time.Time
	airplay2SuOK   bool
)

func airplay2UID() (int, int) {
	st, err := os.Stat("/data/data/" + airplay2Pkg)
	if err == nil {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			return int(sys.Uid), int(sys.Gid)
		}
	}
	return airplay2DefUID, airplay2DefGID
}

func airplay2NqptpRunning() bool {
	out, _ := run(3*time.Second, "/system/bin/pidof", "libnqptp.so")
	return strings.TrimSpace(out) != ""
}

func airplay2FindNqptp() string {
	out, code := run(5*time.Second, "/system/bin/pm", "path", airplay2Pkg)
	if code != 0 {
		return ""
	}
	line := strings.TrimSpace(out)
	if i := strings.Index(line, "package:"); i >= 0 {
		line = line[i+len("package:"):]
	}
	line = strings.Split(line, "\n")[0]
	base := filepath.Dir(strings.TrimSpace(line))
	for _, abi := range []string{"arm64", "arm"} {
		p := filepath.Join(base, "lib", abi, "libnqptp.so")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func airplay2ConfigAuto() bool {
	data, err := os.ReadFile(airplay2Conf)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), `service_type = "auto"`)
}

func airplay2NowPlayingEnabled() bool {
	data, err := os.ReadFile(airplay2Prefs)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), `name="now_playing" value="true"`)
}

// airplay2DisableMediaSession снимает MediaSession приёмника: иначе штатный
// плеер станции не может забрать звук, пока AirPlay-движок держит сессию.
func airplay2DisableMediaSession() {
	if !airplay2NowPlayingEnabled() {
		return
	}
	run(8*time.Second, "/system/bin/am", "broadcast", "--include-stopped-packages",
		"-n", airplay2Pkg+"/.SettingsReceiver", "-a", airplay2Pkg+".CONFIGURE_SETTINGS",
		"--es", "key", "now_playing", "--ez", "value", "false")
}

func airplay2InstallSu() error {
	if _, err := os.Stat(airplay2Su); err == nil {
		_ = os.Chmod(airplay2Su, 0755)
		_ = os.Chmod("/odm/bin", 0755)
		airplay2SuOK = true
		return nil
	}
	run(10*time.Second, "/system/bin/mount", "-o", "rw,remount", "/odm")
	if err := os.MkdirAll("/odm/bin", 0755); err != nil {
		run(10*time.Second, "/system/bin/mount", "-o", "ro,remount", "/odm")
		return err
	}
	if err := os.WriteFile(airplay2Su, []byte(airplay2SuScript), 0755); err != nil {
		run(10*time.Second, "/system/bin/mount", "-o", "ro,remount", "/odm")
		return err
	}
	_ = os.Chmod(airplay2Su, 0755)
	_ = os.Chmod("/odm/bin", 0755)
	run(10*time.Second, "/system/bin/mount", "-o", "ro,remount", "/odm")
	airplay2SuOK = true
	return nil
}

// airplay2PrepareShm создаёт shm от имени приложения через внешний FUSE-путь.
// Файл, созданный root через /data/media, приложение потом не может открыть.
func airplay2PrepareShm() error {
	uid, gid := airplay2UID()
	_ = os.Remove(filepath.Join(airplay2Shm, "nqptp"))
	script := "mkdir -p '" + airplay2ShmApp + "' && dd if=/dev/zero of='" +
		airplay2ShmApp + "/nqptp' bs=88 count=1 2>/dev/null && chmod 0666 '" +
		airplay2ShmApp + "/nqptp'"
	cmd := exec.Command("/system/bin/sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid:    uint32(uid),
			Gid:    uint32(gid),
			Groups: []uint32{uint32(gid), 1015, 1023, 3003},
		},
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("prepare shm: %v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func airplay2StartNqptp(bin string) error {
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "NQPTP_SHM_DIRECTORY="+airplay2ShmApp)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	devnull, _ := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if devnull != nil {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	}
	if err := cmd.Start(); err != nil {
		if devnull != nil {
			_ = devnull.Close()
		}
		return err
	}
	_ = cmd.Process.Release()
	if devnull != nil {
		_ = devnull.Close()
	}
	return nil
}

func airplay2RestartReceiver() {
	airplay2DisableMediaSession()
	// Сначала гасим tap-приложение: его force-stop закрывает сокет, и если
	// новый движок Shairport уже запущен, он получает SIGPIPE и умирает.
	// Поэтому tap останавливаем до перезапуска приёмника, а поднимаем после.
	run(6*time.Second, "/system/bin/am", "force-stop", nativeAppPkg)
	run(8*time.Second, "/system/bin/am", "broadcast",
		"--include-stopped-packages", "-n", airplay2Pkg+"/.SettingsReceiver",
		"-a", airplay2Pkg+".CONFIGURE_SETTINGS",
		"--es", "key", "airplay_2", "--ez", "value", "true")
	time.Sleep(time.Second)
	run(10*time.Second, "/system/bin/am", "force-stop", airplay2Pkg)
	time.Sleep(time.Second)
	run(10*time.Second, "/system/bin/am", "start-foreground-service",
		"-n", airplay2Pkg+"/.ReceiverService")
	time.Sleep(6 * time.Second)
	run(8*time.Second, "/system/bin/am", "start-foreground-service",
		"-n", nativeAppService, "--es", "mode", "native")
}

func airplay2Ensure() {
	if !airplayInstalledNow() {
		return
	}
	airplay2DisableMediaSession()
	info := airplaySnapshot()
	if info.At == 0 || !info.Enabled {
		return
	}
	// Не рвём активную сессию AirPlay.
	if info.State != "idle" && info.State != "" && info.State != "off" {
		return
	}

	airplay2Mu.Lock()
	defer airplay2Mu.Unlock()
	if !airplay2LastAt.IsZero() && time.Since(airplay2LastAt) < 30*time.Second {
		return
	}
	airplay2LastAt = time.Now()

	confAuto := airplay2ConfigAuto()
	nq := airplay2NqptpRunning()
	mode := info.Mode

	if confAuto && nq && mode == "airplay2" {
		return
	}

	log.Printf("airplay2: restart needed (auto=%v nqptp=%v mode=%q state=%q running=%v)", confAuto, nq, mode, info.State, info.Running)

	if err := airplay2InstallSu(); err != nil {
		log.Printf("airplay2: su: %v", err)
		return
	}
	bin := airplay2FindNqptp()
	if bin == "" {
		log.Printf("airplay2: libnqptp.so not found")
		return
	}
	if !nq {
		if out, _ := run(3*time.Second, "/system/bin/pidof", "libnqptp.so"); strings.TrimSpace(out) != "" {
			run(3*time.Second, "/system/bin/kill", "-9", strings.Fields(out)[0])
			time.Sleep(300 * time.Millisecond)
		}
		if err := airplay2PrepareShm(); err != nil {
			log.Printf("airplay2: %v", err)
			return
		}
		if err := airplay2StartNqptp(bin); err != nil {
			log.Printf("airplay2: nqptp: %v", err)
			return
		}
		time.Sleep(700 * time.Millisecond)
	}
	airplay2RestartReceiver()
	log.Printf("airplay2: ready (auto=%v, nqptp=%v)", confAuto, airplay2NqptpRunning())
}

func startAirplay2Manager() {
	go func() {
		for {
			airplay2Ensure()
			time.Sleep(5 * time.Second)
		}
	}()
}
