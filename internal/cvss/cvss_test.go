package cvss

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestCalculateBaseScores(t *testing.T) {
	tests := []struct {
		name, vector, severity string
		score                  float64
	}{
		{"remote critical", "AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H", "CRITICAL", 9.8},
		{"changed scope maximum", "AV:N/AC:L/PR:N/UI:N/S:C/C:H/I:H/A:H", "CRITICAL", 10},
		{"unchanged low privilege", "AV:N/AC:L/PR:L/UI:N/S:U/C:H/I:H/A:H", "HIGH", 8.8},
		{"changed low privilege", "AV:N/AC:L/PR:L/UI:N/S:C/C:H/I:H/A:H", "CRITICAL", 9.9},
		{"changed high privilege", "AV:N/AC:L/PR:H/UI:N/S:C/C:H/I:H/A:H", "CRITICAL", 9.1},
		{"reflected cross site scripting", "AV:N/AC:L/PR:N/UI:R/S:C/C:L/I:L/A:N", "MEDIUM", 6.1},
		{"medium boundary", "AV:N/AC:H/PR:H/UI:R/S:C/C:L/I:L/A:N", "MEDIUM", 4},
		{"low physical", "AV:P/AC:H/PR:H/UI:R/S:U/C:L/I:N/A:N", "LOW", 1.6},
		{"no impact unchanged scope", "AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:N", "NONE", 0},
		{"no impact changed scope", "AV:N/AC:L/PR:N/UI:N/S:C/C:N/I:N/A:N", "NONE", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := Calculate(tt.vector)
			if err != nil {
				t.Fatal(err)
			}
			if result.BaseScore != tt.score || result.Severity != tt.severity {
				t.Fatalf("got %g %s, want %g %s", result.BaseScore, result.Severity, tt.score, tt.severity)
			}
		})
	}
}

func TestCalculateNormalizationAndJSONContract(t *testing.T) {
	result, err := Calculate("  cvss:3.1 / a:h / s:u / ui:n / pr:n / ac:l / i:h / c:h / av:n \n")
	if err != nil {
		t.Fatal(err)
	}
	want := "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"
	if result.Vector != want || result.Version != "3.1" || result.MetricGroup != "Base" {
		t.Fatalf("unexpected normalized result: %+v", result)
	}
	if len(result.Metrics) != 8 || result.Metrics["AV"] != "N" || result.Metrics["S"] != "U" {
		t.Fatalf("unexpected metrics: %v", result.Metrics)
	}
	if math.Abs(result.ISCBase-0.914816) > 1e-12 || math.Abs(result.Impact-5.87311872) > 1e-12 ||
		math.Abs(result.Exploitability-3.887042775) > 1e-12 || result.ImpactLabel != "Scope Unchanged" {
		t.Fatalf("incorrect computation trail: %+v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	keys := []string{"vector", "version", "metricGroup", "metrics", "ISCBase", "impact", "impactLabel", "exploitability", "baseScore", "severity"}
	if len(fields) != len(keys) {
		t.Fatalf("unexpected JSON fields: %s", encoded)
	}
	for _, key := range keys {
		if _, ok := fields[key]; !ok {
			t.Errorf("missing JSON field %s", key)
		}
	}
}

func TestCalculateChangedScopeTrail(t *testing.T) {
	result, err := Calculate("AV:N/AC:L/PR:L/UI:N/S:C/C:H/I:H/A:H")
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(result.Impact-6.0477304915445185) > 1e-12 ||
		math.Abs(result.Exploitability-3.10963422) > 1e-12 || result.ImpactLabel != "Scope Changed" {
		t.Fatalf("incorrect changed-scope computation: %+v", result)
	}
}

func TestCalculateRejectsInvalidVectors(t *testing.T) {
	valid := "AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"
	tests := []struct{ name, vector string }{
		{"empty", ""},
		{"unsupported version", "CVSS:3.0/" + valid},
		{"unsupported major version", "CVSS:4.0/" + valid},
		{"prefix only", "CVSS:3.1"},
		{"duplicate", valid + "/AV:N"},
		{"case insensitive duplicate", valid + "/av:n"},
		{"temporal metric", valid + "/E:F"},
		{"environmental metric", valid + "/MAV:N"},
		{"unknown metric", valid + "/X:N"},
		{"trailing slash", valid + "/"},
		{"leading slash", "/" + valid},
		{"empty metric", strings.Replace(valid, "AC:L", "", 1)},
		{"extra separator", strings.Replace(valid, "AV:N", "AV:N:N", 1)},
		{"empty value", strings.Replace(valid, "AV:N", "AV:", 1)},
		{"empty key", strings.Replace(valid, "AV:N", ":N", 1)},
		{"whitespace in key", strings.Replace(valid, "AV:N", "AV :N", 1)},
		{"whitespace in value", strings.Replace(valid, "AV:N", "AV: N", 1)},
	}
	for _, key := range metricOrder {
		parts := strings.Split(valid, "/")
		for i, part := range parts {
			if strings.HasPrefix(part, key+":") {
				tests = append(tests, struct{ name, vector string }{"missing " + key, strings.Join(append(append([]string{}, parts[:i]...), parts[i+1:]...), "/")})
				tests = append(tests, struct{ name, vector string }{"invalid " + key, strings.Replace(valid, part, key+":X", 1)})
			}
		}
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if result, err := Calculate(tt.vector); err == nil {
				t.Fatalf("accepted invalid vector %q: %+v", tt.vector, result)
			}
		})
	}
}

func TestRoundup(t *testing.T) {
	for _, tt := range []struct{ input, want float64 }{
		{0, 0}, {4, 4}, {4.02, 4.1}, {4.000004, 4}, {4.000006, 4.1},
		{4.099999999999999, 4.1}, {4.100000000000001, 4.1}, {4.100006, 4.2}, {9.999999, 10}, {10, 10},
	} {
		if got := roundup(tt.input); got != tt.want {
			t.Errorf("roundup(%.16g) = %g, want %g", tt.input, got, tt.want)
		}
	}
}

func TestSeverityBoundaries(t *testing.T) {
	for _, tt := range []struct {
		score float64
		want  string
	}{
		{0, "NONE"}, {0.1, "LOW"}, {3.9, "LOW"}, {4, "MEDIUM"}, {6.9, "MEDIUM"},
		{7, "HIGH"}, {8.9, "HIGH"}, {9, "CRITICAL"}, {10, "CRITICAL"},
	} {
		if got := severity(tt.score); got != tt.want {
			t.Errorf("severity(%g) = %s, want %s", tt.score, got, tt.want)
		}
	}
}
