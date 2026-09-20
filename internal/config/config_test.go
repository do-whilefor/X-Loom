package config

import (
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
