//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"io"

	"xloom/internal/config"
	"xloom/internal/dispatcher"
	"xloom/internal/docker"
)

func dispatch(ctx context.Context, args []string, errOut io.Writer) error {
	fs := flag.NewFlagSet("dispatch", flag.ContinueOnError)
	fs.SetOutput(errOut)
	path := fs.String("config", "", "Dispatcher YAML configuration (required)")
	health := fs.Bool("startup-healthcheck-only", false, "Check configured models and exit")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *path == "" {
		return errors.New("--config is required")
	}
	c, err := config.Load(*path)
	if err != nil {
		return err
	}
	runner := docker.New(c.Container)
	defer runner.Close()
	scheduler := dispatcher.New(c, runner)
	if *health {
		return scheduler.Health(ctx, true)
	}
	return scheduler.Run(ctx)
}
