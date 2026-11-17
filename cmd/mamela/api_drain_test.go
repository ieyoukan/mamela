package main

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// startAPI runs `mamela api` on a socket in a short directory, with ctx as the signal that stops it.
func startAPI(t *testing.T, ctx context.Context, extra ...string) (socket string, done chan struct {
	code int
	out  string
}) {
	t.Helper()
	tools := toolchainDir(t)
	dir, err := os.MkdirTemp("", "mapi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket = filepath.Join(dir, "api.sock")
	db := filepath.Join(dir, "state.db")
	done = make(chan struct {
		code int
		out  string
	}, 1)
	go func() {
		var out bytes.Buffer
		args := append([]string{"api", "--socket", socket, "--db", db, "--site", "", "--toolchain", tools}, extra...)
		code := run(ctx, args, &out, &out)
		done <- struct {
			code int
			out  string
		}{code, out.String()}
	}()
	for i := 0; ; i++ {
		if _, err := os.Stat(socket); err == nil {
			return socket, done
		}
		if i > 200 {
			t.Fatal("mamela api did not start")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Stopping the service (systemctl restart sends SIGTERM) must let a request that is being handled
// finish: an apply can be in the middle of creating a machine. mamela api used to return, and the
// process to exit, as soon as it was told to stop (B8). The request here is a half-sent one: it is in
// flight, like the apply, and holds the connection open.
func TestAPIFinishesWhatRunsBeforeItStops(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	socket, done := startAPI(t, ctx)

	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	body := `{"digest":"sha256:` + strings.Repeat("a", 64) + `"}`
	conn.Write([]byte("POST /v1/approved HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: " +
		strconv.Itoa(len(body)) + "\r\n\r\n" + body[:10]))
	time.Sleep(200 * time.Millisecond) // being handled

	stop() // SIGTERM
	select {
	case r := <-done:
		t.Fatalf("mamela api returned (exit %d) while a request was being handled:\n%s", r.code, r.out)
	case <-time.After(700 * time.Millisecond):
	}

	conn.Write([]byte(body[10:])) // it arrives, and is answered
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("the request that was being handled was not answered: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d", resp.StatusCode) // 503: it began to be handled after the stop, and is sent again
	}
	select {
	case r := <-done:
		if r.code != 0 {
			t.Fatalf("exit %d:\n%s", r.code, r.out)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("mamela api did not stop after its last request")
	}
}

// shortSocket is a listener on a Unix socket in a short directory (socket paths are limited).
func shortSocket(t *testing.T) (net.Listener, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "mapi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	return l, path
}

func getOver(socket string) (*http.Response, error) {
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}}
	return c.Get("http://x/")
}

// A request that is running when the service is told to stop finishes, and is answered; serveAPI
// returns after it.
func TestServeAPILetsARunningRequestFinish(t *testing.T) {
	l, socket := shortSocket(t)
	running := make(chan struct{})
	hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(running)
		time.Sleep(600 * time.Millisecond) // the apply
		w.Write([]byte("applied"))
	})}
	ctx, stop := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() {
		returned <- serveAPI(ctx, hs, l, 5*time.Second, func() {}, func() { t.Error("aborted a request within the drain limit") },
			func(context.Context) { t.Error("gave up on a request within the drain limit") }, t.Logf)
	}()
	answer := make(chan string, 1)
	go func() {
		resp, err := getOver(socket)
		if err != nil {
			answer <- err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := bufio.NewReader(resp.Body).ReadString(0)
		answer <- b
	}()
	<-running
	stop()
	select {
	case err := <-returned:
		t.Fatalf("serveAPI returned (%v) while the request was running", err)
	case <-time.After(300 * time.Millisecond):
	}
	if got := <-answer; got != "applied" {
		t.Fatalf("the running request was answered %q", got)
	}
	if err := <-returned; err != nil {
		t.Fatal(err)
	}
}

// A request that outlasts the drain limit is cancelled; what it left claimed is given up (its
// outcome becomes unknown) after it has ended, and serveAPI returns.
func TestServeAPICancelsARequestAfterTheDrainLimit(t *testing.T) {
	l, socket := shortSocket(t)
	base, abort := context.WithCancel(context.Background())
	running := make(chan struct{})
	var mu sync.Mutex
	var events []string
	note := func(e string) { mu.Lock(); events = append(events, e); mu.Unlock() }
	hs := &http.Server{
		BaseContext: func(net.Listener) context.Context { return base },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(running)
			<-r.Context().Done() // an apply that does not end by itself
			note("request ended")
		}),
	}
	ctx, stop := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() {
		returned <- serveAPI(ctx, hs, l, 300*time.Millisecond, func() { note("stop") }, func() { note("abort"); abort() },
			func(context.Context) { note("give up") }, t.Logf)
	}()
	go getOver(socket)
	<-running
	stop()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serveAPI did not return after the drain limit")
	}
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(events, ","); got != "stop,abort,request ended,give up" {
		t.Fatalf("order of events: %s (the claims are given up after the request has ended)", got)
	}
}
