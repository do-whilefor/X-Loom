//go:build linux

package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestInvalidCommandsFailBeforeSideEffects(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"serve", "--port", "0"}, {"dispatch"}, {"worker"}, {"worker", "--job", "a", "--cancel", "b"}, {"worker", "--job", "a", "--force"}, {"serve", "unexpected"}} {
		if err := run(context.Background(), args, io.Discard, io.Discard); err == nil {
			t.Errorf("%v: expected error", args)
		}
	}
}

func TestHelpDoesNotStartServices(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"serve", "--help"}, {"dispatch", "--help"}, {"worker", "--help"}} {
		var out bytes.Buffer
		if err := run(context.Background(), args, &out, &out); err != nil || !strings.Contains(out.String(), "Usage") {
			t.Fatalf("%v: output %q, error %v", args, out.String(), err)
		}
	}
}

func TestHTTPGracefulCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })}
	go func() { done <- serveHTTP(ctx, srv, listener) }()
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get("http://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatal(response.Status)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not shut down")
	}
}
