package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWorkerEnvironmentAndDuplicateNames(t *testing.T) {
	t.Setenv("XLOOM_TEST_TOKEN", "test-token")
	c := Config{Server: "http://server:8000", Runtime: Runtime{Interval: 3, MaxWorkers: 4, MaxProjects: 2, MaxProjectWorkers: 2, HealthTimeout: 10}, Tasks: Tasks{Bootstrap: Task{ConcludeTimeout: 90}, Explore: Task{ConcludeTimeout: 90}}, Container: Container{Image: "worker", Network: "bridge", CompletedAction: "stop"}, CommonEnv: map[string]string{"ANTHROPIC_AUTH_TOKEN": "${XLOOM_TEST_TOKEN}", "ANTHROPIC_BASE_URL": "https://example.invalid", "ANTHROPIC_DEFAULT_FABLE_MODEL": "model"}, Workers: []Worker{{Name: "one", TaskTypes: []string{"reason"}, MaxRunning: 1}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Workers[0].Env["ANTHROPIC_MODEL"] != "model" || c.Workers[0].Env["ANTHROPIC_AUTH_TOKEN"] != "test-token" {
		t.Fatal("environment merge failed")
	}
	c.Workers = append(c.Workers, c.Workers[0])
	if err := c.Validate(); err == nil {
		t.Fatal("duplicate workers accepted")
	}
}

func validConfig() Config {
	return Config{Server: "http://server:8000", Runtime: Runtime{Interval: 2, MaxWorkers: 2, MaxProjects: 2, MaxProjectWorkers: 3, HealthTimeout: 5}, Tasks: Tasks{Bootstrap: Task{ConcludeTimeout: 30}, Explore: Task{ConcludeTimeout: 30}}, Container: Container{Image: "worker", Network: "bridge", CompletedAction: "stop"}, Workers: []Worker{{Name: "mock", Type: "mock", TaskTypes: []string{"reason", "explore"}, MaxRunning: 2}}}
}

func TestValidationAndZeroTaskBudget(t *testing.T) {
	c := validConfig()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Tasks.Explore.Timeout != 0 || c.Tasks.Reason.MaxIntents != 3 || c.Container.Socket != "/var/run/docker.sock" {
		t.Fatalf("bad defaults: %+v", c)
	}
	for name, change := range map[string]func(*Config){
		"server":              func(c *Config) { c.Server = "file:///tmp/db" },
		"local":               func(c *Config) { c.Runtime.Execution = "local" },
		"unknown tool worker": func(c *Config) { c.Workers[0].Type = "pi" },
		"negative budget":     func(c *Config) { c.Tasks.Explore.Timeout = -1 },
		"conclude deadline":   func(c *Config) { c.Tasks.Bootstrap.ConcludeTimeout = 0 },
		"namespace":           func(c *Config) { c.Container.Namespace = "../other" },
		"duplicate task":      func(c *Config) { c.Workers[0].TaskTypes = []string{"reason", "reason"} },
		"concurrency":         func(c *Config) { c.Workers[0].MaxRunning = 0 },
		"missing env":         func(c *Config) { c.Workers[0].Env = map[string]string{"X": "${XLOOM_UNSET_CONFIG_TEST_912}"} },
	} {
		t.Run(name, func(t *testing.T) {
			c := validConfig()
			change(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestLoadRejectsUnknownFieldsAndMultipleDocuments(t *testing.T) {
	for _, data := range []string{"unknown_field: value\n", "server: http://server\n---\nserver: http://other\n"} {
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Fatal("invalid YAML accepted")
		}
	}
}

func TestExampleConfiguration(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "example-test-token")
	t.Setenv("ANTHROPIC_BASE_URL", "https://api.stepfun.com/step_plan")
	t.Setenv("ANTHROPIC_DEFAULT_FABLE_MODEL", "step-5-preview")
	c, err := Load(filepath.Join("..", "..", "dispatch.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Runtime.MaxWorkers != 16 || c.Runtime.MaxProjectWorkers != 4 || c.Runtime.MaxProjects != 4 {
		t.Fatalf("example concurrency limits: %+v", c.Runtime)
	}
	if len(c.Workers) != 1 || c.Workers[0].MaxRunning != 16 {
		t.Fatalf("default backend cannot use the configured global capacity: %+v", c.Workers)
	}
	if c.Container.Image != "xloom-worker:dev" {
		t.Fatalf("worker image = %q", c.Container.Image)
	}
	if c.Workers[0].Env["XLOOM_REASONING_EFFORT"] != "max" || c.Workers[0].Env["XLOOM_MAX_OUTPUT_TOKENS"] != "32768" {
		t.Fatal("example is missing maximum reasoning or its output budget")
	}
	for key, want := range map[string]string{
		"ANTHROPIC_AUTH_TOKEN":          "example-test-token",
		"ANTHROPIC_BASE_URL":            "https://api.stepfun.com/step_plan",
		"ANTHROPIC_DEFAULT_FABLE_MODEL": "step-5-preview",
		"ANTHROPIC_MODEL":               "step-5-preview",
	} {
		if c.Workers[0].Env[key] != want {
			t.Fatalf("example did not propagate %s", key)
		}
	}
}

func TestExampleModelEnvironmentSwitchAndWorkerOverrides(t *testing.T) {
	// A saved dispatch.yaml should follow changed process environment without
	// editing provider values in that file. Scoped backend overrides still win.
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "switched-test-token")
	t.Setenv("ANTHROPIC_BASE_URL", "https://switch.example.invalid/anthropic")
	t.Setenv("ANTHROPIC_DEFAULT_FABLE_MODEL", "switched-test-model")
	t.Setenv("XLOOM_SCOPED_TEST_TOKEN", "scoped-test-token")
	raw, err := os.ReadFile(filepath.Join("..", "..", "dispatch.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var input Config
	if err = yaml.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	input.Workers = append(input.Workers,
		Worker{Name: "scoped", Type: "go", TaskTypes: []string{"reason"}, MaxRunning: 1, Env: map[string]string{
			"ANTHROPIC_AUTH_TOKEN": "${XLOOM_SCOPED_TEST_TOKEN}", "ANTHROPIC_BASE_URL": "https://scoped.example.invalid/v1",
			"ANTHROPIC_DEFAULT_FABLE_MODEL": "scoped-default-model",
		}},
		Worker{Name: "explicit", Type: "go", TaskTypes: []string{"explore"}, MaxRunning: 1, Env: map[string]string{
			"ANTHROPIC_MODEL": "explicit-model", "ANTHROPIC_DEFAULT_FABLE_MODEL": "unused-default-model",
		}},
	)
	raw, err = yaml.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "dispatch.yaml")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string]string{
		{"ANTHROPIC_AUTH_TOKEN": "switched-test-token", "ANTHROPIC_BASE_URL": "https://switch.example.invalid/anthropic", "ANTHROPIC_DEFAULT_FABLE_MODEL": "switched-test-model", "ANTHROPIC_MODEL": "switched-test-model"},
		{"ANTHROPIC_AUTH_TOKEN": "scoped-test-token", "ANTHROPIC_BASE_URL": "https://scoped.example.invalid/v1", "ANTHROPIC_DEFAULT_FABLE_MODEL": "scoped-default-model", "ANTHROPIC_MODEL": "scoped-default-model"},
		{"ANTHROPIC_AUTH_TOKEN": "switched-test-token", "ANTHROPIC_BASE_URL": "https://switch.example.invalid/anthropic", "ANTHROPIC_DEFAULT_FABLE_MODEL": "unused-default-model", "ANTHROPIC_MODEL": "explicit-model"},
	}
	for n, env := range want {
		for key, value := range env {
			if loaded.Workers[n].Env[key] != value {
				t.Fatalf("worker %s did not resolve %s according to override precedence", loaded.Workers[n].Name, key)
			}
		}
	}
}
