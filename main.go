package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/manifoldco/promptui"
	"golang.org/x/term"
)

type Request struct {
	Action string // "new", "attach", "ls", "save", "resize", "kill"
	Name   string
	Rows   uint16
	Cols   uint16
}

type Response struct {
	Error    string   `json:"error,omitempty"`
	Message  string   `json:"message,omitempty"`
	Sessions []string `json:"sessions,omitempty"`
}

type SavedSession struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
}

type Session struct {
	Name    string
	Dir     string
	Pty     *os.File
	Cmd     *exec.Cmd
	Clients map[net.Conn]bool
	History []byte
	mu      sync.Mutex
}

const MaxHistory = 512 * 1024 // 512KB

var (
	sessions   = make(map[string]*Session)
	sessionsMu sync.Mutex
)

func getSocketPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/tmp"
	}
	dir := filepath.Join(home, ".tfm")
	os.MkdirAll(dir, 0700)
	return filepath.Join(dir, "tfm.sock")
}

func getSessionsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/tmp"
	}
	return filepath.Join(home, ".tfm", "sessions.json")
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	command := os.Args[1]

	switch command {
	case "daemon":
		runDaemon()
	case "new":
		nameFlag := flag.String("s", "", "Session name")
		flag.CommandLine.Parse(os.Args[2:])
		name := *nameFlag
		if name == "" && flag.NArg() > 0 {
			name = flag.Arg(0)
		}
		if name == "" {
			fmt.Println("Usage: tfm new [-s] <session_name>")
			os.Exit(1)
		}
		next := runClient("new", name)
		for next != "" {
			next = runClient("attach", next)
		}
	case "attach":
		nameFlag := flag.String("t", "", "Session name")
		flag.CommandLine.Parse(os.Args[2:])
		name := *nameFlag
		if name == "" && flag.NArg() > 0 {
			name = flag.Arg(0)
		}
		if name == "" {
			fmt.Println("Usage: tfm attach [-t] <session_name>")
			os.Exit(1)
		}
		next := runClient("attach", name)
		for next != "" {
			next = runClient("attach", next)
		}
	case "ls":
		runClient("ls", "")
	case "save":
		runClient("save", "")
	case "kill":
		nameFlag := flag.String("t", "", "Session name")
		flag.CommandLine.Parse(os.Args[2:])
		name := *nameFlag
		if name == "" && flag.NArg() > 0 {
			name = flag.Arg(0)
		}
		if name == "" {
			fmt.Println("Usage: tfm kill [-t] <session_name>")
			os.Exit(1)
		}
		runClient("kill", name)
	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("Terminal for Mac (tfm) - A simple terminal multiplexer")
	fmt.Println("Commands:")
	fmt.Println("  new [-s] <name>     Create a new session")
	fmt.Println("  attach [-t] <name>  Attach to a session")
	fmt.Println("  ls                  List active and saved sessions")
	fmt.Println("  save                Save current sessions to disk")
	fmt.Println("  kill [-t] <name>    Kill a session")
	fmt.Println("  daemon              Start the background server")
}

// ---------------------------------------------------------
// DAEMON (SERVER)
// ---------------------------------------------------------

func runDaemon() {
	sockPath := getSocketPath()
	os.Remove(sockPath)

	l, err := net.Listen("unix", sockPath)
	if err != nil {
		log.Fatalf("Failed to listen on socket: %v", err)
	}
	defer l.Close()

	// Load saved sessions on startup
	loadSavedSessions()

	log.Printf("Daemon listening on %s", sockPath)

	for {
		conn, err := l.Accept()
		if err != nil {
			log.Printf("Accept error: %v", err)
			continue
		}
		go handleConnection(conn)
	}
}

func handleConnection(conn net.Conn) {
	defer conn.Close()

	decoder := json.NewDecoder(conn)
	var req Request
	if err := decoder.Decode(&req); err != nil {
		log.Printf("Failed to read request: %v", err)
		return
	}

	encoder := json.NewEncoder(conn)

	switch req.Action {
	case "new":
		err := createSession(req.Name, req.Rows, req.Cols)
		if err != nil {
			encoder.Encode(Response{Error: err.Error()})
			return
		}
		encoder.Encode(Response{Message: "Created"})
		attachToSession(conn, req.Name)
	case "attach":
		sessionsMu.Lock()
		_, exists := sessions[req.Name]
		sessionsMu.Unlock()
		if !exists {
			encoder.Encode(Response{Error: "Session not found"})
			return
		}
		encoder.Encode(Response{Message: "Attached"})
		attachToSession(conn, req.Name)
	case "ls":
		sessionsMu.Lock()
		var names []string
		for name := range sessions {
			names = append(names, name)
		}
		sessionsMu.Unlock()
		encoder.Encode(Response{Sessions: names})
	case "save":
		err := saveSessions()
		if err != nil {
			encoder.Encode(Response{Error: err.Error()})
		} else {
			encoder.Encode(Response{Message: "Sessions saved successfully"})
		}
	case "kill":
		sessionsMu.Lock()
		sess, exists := sessions[req.Name]
		if exists {
			sess.Cmd.Process.Kill()
			delete(sessions, req.Name)
		}
		sessionsMu.Unlock()
		encoder.Encode(Response{Message: "Session killed"})
	case "resize":
		sessionsMu.Lock()
		sess, exists := sessions[req.Name]
		sessionsMu.Unlock()
		if exists {
			pty.Setsize(sess.Pty, &pty.Winsize{Rows: req.Rows, Cols: req.Cols})
		}
		encoder.Encode(Response{Message: "Resized"})
	}
}

func createSession(name string, rows, cols uint16) error {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()

	if _, exists := sessions[name]; exists {
		return fmt.Errorf("session already exists")
	}

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "bash"
	}

	cmd := exec.Command(shell)
	dir, _ := os.Getwd()
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), fmt.Sprintf("TFM_SESSION=%s", name))

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return err
	}

	if rows > 0 && cols > 0 {
		pty.Setsize(ptmx, &pty.Winsize{Rows: rows, Cols: cols})
	}

	sess := &Session{
		Name:    name,
		Dir:     dir,
		Pty:     ptmx,
		Cmd:     cmd,
		Clients: make(map[net.Conn]bool),
	}
	sessions[name] = sess

	go pumpPty(sess)

	// Wait for process to exit and clean up
	go func() {
		cmd.Wait()
		sessionsMu.Lock()
		delete(sessions, name)
		sessionsMu.Unlock()
	}()

	return nil
}

func pumpPty(sess *Session) {
	buf := make([]byte, 32*1024)
	for {
		n, err := sess.Pty.Read(buf)
		if err != nil {
			break
		}
		sess.mu.Lock()
		sess.History = append(sess.History, buf[:n]...)
		if len(sess.History) > MaxHistory {
			sess.History = sess.History[len(sess.History)-MaxHistory:]
		}
		for conn := range sess.Clients {
			conn.Write(buf[:n])
		}
		sess.mu.Unlock()
	}
}

func attachToSession(conn net.Conn, name string) {
	sessionsMu.Lock()
	sess := sessions[name]
	sessionsMu.Unlock()

	if sess == nil {
		return
	}

	sess.mu.Lock()
	sess.Clients[conn] = true
	if len(sess.History) > 0 {
		conn.Write(sess.History)
	}
	sess.mu.Unlock()

	defer func() {
		sess.mu.Lock()
		delete(sess.Clients, conn)
		sess.mu.Unlock()
	}()

	// Read from client and write to PTY
	buf := make([]byte, 32*1024)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			break
		}
		// Write client input directly to PTY
		sess.Pty.Write(buf[:n])
	}
}

func saveSessions() error {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()

	var saved []SavedSession
	for _, sess := range sessions {
		saved = append(saved, SavedSession{
			Name: sess.Name,
			Dir:  sess.Dir,
		})
	}

	data, err := json.MarshalIndent(saved, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(getSessionsPath(), data, 0644)
}

func loadSavedSessions() {
	data, err := os.ReadFile(getSessionsPath())
	if err != nil {
		return // File might not exist
	}

	var saved []SavedSession
	if err := json.Unmarshal(data, &saved); err != nil {
		log.Printf("Failed to parse saved sessions: %v", err)
		return
	}

	for _, s := range saved {
		// Ignore if already exists
		sessionsMu.Lock()
		_, exists := sessions[s.Name]
		sessionsMu.Unlock()

		if !exists {
			// Create it with default sizes
			err := createSessionWithDir(s.Name, s.Dir)
			if err != nil {
				log.Printf("Failed to restore session %s: %v", s.Name, err)
			}
		}
	}
}

func createSessionWithDir(name, dir string) error {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "bash"
	}

	cmd := exec.Command(shell)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), fmt.Sprintf("TFM_SESSION=%s", name))

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return err
	}

	sess := &Session{
		Name:    name,
		Dir:     dir,
		Pty:     ptmx,
		Cmd:     cmd,
		Clients: make(map[net.Conn]bool),
	}
	sessions[name] = sess

	go pumpPty(sess)

	go func() {
		cmd.Wait()
		sessionsMu.Lock()
		delete(sessions, name)
		sessionsMu.Unlock()
	}()

	return nil
}

// ---------------------------------------------------------
// CLIENT
// ---------------------------------------------------------

func runClient(action, name string) string {
	conn := ensureDaemonConnected()
	defer conn.Close()

	rows, cols := getTerminalSize()

	req := Request{
		Action: action,
		Name:   name,
		Rows:   rows,
		Cols:   cols,
	}

	encoder := json.NewEncoder(conn)
	encoder.Encode(req)

	var respBytes []byte
	singleByte := make([]byte, 1)
	for {
		n, err := conn.Read(singleByte)
		if err != nil {
			if err == io.EOF && (action == "ls" || action == "save" || action == "kill") {
				return ""
			}
			log.Fatalf("Failed to read server response: %v", err)
		}
		if n > 0 {
			respBytes = append(respBytes, singleByte[0])
			if singleByte[0] == '\n' {
				break
			}
		}
	}

	var resp Response
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		log.Fatalf("Failed to parse server response: %v", err)
	}

	if resp.Error != "" {
		fmt.Printf("Error: %s\n", resp.Error)
		os.Exit(1)
	}

	switch action {
	case "ls":
		fmt.Println("Sessions:")
		for _, s := range resp.Sessions {
			fmt.Printf("  %s\n", s)
		}
		return ""
	case "save", "kill":
		fmt.Println(resp.Message)
		return ""
	}

	// Enter alternate screen buffer & clear screen
	fmt.Print("\033[?1049h\033[2J\033[H")

	// For new and attach, enter raw mode and proxy I/O
	fmt.Printf("Attached to session %s. Press Ctrl+B then d to detach.\r\n", name)

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Print("\033[?1049l") // cleanup just in case
		log.Fatalf("Failed to set raw mode: %v", err)
	}

	defer func() {
		// Leave alternate screen buffer and restore terminal state
		term.Restore(int(os.Stdin.Fd()), oldState)
		fmt.Print("\033[?1049l")
	}()

	// Handle window resizing
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGWINCH)
		for range sig {
			r, c := getTerminalSize()
			// Need a separate connection to send resize to avoid multiplexing over same stream
			resizeConn, err := net.Dial("unix", getSocketPath())
			if err == nil {
				json.NewEncoder(resizeConn).Encode(Request{
					Action: "resize",
					Name:   name,
					Rows:   r,
					Cols:   c,
				})
				resizeConn.Close()
			}
		}
	}()

	// Proxy output from server to stdout
	go func() {
		io.Copy(os.Stdout, conn)
	}()

	// Proxy input from stdin to server, parsing Ctrl+B d for detach
	buf := make([]byte, 1024)
	ctrlB := false
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			break
		}

		var outBuf bytes.Buffer
		for i := 0; i < n; i++ {
			b := buf[i]
			if ctrlB {
				if b == 'd' || b == 'D' {
					// Detach
					return ""
				} else if b == 'w' || b == 'W' {
					// Window list
					// Temporarily leave raw mode to allow TUI
					term.Restore(int(os.Stdin.Fd()), oldState)
					
					// Clear alternate screen buffer for menu overlay
					fmt.Print("\033[2J\033[H")
					
					sessions := fetchSessions()
					selected := selectSession(sessions, name)
					
					if selected != "" && selected != name {
						return selected
					}
					
					// Re-enter raw mode and clear screen for the old session's UI
					fmt.Print("\033[2J\033[H")
					newState, modeErr := term.MakeRaw(int(os.Stdin.Fd()))
					if modeErr == nil {
						oldState = newState
					}
				} else if b == 2 { // Ctrl+B again to send literal Ctrl+B
					outBuf.WriteByte(2)
				} else {
					outBuf.WriteByte(2)
					outBuf.WriteByte(b)
				}
				ctrlB = false
			} else {
				if b == 2 { // Ctrl+B
					ctrlB = true
				} else {
					outBuf.WriteByte(b)
				}
			}
		}

		if outBuf.Len() > 0 {
			conn.Write(outBuf.Bytes())
		}
	}
	return ""
}

func fetchSessions() []string {
	sockPath := getSocketPath()
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return nil
	}
	defer conn.Close()

	req := Request{Action: "ls"}
	json.NewEncoder(conn).Encode(req)

	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil
	}
	return resp.Sessions
}

func selectSession(sessions []string, current string) string {
	if len(sessions) == 0 {
		return ""
	}

	var items []string
	for i, s := range sessions {
		prefix := "  "
		if s == current {
			prefix = "* "
		}
		items = append(items, fmt.Sprintf("%s%d) %s", prefix, i+1, s))
	}

	prompt := promptui.Select{
		Label: "Select Window",
		Items: items,
		Searcher: func(input string, index int) bool {
			return strings.Contains(strings.ToLower(items[index]), strings.ToLower(input))
		},
		Size: 15,
	}

	index, _, err := prompt.Run()
	if err != nil {
		return ""
	}

	return sessions[index]
}

func getTerminalSize() (uint16, uint16) {
	width, height, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 0, 0
	}
	return uint16(height), uint16(width)
}

func ensureDaemonConnected() net.Conn {
	sockPath := getSocketPath()
	conn, err := net.Dial("unix", sockPath)
	if err == nil {
		return conn
	}

	// Start daemon
	cmd := exec.Command(os.Args[0], "daemon")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	if err != nil {
		log.Fatalf("Failed to start daemon: %v", err)
	}

	// Wait for socket
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		conn, err = net.Dial("unix", sockPath)
		if err == nil {
			return conn
		}
	}

	log.Fatalf("Could not connect to daemon: %v", err)
	return nil
}
