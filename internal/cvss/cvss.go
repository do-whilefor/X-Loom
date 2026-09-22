// Package cvss calculates CVSS 3.1 Base scores from supplied metric vectors.
// It does not assess whether the metric choices are supported by evidence.
// Formulas and Roundup follow FIRST CVSS v3.1 sections 7.1 and Appendix A:
// https://www.first.org/cvss/v3.1/specification-document
package cvss

import (
	"fmt"
	"math"
	"strings"
)

// Result includes the normalized vector and the complete Base calculation.
// JSON field names preserve the reference calculator's output contract.
type Result struct {
	Vector         string            `json:"vector"`
	Version        string            `json:"version"`
	MetricGroup    string            `json:"metricGroup"`
	Metrics        map[string]string `json:"metrics"`
	ISCBase        float64           `json:"ISCBase"`
	Impact         float64           `json:"impact"`
	ImpactLabel    string            `json:"impactLabel"`
	Exploitability float64           `json:"exploitability"`
	BaseScore      float64           `json:"baseScore"`
	Severity       string            `json:"severity"`
}

var metricOrder = [...]string{"AV", "AC", "PR", "UI", "S", "C", "I", "A"}

var values = map[string]map[string]float64{
	"AV": {"N": 0.85, "A": 0.62, "L": 0.55, "P": 0.2},
	"AC": {"L": 0.77, "H": 0.44},
	"PR": {"N": 0.85, "L": 0.62, "H": 0.27},
	"UI": {"N": 0.85, "R": 0.62},
	"S":  {"U": 0, "C": 1},
	"C":  {"H": 0.56, "L": 0.22, "N": 0},
	"I":  {"H": 0.56, "L": 0.22, "N": 0},
	"A":  {"H": 0.56, "L": 0.22, "N": 0},
}

// Calculate accepts exactly the eight Base metrics, in any order and case,
// with optional CVSS:3.1/ prefix and whitespace surrounding vector segments.
// Other CVSS versions, duplicate, missing and non-Base metrics are rejected.
func Calculate(vector string) (Result, error) {
	parts := strings.Split(strings.ToUpper(strings.TrimSpace(vector)), "/")
	if strings.HasPrefix(strings.TrimSpace(parts[0]), "CVSS:") {
		if strings.TrimSpace(parts[0]) != "CVSS:3.1" {
			return Result{}, fmt.Errorf("only CVSS:3.1 Base vectors are supported")
		}
		parts = parts[1:]
	}
	metrics := make(map[string]string, len(metricOrder))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		key, value, ok := strings.Cut(part, ":")
		if !ok || key == "" || value == "" || strings.Contains(value, ":") {
			return Result{}, fmt.Errorf("bad metric %q: expected KEY:VALUE", part)
		}
		if _, ok := values[key]; !ok {
			return Result{}, fmt.Errorf("unsupported Base metric %s", key)
		}
		if _, ok := metrics[key]; ok {
			return Result{}, fmt.Errorf("duplicate metric %s", key)
		}
		metrics[key] = value
	}
	canonical := make([]string, 0, len(metricOrder))
	for _, key := range metricOrder {
		value, ok := metrics[key]
		if !ok {
			return Result{}, fmt.Errorf("missing metric %s", key)
		}
		if _, ok := values[key][value]; !ok {
			return Result{}, fmt.Errorf("bad %s value %s", key, value)
		}
		canonical = append(canonical, key+":"+value)
	}
	weight := func(key string) float64 { return values[key][metrics[key]] }
	result := Result{
		Vector:      "CVSS:3.1/" + strings.Join(canonical, "/"),
		Version:     "3.1",
		MetricGroup: "Base",
		Metrics:     metrics,
		ISCBase:     1 - (1-weight("C"))*(1-weight("I"))*(1-weight("A")),
		ImpactLabel: "Scope Unchanged",
	}
	pr := weight("PR")
	scopeChanged := metrics["S"] == "C"
	if scopeChanged {
		if metrics["PR"] == "L" {
			pr = 0.68
		} else if metrics["PR"] == "H" {
			pr = 0.5
		}
		// The Base Impact formula differs from the Environmental formula.
		result.Impact = 7.52*(result.ISCBase-0.029) - 3.25*math.Pow(result.ISCBase-0.02, 15)
		result.ImpactLabel = "Scope Changed"
	} else {
		result.Impact = 6.42 * result.ISCBase
	}
	result.Exploitability = 8.22 * weight("AV") * weight("AC") * pr * weight("UI")
	if result.Impact > 0 {
		score := result.Impact + result.Exploitability
		if scopeChanged {
			score *= 1.08
		}
		result.BaseScore = roundup(math.Min(score, 10))
	}
	result.Severity = severity(result.BaseScore)
	return result, nil
}

// roundup implements FIRST Appendix A for nonnegative score inputs, avoiding
// binary floating-point noise that would make a plain ceil(x*10)/10 inaccurate.
func roundup(score float64) float64 {
	fixed := int64(math.Round(score * 100000))
	if fixed%10000 == 0 {
		return float64(fixed) / 100000
	}
	return float64(fixed/10000+1) / 10
}

func severity(score float64) string {
	switch {
	case score == 0:
		return "NONE"
	case score < 4:
		return "LOW"
	case score < 7:
		return "MEDIUM"
	case score < 9:
		return "HIGH"
	default:
		return "CRITICAL"
	}
}
