//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"io"

	"xloom/internal/process"
	"xloom/internal/worker"
)

func work(ctx context.Context, args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	fs.SetOutput(errOut)
	job := fs.String("job", "", "Task JSON file")
	cancel := fs.String("cancel", "", "Execution directory to cancel")
	force := fs.Bool("force", false, "Force cancellation of this execution")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if (*job == "") == (*cancel == "") {
		return errors.New("exactly one of --job or --cancel is required")
	}
	if *cancel != "" {
		return process.Cancel(*cancel, *force)
	}
	if *force {
		return errors.New("--force requires --cancel")
	}
	return worker.Execute(ctx, *job, out)
}
