package credentials_test

import (
	"testing"

	"github.com/mariotoffia/gobridge/bridge/credentials"
	"github.com/mariotoffia/gobridge/bridge/types"
)

// dummyRepo implements types.CredentialsRepository for test purposes.
type dummyRepo struct {
	scheme    string
	namespace string
}

func (d dummyRepo) GetScheme() string    { return d.scheme }
func (d dummyRepo) GetNamespace() string { return d.namespace }
func (d dummyRepo) GetCredentials(serverURI string) (*types.Credentials, error) {
	return &types.Credentials{}, nil
}

func TestResolver_ResolveRepository(t *testing.T) {
	r := credentials.NewResolver()

	// Register several repositories
	root := dummyRepo{scheme: "pms", namespace: ""}
	tenantA := dummyRepo{scheme: "pms", namespace: "tenantA"}
	tenantAApp1 := dummyRepo{scheme: "pms", namespace: "tenantA/app1"}
	otherScheme := dummyRepo{scheme: "mqtt", namespace: "tenantA/app1"}

	r.RegisterRepository(root)
	r.RegisterRepository(tenantA)
	r.RegisterRepository(tenantAApp1)
	r.RegisterRepository(otherScheme)

	tests := []struct {
		serverURI     string
		wantNamespace string
		wantFound     bool
	}{
		{
			serverURI:     "pms://tenantA/app1/prod/db/password",
			wantNamespace: "tenantA/app1",
			wantFound:     true,
		},
		{
			serverURI:     "pms://tenantA/app2/prod/db/password",
			wantNamespace: "tenantA",
			wantFound:     true,
		},
		{
			serverURI:     "pms://tenantB/appX/xyz",
			wantNamespace: "",
			wantFound:     true,
		},
		{
			serverURI:     "mqtt://tenantA/app1/broker",
			wantNamespace: "tenantA/app1",
			wantFound:     true,
		},
		{
			serverURI:     "mqtt://tenantA/app2/broker",
			wantNamespace: "",
			wantFound:     false,
		},
		{
			serverURI:     "invalid-uri-format",
			wantNamespace: "",
			wantFound:     false,
		},
	}

	for _, tc := range tests {
		repo, found, err := r.ResolveRepository(tc.serverURI)
		if err != nil {
			if tc.wantFound {
				t.Errorf("ResolveRepository(%q) returned error %v, want no error", tc.serverURI, err)
			}
			// skip further checks if error
			continue
		}
		if found != tc.wantFound {
			t.Errorf("ResolveRepository(%q) found = %v, want %v", tc.serverURI, found, tc.wantFound)
			continue
		}
		if !found {
			// we expected no repository
			continue
		}
		gotNs := repo.GetNamespace()
		if gotNs != tc.wantNamespace {
			t.Errorf("ResolveRepository(%q) namespace = %q, want %q", tc.serverURI, gotNs, tc.wantNamespace)
		}
	}
}
