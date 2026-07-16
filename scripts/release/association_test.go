package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

const (
	testReleaseImage  = "ghcr.io/mariotoffia/gobridge"
	testReleaseDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func TestFetchImageAssociation_NoAsset(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		body   any
	}{
		{name: "release not found", status: http.StatusNotFound},
		{name: "release has no asset", status: http.StatusOK, body: map[string]any{"assets": []any{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(tt.status)
				if tt.body != nil {
					if err := json.NewEncoder(writer).Encode(tt.body); err != nil {
						t.Errorf("encode response: %v", err)
					}
				}
			}))
			t.Cleanup(server.Close)

			association, err := fetchImageAssociation(
				context.Background(),
				server.Client(),
				server.URL,
				"mariotoffia/gobridge",
				"cmd/gobridge/v0.3.0",
				testReleaseImage,
				"token",
			)
			if err != nil {
				t.Fatalf("fetchImageAssociation() error = %v", err)
			}
			if association.Exists || association.Digest != "" {
				t.Fatalf("association = %#v, want absent", association)
			}
		})
	}
}

func TestFetchImageAssociation_ValidAssetResumesExactDigest(t *testing.T) {
	t.Parallel()

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/repos/mariotoffia/gobridge/releases/tags/cmd/gobridge/v0.3.0":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"assets": []map[string]string{{
					"name": "gobridge-image-digest.txt",
					"url":  server.URL + "/asset/1",
				}},
			})
		case "/asset/1":
			_, _ = fmt.Fprintf(writer, "%s@%s\n", testReleaseImage, testReleaseDigest)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)

	association, err := fetchImageAssociation(
		context.Background(),
		server.Client(),
		server.URL,
		"mariotoffia/gobridge",
		"cmd/gobridge/v0.3.0",
		testReleaseImage,
		"token",
	)
	if err != nil {
		t.Fatalf("fetchImageAssociation() error = %v", err)
	}
	if !association.Exists || association.Digest != testReleaseDigest {
		t.Fatalf("association = %#v", association)
	}
}

func TestFetchImageAssociation_RejectsMalformedWrongOrFailedAsset(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		releaseCode int
		assetCode   int
		assetBody   string
	}{
		{name: "malformed", releaseCode: http.StatusOK, assetCode: http.StatusOK, assetBody: "not-a-digest\n"},
		{
			name:        "wrong image",
			releaseCode: http.StatusOK,
			assetCode:   http.StatusOK,
			assetBody:   "ghcr.io/other/image@" + testReleaseDigest + "\n",
		},
		{name: "release auth failure", releaseCode: http.StatusUnauthorized},
		{name: "release network class failure", releaseCode: http.StatusInternalServerError},
		{name: "asset failure", releaseCode: http.StatusOK, assetCode: http.StatusBadGateway},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/asset/1" {
					writer.WriteHeader(tt.assetCode)
					_, _ = writer.Write([]byte(tt.assetBody))
					return
				}
				writer.WriteHeader(tt.releaseCode)
				if tt.releaseCode == http.StatusOK {
					_ = json.NewEncoder(writer).Encode(map[string]any{
						"assets": []map[string]string{{
							"name": "gobridge-image-digest.txt",
							"url":  server.URL + "/asset/1",
						}},
					})
				}
			}))
			t.Cleanup(server.Close)

			if _, err := fetchImageAssociation(
				context.Background(),
				server.Client(),
				server.URL,
				"mariotoffia/gobridge",
				"cmd/gobridge/v0.3.0",
				testReleaseImage,
				"token",
			); err == nil {
				t.Fatal("fetchImageAssociation() error = nil")
			}
		})
	}
}

func TestFetchImageAssociation_RejectsNetworkFailure(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	client := server.Client()
	apiURL := server.URL
	server.Close()

	if _, err := fetchImageAssociation(
		context.Background(),
		client,
		apiURL,
		"mariotoffia/gobridge",
		"cmd/gobridge/v0.3.0",
		testReleaseImage,
		"token",
	); err == nil {
		t.Fatal("fetchImageAssociation() error = nil for network failure")
	}
}

func TestVerifyRegistryDigest_RejectsMissingOrMismatchedDigest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		output string
		err    error
	}{
		{name: "missing", err: errors.New("manifest unknown")},
		{name: "mismatch", output: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			runner := &recordingRunner{outputs: [][]byte{[]byte(tt.output)}, err: tt.err}
			if err := verifyRegistryDigest(
				context.Background(),
				runner,
				testReleaseImage,
				testReleaseDigest,
			); err == nil {
				t.Fatal("verifyRegistryDigest() error = nil")
			}
		})
	}
}

func TestVerifyRegistryDigest_AcceptsExactDigest(t *testing.T) {
	t.Parallel()

	runner := &recordingRunner{outputs: [][]byte{[]byte(testReleaseDigest + "\n")}}
	if err := verifyRegistryDigest(
		context.Background(),
		runner,
		testReleaseImage,
		testReleaseDigest,
	); err != nil {
		t.Fatalf("verifyRegistryDigest() error = %v", err)
	}
}

func TestDecideImageAssociationUpload_HandlesConcurrentAsset(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		initial imageAssociation
		current imageAssociation
		want    bool
		wantErr bool
	}{
		{name: "no asset uploads", want: true},
		{
			name:    "initial same digest resumes",
			initial: imageAssociation{Exists: true, Digest: testReleaseDigest},
			current: imageAssociation{Exists: true, Digest: testReleaseDigest},
		},
		{
			name:    "initial digest disappearance fails",
			initial: imageAssociation{Exists: true, Digest: testReleaseDigest},
			wantErr: true,
		},
		{
			name:    "concurrent same digest resumes",
			current: imageAssociation{Exists: true, Digest: testReleaseDigest},
		},
		{
			name:    "concurrent different digest fails",
			current: imageAssociation{Exists: true, Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := decideImageAssociationUpload(
				tt.initial,
				tt.current,
				testReleaseDigest,
			)
			if (err != nil) != tt.wantErr {
				t.Fatalf("decideImageAssociationUpload() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("decideImageAssociationUpload() = %v, want %v", got, tt.want)
			}
		})
	}
}
