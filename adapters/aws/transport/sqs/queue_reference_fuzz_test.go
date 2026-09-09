package sqs

import "testing"

func FuzzQueueReference(f *testing.F) {
	f.Add("app", "bridge", "orders-")
	f.Add("", "", "")
	f.Add("\xff", "\xff", "*")
	f.Fuzz(func(t *testing.T, key, value, prefix string) {
		cfg := Config{QueueTags: map[string]string{key: value}, QueueNamePrefix: prefix}
		err := cfg.Validate()
		queueErr := cfg.ValidateQueue()
		if (err == nil) != (queueErr == nil) {
			t.Fatalf("selector validation diverged: Validate=%v ValidateQueue=%v", err, queueErr)
		}
		if err == nil {
			frozen := cfg.FreezePluginConfig().(*Config)
			cfg.QueueTags[key] = "changed"
			if frozen.QueueTags[key] != value {
				t.Fatal("selector snapshot aliases caller map")
			}
		}
	})
}
