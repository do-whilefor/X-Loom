//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"xloom/internal/board"
	"xloom/internal/server"
)

func serve(ctx context.Context, args []string, errOut io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(errOut)
	host := fs.String("host", "127.0.0.1", "HTTP bind address")
	port := fs.Int("port", 8000, "HTTP port")
	home, _ := os.UserHomeDir()
	db := fs.String("db-path", filepath.Join(home, ".local", "share", "xloom", "xloom.db"), "SQLite database path")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *port < 1 || *port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	if *db == "" {
		return errors.New("db-path must not be empty")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	store, err := board.Open(*db)
	if err != nil {
		return err
	}
	defer store.Close()
	srv := &http.Server{
		Addr: net.JoinHostPort(*host, strconv.Itoa(*port)), Handler: server.New(store),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 60 * time.Second, IdleTimeout: 90 * time.Second,
	}
	listener, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return err
	}
	fmt.Fprintln(errOut, "xloom serving", listener.Addr())
	return serveHTTP(ctx, srv, listener)
}

func serveHTTP(ctx context.Context, srv *http.Server, listener net.Listener) error {
	done := make(chan error, 1)
	go func() { done <- srv.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := srv.Shutdown(shutdown)
		if err != nil {
			_ = srv.Close()
		}
		<-done
		return err
	}
}
