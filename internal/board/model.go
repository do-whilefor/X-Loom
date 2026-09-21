// Package board contains the Cairn-compatible graph protocol.
package board

type Settings struct {
	IntentTimeout int `json:"intent_timeout"`
	ReasonTimeout int `json:"reason_timeout"`
}
type Fact struct {
	ID          string `json:"id" yaml:"id"`
	Description string `json:"description" yaml:"description"`
}
type Intent struct {
	ID          string   `json:"id"`
	From        []string `json:"from"`
	To          *string  `json:"to"`
	Description string   `json:"description"`
	Creator     string   `json:"creator"`
	Worker      *string  `json:"worker"`
	Heartbeat   *string  `json:"last_heartbeat_at"`
	CreatedAt   string   `json:"created_at"`
	ConcludedAt *string  `json:"concluded_at"`
}
type Hint struct {
	ID        string `json:"id"`
	Content   string `json:"content"`
	Creator   string `json:"creator"`
	CreatedAt string `json:"created_at"`
}
type Reason struct {
	Worker    string `json:"worker"`
	Trigger   string `json:"trigger"`
	StartedAt string `json:"started_at"`
	Heartbeat string `json:"last_heartbeat_at"`
}
type Project struct {
	ID           string  `json:"id"`
	Title        string  `json:"title"`
	Status       string  `json:"status"`
	Bootstrap    bool    `json:"bootstrap_enabled"`
	CreatedAt    string  `json:"created_at"`
	Reason       *Reason `json:"reason"`
	Scenario     string  `json:"scenario,omitempty"`
	Generation   int64   `json:"generation,omitempty"`
	RestartedAt  string  `json:"restarted_at,omitempty"`
	TerminatedAt string  `json:"terminated_at,omitempty"`
}
type Graph struct {
	Project Project  `json:"project"`
	Facts   []Fact   `json:"facts"`
	Intents []Intent `json:"intents"`
	Hints   []Hint   `json:"hints"`
}
type Summary struct {
	Project
	FactCount   int `json:"fact_count"`
	IntentCount int `json:"intent_count"`
	Working     int `json:"working_intent_count"`
	Unclaimed   int `json:"unclaimed_intent_count"`
	HintCount   int `json:"hint_count"`
}
type Conclusion struct {
	Fact   Fact   `json:"fact"`
	Intent Intent `json:"intent"`
}
type Reopened struct {
	Project Project `json:"project"`
	Fact    Fact    `json:"fact"`
	Intent  Intent  `json:"intent"`
}

func Ptr(s string) *string { return &s }
func Value(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
func (g Graph) OpenCount() int {
	n := 0
	for _, i := range g.Intents {
		if i.To == nil {
			n++
		}
	}
	return n
}
func (g Graph) Summarize() Summary {
	s := Summary{Project: g.Project, FactCount: len(g.Facts), IntentCount: len(g.Intents), HintCount: len(g.Hints)}
	for _, i := range g.Intents {
		if i.ConcludedAt == nil {
			if i.Worker == nil {
				s.Unclaimed++
			} else {
				s.Working++
			}
		}
	}
	return s
}
