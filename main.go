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
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"golang.design/x/clipboard"
)

const (
	SOCKET_PATH          = "/tmp/clipboard.sock"
	LOCAL_ADDRESS_HEADER = "X-Local-Address"
)

func main() {
	if len(os.Args[1]) < 2 {
		log.Fatal("Invalid arguments")
	}

	command := os.Args[1]

	switch command {
	case "copy":
		copyToRemoteClipboard()
	case "paste":
		pasteFromRemoteClipboard()
	case "remote":
		runRemote()
	case "local":
		runLocal()
	default:
		log.Fatal("unrecognized command", command)
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
	clients map[string]*localClipboard
	mu      sync.Mutex
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
	if c, ok := m.clients[address]; ok {
		c.value = value
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

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	<-c
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second*1)
	defer cancel()

	go socketServer.Shutdown(shutdownCtx)
	go server.Shutdown(shutdownCtx)

	wg.Wait()
	os.Remove(SOCKET_PATH)
}

func runLocal() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		<-c
		cancel()
	}()

	u := url.URL{Scheme: "ws", Host: "dragonite:2679", Path: "/"}
	log.Printf("connecting to %s", u.String())

	ws, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		log.Fatal("dial:", err)
	}
	defer ws.Close()

	go func() {
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				log.Println("read:", err)
				cancel()
				return
			}
			if mt == websocket.BinaryMessage {
				clipboard.Write(clipboard.FmtText, data)
			}
		}
	}()

	pb := clipboard.Watch(ctx, clipboard.FmtText)
	for {
		select {
		case <-ctx.Done():
			return

		case data := <-pb:
			if err := ws.WriteMessage(websocket.BinaryMessage, data); err != nil {
				log.Println("write:", err)
				cancel()
			}
		}
	}
}
