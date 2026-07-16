package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

const imageDigestAssetName = "gobridge-image-digest.txt"

var imageDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type imageAssociation struct {
	Exists bool
	Digest string
}

type githubReleaseResponse struct {
	Assets []githubReleaseAsset `json:"assets"`
}

type githubReleaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

func fetchImageAssociation(
	ctx context.Context,
	client *http.Client,
	apiURL string,
	repository string,
	tag string,
	image string,
	token string,
) (imageAssociation, error) {
	if client == nil {
		return imageAssociation{}, errors.New("github API client is nil")
	}
	if token == "" {
		return imageAssociation{}, errors.New("github API token is empty")
	}
	owner, repo, found := strings.Cut(repository, "/")
	if !found || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return imageAssociation{}, fmt.Errorf("github repository %q is not owner/name", repository)
	}
	if err := validateImageName(image); err != nil {
		return imageAssociation{}, err
	}
	base, err := url.Parse(apiURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return imageAssociation{}, fmt.Errorf("parsing GitHub API URL %q: %w", apiURL, err)
	}
	releaseURL := strings.TrimRight(apiURL, "/") +
		"/repos/" + url.PathEscape(owner) +
		"/" + url.PathEscape(repo) +
		"/releases/tags/" + url.PathEscape(tag)

	response, err := githubAPIRequest(ctx, client, http.MethodGet, releaseURL, token, "application/vnd.github+json")
	if err != nil {
		return imageAssociation{}, err
	}
	defer func() {
		_ = response.Body.Close()
	}()
	if response.StatusCode == http.StatusNotFound {
		return imageAssociation{}, nil
	}
	if response.StatusCode != http.StatusOK {
		return imageAssociation{}, githubStatusError("fetching release", response)
	}
	var release githubReleaseResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&release); err != nil {
		return imageAssociation{}, fmt.Errorf("decoding GitHub Release response: %w", err)
	}

	assetURL := ""
	assetFound := false
	for _, asset := range release.Assets {
		if asset.Name != imageDigestAssetName {
			continue
		}
		if assetFound {
			return imageAssociation{}, fmt.Errorf("release has duplicate %s assets", imageDigestAssetName)
		}
		assetFound = true
		if asset.URL == "" {
			return imageAssociation{}, fmt.Errorf("release asset %s has no API URL", imageDigestAssetName)
		}
		assetURL = asset.URL
	}
	if !assetFound {
		return imageAssociation{}, nil
	}
	parsedAssetURL, err := url.Parse(assetURL)
	if err != nil {
		return imageAssociation{}, fmt.Errorf("parsing digest asset URL: %w", err)
	}
	if parsedAssetURL.Scheme != base.Scheme || parsedAssetURL.Host != base.Host {
		return imageAssociation{}, fmt.Errorf("digest asset URL %q is outside GitHub API origin", assetURL)
	}

	assetResponse, err := githubAPIRequest(ctx, client, http.MethodGet, assetURL, token, "application/octet-stream")
	if err != nil {
		return imageAssociation{}, err
	}
	defer func() {
		_ = assetResponse.Body.Close()
	}()
	if assetResponse.StatusCode == http.StatusNotFound {
		return imageAssociation{}, nil
	}
	if assetResponse.StatusCode != http.StatusOK {
		return imageAssociation{}, githubStatusError("fetching digest asset", assetResponse)
	}
	data, err := io.ReadAll(io.LimitReader(assetResponse.Body, 4097))
	if err != nil {
		return imageAssociation{}, fmt.Errorf("reading digest asset: %w", err)
	}
	if len(data) > 4096 {
		return imageAssociation{}, errors.New("digest asset exceeds 4096 bytes")
	}
	return parseImageAssociation(image, data)
}

func githubAPIRequest(
	ctx context.Context,
	client *http.Client,
	method string,
	endpoint string,
	token string,
	accept string,
) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("creating GitHub API request: %w", err)
	}
	request.Header.Set("Accept", accept)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("User-Agent", "gobridge-release-verifier")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("calling GitHub API: %w", err)
	}
	return response, nil
}

func githubStatusError(operation string, response *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
	detail := strings.TrimSpace(string(data))
	if detail == "" {
		return fmt.Errorf("%s: GitHub API returned HTTP %d", operation, response.StatusCode)
	}
	return fmt.Errorf("%s: GitHub API returned HTTP %d: %s", operation, response.StatusCode, detail)
}

func parseImageAssociation(image string, data []byte) (imageAssociation, error) {
	if err := validateImageName(image); err != nil {
		return imageAssociation{}, err
	}
	value := string(data)
	if !strings.HasSuffix(value, "\n") || strings.Count(value, "\n") != 1 || strings.Contains(value, "\r") {
		return imageAssociation{}, errors.New("digest asset must contain exactly one newline-terminated line")
	}
	line := strings.TrimSuffix(value, "\n")
	prefix := image + "@"
	if !strings.HasPrefix(line, prefix) {
		return imageAssociation{}, fmt.Errorf("digest asset names %q, want image %q", line, image)
	}
	digest := strings.TrimPrefix(line, prefix)
	if !imageDigestPattern.MatchString(digest) {
		return imageAssociation{}, fmt.Errorf("digest asset contains invalid digest %q", digest)
	}
	return imageAssociation{Exists: true, Digest: digest}, nil
}

func validateImageName(image string) error {
	if image == "" || strings.ContainsAny(image, "@\r\n\t ") {
		return fmt.Errorf("image name %q is invalid", image)
	}
	return nil
}

func verifyRegistryDigest(
	ctx context.Context,
	runner commandRunner,
	image string,
	digest string,
) error {
	if err := validateImageName(image); err != nil {
		return err
	}
	if !imageDigestPattern.MatchString(digest) {
		return fmt.Errorf("registry digest %q is invalid", digest)
	}
	output, err := runner.run(ctx, commandRequest{
		Name: "docker",
		Args: []string{
			"buildx",
			"imagetools",
			"inspect",
			image + "@" + digest,
			"--format",
			"{{.Manifest.Digest}}",
		},
		Timeout: moduleQueryTimeout,
	})
	if err != nil {
		return fmt.Errorf("resolving registry digest %s@%s: %w", image, digest, err)
	}
	resolved := strings.TrimSpace(string(output))
	if resolved != digest {
		return fmt.Errorf("registry resolved %s@%s as %q", image, digest, resolved)
	}
	return nil
}

func decideImageAssociationUpload(
	initial imageAssociation,
	current imageAssociation,
	digest string,
) (bool, error) {
	if !imageDigestPattern.MatchString(digest) {
		return false, fmt.Errorf("release image digest %q is invalid", digest)
	}
	for name, association := range map[string]imageAssociation{
		"initial": initial,
		"current": current,
	} {
		if association.Exists {
			if association.Digest != digest {
				return false, fmt.Errorf(
					"%s release image digest is %q, want %q",
					name,
					association.Digest,
					digest,
				)
			}
		} else if association.Digest != "" {
			return false, fmt.Errorf("%s absent association has digest %q", name, association.Digest)
		}
	}
	if initial.Exists {
		if !current.Exists {
			return false, errors.New("recorded release image digest disappeared during workflow")
		}
		return false, nil
	}
	if current.Exists {
		return false, nil
	}
	return true, nil
}
