// Package contract preserves Cairn's tolerant output extraction and task results.
package contract

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type Direction struct {
	From        []string `json:"from"`
	Description string   `json:"description"`
}
type Result struct {
	Kind     string
	Intents  []Direction
	Complete Direction
	Fact     string
}

func Extract(text string) (map[string]json.RawMessage, error) {
	text = strings.TrimSpace(text)
	var whole map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &whole) == nil && whole != nil {
		return whole, nil
	}
	for index, ch := range text {
		if ch != '{' {
			continue
		}
		var candidate map[string]json.RawMessage
		if json.NewDecoder(strings.NewReader(text[index:])).Decode(&candidate) == nil && candidate != nil {
			return candidate, nil
		}
	}
	return nil, errors.New("no JSON object found in output")
}
func object(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	err := json.Unmarshal(raw, &m)
	if err == nil && m == nil {
		err = errors.New("expected object")
	}
	return m, err
}
func text(raw json.RawMessage) (string, error) {
	var s string
	err := json.Unmarshal(raw, &s)
	s = strings.TrimSpace(s)
	if err == nil && s == "" {
		err = errors.New("description is required")
	}
	return s, err
}
func direction(raw json.RawMessage) (Direction, error) {
	var d Direction
	err := json.Unmarshal(raw, &d)
	if err != nil {
		return d, err
	}
	d.Description = strings.TrimSpace(d.Description)
	if d.Description == "" || len(d.From) == 0 {
		return d, errors.New("direction requires from and description")
	}
	for n, id := range d.From {
		d.From[n] = strings.TrimSpace(id)
		if d.From[n] == "" {
			return d, errors.New("empty fact id")
		}
	}
	return d, nil
}
func Parse(output, kind string, conclude bool, openIntents, maxIntents int) (Result, error) {
	m, err := Extract(output)
	if err != nil {
		return Result{}, err
	}
	data := m
	var accepted bool
	wrapped := false
	if raw, ok := m["accepted"]; ok && json.Unmarshal(raw, &accepted) == nil && string(raw) != "null" {
		if !accepted {
			return Result{Kind: "rejected"}, nil
		}
		wrapped = true
		data, err = object(m["data"])
		if err != nil {
			return Result{}, errors.New("data must be an object")
		}
	}
	if !wrapped {
		valid := false
		switch kind {
		case "reason":
			_, a := data["complete"]
			_, b := data["intents"]
			_, c := data["intent"]
			valid = len(data) == 1 && (a || b || c)
		case "explore":
			_, ok := data["description"]
			valid = len(data) == 1 && ok
		case "bootstrap":
			_, f := data["fact"]
			_, c := data["complete"]
			valid = f && ((len(data) == 2 && c) || (conclude && len(data) == 1))
		}
		if !valid {
			return Result{}, errors.New("accepted must be true or false")
		}
	}
	switch kind {
	case "reason":
		complete := data["complete"]
		intents := data["intents"]
		if len(intents) == 0 || string(intents) == "null" {
			if singular := data["intent"]; len(singular) > 0 && string(singular) != "null" {
				intents = append(append(json.RawMessage{'['}, singular...), ']')
			}
		}
		if len(complete) > 0 && string(complete) != "null" {
			if len(intents) > 0 && string(intents) != "null" {
				return Result{}, errors.New("complete and intents cannot coexist")
			}
			d, err := direction(complete)
			return Result{Kind: "complete", Complete: d}, err
		}
		if len(intents) > 0 && string(intents) != "null" {
			var entries []json.RawMessage
			if err = json.Unmarshal(intents, &entries); err != nil {
				return Result{}, err
			}
			if len(entries) == 0 && openIntents == 0 {
				return Result{}, errors.New("intents must not be empty when no intents are open")
			}
			out := Result{Kind: "intents"}
			for _, raw := range entries {
				d, err := direction(raw)
				if err != nil {
					return Result{}, err
				}
				out.Intents = append(out.Intents, d)
			}
			if len(out.Intents) > maxIntents {
				out.Intents = out.Intents[:maxIntents]
			}
			if len(out.Intents) == 0 {
				out.Kind = "noop"
			}
			return out, nil
		}
		if openIntents == 0 {
			return Result{}, errors.New("intents required when no intents are open")
		}
		return Result{Kind: "noop"}, nil
	case "explore":
		s, err := text(data["description"])
		return Result{Kind: "fact", Fact: s}, err
	case "bootstrap":
		if conclude {
			for key := range data {
				if key != "fact" && key != "complete" {
					return Result{}, errors.New("unexpected conclude field")
				}
			}
		}
		fact, err := object(data["fact"])
		if err != nil {
			return Result{}, errors.New("fact is required")
		}
		desc, err := text(fact["description"])
		if err != nil {
			return Result{}, err
		}
		if conclude {
			return Result{Kind: "fact", Fact: desc}, nil
		}
		complete, err := object(data["complete"])
		if err != nil {
			return Result{}, errors.New("complete is required")
		}
		why, err := text(complete["description"])
		return Result{Kind: "complete", Fact: desc, Complete: Direction{Description: why}}, err
	}
	return Result{}, fmt.Errorf("unknown task type %q", kind)
}
