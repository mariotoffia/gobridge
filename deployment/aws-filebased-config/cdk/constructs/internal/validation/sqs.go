package validation

import (
	"fmt"
	"sort"

	"github.com/aws/constructs-go/constructs/v10"
	"github.com/mariotoffia/gobridge/adapters/aws/transport/sqs"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/bridgecfg"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/cdk/registry"
	"github.com/mariotoffia/gobridge/ports"
)

func checkSQS(scope constructs.Construct, cfg *ports.BridgeConfig, reg *registry.QueueRegistry, emit func(string)) {
	if err := bridgecfg.ValidateSQSConfig(cfg); err != nil {
		emit(err.Error())
		return
	}
	// Keep complete configs: reducing them to queue names loses Region and
	// credential-scope information and can select the wrong queue for IAM.
	configs := make([]sqs.Config, 0, len(cfg.Receivers)+len(cfg.Senders))
	add := func(kind string, pc ports.PluginConfig) {
		if !sqs.IsKind(kind) {
			return
		}
		c, ok := sqsConfig(pc)
		if ok {
			configs = append(configs, c)
		}
	}
	// ValidateSQSConfig explicitly rejects inherited SQS session attachments,
	// so a missing literal transport cannot silently skip a live SQS endpoint.
	for _, receiver := range cfg.Receivers {
		add(receiver.Transport, receiver.Config)
	}
	for _, sender := range cfg.Senders {
		add(sender.Transport, sender.Config)
	}
	// Bindings were proven to reference their sender's queue; they do not
	// create an independent sender or an additional registry lookup.
	if reg == nil {
		refs := make(map[string]struct{})
		for _, c := range configs {
			if c.QueueURL != "" {
				continue // Explicit URL retains its externally managed IAM contract.
			}
			if c.QueueTags != nil {
				refs["queue_tags"] = struct{}{}
			} else {
				refs[c.QueueName] = struct{}{}
			}
		}
		if len(refs) > 0 {
			emit(fmt.Sprintf("SQS reference(s) %s require the QueueRegistry prop, but no QueueRegistry was supplied. "+
				"Call registry.NewQueueRegistry(), AddQueue(name, queue), and BindQueueTags for tag selectors.",
				quoteList(sortedKeys(refs))))
		}
		return
	}
	sort.SliceStable(configs, func(i, j int) bool {
		if configs[i].QueueName != configs[j].QueueName {
			return configs[i].QueueName < configs[j].QueueName
		}
		return configs[i].Region < configs[j].Region
	})
	seen := make(map[string]struct{})
	for _, c := range configs {
		if _, err := reg.ResolveQueue(c, scope); err != nil {
			message := err.Error()
			if _, exists := seen[message]; !exists {
				emit("yaml references " + message)
				seen[message] = struct{}{}
			}
		}
	}
}

func sqsConfig(pc ports.PluginConfig) (sqs.Config, bool) {
	switch c := pc.(type) {
	case *sqs.Config:
		if c != nil {
			return *c, true
		}
	case sqs.Config:
		return c, true
	}
	return sqs.Config{}, false
}
