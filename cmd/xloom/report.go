//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io"

	"xloom/internal/report"
)

func renderReport(args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(errOut)
	root := fs.String("root", ".", "Workspace root containing source, evidence and outputs")
	source := fs.String("source", "", "Structured JSON source (required)")
	output := fs.String("output", "", "Output prefix for .json, .md and .index.json (required)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *source == "" || *output == "" {
		return errors.New("--source and --output are required")
	}
	index, err := report.Generate(*root, *source, *output)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(struct {
		Output          string        `json:"output"`
		Counts          report.Counts `json:"counts"`
		ValidationScope string        `json:"validation_scope"`
	}{*output, index.Counts, index.ValidationScope})
}
