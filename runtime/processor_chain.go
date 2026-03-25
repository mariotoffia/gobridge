package runtime

import (
	"context"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// RunChain executes processors in order, each wrapping the next.
// If processors is empty, returns nil immediately.
func RunChain(ctx context.Context, processors []ports.Processor, env *domain.Envelope) error {
	if len(processors) == 0 {
		return nil
	}

	var terminal ports.ProcessorFunc = func(_ context.Context, _ *domain.Envelope) error {
		return nil
	}

	chain := buildChain(processors, 0, terminal)
	return chain(ctx, env)
}

func buildChain(processors []ports.Processor, index int, terminal ports.ProcessorFunc) ports.ProcessorFunc {
	if index >= len(processors) {
		return terminal
	}
	next := buildChain(processors, index+1, terminal)
	p := processors[index]
	return func(ctx context.Context, env *domain.Envelope) error {
		return p.Process(ctx, env, next)
	}
}
