// Entry lifecycle: bind an ephemeral port, register it with the env, then poll
// /done until the trial is over and exit 0. The forge itself lives in app.go;
// this file only owns the handshake with the env that runs the suite.
//
//  1. MAKEFUN_ENV_URL names the env origin.
//  2. Listen on port 0 and let the kernel pick a free port. Never 8080 — the
//     env owns it.
//  3. POST $MAKEFUN_ENV_URL/register {"port": N}. The env records the address
//     the request came from plus N and dials it from then on. 200 is the
//     normal answer; 409 means another entry already registered first, which
//     is not this process's to fix — it waits like any other.
//  4. GET $MAKEFUN_ENV_URL/done until the body is {"done": true}. The env
//     answers at once ({"done": false} while the suite runs), so this is a
//     poll, not a long-held request.
//  5. Close the server and exit 0. The harness then stops the env, which
//     writes the export.
//
// Without MAKEFUN_ENV_URL the process serves on FORGE_PORT (default 8080)
// forever instead: a local smoke run, where there is no env to register with.
// The trial always sets MAKEFUN_ENV_URL, so the trial path is the one above.
//
// The trial is replayed and the export must come out byte-identical, so the
// forge is fully deterministic: fixed commit timestamps and REST dates,
// counter-based request ids, sorted pack order, a constant Date header, no
// wall clock and no unseeded randomness anywhere.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// callTimeout is how long one register or done request may take before it is
// abandoned and retried.
const callTimeout = 30 * time.Second

// retryStart and retryCap bound the exponential backoff between retries.
const retryStart = 250 * time.Millisecond
const retryCap = 2 * time.Second

// retryDelay is the exponential backoff after `attempt` failed requests,
// capped at retryCap.
func retryDelay(attempt int) time.Duration {
	if attempt > 8 {
		attempt = 8
	}
	delay := retryStart * time.Duration(1<<uint(attempt))
	if delay > retryCap {
		delay = retryCap
	}
	return delay
}

// registerPort POSTs the bound port until the env accepts it. 400/405 are
// contract errors and end the program; anything else (a network error,
// another status) is retried, since the env may still be starting.
func registerPort(envOrigin string, port int) error {
	client := &http.Client{Timeout: callTimeout}
	body, _ := json.Marshal(map[string]int{"port": port})
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest("POST", envOrigin+"/register", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err == nil {
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode == 200 || res.StatusCode == 409 {
				return nil
			}
			if res.StatusCode == 400 || res.StatusCode == 405 {
				return fmt.Errorf("register answered HTTP %d", res.StatusCode)
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

// waitUntilDone polls GET /done until it answers {"done": true}; it never
// returns before that.
func waitUntilDone(envOrigin string) {
	client := &http.Client{Timeout: callTimeout}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest("GET", envOrigin+"/done", nil)
		if err == nil {
			res, err := client.Do(req)
			if err == nil {
				var parsed struct {
					Done bool `json:"done"`
				}
				decodeErr := json.NewDecoder(res.Body).Decode(&parsed)
				res.Body.Close()
				if res.StatusCode >= 200 && res.StatusCode < 300 && decodeErr == nil && parsed.Done {
					return
				}
			}
		}
		// The env is not answering yet, or the poll timed out: it is not
		// done, so poll again.
		time.Sleep(retryDelay(attempt))
	}
}

func run() error {
	envOrigin := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	srv := newServer()
	if envOrigin == "" {
		// Local smoke run: serve on FORGE_PORT forever.
		port := os.Getenv("FORGE_PORT")
		if port == "" {
			port = "8080"
		}
		if _, err := strconv.Atoi(port); err != nil {
			return fmt.Errorf("FORGE_PORT %q is not a port number", port)
		}
		listener, err := net.Listen("tcp", ":"+port)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "[entry] listening on port %s (local mode, no env)\n", port)
		return srv.http.Serve(listener)
	}

	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return err
	}
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return fmt.Errorf("the server did not bind an ephemeral TCP port")
	}
	fmt.Fprintf(os.Stdout, "[entry] listening on port %d\n", addr.Port)

	go func() {
		// Serve returns when the listener closes; Shutdown below handles the
		// normal path, so errors here are only logged.
		if err := srv.http.Serve(listener); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "[entry] serve error: %v\n", err)
		}
	}()

	if err := registerPort(envOrigin, addr.Port); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "[entry] registered; waiting for the env to say done")

	waitUntilDone(envOrigin)
	// Let in-flight responses flush before the process exits: Shutdown stops
	// new connections and waits for the existing ones to finish.
	if err := srv.http.Shutdown(context.Background()); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "[entry] done")
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "[entry] %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}
