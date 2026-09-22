//go:build linux

package worker

import (
	"context"
	"sync"

	"xloom/internal/agent"
)

type replanProvider struct {
	provider  agent.Provider
	mu        sync.Mutex
	remaining int
}

type sizedReplanProvider struct {
	*replanProvider
	agent.RequestSizer
}

// Apply the allowance at the provider boundary so overflow recovery and
// compaction summaries cannot bypass the shadow check's total call limit.
func limitReplanProvider(p agent.Provider, limit int) agent.Provider {
	limited := &replanProvider{provider: p, remaining: limit}
	if sizer, ok := p.(agent.RequestSizer); ok {
		return &sizedReplanProvider{replanProvider: limited, RequestSizer: sizer}
	}
	return limited
}

func (p *replanProvider) takeCall() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.remaining <= 0 {
		return false
	}
	p.remaining--
	return true
}

func (p *replanProvider) Generate(ctx context.Context, messages []agent.Message, tools []agent.Definition, emit agent.Emit) (agent.Message, error) {
	if !p.takeCall() {
		return agent.Message{}, errReplanBudget
	}
	return p.provider.Generate(ctx, messages, tools, emit)
}

func (p *replanProvider) GenerateSummary(ctx context.Context, messages []agent.Message, tokens int, emit agent.Emit) (agent.Message, error) {
	if !p.takeCall() {
		return agent.Message{}, errReplanBudget
	}
	if summary, ok := p.provider.(agent.SummaryProvider); ok {
		return summary.GenerateSummary(ctx, messages, tokens, emit)
	}
	return p.provider.Generate(ctx, messages, nil, nil)
}
