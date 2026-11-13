package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/creack/pty"
	"golang.org/x/crypto/ssh"
)

var (
	addr     = flag.String("addr", ":2222", "listen address")
	authKeys = flag.String("authorized_keys", "/data/local/tmp/absshd/authorized_keys", "authorized_keys file")
	logPath  = flag.String("log", "/data/local/tmp/absshd/sshd.log", "log file")
	hostKey  = flag.String("hostkey", "/odm/etc/absshd/host_ed25519", "persistent host private key")
)

func loadOrCreateHostKey(path string) (ssh.Signer, error) {
	if data, err := os.ReadFile(path); err == nil {
		return ssh.ParsePrivateKey(data)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, err
	}
	_ = os.MkdirAll(filepath.Dir(path), 0700)
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		log.Printf("cannot save host key %s: %v (key will change on restart)", path, err)
	}
	return ssh.NewSignerFromKey(priv)
}

type winSize struct {
	Rows uint16
	Cols uint16
}

func loadAuthorizedKeys(path string) (map[string]bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			log.Printf("bad authorized key: %v", err)
			continue
		}
		keys[string(pk.Marshal())] = true
	}
	return keys, nil
}

func main() {
	flag.Parse()
	_ = os.MkdirAll(filepath.Dir(*logPath), 0755)
	if f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
		log.SetOutput(f)
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	keys, err := loadAuthorizedKeys(*authKeys)
	if err != nil {
		log.Fatalf("authorized_keys: %v", err)
	}
	if len(keys) == 0 {
		log.Fatalf("no authorized keys in %s", *authKeys)
	}
	signer, err := loadOrCreateHostKey(*hostKey)
	if err != nil {
		log.Fatalf("host key: %v", err)
	}
	cfg := &ssh.ServerConfig{
		ServerVersion: "SSH-2.0-absshd",
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if keys[string(key.Marshal())] {
				return nil, nil
			}
			return nil, fmt.Errorf("unknown public key for %s", c.User())
		},
	}
	cfg.AddHostKey(signer)
	var ln net.Listener
	for attempt := 1; ; attempt++ {
		ln, err = net.Listen("tcp4", *addr)
		if err == nil {
			break
		}
		log.Printf("listen attempt %d failed: %v", attempt, err)
		if attempt >= 60 {
			log.Fatalf("listen %s: %v", *addr, err)
		}
		time.Sleep(2 * time.Second)
	}
	log.Printf("listening on %s, %d authorized key(s)", *addr, len(keys))
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go handleConn(conn, cfg)
	}
}

func handleConn(c net.Conn, cfg *ssh.ServerConfig) {
	sconn, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		log.Printf("handshake from %s: %v", c.RemoteAddr(), err)
		_ = c.Close()
		return
	}
	log.Printf("login user=%s from=%s", sconn.User(), sconn.RemoteAddr())
	go ssh.DiscardRequests(reqs)
	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			_ = newChan.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}
		ch, chReqs, err := newChan.Accept()
		if err != nil {
			continue
		}
		go handleSession(ch, chReqs)
	}
}

func handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	var ws *winSize
	var term string
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			if len(req.Payload) >= 4 {
				n := binary.BigEndian.Uint32(req.Payload[:4])
				if int(4+n+16) <= len(req.Payload) {
					term = string(req.Payload[4 : 4+n])
					p := req.Payload[4+n:]
					cols := binary.BigEndian.Uint32(p[0:4])
					rows := binary.BigEndian.Uint32(p[4:8])
					ws = &winSize{Rows: uint16(rows), Cols: uint16(cols)}
					_ = req.Reply(true, nil)
					continue
				}
			}
			_ = req.Reply(false, nil)
		case "shell":
			_ = req.Reply(true, nil)
			run(ch, "/system/bin/sh", nil, ws, term)
			return
		case "exec":
			cmd := ""
			if len(req.Payload) >= 4 {
				n := binary.BigEndian.Uint32(req.Payload[:4])
				if int(4+n) <= len(req.Payload) {
					cmd = string(req.Payload[4 : 4+n])
				}
			}
			_ = req.Reply(true, nil)
			run(ch, "/system/bin/sh", []string{"-c", cmd}, ws, term)
			return
		case "env":
			_ = req.Reply(true, nil)
		case "window-change":
			if ws != nil && len(req.Payload) >= 8 {
				ws.Cols = uint16(binary.BigEndian.Uint32(req.Payload[0:4]))
				ws.Rows = uint16(binary.BigEndian.Uint32(req.Payload[4:8]))
			}
			_ = req.Reply(true, nil)
		default:
			_ = req.Reply(false, nil)
		}
	}
}

func run(ch ssh.Channel, shell string, args []string, ws *winSize, term string) {
	cmd := exec.Command(shell, args...)
	cmd.Env = append(os.Environ(),
		"TERM="+term,
		"HOME=/data",
		"PATH=/sbin:/system/sbin:/system/bin:/system/xbin:/odm/bin",
	)
	status := 0
	if ws != nil {
		f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: ws.Rows, Cols: ws.Cols})
		if err != nil {
			log.Printf("pty start: %v", err)
			_ = ch.Close()
			return
		}
		go func() { _, _ = io.Copy(f, ch) }()
		_, _ = io.Copy(ch, f)
		err = cmd.Wait()
		_ = f.Close()
		if ee, ok := err.(*exec.ExitError); ok {
			status = ee.ExitCode()
		} else if err != nil {
			status = 1
		}
	} else {
		stdin, err := cmd.StdinPipe()
		if err != nil {
			log.Printf("stdin pipe: %v", err)
			_ = ch.Close()
			return
		}
		cmd.Stdout = ch
		cmd.Stderr = ch.Stderr()
		go func() {
			_, _ = io.Copy(stdin, ch)
			_ = stdin.Close()
		}()
		err = cmd.Run()
		if ee, ok := err.(*exec.ExitError); ok {
			status = ee.ExitCode()
		} else if err != nil {
			status = 1
		}
	}
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(status)}))
	_ = ch.Close()
}
