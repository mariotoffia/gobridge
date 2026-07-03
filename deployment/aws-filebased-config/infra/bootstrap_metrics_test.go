package infra

import "testing"

func baseValidMetricsConfig() BootstrapConfig {
	return BootstrapConfig{
		BridgeID:         "b",
		ConfigFilePath:   "/f",
		AdminAPIKeyParam: "/a",
	}
}

func TestBootstrapConfig_Validate_MetricsExporter(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"empty-noop", "", false},
		{"explicit-noop", MetricsExporterNoop, false},
		{"cloudwatch", MetricsExporterCloudWatch, false},
		{"unknown", "prometheus", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := baseValidMetricsConfig()
			c.MetricsExporter = tc.value
			err := c.Normalized().Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for metrics_exporter=%q", tc.value)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for metrics_exporter=%q: %v", tc.value, err)
			}
		})
	}
}

func TestBootstrapConfig_MetricsExporterEnabled(t *testing.T) {
	if (BootstrapConfig{}).MetricsExporterEnabled() {
		t.Fatalf("empty exporter should not be enabled")
	}
	if (BootstrapConfig{MetricsExporter: MetricsExporterNoop}).MetricsExporterEnabled() {
		t.Fatalf("noop exporter should not be enabled")
	}
	if !(BootstrapConfig{MetricsExporter: MetricsExporterCloudWatch}).MetricsExporterEnabled() {
		t.Fatalf("cloudwatch exporter should be enabled")
	}
}

func TestBootstrapConfig_EffectiveMetricsNamespace(t *testing.T) {
	if got := (BootstrapConfig{}).EffectiveMetricsNamespace(); got != DefaultMetricsNamespace {
		t.Fatalf("default namespace = %q, want %q", got, DefaultMetricsNamespace)
	}
	if got := (BootstrapConfig{MetricsNamespace: "Acme/Bridge"}).EffectiveMetricsNamespace(); got != "Acme/Bridge" {
		t.Fatalf("override namespace = %q, want Acme/Bridge", got)
	}
}
