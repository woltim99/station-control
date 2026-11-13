package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	inputHelperAddr = "127.0.0.1:8092"
	inputHelperDex  = "/data/local/tmp/station-web/input.dex"
)

var (
	inputHelperMu   sync.Mutex
	inputHelperConn net.Conn
	inputHelperBuf  *bufio.Reader
	inputHelperProc *os.Process
)

// inputHelperEnsure подключается к резидентному InputHelper, а если его нет —
// запускает. Обычный `input` стартует ~0,8 с, helper отвечает за десятки мс.
func inputHelperEnsure() error {
	if inputHelperConn != nil {
		return nil
	}
	if c, err := net.DialTimeout("tcp", inputHelperAddr, 250*time.Millisecond); err == nil {
		inputHelperConn = c
		inputHelperBuf = bufio.NewReader(c)
		return nil
	}
	if _, err := os.Stat(inputHelperDex); err != nil {
		return err
	}
	if inputHelperProc == nil {
		cmd := exec.Command("/system/bin/app_process", "/system/bin", "InputHelper")
		cmd.Env = append(os.Environ(), "CLASSPATH="+inputHelperDex)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		inputHelperProc = cmd.Process
		_ = cmd.Process.Release()
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", inputHelperAddr, 250*time.Millisecond); err == nil {
			inputHelperConn = c
			inputHelperBuf = bufio.NewReader(c)
			return nil
		}
		time.Sleep(80 * time.Millisecond)
	}
	return errors.New("input helper не запустился")
}

func inputHelperDrop() {
	if inputHelperConn != nil {
		_ = inputHelperConn.Close()
	}
	inputHelperConn = nil
	inputHelperBuf = nil
}

func inputHelperSend(cmd string) error {
	inputHelperMu.Lock()
	defer inputHelperMu.Unlock()
	if err := inputHelperEnsure(); err != nil {
		return err
	}
	_ = inputHelperConn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.WriteString(inputHelperConn, cmd+"\n"); err != nil {
		inputHelperDrop()
		return err
	}
	_ = inputHelperConn.SetReadDeadline(time.Now().Add(4 * time.Second))
	line, err := inputHelperBuf.ReadString('\n')
	if err != nil {
		inputHelperDrop()
		return err
	}
	line = strings.TrimSpace(line)
	if line != "ok" {
		return fmt.Errorf("input helper: %s", line)
	}
	return nil
}

// injectKey нажимает keycode через резидентный helper, а если тот недоступен —
// падает на медленный /system/bin/input (~0,8 с на этой станции).
func injectKey(code string) (string, int) {
	if err := inputHelperSend("key " + code); err == nil {
		return "", 0
	}
	return run(5*time.Second, "/system/bin/input", "keyevent", code)
}

// injectTap и injectSwipe работают так же: helper отвечает за десятки мс.
func injectTap(x, y int) (string, int) {
	if err := inputHelperSend(fmt.Sprintf("tap %d %d", x, y)); err == nil {
		return "", 0
	}
	return run(5*time.Second, "/system/bin/input", "tap", strconv.Itoa(x), strconv.Itoa(y))
}

func injectSwipe(x1, y1, x2, y2, duration int) (string, int) {
	if err := inputHelperSend(fmt.Sprintf("swipe %d %d %d %d %d", x1, y1, x2, y2, duration)); err == nil {
		return "", 0
	}
	return run(5*time.Second, "/system/bin/input", "swipe",
		strconv.Itoa(x1), strconv.Itoa(y1), strconv.Itoa(x2), strconv.Itoa(y2), strconv.Itoa(duration))
}
