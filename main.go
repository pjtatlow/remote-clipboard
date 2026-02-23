package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
	"golang.design/x/clipboard"
)

const (
	SOCKET_PATH          = "/tmp/clipboard.sock"
	LOCAL_ADDRESS_HEADER = "X-Local-Address"

	serviceName     = "dev.pjtatlow.remote-clipboard"
	serviceNameUnit = "remote-clipboard"
)

var rootCmd = &cobra.Command{
	Use:   "remote-clipboard",
	Short: "Share clipboard between local and remote machines",
}

var copyCmd = &cobra.Command{
	Use:   "copy",
	Short: "Copy stdin to the remote clipboard",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		copyToRemoteClipboard()
	},
}

var pasteCmd = &cobra.Command{
	Use:   "paste",
	Short: "Paste from the remote clipboard to stdout",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		pasteFromRemoteClipboard()
	},
}

var remoteCmd = &cobra.Command{
	Use:   "remote",
	Short: "Run the remote clipboard server",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		runRemote()
	},
}

var localCmd = &cobra.Command{
	Use:   "local [hosts...]",
	Short: "Run the local clipboard client",
	Args:  cobra.MinimumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		runLocal(args)
	},
}

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Install as a system service",
}

var installLocalCmd = &cobra.Command{
	Use:   "local [hosts...]",
	Short: "Install the local clipboard client as a service",
	Args:  cobra.MinimumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		runInstall(append([]string{"local"}, args...))
	},
}

var installRemoteCmd = &cobra.Command{
	Use:   "remote",
	Short: "Install the remote clipboard server as a service",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		runInstall([]string{"remote"})
	},
}

var uninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Uninstall the system service",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		runUninstall()
	},
}

func init() {
	installCmd.AddCommand(installLocalCmd, installRemoteCmd)
	rootCmd.AddCommand(copyCmd, pasteCmd, remoteCmd, localCmd, installCmd, uninstallCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func unixClient() http.Client {
	conn, err := net.Dial("unix", SOCKET_PATH)
	if err != nil {
		panic(err)
	}

	return http.Client{
		Timeout: time.Second * 5,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return conn, nil
			},
		},
	}
}

func getLocalAddressFromSSH() string {
	return strings.Split(os.Getenv("SSH_CLIENT"), " ")[0]
}

func copyToRemoteClipboard() {
	client := unixClient()
	var buf bytes.Buffer
	io.Copy(&buf, os.Stdin)
	if buf.Len() <= 2 {
		return
	}
	req, err := http.NewRequest("POST", "http://foo/copy", &buf)
	if err != nil {
		log.Fatal(err)
	}
	req.Header.Set(LOCAL_ADDRESS_HEADER, getLocalAddressFromSSH())
	res, err := client.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer res.Body.Close()
}

func pasteFromRemoteClipboard() {
	client := unixClient()
	req, err := http.NewRequest("GET", "http://bar/paste", nil)
	if err != nil {
		log.Fatal(err)
	}
	req.Header.Set(LOCAL_ADDRESS_HEADER, getLocalAddressFromSSH())
	res, err := client.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer res.Body.Close()
	io.Copy(os.Stdout, res.Body)
}

var upgrader = websocket.Upgrader{
	HandshakeTimeout: time.Second * 5,
	ReadBufferSize:   1024 * 16,
	WriteBufferSize:  1024 * 16,
}

type localClipboard struct {
	conn  *websocket.Conn
	value []byte
}

type clipboardManager struct {
	clients       map[string]*localClipboard
	mu            sync.Mutex
	lastClipboard []byte
}

func (m *clipboardManager) addClient(address string, c *localClipboard) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clients[address] = c
}

func (m *clipboardManager) removeClient(address string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.clients, address)
}

func (m *clipboardManager) setValue(address string, value []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastClipboard = value
	if c, ok := m.clients[address]; ok {
		c.value = value
	}
}

func (m *clipboardManager) broadcastToAllClients(data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastClipboard = data
	for addr, c := range m.clients {
		c.value = data
		if err := c.conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
			log.Printf("broadcast to %s: %v", addr, err)
			c.conn.Close()
		}
	}
}

func (m *clipboardManager) copyToLocalClipboard(address string, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if c, ok := m.clients[address]; ok {
		c.value = value

		err := c.conn.WriteMessage(websocket.BinaryMessage, value)
		if err != nil {
			c.conn.Close()
			return err
		}
	}
	return fmt.Errorf("no client found with address %s", address)
}

func (m *clipboardManager) pasteFromLocalClipboard(address string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.clients[address]; ok {
		return c.value, nil
	}
	return nil, fmt.Errorf("no client found with address %s", address)
}

func runRemote() {
	m := clipboardManager{
		clients: make(map[string]*localClipboard),
	}

	socketMux := http.NewServeMux()

	socketMux.HandleFunc("/copy", func(w http.ResponseWriter, r *http.Request) {
		log.Println("copy")
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		address := r.Header.Get(LOCAL_ADDRESS_HEADER)
		if address == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		var b bytes.Buffer
		_, err := io.Copy(&b, r.Body)
		if err != nil {
			log.Printf("Error reading body: %v\n", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// Copy to local clipboard asynchronously to neovim doesn't hang
		go func() {
			m.copyToLocalClipboard(address, b.Bytes())
		}()
	})

	socketMux.HandleFunc("/paste", func(w http.ResponseWriter, r *http.Request) {
		log.Println("paste")
		if r.Method != "GET" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		address := r.Header.Get(LOCAL_ADDRESS_HEADER)
		if address == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		data, err := m.pasteFromLocalClipboard(address)
		if err != nil {
			log.Printf("Error reading clipboard: %v\n", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		w.Write(data)
	})

	unixSocket, err := net.Listen("unix", SOCKET_PATH)
	if err != nil {
		panic(err)
	}

	var wg sync.WaitGroup

	socketServer := http.Server{
		Handler: socketMux,
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		socketServer.Serve(unixSocket)
	}()

	wsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteIP, _, err := net.SplitHostPort(r.RemoteAddr)
		log.Println("ws connection from", remoteIP)

		c, err := upgrader.Upgrade(w, r, nil)
		m.addClient(remoteIP, &localClipboard{conn: c})
		defer m.removeClient(remoteIP)
		if err != nil {
			log.Print("upgrade:", err)
			return
		}
		defer c.Close()
		for {
			mt, data, err := c.ReadMessage()
			if err != nil {
				log.Println("read:", err)
				break
			}
			if mt == websocket.BinaryMessage {
				m.setValue(remoteIP, data)
			}
		}
	})
	server := &http.Server{
		Handler: wsHandler,
	}

	wg.Add(1)
	l, err := net.Listen("tcp", "0.0.0.0:2679")
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		defer wg.Done()
		server.Serve(l)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		<-c
		cancel()
	}()

	// Watch the remote system clipboard for changes
	go func() {
		cb := clipboard.Watch(ctx, clipboard.FmtText)
		for {
			select {
			case <-ctx.Done():
				return
			case data := <-cb:
				m.mu.Lock()
				isEcho := bytes.Equal(data, m.lastClipboard)
				m.mu.Unlock()
				if isEcho {
					continue
				}
				m.broadcastToAllClients(data)
			}
		}
	}()

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second*1)
	defer shutdownCancel()

	go socketServer.Shutdown(shutdownCtx)
	go server.Shutdown(shutdownCtx)

	wg.Wait()
	os.Remove(SOCKET_PATH)
}

// --- install / uninstall commands ---

var launchdPlist = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>{{.Label}}</string>
  <key>ProgramArguments</key>
  <array>
    <string>{{.ExecPath}}</string>
{{- range .Args}}
    <string>{{.}}</string>
{{- end}}
  </array>
  <key>KeepAlive</key>
  <true/>
  <key>RunAtLoad</key>
  <true/>
  <key>StandardOutPath</key>
  <string>{{.LogDir}}/stdout.log</string>
  <key>StandardErrorPath</key>
  <string>{{.LogDir}}/stderr.log</string>
</dict>
</plist>
`))

var systemdUnit = template.Must(template.New("unit").Parse(`[Unit]
Description=Remote Clipboard Service

[Service]
ExecStart={{.ExecPath}}{{range .Args}} {{.}}{{end}}
Restart=on-failure
RestartSec=5
StandardOutput=append:{{.LogDir}}/stdout.log
StandardError=append:{{.LogDir}}/stderr.log

[Install]
WantedBy=default.target
`))

func runInstall(args []string) {
	programArgs := args

	rawPath, err := os.Executable()
	if err != nil {
		log.Fatalf("Could not determine executable path: %v", err)
	}
	execPath, err := filepath.EvalSymlinks(rawPath)
	if err != nil {
		log.Fatalf("Could not resolve symlinks: %v", err)
	}

	switch runtime.GOOS {
	case "darwin":
		installLaunchd(execPath, programArgs)
	case "linux":
		installSystemd(execPath, programArgs)
	default:
		log.Fatalf("Unsupported OS: %s", runtime.GOOS)
	}
}

func installLaunchd(execPath string, programArgs []string) {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("Could not determine home directory: %v", err)
	}

	agentsDir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(agentsDir, 0755); err != nil {
		log.Fatalf("Could not create LaunchAgents directory: %v", err)
	}

	logDir := filepath.Join(home, "Library", "Logs", "remote-clipboard")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		log.Fatalf("Could not create log directory: %v", err)
	}

	plistPath := filepath.Join(agentsDir, serviceName+".plist")

	data := struct {
		Label    string
		ExecPath string
		Args     []string
		LogDir   string
	}{
		Label:    serviceName,
		ExecPath: execPath,
		Args:     programArgs,
		LogDir:   logDir,
	}

	var buf bytes.Buffer
	if err := launchdPlist.Execute(&buf, data); err != nil {
		log.Fatalf("Could not render plist template: %v", err)
	}

	if err := os.WriteFile(plistPath, buf.Bytes(), 0644); err != nil {
		log.Fatalf("Could not write plist: %v", err)
	}

	// Unload existing (ignore errors — may not be loaded)
	exec.Command("launchctl", "unload", plistPath).Run()

	if out, err := exec.Command("launchctl", "load", plistPath).CombinedOutput(); err != nil {
		log.Fatalf("launchctl load failed: %v\n%s", err, out)
	}

	fmt.Println("Service installed and started.")
	fmt.Printf("  Plist:  %s\n", plistPath)
	fmt.Printf("  Logs:   %s\n", logDir)
	fmt.Printf("  Status: launchctl list | grep %s\n", serviceName)
}

func installSystemd(execPath string, programArgs []string) {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("Could not determine home directory: %v", err)
	}

	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0755); err != nil {
		log.Fatalf("Could not create systemd user directory: %v", err)
	}

	logDir := filepath.Join(home, ".local", "log", "remote-clipboard")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		log.Fatalf("Could not create log directory: %v", err)
	}

	unitPath := filepath.Join(unitDir, serviceNameUnit+".service")

	data := struct {
		ExecPath string
		Args     []string
		LogDir   string
	}{
		ExecPath: execPath,
		Args:     programArgs,
		LogDir:   logDir,
	}

	var buf bytes.Buffer
	if err := systemdUnit.Execute(&buf, data); err != nil {
		log.Fatalf("Could not render unit template: %v", err)
	}

	// Stop existing service (ignore errors)
	exec.Command("systemctl", "--user", "stop", serviceNameUnit).Run()

	if err := os.WriteFile(unitPath, buf.Bytes(), 0644); err != nil {
		log.Fatalf("Could not write unit file: %v", err)
	}

	for _, args := range [][]string{
		{"systemctl", "--user", "daemon-reload"},
		{"systemctl", "--user", "enable", serviceNameUnit},
		{"systemctl", "--user", "start", serviceNameUnit},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			log.Fatalf("%s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	fmt.Println("Service installed and started.")
	fmt.Printf("  Unit:   %s\n", unitPath)
	fmt.Printf("  Logs:   %s\n", logDir)
	fmt.Printf("  Status: systemctl --user status %s\n", serviceNameUnit)
}

func runUninstall() {
	switch runtime.GOOS {
	case "darwin":
		uninstallLaunchd()
	case "linux":
		uninstallSystemd()
	default:
		log.Fatalf("Unsupported OS: %s", runtime.GOOS)
	}
}

func uninstallLaunchd() {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("Could not determine home directory: %v", err)
	}

	plistPath := filepath.Join(home, "Library", "LaunchAgents", serviceName+".plist")

	// Unload (ignore errors)
	exec.Command("launchctl", "unload", plistPath).Run()

	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		log.Fatalf("Could not remove plist: %v", err)
	}

	fmt.Println("Service uninstalled.")
	fmt.Println("  Log files have been preserved.")
}

func uninstallSystemd() {
	for _, args := range [][]string{
		{"systemctl", "--user", "stop", serviceNameUnit},
		{"systemctl", "--user", "disable", serviceNameUnit},
	} {
		exec.Command(args[0], args[1:]...).Run() // ignore errors
	}

	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("Could not determine home directory: %v", err)
	}

	unitPath := filepath.Join(home, ".config", "systemd", "user", serviceNameUnit+".service")
	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		log.Fatalf("Could not remove unit file: %v", err)
	}

	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		log.Fatalf("daemon-reload failed: %v\n%s", err, out)
	}

	fmt.Println("Service uninstalled.")
	fmt.Println("  Log files have been preserved.")
}

func runLocal(hosts []string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		<-c
		cancel()
	}()

	type remote struct {
		host string
		conn *websocket.Conn
	}

	var (
		mu            sync.Mutex
		remotes       []*remote
		lastClipboard []byte
	)

	// sendToAll sends data to all remotes except exclude (which may be nil).
	sendToAll := func(data []byte, exclude *remote) {
		mu.Lock()
		defer mu.Unlock()
		lastClipboard = data
		for _, r := range remotes {
			if r == exclude {
				continue
			}
			if err := r.conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
				log.Printf("write to %s: %v", r.host, err)
			}
		}
	}

	for _, host := range hosts {
		if !strings.Contains(host, ":") {
			host = host + ":2679"
		}
		u := url.URL{Scheme: "ws", Host: host, Path: "/"}
		log.Printf("connecting to %s", u.String())

		ws, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
		if err != nil {
			log.Printf("dial %s: %v", host, err)
			continue
		}
		r := &remote{host: host, conn: ws}
		remotes = append(remotes, r)

		go func(r *remote) {
			defer r.conn.Close()
			for {
				mt, data, err := r.conn.ReadMessage()
				if err != nil {
					log.Printf("read from %s: %v", r.host, err)
					return
				}
				if mt == websocket.BinaryMessage {
					clipboard.Write(clipboard.FmtText, data)
					sendToAll(data, r)
				}
			}
		}(r)
	}

	if len(remotes) == 0 {
		log.Fatal("failed to connect to any hosts")
	}

	pb := clipboard.Watch(ctx, clipboard.FmtText)
	for {
		select {
		case <-ctx.Done():
			return

		case data := <-pb:
			mu.Lock()
			isEcho := bytes.Equal(data, lastClipboard)
			mu.Unlock()
			if isEcho {
				continue
			}
			sendToAll(data, nil)
		}
	}
}
