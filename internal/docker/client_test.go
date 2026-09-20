//go:build linux

package docker

import (
	"testing"
)

func TestResultSinkHandlesFragments(t *testing.T) {
	s := &resultSink{}
	for _, p := range []string{"{\"type\":\"text_delta\",\"text\":\"x\"}\n{\"type\":", "\"result\",\"status\":\"success\",\"text\":\"ok\"}\n"} {
		if _, err := s.Write([]byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	if !s.found || s.result.Text != "ok" {
		t.Fatalf("%+v", s)
	}
}
