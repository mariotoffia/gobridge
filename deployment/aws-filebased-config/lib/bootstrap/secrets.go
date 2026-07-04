package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awsssm "github.com/aws/aws-sdk-go-v2/service/ssm"

	ssmrepo "github.com/mariotoffia/gobridge/adapters/aws/credentials/ssm"
	httptransport "github.com/mariotoffia/gobridge/adapters/http/transport"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/httpapi"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

type parameterResolver interface {
	ResolveString(ctx context.Context, ref string) (string, error)
}

type ssmParameterResolver struct {
	client *awsssm.Client
}

func newSSMParameterResolver(ctx context.Context, cfg deployinfra.BootstrapConfig) (parameterResolver, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	region := cfg.AWSRegion
	if region == "" && cfg.SSMEndpoint != "" {
		region = "us-west-1"
	}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	if cfg.SSMEndpoint != "" && cfg.DevMode {
		// Static test credentials for local emulation (e.g. LocalStack).
		// Only active when DevMode is explicitly enabled; Validate()
		// rejects SSMEndpoint without DevMode to prevent production bypass.
		opts = append(opts, awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: load AWS config for SSM: %w", err)
	}

	client := awsssm.NewFromConfig(awsCfg, func(o *awsssm.Options) {
		if cfg.SSMEndpoint != "" {
			o.BaseEndpoint = aws.String(cfg.SSMEndpoint)
		}
	})
	return &ssmParameterResolver{client: client}, nil
}

func (r *ssmParameterResolver) ResolveString(ctx context.Context, ref string) (string, error) {
	name, err := normalizeParameterRef(ref)
	if err != nil {
		return "", err
	}
	out, err := r.client.GetParameter(ctx, &awsssm.GetParameterInput{
		Name:           aws.String(name),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		return "", fmt.Errorf("bootstrap: read SSM parameter %s: %w", name, err)
	}
	if out.Parameter == nil || out.Parameter.Value == nil {
		return "", fmt.Errorf("bootstrap: SSM parameter %s returned no value", name)
	}
	return *out.Parameter.Value, nil
}

func newDefaultCredentialStore(ctx context.Context, cfg deployinfra.BootstrapConfig) (*runtime.CredentialResolver, error) {
	resolver := runtime.NewCredentialResolver()

	var opts []ssmrepo.Option
	region := cfg.AWSRegion
	if region == "" && cfg.SSMEndpoint != "" {
		region = "us-west-1"
	}
	if region != "" {
		opts = append(opts, ssmrepo.WithRegion(region))
	}
	if cfg.SSMEndpoint != "" {
		opts = append(opts, ssmrepo.WithEndpoint(cfg.SSMEndpoint))
	}
	if cfg.SSMEndpoint != "" && cfg.DevMode {
		// In DevMode, build a pre-configured SSM client with static test
		// credentials so the credential store is consistent with the
		// parameter resolver (both use the same auth for LocalStack).
		client, err := buildDevModeSSMClient(ctx, region, cfg.SSMEndpoint)
		if err != nil {
			return nil, fmt.Errorf("bootstrap: build dev-mode SSM client for credential store: %w", err)
		}
		opts = append(opts, ssmrepo.WithClient(client))
	}

	resolver.Register(ssmrepo.New(opts...))
	return resolver, nil
}

func buildDevModeSSMClient(ctx context.Context, region, endpoint string) (*awsssm.Client, error) {
	cfgOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	}
	if region != "" {
		cfgOpts = append(cfgOpts, awsconfig.WithRegion(region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, cfgOpts...)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: load default aws config: %w", err)
	}
	return awsssm.NewFromConfig(awsCfg, func(o *awsssm.Options) {
		o.BaseEndpoint = aws.String(endpoint)
	}), nil
}

// newDynamoDBClient builds the *dynamodb.Client backing the DynamoDB
// store factory (see App.newFactoryRegistry). The region comes from
// BootstrapConfig.AWSRegion when set, otherwise from the ambient AWS
// environment (task role / AWS_REGION). LoadDefaultConfig is
// offline-safe -- credentials are resolved lazily on first call -- so
// building the client never blocks startup or makes a network call.
//
// BootstrapConfig has no DynamoDB-endpoint knob: local emulation (e.g.
// LocalStack) must inject a pre-configured client via WithDynamoDBClient
// rather than relying on this constructor.
func newDynamoDBClient(ctx context.Context, cfg deployinfra.BootstrapConfig) (*dynamodb.Client, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if cfg.AWSRegion != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.AWSRegion))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: load AWS config for DynamoDB: %w", err)
	}
	return dynamodb.NewFromConfig(awsCfg), nil
}

type resolvedInputs struct {
	AdminAPIKey   string
	MonitorAPIKey string
	RuntimeConfig *ports.BridgeConfig
}

func resolveInputs(
	ctx context.Context,
	resolver parameterResolver,
	bootstrapCfg deployinfra.BootstrapConfig,
	registry *ports.Registry,
	logical *ports.BridgeConfig,
) (*resolvedInputs, error) {
	adminKey, err := resolver.ResolveString(ctx, bootstrapCfg.AdminAPIKeyParam)
	if err != nil {
		return nil, err
	}
	// Validate the admin-key parameter at startup AND on every reload so a
	// malformed, below-floor, or unsafe-named value fails fast rather than
	// installing a key that startup would have rejected. On the reload path a
	// returned error makes watchLoop discard the plan and keep the last-good
	// runtime. parseAdminKeys enforces the JSON SHAPE (a leading '{' is a
	// name->key map; malformed JSON is a hard error, never a literal key);
	// httpapi.ValidateAdminKeys then enforces the SAME tag-safe name and
	// minAPIKeyLen rules as httpapi's startup validateConfig (DRY).
	parsedAdminKeys, err := parseAdminKeys(adminKey)
	if err != nil {
		return nil, err
	}
	if err := httpapi.ValidateAdminKeys(parsedAdminKeys); err != nil {
		return nil, err
	}

	monitorKey := ""
	if bootstrapCfg.MonitorAPIKeyParam != "" {
		monitorKey, err = resolver.ResolveString(ctx, bootstrapCfg.MonitorAPIKeyParam)
		if err != nil {
			return nil, err
		}
	}

	resolvedCfg, err := cloneBridgeConfig(logical, registry)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: clone logical config: %w", err)
	}

	for i := range resolvedCfg.Receivers {
		recv := &resolvedCfg.Receivers[i]
		if recv.Transport != "http" {
			continue
		}
		current, _ := recv.Config.(httptransport.Config)
		if !current.APIKey.IsZero() {
			continue
		}
		ref, ok := bootstrapCfg.HTTPReceiverAPIKeyParams[recv.ID]
		if !ok || ref == "" {
			continue
		}
		value, err := resolver.ResolveString(ctx, ref)
		if err != nil {
			return nil, err
		}
		current.APIKey = shared.NewSecret(value)
		recv.Config = current
	}

	for i := range resolvedCfg.Senders {
		sender := &resolvedCfg.Senders[i]
		if sender.Transport != "http" {
			continue
		}
		current, _ := sender.Config.(httptransport.Config)
		if !current.APIKey.IsZero() {
			continue
		}
		ref, ok := bootstrapCfg.HTTPSenderAPIKeyParams[sender.ID]
		if !ok || ref == "" {
			continue
		}
		value, err := resolver.ResolveString(ctx, ref)
		if err != nil {
			return nil, err
		}
		current.APIKey = shared.NewSecret(value)
		sender.Config = current
	}

	return &resolvedInputs{
		AdminAPIKey:   adminKey,
		MonitorAPIKey: monitorKey,
		RuntimeConfig: resolvedCfg,
	}, nil
}

// parseAdminKeys interprets a resolved admin-key parameter value. A value whose
// first non-space byte is '{' is parsed as a JSON object of name->key; any other
// value is the legacy single key under the name "admin". Malformed JSON starting
// with '{' is a hard error — never fall back to treating the JSON text as a
// literal key (that would silently create a key equal to the JSON blob).
func parseAdminKeys(raw string) (map[string]string, error) {
	if strings.HasPrefix(strings.TrimSpace(raw), "{") {
		var m map[string]string
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, fmt.Errorf("bootstrap: admin API key parameter is malformed JSON: %w", err)
		}
		return m, nil
	}
	return map[string]string{"admin": raw}, nil
}

func normalizeParameterRef(ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("bootstrap: empty parameter reference")
	}
	if !strings.HasPrefix(ref, "pms://") {
		if strings.HasPrefix(ref, "/") {
			return ref, nil
		}
		return "/" + ref, nil
	}

	u, err := url.Parse(ref)
	if err != nil {
		return "", fmt.Errorf("bootstrap: invalid parameter reference %q: %w", ref, err)
	}
	if u.Scheme != "pms" {
		return "", fmt.Errorf("bootstrap: unsupported parameter scheme %q", u.Scheme)
	}
	path := "/" + u.Host
	if u.Path != "" && u.Path != "/" {
		path += u.Path
	}
	path = strings.TrimSuffix(path, "/")
	if path == "" {
		return "", fmt.Errorf("bootstrap: invalid empty parameter path in %q", ref)
	}
	return path, nil
}
