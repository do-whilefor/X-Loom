package config

import (
	"os"
	"path/filepath"
	"testing"
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
