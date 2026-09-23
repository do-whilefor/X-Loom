package config

import (
	"strings"
	"testing"
)

func coldStartConfig() Config {
	return Config{
		Server:    "http://server:8000",
		Runtime:   Runtime{Interval: 2, MaxWorkers: 4, MaxProjects: 2, MaxProjectWorkers: 2, HealthTimeout: 30},
		Tasks:     Tasks{Reason: Task{Timeout: 300, MaxIntents: 3}, Explore: Task{ConcludeTimeout: 60}},
		Container: Container{Image: "fixture", Network: "bridge", CompletedAction: "stop"},
		Workers:   []Worker{{Name: "fixture", Type: "mock", TaskTypes: []string{"reason", "explore"}, MaxRunning: 4}},
	}
}

func TestDecideExecuteConfigDoesNotRequireBootstrapBudget(t *testing.T) {
	c := coldStartConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("reason + explore configuration: %v", err)
	}
	if c.Tasks.Bootstrap.ConcludeTimeout != 0 {
		t.Fatal("unused bootstrap budget was added")
	}
}

func TestConfigRequiresDecideCapability(t *testing.T) {
	c := coldStartConfig()
	c.Workers[0].TaskTypes = []string{"explore"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "reason (Decide)") {
		t.Fatalf("missing Decide capability: %v", err)
	}
}

func TestLegacyBootstrapCapabilityStillRequiresItsBudget(t *testing.T) {
	c := coldStartConfig()
	c.Workers[0].TaskTypes = append(c.Workers[0].TaskTypes, "bootstrap")
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "bootstrap conclude_timeout") {
		t.Fatalf("missing legacy bootstrap budget: %v", err)
	}
	c.Tasks.Bootstrap.ConcludeTimeout = 60
	if err := c.Validate(); err != nil {
		t.Fatalf("legacy bootstrap configuration: %v", err)
	}
}

func TestDecideCapabilityCanBeProvidedBySeparateWorker(t *testing.T) {
	c := coldStartConfig()
	c.Workers[0].TaskTypes = []string{"explore"}
	c.Workers = append(c.Workers, Worker{Name: "planner", Type: "mock", TaskTypes: []string{"reason"}, MaxRunning: 1})
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
