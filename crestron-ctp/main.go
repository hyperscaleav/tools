// crestron-ctp — Zabbix external check for Crestron device monitoring via CTP.
//
// Sends chained commands to a Crestron console over an interactive SSH shell
// session and returns structured JSON. Required because Crestron's proprietary
// SSH daemon only supports interactive shell channels (ssh -t), not the exec
// channels that Zabbix's built-in SSH agent uses.
//
// Usage:
//
//	crestron-ctp "info;ipconfig;ver" 10.0.0.50 22 admin password
//	crestron-ctp "info;free" 10.0.0.50 22 admin password "READ_TIMEOUT=15;INTER_CMD_DELAY=1"
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// defaults mirrors the Python DEFAULTS dict.
var defaults = map[string]any{
	"READ_TIMEOUT":    10.0,
	"INTER_CMD_DELAY": 0.5,
	"SESSION_TIMEOUT": 25.0,
	"SHELL_WIDTH":     160,
	"ECHO":            false,
}

// initialPromptRE discovers the device's actual prompt string during banner
// flush.  Intentionally broad: device names can contain letters, digits,
// hyphens, underscores, dots, spaces, and brackets.
// e.g.  CP4>   DMPS3-4K-350-C>   DM-MD32X32 [Room 101]>
var initialPromptRE = regexp.MustCompile(`\r?\n?([A-Za-z][A-Za-z0-9 _.\[\]()-]*>)[ ]?`)

// trailingSpacesRE strips trailing spaces from each line.
var trailingSpacesRE = regexp.MustCompile(`[ ]+$`)

// result is the JSON structure for each command.
type result struct {
	Command  string `json:"command"`
	Response string `json:"response"`
	Raw      string `json:"raw"`
	Timeout  bool   `json:"timeout"`
}

// opts holds parsed runtime options.
type opts struct {
	ReadTimeout   time.Duration
	InterCmdDelay time.Duration
	SessionTimeout time.Duration
	ShellWidth    int
	Echo          bool
}

func errorExit(msg string) {
	b, _ := json.Marshal(map[string]string{"error": msg})
	fmt.Println(string(b))
	os.Exit(1)
}

func parseOptions(raw string) opts {
	o := opts{
		ReadTimeout:    time.Duration(defaults["READ_TIMEOUT"].(float64) * float64(time.Second)),
		InterCmdDelay:  time.Duration(defaults["INTER_CMD_DELAY"].(float64) * float64(time.Second)),
		SessionTimeout: time.Duration(defaults["SESSION_TIMEOUT"].(float64) * float64(time.Second)),
		ShellWidth:     defaults["SHELL_WIDTH"].(int),
		Echo:           defaults["ECHO"].(bool),
	}
	if strings.TrimSpace(raw) == "" {
		return o
	}
	for _, pair := range strings.Split(raw, ";") {
		pair = strings.TrimSpace(pair)
		if pair == "" || !strings.Contains(pair, "=") {
			continue
		}
		parts := strings.SplitN(pair, "=", 2)
		key := strings.ToUpper(strings.TrimSpace(parts[0]))
		val := strings.TrimSpace(parts[1])
		switch key {
		case "READ_TIMEOUT":
			if f, err := strconv.ParseFloat(val, 64); err == nil {
				o.ReadTimeout = time.Duration(f * float64(time.Second))
			}
		case "INTER_CMD_DELAY":
			if f, err := strconv.ParseFloat(val, 64); err == nil {
				o.InterCmdDelay = time.Duration(f * float64(time.Second))
			}
		case "SESSION_TIMEOUT":
			if f, err := strconv.ParseFloat(val, 64); err == nil {
				o.SessionTimeout = time.Duration(f * float64(time.Second))
			}
		case "SHELL_WIDTH":
			if v, err := strconv.Atoi(val); err == nil {
				o.ShellWidth = v
			}
		case "ECHO":
			o.Echo = strings.ToLower(val) == "true" ||
				val == "1" ||
				strings.ToLower(val) == "yes" ||
				strings.ToLower(val) == "on"
		}
	}
	return o
}

// readUntilPrompt reads from the session until the exact prompt string appears
// or the timeout expires.  Returns the accumulated buffer and whether it timed out.
func readUntilPrompt(session *ssh.Session, stdout <-chan byte, prompt string, timeout time.Duration) (string, bool) {
	var buf strings.Builder
	deadline := time.After(timeout)
	for {
		select {
		case b, ok := <-stdout:
			if !ok {
				return buf.String(), !strings.Contains(buf.String(), prompt)
			}
			buf.WriteByte(b)
			if strings.Contains(buf.String(), prompt) {
				return buf.String(), false
			}
		case <-deadline:
			return buf.String(), true
		}
	}
}

// discoverPrompt reads from the session until the initial prompt regex matches.
func discoverPrompt(stdout <-chan byte, timeout time.Duration) (string, string, error) {
	var buf strings.Builder
	deadline := time.After(timeout)
	for {
		select {
		case b, ok := <-stdout:
			if !ok {
				return buf.String(), "", fmt.Errorf(
					"timed out waiting for initial prompt (got %d bytes: %q)",
					buf.Len(), lastN(buf.String(), 200))
			}
			buf.WriteByte(b)
			if m := initialPromptRE.FindStringSubmatch(buf.String()); m != nil {
				return buf.String(), m[1], nil
			}
		case <-deadline:
			return buf.String(), "", fmt.Errorf(
				"timed out waiting for initial prompt (got %d bytes: %q)",
				buf.Len(), lastN(buf.String(), 200))
		}
	}
}

// drain reads and discards any pending data on the channel for the given duration.
func drain(stdout <-chan byte, timeout time.Duration) {
	deadline := time.After(timeout)
	for {
		select {
		case _, ok := <-stdout:
			if !ok {
				return
			}
		case <-deadline:
			return
		}
	}
}

// sanitize strips the device prompt, echoed command line, and surrounding whitespace.
func sanitize(raw, command, prompt string) string {
	cleaned := strings.ReplaceAll(raw, prompt, "")
	cleaned = trailingSpacesRE.ReplaceAllString(cleaned, "")
	lines := strings.Split(cleaned, "\n")
	if len(lines) > 0 && strings.EqualFold(strings.TrimSpace(lines[0]), strings.TrimSpace(command)) {
		lines = lines[1:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func lastN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

const usageText = `crestron-ctp — Zabbix external check for Crestron devices via CTP console.

Sends semicolon-delimited commands to a Crestron device over an interactive
SSH shell session and returns structured JSON. Required because Crestron's
proprietary SSH daemon only supports interactive shell channels, not the
exec channels that Zabbix's built-in SSH agent uses.

Usage:
  crestron-ctp COMMANDS HOST PORT USER PASSWORD [OPTIONS]

Arguments:
  COMMANDS   Semicolon-delimited CTP commands (e.g. "info;ipconfig;ver")
  HOST       Device IP or hostname
  PORT       SSH port (usually 22)
  USER       SSH username
  PASSWORD   SSH password
  OPTIONS    Optional semicolon-delimited key=value pairs:
               READ_TIMEOUT=10      Per-command read timeout in seconds
               INTER_CMD_DELAY=0.5  Delay between commands in seconds
               SESSION_TIMEOUT=25   Overall session timeout in seconds
               SHELL_WIDTH=160      PTY width in columns
               ECHO=false           Enable CTP echo mode (true/false)

Examples:
  crestron-ctp "info;ipconfig;ver" 10.0.0.50 22 admin password
  crestron-ctp "info;free" 10.0.0.50 22 admin password "READ_TIMEOUT=15;INTER_CMD_DELAY=1"
`

// run is the core logic, separated from main() for testability.
func run(args []string) (string, int) {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		return usageText, 0
	}
	if len(args) < 5 || len(args) > 6 {
		return jsonError(fmt.Sprintf(
			"expected 5-6 arguments (commands, host, port, user, password [, options]), got %d",
			len(args))), 1
	}

	commandsRaw, host, portStr, username, password := args[0], args[1], args[2], args[3], args[4]
	optionsRaw := ""
	if len(args) == 6 {
		optionsRaw = args[5]
	}

	if strings.TrimSpace(username) == "" {
		return jsonError("username is empty — check {$CRESTRON.USERNAME} macro"), 1
	}
	if strings.TrimSpace(password) == "" {
		return jsonError("password is empty — check {$CRESTRON.PASSWORD} macro"), 1
	}

	o := parseOptions(optionsRaw)

	var commands []string
	for _, c := range strings.Split(commandsRaw, ";") {
		c = strings.TrimSpace(c)
		if c != "" {
			commands = append(commands, c)
		}
	}
	if len(commands) == 0 {
		return jsonError("no commands provided"), 1
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		return jsonError(fmt.Sprintf("invalid port: %s", portStr)), 1
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))

	// Session-level timeout context.
	ctx, cancel := context.WithTimeout(context.Background(), o.SessionTimeout)
	defer cancel()

	// Crestron devices run old SSH daemons that use legacy algorithms.
	// Go's x/crypto/ssh supports them when explicitly configured.
	config := &ssh.ClientConfig{
		User: username,
		Auth: []ssh.AuthMethod{
			ssh.Password(password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         o.ReadTimeout,
	}

	// Connect with session timeout awareness.
	var client *ssh.Client
	connDone := make(chan struct{})
	var connErr error
	go func() {
		client, connErr = ssh.Dial("tcp", addr, config)
		close(connDone)
	}()
	select {
	case <-connDone:
	case <-ctx.Done():
		return jsonError(fmt.Sprintf(
			"session timeout (%ds) exceeded talking to %s",
			int(o.SessionTimeout.Seconds()), addr)), 1
	}
	if connErr != nil {
		return classifyConnError(connErr, username, host, port), 1
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return jsonError(fmt.Sprintf("SSH error connecting to %s: %s", addr, err)), 1
	}
	defer session.Close()

	// Request a PTY with the configured width.
	if err := session.RequestPty("xterm", 24, o.ShellWidth, ssh.TerminalModes{}); err != nil {
		return jsonError(fmt.Sprintf("SSH error connecting to %s: %s", addr, err)), 1
	}

	// Get stdin/stdout pipes for the interactive shell.
	stdinPipe, err := session.StdinPipe()
	if err != nil {
		return jsonError(fmt.Sprintf("SSH error connecting to %s: %s", addr, err)), 1
	}
	stdoutPipe, err := session.StdoutPipe()
	if err != nil {
		return jsonError(fmt.Sprintf("SSH error connecting to %s: %s", addr, err)), 1
	}

	if err := session.Shell(); err != nil {
		return jsonError(fmt.Sprintf("SSH error connecting to %s: %s", addr, err)), 1
	}

	// Pump stdout into a channel byte-by-byte so we can select with timeouts.
	stdoutCh := make(chan byte, 65536)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdoutPipe.Read(buf)
			for i := 0; i < n; i++ {
				stdoutCh <- buf[i]
			}
			if err != nil {
				close(stdoutCh)
				return
			}
		}
	}()

	// sendBye is the cleanup function — always called via defer.
	sendBye := func() {
		_, _ = stdinPipe.Write([]byte("bye\r\n"))
		drain(stdoutCh, 300*time.Millisecond)
	}
	defer sendBye()

	// Discover the device prompt from the welcome banner.
	_, promptStr, err := discoverPrompt(stdoutCh, o.ReadTimeout)
	if err != nil {
		return jsonError(fmt.Sprintf("session error with %s: %s", addr, err)), 1
	}

	// Optionally enable echo.
	if o.Echo {
		_, _ = stdinPipe.Write([]byte("echo on\r\n"))
		readUntilPrompt(session, stdoutCh, promptStr, o.ReadTimeout)
	}

	// Drain residual buffer.
	drain(stdoutCh, 300*time.Millisecond)

	// Execute commands.
	results := make([]result, 0, len(commands))
	for i, cmd := range commands {
		// Check session timeout before each command.
		select {
		case <-ctx.Done():
			return jsonError(fmt.Sprintf(
				"session timeout (%ds) exceeded talking to %s",
				int(o.SessionTimeout.Seconds()), addr)), 1
		default:
		}

		_, _ = stdinPipe.Write([]byte(cmd + "\r\n"))
		raw, timedOut := readUntilPrompt(session, stdoutCh, promptStr, o.ReadTimeout)
		results = append(results, result{
			Command:  cmd,
			Response: sanitize(raw, cmd, promptStr),
			Raw:      strings.TrimSpace(raw),
			Timeout:  timedOut,
		})
		// Skip delay after last command.
		if o.InterCmdDelay > 0 && i < len(commands)-1 {
			time.Sleep(o.InterCmdDelay)
		}
	}

	b, _ := json.Marshal(results)
	return string(b), 0
}

func classifyConnError(err error, username, host string, port int) string {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	msg := err.Error()
	switch {
	case strings.Contains(msg, "unable to authenticate"):
		return jsonError(fmt.Sprintf("authentication failed for %s@%s", username, addr))
	case strings.Contains(msg, "i/o timeout") || strings.Contains(msg, "connection timed out"):
		return jsonError(fmt.Sprintf("connection timed out to %s", addr))
	case strings.Contains(msg, "connection refused") || strings.Contains(msg, "no route"):
		return jsonError(fmt.Sprintf("network error connecting to %s: %s", addr, err))
	default:
		return jsonError(fmt.Sprintf("SSH error connecting to %s: %s", addr, err))
	}
}

func jsonError(msg string) string {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return string(b)
}

func main() {
	output, code := run(os.Args[1:])
	fmt.Println(output)
	os.Exit(code)
}
