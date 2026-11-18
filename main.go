// main.go — the entry <-> env lifecycle: serve the forge on an ephemeral port,
// register that port with the env, wait until the env says the trial is done,
// then close and exit. Kept deliberately thin; the whole forge lives behind
// newApp() in app.go, and the serving transport is chosen at build time
// (transport_raw.go / transport_std.go).

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

func main() {
	envURL := os.Getenv("MAKEFUN_ENV_URL")
	if envURL == "" {
		fmt.Fprintln(os.Stderr, "MAKEFUN_ENV_URL is not set: the env base URL is required")
		os.Exit(1)
	}
	application := newApp(os.Getenv("MAKEFUN_SHOP_TOKEN"))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen failed:", err)
		os.Exit(1)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	done := make(chan struct{})
	go func() {
		defer close(done)
		serve(listener, application.handle)
	}()

	if err := register(envURL, port); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	for {
		time.Sleep(250 * time.Millisecond)
		resp, err := client.Get(envURL + "/done")
		if err != nil {
			continue
		}
		var body struct {
			Done bool `json:"done"`
		}
		json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 && body.Done {
			break
		}
	}

	listener.Close()
	<-done
	os.Exit(0)
}

// POST the registration until the env accepts it (200/409), backing off between attempts.
func register(envURL string, port int) error {
	deadline := time.Now().Add(30 * time.Second)
	delay := 250 * time.Millisecond
	client := &http.Client{Timeout: 10 * time.Second}
	payload, _ := json.Marshal(map[string]int{"port": port})
	for {
		resp, err := client.Post(envURL+"/register", "application/json", bytes.NewReader(payload))
		if err == nil {
			status := resp.StatusCode
			resp.Body.Close()
			if status == 200 || status == 409 {
				return nil
			}
			if status == 400 || status == 405 {
				return fmt.Errorf("registration refused with HTTP %d", status)
			}
		}
		if time.Now().After(deadline) {
			return errors.New("registration did not succeed inside 30 s")
		}
		time.Sleep(delay)
		delay *= 2
		if delay > 2*time.Second {
			delay = 2 * time.Second
		}
	}
}
