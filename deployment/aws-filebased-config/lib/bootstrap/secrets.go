package bootstrap

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awsssm "github.com/aws/aws-sdk-go-v2/service/ssm"

	ssmrepo "github.com/mariotoffia/gobridge/adapters/aws/credentials/ssm"
	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/deployment/aws-filebased-config/lib/model"
	"github.com/mariotoffia/gobridge/runtime"
)

type parameterResolver interface {
	ResolveString(ctx context.Context, ref string) (string, error)
}

type ssmParameterResolver struct {
	client *awsssm.Client
}

func newSSMParameterResolver(ctx context.Context, cfg model.BootstrapConfig) (parameterResolver, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	region := cfg.AWSRegion
	if region == "" && cfg.SSMEndpoint != "" {
		region = "us-east-1"
	}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	if cfg.SSMEndpoint != "" {
		// Static test credentials for local emulation (e.g. LocalStack).
		// If SSMEndpoint is accidentally set in production, real IAM
		// credentials will be bypassed.
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

func newDefaultCredentialStore(cfg model.BootstrapConfig) *runtime.CredentialResolver {
	resolver := runtime.NewCredentialResolver()

	var opts []ssmrepo.Option
	region := cfg.AWSRegion
	if region == "" && cfg.SSMEndpoint != "" {
		region = "us-east-1"
	}
	if region != "" {
		opts = append(opts, ssmrepo.WithRegion(region))
	}
	if cfg.SSMEndpoint != "" {
		opts = append(opts, ssmrepo.WithEndpoint(cfg.SSMEndpoint))
	}

	resolver.Register(ssmrepo.New(opts...))
	return resolver
}

type resolvedInputs struct {
	AdminAPIKey   string
	MonitorAPIKey string
	RuntimeConfig *config.BridgeConfig
}

func resolveInputs(
	ctx context.Context,
	resolver parameterResolver,
	bootstrapCfg model.BootstrapConfig,
	logical *config.BridgeConfig,
) (*resolvedInputs, error) {
	adminKey, err := resolver.ResolveString(ctx, bootstrapCfg.AdminAPIKeyParam)
	if err != nil {
		return nil, err
	}

	monitorKey := ""
	if bootstrapCfg.MonitorAPIKeyParam != "" {
		monitorKey, err = resolver.ResolveString(ctx, bootstrapCfg.MonitorAPIKeyParam)
		if err != nil {
			return nil, err
		}
	}

	resolvedCfg, err := cloneBridgeConfig(logical)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: clone logical config: %w", err)
	}

	for i := range resolvedCfg.Receivers {
		recv := &resolvedCfg.Receivers[i]
		if recv.Transport != "http" {
			continue
		}
		if optString(recv.Options, "api_key") != "" {
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
		if recv.Options == nil {
			recv.Options = map[string]any{}
		}
		recv.Options["api_key"] = value
	}

	for i := range resolvedCfg.Senders {
		sender := &resolvedCfg.Senders[i]
		if sender.Transport != "http" {
			continue
		}
		if optString(sender.Options, "api_key") != "" {
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
		if sender.Options == nil {
			sender.Options = map[string]any{}
		}
		sender.Options["api_key"] = value
	}

	return &resolvedInputs{
		AdminAPIKey:   adminKey,
		MonitorAPIKey: monitorKey,
		RuntimeConfig: resolvedCfg,
	}, nil
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

func optString(options map[string]any, key string) string {
	if len(options) == 0 {
		return ""
	}
	value, ok := options[key]
	if !ok {
		return ""
	}
	if str, ok := value.(string); ok {
		return str
	}
	return ""
}
