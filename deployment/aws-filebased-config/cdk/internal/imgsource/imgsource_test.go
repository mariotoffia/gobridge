package imgsource

import (
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/stretchr/testify/require"
)

func TestDeriveBuildTags_TransportFamilies(t *testing.T) {
	for _, tc := range []struct {
		kind string
		tag  string
	}{
		{"mqtt", ""}, {"mqtt.paho", ""}, {"sqs", ""}, {"aws.sqs", ""}, {"http", ""},
		{"amqp091", "gobridge_amqp091"}, {"amqp.amqp091", "gobridge_amqp091"},
		{"amqp10", "gobridge_amqp10"}, {"amqp.amqp10", "gobridge_amqp10"},
		{"servicebus", "gobridge_azure"}, {"azure.servicebus", "gobridge_azure"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			for _, cfg := range []*ports.BridgeConfig{
				{Sessions: []ports.SessionDef{{Transport: tc.kind}}},
				{Receivers: []ports.ReceiverDef{{Transport: tc.kind}}},
				{Senders: []ports.SenderDef{{Transport: tc.kind}}},
			} {
				got := DeriveBuildTags(cfg)
				if tc.tag == "" {
					require.Empty(t, got)
				} else {
					require.Equal(t, []string{tc.tag}, got)
				}
			}
		})
	}
}

func TestDeriveBuildTags_BaseStoresNeedNoTags(t *testing.T) {
	require.Empty(t, DeriveBuildTags(nil))
	for _, kind := range []string{"memory", "sqlite", "dynamodb"} {
		t.Run(kind, func(t *testing.T) {
			store := &ports.StoreConfig{Type: kind}
			require.Empty(t, DeriveBuildTags(&ports.BridgeConfig{Stores: ports.StoresConfig{
				Lease: store, Outbox: store, DLQ: store, ManagedSubscriptions: store,
			}}))
		})
	}
}

func TestDeriveBuildTags_SortedAndDeduplicated(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Sessions:  []ports.SessionDef{{Transport: "servicebus"}, {Transport: "amqp10"}},
		Receivers: []ports.ReceiverDef{{Transport: "amqp091"}, {Transport: "azure.servicebus"}},
		Senders:   []ports.SenderDef{{Transport: "amqp.amqp10"}},
	}
	require.Equal(t, []string{"gobridge_amqp091", "gobridge_amqp10", "gobridge_azure"}, DeriveBuildTags(cfg))
}

func TestDeriveBuildTags_InheritedTransportIncludesTopicsAndBindings(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Sessions: []ports.SessionDef{{ID: "bus", Transport: "azure.servicebus"}},
		Receivers: []ports.ReceiverDef{{
			ID: "in", SessionID: "bus", Topics: []ports.SubscriptionDef{{Topic: "events"}},
		}},
		Senders:  []ports.SenderDef{{ID: "out", SessionID: "bus"}},
		Bindings: []ports.BindingDef{{ID: "destination", SenderID: "out", Address: "events"}},
		Routes:   []ports.RouteDef{{ReceiverID: "in", Bindings: []string{"destination"}}},
	}
	require.Equal(t, []string{"gobridge_azure"}, DeriveBuildTags(cfg))
	require.Empty(t, cfg.Receivers[0].Transport, "derivation must not mutate inherited fields")
	require.Empty(t, cfg.Senders[0].Transport)
}

func TestDeriveBuildTags_RejectsUnmappedKinds(t *testing.T) {
	for name, cfg := range map[string]*ports.BridgeConfig{
		"session":                 {Sessions: []ports.SessionDef{{Transport: "custom"}}},
		"receiver":                {Receivers: []ports.ReceiverDef{{Transport: "custom"}}},
		"sender":                  {Senders: []ports.SenderDef{{Transport: "custom"}}},
		"lease":                   {Stores: ports.StoresConfig{Lease: &ports.StoreConfig{Type: "custom"}}},
		"outbox":                  {Stores: ports.StoresConfig{Outbox: &ports.StoreConfig{Type: "custom"}}},
		"dlq":                     {Stores: ports.StoresConfig{DLQ: &ports.StoreConfig{Type: "custom"}}},
		"managed subscriptions":   {Stores: ports.StoresConfig{ManagedSubscriptions: &ports.StoreConfig{Type: "custom"}}},
		"transport used as store": {Stores: ports.StoresConfig{Outbox: &ports.StoreConfig{Type: "amqp10"}}},
		"store used as transport": {Senders: []ports.SenderDef{{Transport: "sqlite"}}},
		// Processors are named registrations, not transport-family kinds.
		// The profile currently wires none, including the library's filter.
		"processor": {Routes: []ports.RouteDef{{Processors: []string{"filter"}}}},
	} {
		t.Run(name, func(t *testing.T) {
			require.Panics(t, func() { DeriveBuildTags(cfg) })
		})
	}
}

func TestGoBuild_RendersPublishedPackageAndHardenedRuntime(t *testing.T) {
	dockerfile := renderDockerfile(GoBuildProps{Version: "v0.4.0"}, &ports.BridgeConfig{
		Senders: []ports.SenderDef{{Transport: "amqp10"}, {Transport: "amqp091"}},
	})
	require.Contains(t, dockerfile, "go install -trimpath -tags=gobridge_amqp091,gobridge_amqp10")
	require.Contains(t, dockerfile, `-ldflags "-s -w -X main.version=v0.4.0 -X main.gitSHA=module@v0.4.0"`)
	require.Contains(t, dockerfile, "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/lib/cmd/gobridge-filebased@v0.4.0")
	require.Contains(t, dockerfile, "CGO_ENABLED=0")
	require.Contains(t, dockerfile, "GOWORK=off")
	require.Contains(t, dockerfile, "GOBIN=/out")
	require.Contains(t, dockerfile, "USER 65532:65532")
	require.Contains(t, dockerfile, `ENTRYPOINT ["/usr/local/bin/gobridge-filebased"]`)
	require.Contains(t, dockerfile, `CMD ["/usr/local/bin/gobridge-filebased", "-healthcheck"]`)
	require.NotContains(t, dockerfile, "COPY .")
	require.NotContains(t, dockerfile, "git clone")
	var bases int
	for _, line := range strings.Split(dockerfile, "\n") {
		if strings.HasPrefix(line, "FROM ") {
			bases++
			require.Regexp(t, `@sha256:[0-9a-f]{64}`, line)
		}
	}
	require.Equal(t, 2, bases)
}

func TestGoBuild_ExplicitTagsOverrideDerivation(t *testing.T) {
	cfg := &ports.BridgeConfig{Senders: []ports.SenderDef{{Transport: "custom"}}}
	for _, tc := range []struct {
		name string
		tags []string
		want string
	}{
		{"empty", []string{}, "-tags= "},
		{"custom", []string{"custom", "gobridge_amqp10", "custom"}, "-tags=custom,gobridge_amqp10 "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Contains(t, renderDockerfile(GoBuildProps{
				Version: "v1.2.3-rc.1", Package: "example.com/bridge/cmd/custom", BuildTags: tc.tags,
			}, cfg), tc.want)
		})
	}
}

func TestGoBuild_OverridesPinnedBasesAndPackage(t *testing.T) {
	builder := "example.com/builder@sha256:" + strings.Repeat("a", 64)
	base := "example.com/runtime@sha256:" + strings.Repeat("b", 64)
	dockerfile := renderDockerfile(GoBuildProps{
		Version: "v1.2.3", Package: "example.com/bridge/v2/cmd/custom",
		GoImage: builder, BaseImage: base, BuildTags: []string{},
	}, nil)
	require.Contains(t, dockerfile, "FROM "+builder+" AS build")
	require.Contains(t, dockerfile, "FROM "+base+" AS runtime")
	require.Contains(t, dockerfile, "example.com/bridge/v2/cmd/custom@v1.2.3")
}

func TestGoBuild_AcceptsPublishedVersionForms(t *testing.T) {
	for _, version := range []string{
		"v0.4.0", "v1.2.3-rc.1", "v1.2.3-0", "v1.2.3-01alpha",
		"v2.0.0+incompatible", "v0.0.0-20260909085016-abcdef123456",
	} {
		t.Run(version, func(t *testing.T) {
			require.Contains(t, renderDockerfile(GoBuildProps{Version: version}, nil), defaultPackage+"@"+version)
		})
	}
}

func TestGoBuild_RejectsUnsafeOrMutableInputs(t *testing.T) {
	for name, props := range map[string]GoBuildProps{
		"missing version":      {},
		"main":                 {Version: "main"},
		"latest":               {Version: "latest"},
		"version injection":    {Version: "v1.2.3\nRUN false"},
		"empty prerelease":     {Version: "v1.2.3-."},
		"empty identifier":     {Version: "v1.2.3-rc..1"},
		"numeric prerelease":   {Version: "v1.2.3-01"},
		"empty build metadata": {Version: "v1.2.3+."},
		"package injection":    {Version: "v1.2.3", Package: "example.com/a;false"},
		"package traversal":    {Version: "v1.2.3", Package: "example.com/../cmd"},
		"package version":      {Version: "v1.2.3", Package: "example.com/cmd@latest"},
		"package wildcard":     {Version: "v1.2.3", Package: "example.com/..."},
		"tag injection":        {Version: "v1.2.3", BuildTags: []string{"foo;false"}},
		"combined tags":        {Version: "v1.2.3", BuildTags: []string{"foo,bar"}},
		"empty tag":            {Version: "v1.2.3", BuildTags: []string{""}},
		"builder tag":          {Version: "v1.2.3", GoImage: "golang:latest"},
		"runtime tag":          {Version: "v1.2.3", BaseImage: "distroless:latest"},
		"runtime injection":    {Version: "v1.2.3", BaseImage: "bad\nFROM evil@sha256:" + strings.Repeat("a", 64)},
		"unsupported platform": {Version: "v1.2.3", Platform: "windows/amd64"},
	} {
		t.Run(name, func(t *testing.T) {
			require.Panics(t, func() { renderDockerfile(props, nil) })
		})
	}
}
