package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	testUser     = "admin"
	testPassword = "password"
	devicePrompt = "CP4-MOCK>"
)

// mockResponses maps commands to fake Crestron output.
var mockResponses = map[string]string{
	"ver": "Crestron Firmware Version: 2.8001.00054\r\n" +
		"Build Date: Oct 10 2024 14:32:00\r\n",
	"info": "Category: Control System\r\n" +
		"SIMPL: Level 1\r\n" +
		"Hostname: CP4-MOCK\r\n",
}

// startMockServer starts an SSH server that mimics a Crestron device.
// Returns the listener address and a cleanup function.
func startMockServer(t *testing.T) (string, func()) {
	t.Helper()

	hostKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatalf("create signer: %v", err)
	}

	config := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if c.User() == testUser && string(pass) == testPassword {
				return nil, nil
			}
			return nil, fmt.Errorf("auth failed")
		},
	}
	config.AddHostKey(signer)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	var wg sync.WaitGroup
	done := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				select {
				case <-done:
					return
				default:
					continue
				}
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				handleConn(conn, config)
			}()
		}
	}()

	cleanup := func() {
		close(done)
		listener.Close()
		wg.Wait()
	}

	return listener.Addr().String(), cleanup
}

func handleConn(conn net.Conn, config *ssh.ServerConfig) {
	defer conn.Close()

	sshConn, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)

	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			newChan.Reject(ssh.UnknownChannelType, "unsupported")
			continue
		}
		ch, requests, err := newChan.Accept()
		if err != nil {
			return
		}
		go handleSession(ch, requests)
	}
}

func handleSession(ch ssh.Channel, requests <-chan *ssh.Request) {
	defer ch.Close()

	shellReady := make(chan struct{}, 1)

	go func() {
		for req := range requests {
			switch req.Type {
			case "pty-req":
				if req.WantReply {
					req.Reply(true, nil)
				}
			case "shell":
				if req.WantReply {
					req.Reply(true, nil)
				}
				select {
				case shellReady <- struct{}{}:
				default:
				}
			default:
				if req.WantReply {
					req.Reply(false, nil)
				}
			}
		}
	}()

	// Wait for shell request.
	select {
	case <-shellReady:
	case <-time.After(5 * time.Second):
		return
	}

	// Send banner + prompt like a Crestron device.
	ch.Write([]byte("\r\nWelcome to the Crestron Console\r\n\r\n" + devicePrompt + " "))

	// Read commands and respond.
	buf := make([]byte, 4096)
	var lineBuf strings.Builder
	for {
		n, err := ch.Read(buf)
		if err != nil {
			return
		}
		lineBuf.Write(buf[:n])
		for {
			s := lineBuf.String()
			idx := strings.Index(s, "\r\n")
			if idx < 0 {
				break
			}
			cmd := strings.TrimSpace(s[:idx])
			lineBuf.Reset()
			lineBuf.WriteString(s[idx+2:])

			if strings.EqualFold(cmd, "bye") {
				ch.Write([]byte("Goodbye.\r\n"))
				time.Sleep(50 * time.Millisecond)
				return
			}
			if strings.EqualFold(cmd, "echo on") {
				ch.Write([]byte(devicePrompt + " "))
				continue
			}

			resp, ok := mockResponses[cmd]
			if !ok {
				resp = fmt.Sprintf("Bad command: %s\r\n", cmd)
			}
			// Echo command, send response, then prompt.
			io.WriteString(ch, cmd+"\r\n"+resp+devicePrompt+" ")
		}
	}
}

func TestRunAgainstMockServer(t *testing.T) {
	addr, cleanup := startMockServer(t)
	defer cleanup()

	parts := strings.SplitN(addr, ":", 2)
	host, port := parts[0], parts[1]

	output, code := run([]string{
		"ver;info",
		host,
		port,
		testUser,
		testPassword,
		"READ_TIMEOUT=5;SESSION_TIMEOUT=10",
	})

	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; output: %s", code, output)
	}

	var results []result
	if err := json.Unmarshal([]byte(output), &results); err != nil {
		t.Fatalf("invalid JSON output: %v\nraw: %s", err, output)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Check "ver" command.
	ver := results[0]
	if ver.Command != "ver" {
		t.Errorf("expected command 'ver', got %q", ver.Command)
	}
	if ver.Timeout {
		t.Error("ver should not have timed out")
	}
	if !strings.Contains(ver.Response, "2.8001.00054") {
		t.Errorf("ver response missing firmware version: %q", ver.Response)
	}

	// Check "info" command.
	info := results[1]
	if info.Command != "info" {
		t.Errorf("expected command 'info', got %q", info.Command)
	}
	if info.Timeout {
		t.Error("info should not have timed out")
	}
	if !strings.Contains(info.Response, "CP4-MOCK") {
		t.Errorf("info response missing hostname: %q", info.Response)
	}

	// Prompt should not leak into any response.
	for _, r := range results {
		if strings.Contains(r.Response, devicePrompt) {
			t.Errorf("prompt leaked into response for %q: %q", r.Command, r.Response)
		}
	}
}

func TestRunArgValidation(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "too few args",
			args:    []string{"ver", "host", "22"},
			wantErr: "expected 5-6 arguments",
		},
		{
			name:    "invalid port",
			args:    []string{"ver", "host", "abc", "admin", "pass"},
			wantErr: "invalid port",
		},
		{
			name:    "no commands",
			args:    []string{"", "host", "22", "admin", "pass"},
			wantErr: "no commands provided",
		},
		{
			name:    "empty username",
			args:    []string{"ver", "host", "22", "", "pass"},
			wantErr: "username is empty",
		},
		{
			name:    "empty password",
			args:    []string{"ver", "host", "22", "admin", ""},
			wantErr: "password is empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, code := run(tt.args)
			if code != 1 {
				t.Fatalf("expected exit code 1, got %d", code)
			}
			var errObj map[string]string
			if err := json.Unmarshal([]byte(output), &errObj); err != nil {
				t.Fatalf("expected JSON error, got: %s", output)
			}
			if !strings.Contains(errObj["error"], tt.wantErr) {
				t.Errorf("expected error containing %q, got %q", tt.wantErr, errObj["error"])
			}
		})
	}
}

func TestParseOptions(t *testing.T) {
	o := parseOptions("READ_TIMEOUT=15;INTER_CMD_DELAY=1;ECHO=true;BOGUS=ignored")
	if o.ReadTimeout != 15*time.Second {
		t.Errorf("ReadTimeout = %v, want 15s", o.ReadTimeout)
	}
	if o.InterCmdDelay != 1*time.Second {
		t.Errorf("InterCmdDelay = %v, want 1s", o.InterCmdDelay)
	}
	if !o.Echo {
		t.Error("Echo should be true")
	}
}

func TestSanitize(t *testing.T) {
	raw := "ver\r\nCrestron Firmware Version: 2.8001.00054\r\nCP4-MOCK> "
	got := sanitize(raw, "ver", "CP4-MOCK>")
	if !strings.Contains(got, "2.8001.00054") {
		t.Errorf("sanitize should preserve firmware version, got: %q", got)
	}
	if strings.Contains(got, "CP4-MOCK>") {
		t.Errorf("sanitize should strip prompt, got: %q", got)
	}
	if strings.HasPrefix(got, "ver") {
		t.Errorf("sanitize should strip echoed command, got: %q", got)
	}
}
