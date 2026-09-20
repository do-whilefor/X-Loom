package config

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

type Runtime struct {
	Interval          int    `yaml:"interval"`
	MaxWorkers        int    `yaml:"max_workers"`
	MaxProjects       int    `yaml:"max_running_projects"`
	MaxProjectWorkers int    `yaml:"max_project_workers"`
	HealthTimeout     int    `yaml:"healthcheck_timeout"`
	HealthMode        string `yaml:"worker_healthcheck"`
	Execution         string `yaml:"execution"`
	PromptGroup       string `yaml:"prompt_group"`
}
type Task struct {
	Timeout         int `yaml:"timeout" json:"timeout"`
	ConcludeTimeout int `yaml:"conclude_timeout" json:"conclude_timeout"`
	MaxIntents      int `yaml:"max_intents" json:"max_intents"`
}
type Tasks struct {
	Bootstrap Task `yaml:"bootstrap"`
	Reason    Task `yaml:"reason"`
	Explore   Task `yaml:"explore"`
}
type Container struct {
	Image           string   `yaml:"image"`
	Network         string   `yaml:"network_mode"`
	CompletedAction string   `yaml:"completed_action"`
	CapAdd          []string `yaml:"cap_add"`
	Socket          string   `yaml:"socket"`
	Namespace       string   `yaml:"namespace"`
}
type Worker struct {
	Name       string            `yaml:"name"`
	Type       string            `yaml:"type"`
	TaskTypes  []string          `yaml:"task_types"`
	MaxRunning int               `yaml:"max_running"`
	Priority   int               `yaml:"priority"`
	Env        map[string]string `yaml:"env"`
}
type Config struct {
	Server    string            `yaml:"server"`
	Runtime   Runtime           `yaml:"runtime"`
	Tasks     Tasks             `yaml:"tasks"`
	Container Container         `yaml:"container"`
	CommonEnv map[string]string `yaml:"common_env"`
	Workers   []Worker          `yaml:"workers"`
}

func Load(path string) (Config, error) {
	var c Config
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	d := yaml.NewDecoder(f)
	d.KnownFields(true)
	if err = d.Decode(&c); err != nil {
		return c, err
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return c, fmt.Errorf("configuration must contain exactly one YAML document")
	}
	return c, c.Validate()
}
func (c *Config) Validate() error {
	if c.Runtime.Execution == "" {
		c.Runtime.Execution = "container"
	}
	if c.Runtime.HealthMode == "" {
		c.Runtime.HealthMode = "startup_only"
	}
	if c.Runtime.PromptGroup == "" {
		c.Runtime.PromptGroup = "default"
	}
	if c.Tasks.Reason.MaxIntents == 0 {
		c.Tasks.Reason.MaxIntents = 3
	}
	if c.Container.Socket == "" {
		c.Container.Socket = "/var/run/docker.sock"
	}
	if c.Container.Namespace == "" {
		c.Container.Namespace = "xloom"
	}
	u, err := url.Parse(c.Server)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("server must be an HTTP URL")
	}
	if c.Runtime.Execution != "container" {
		return fmt.Errorf("only container execution is supported")
	}
	if c.Runtime.PromptGroup != "default" {
		return fmt.Errorf("only the default prompt group is supported")
	}
	if !slices.Contains([]string{"disabled", "startup_only", "startup_and_task"}, c.Runtime.HealthMode) {
		return fmt.Errorf("unknown worker_healthcheck mode")
	}
	if c.Runtime.Interval <= 0 || c.Runtime.MaxWorkers <= 0 || c.Runtime.MaxProjects <= 0 || c.Runtime.MaxProjectWorkers <= 0 || c.Runtime.HealthTimeout <= 0 {
		return fmt.Errorf("invalid runtime limits")
	}
	for _, t := range []Task{c.Tasks.Bootstrap, c.Tasks.Reason, c.Tasks.Explore} {
		if t.Timeout < 0 {
			return fmt.Errorf("task timeout must be nonnegative (0 disables the execution deadline)")
		}
	}
	if c.Tasks.Bootstrap.ConcludeTimeout <= 0 || c.Tasks.Explore.ConcludeTimeout <= 0 || c.Tasks.Reason.MaxIntents <= 0 {
		return fmt.Errorf("conclude timeouts and max_intents must be positive")
	}
	if c.Container.Image == "" || c.Container.Network == "" || !slices.Contains([]string{"stop", "remove"}, c.Container.CompletedAction) {
		return fmt.Errorf("invalid container configuration")
	}
	for _, ch := range c.Container.Namespace {
		if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
			return fmt.Errorf("container namespace must contain lowercase letters, digits or hyphens")
		}
	}
	if len(c.Workers) == 0 {
		return fmt.Errorf("at least one worker is required")
	}
	seen := map[string]bool{}
	for n := range c.Workers {
		w := &c.Workers[n]
		if strings.TrimSpace(w.Name) == "" || seen[w.Name] || w.MaxRunning <= 0 || w.Priority < 0 {
			return fmt.Errorf("invalid or duplicate worker %q", w.Name)
		}
		seen[w.Name] = true
		if w.Type == "" {
			w.Type = "go"
		}
		if w.Type != "go" && w.Type != "mock" {
			return fmt.Errorf("worker %q must use go or mock", w.Name)
		}
		if len(w.TaskTypes) == 0 {
			return fmt.Errorf("worker %q has no task types", w.Name)
		}
		types := map[string]bool{}
		for _, typ := range w.TaskTypes {
			if !slices.Contains([]string{"bootstrap", "reason", "explore"}, typ) || types[typ] {
				return fmt.Errorf("invalid task types for %q", w.Name)
			}
			types[typ] = true
		}
		env := map[string]string{}
		for k, v := range c.CommonEnv {
			env[k] = v
		}
		for k, v := range w.Env {
			env[k] = v
		}
		for k, v := range env {
			var missing string
			env[k] = os.Expand(v, func(name string) string {
				value, ok := os.LookupEnv(name)
				if !ok {
					missing = name
				}
				return value
			})
			if missing != "" {
				return fmt.Errorf("environment variable %s is required for worker %q", missing, w.Name)
			}
		}
		w.Env = env
		if w.Type == "go" {
			if w.Env["ANTHROPIC_MODEL"] == "" {
				w.Env["ANTHROPIC_MODEL"] = w.Env["ANTHROPIC_DEFAULT_FABLE_MODEL"]
			}
			for _, key := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_MODEL"} {
				if strings.TrimSpace(w.Env[key]) == "" {
					return fmt.Errorf("worker %q is missing %s", w.Name, key)
				}
			}
		}
	}
	return nil
}
func (c Config) Task(kind string) Task {
	switch kind {
	case "bootstrap":
		return c.Tasks.Bootstrap
	case "reason":
		return c.Tasks.Reason
	default:
		return c.Tasks.Explore
	}
}
