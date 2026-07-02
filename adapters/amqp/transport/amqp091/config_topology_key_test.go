package amqp091

import "testing"

// TestConfig_PublisherTopologyKey verifies the descriptor the bridge uses to
// tell a legitimate identical re-declaration of an exchange from a genuinely
// divergent one when it dedups senders by exchange name (REV-2-topowarn). Two
// configs that declare the SAME exchange topology must yield an identical key;
// any difference in a field ExchangeDeclare actually consumes must change it;
// and per-message routing keys must NOT affect it.
func TestConfig_PublisherTopologyKey(t *testing.T) {
	base := func() Config {
		return Config{
			Sender: SenderParams{Exchange: "ex.orders", RoutingKey: "rk.a"},
			Publisher: PublisherParams{
				ExchangeType:      "topic",
				Durable:           true,
				AutoDelete:        false,
				ExchangeArguments: map[string]any{"alternate-exchange": "ae", "x-flag": 1},
			},
		}
	}

	t.Run("identical topology yields identical key", func(t *testing.T) {
		if a, b := base().PublisherTopologyKey(), base().PublisherTopologyKey(); a != b {
			t.Fatalf("identical topology must match: %q != %q", a, b)
		}
	})

	t.Run("empty exchange_type defaults to direct", func(t *testing.T) {
		var deflt Config // zero: exchange_type ""
		explicit := Config{Publisher: PublisherParams{ExchangeType: "direct"}}
		if a, b := deflt.PublisherTopologyKey(), explicit.PublisherTopologyKey(); a != b {
			t.Fatalf("empty exchange_type must equal explicit \"direct\": %q != %q", a, b)
		}
	})

	t.Run("argument map order does not affect the key", func(t *testing.T) {
		c1 := base()
		c2 := base()
		c2.Publisher.ExchangeArguments = map[string]any{"x-flag": 1, "alternate-exchange": "ae"}
		if a, b := c1.PublisherTopologyKey(), c2.PublisherTopologyKey(); a != b {
			t.Fatalf("argument order must not matter: %q != %q", a, b)
		}
	})

	t.Run("routing key is excluded (per-message, not declaration)", func(t *testing.T) {
		c1 := base()
		c2 := base()
		c2.Sender.RoutingKey = "rk.z"
		c2.Publisher.RoutingKey = "rk.z"
		if a, b := c1.PublisherTopologyKey(), c2.PublisherTopologyKey(); a != b {
			t.Fatalf("routing key must not affect topology: %q != %q", a, b)
		}
	})

	// Each mutator changes a field ExchangeDeclare consumes, so it MUST change the key.
	divergent := map[string]func(*Config){
		"exchange_type": func(c *Config) { c.Publisher.ExchangeType = "fanout" },
		"durable":       func(c *Config) { c.Publisher.Durable = false },
		"auto_delete":   func(c *Config) { c.Publisher.AutoDelete = true },
		"args value":    func(c *Config) { c.Publisher.ExchangeArguments["x-flag"] = 2 },
		"args key":      func(c *Config) { c.Publisher.ExchangeArguments["x-new"] = true },
	}
	for name, mutate := range divergent {
		t.Run("diverges on "+name, func(t *testing.T) {
			c := base()
			mutate(&c)
			if got, want := c.PublisherTopologyKey(), base().PublisherTopologyKey(); got == want {
				t.Fatalf("expected divergent key when %s changes, both were %q", name, got)
			}
		})
	}
}
