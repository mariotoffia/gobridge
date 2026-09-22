package mqttlocal

import (
	"crypto/pbkdf2"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"maps"
	"slices"
	"testing"
)

// The broker's dynamic security file is generated once, when the fixture
// starts, while the fixture keeps the ACL it was given as its recorded
// configuration. If WithACL kept the caller's slice and map, a caller reusing
// them afterwards would change the recorded ACL but not the file the broker
// enforces, and a later RestartWith would restart a broker whose recorded
// configuration disagrees with its mounted ACL. Pinned at render time, without
// Docker.
//
// Category: unit (TESTS.md §1).

func TestWithACL_CallerMutationsDoNotReachTheBrokerConfig(t *testing.T) {
	denied := []string{"gobridge/acl/denied"}
	users := map[string]string{"bridge": "s3cret"}
	option := WithACL(ACL{DeniedSubscriptions: denied, Users: users})

	// The caller reuses its collections after building the option ...
	denied[0], users["bridge"], users["intruder"] = "gobridge/acl/other", "changed", "guess"
	c := defaultConfig()
	option(&c)
	// ... and again after a fixture applied it.
	denied[0], users["bridge"] = "gobridge/acl/later", "later"

	if want := []string{"gobridge/acl/denied"}; !slices.Equal(c.acl.DeniedSubscriptions, want) {
		t.Fatalf("recorded DeniedSubscriptions = %v, want %v", c.acl.DeniedSubscriptions, want)
	}
	if want := map[string]string{"bridge": "s3cret"}; !maps.Equal(c.acl.Users, want) {
		t.Fatalf("recorded Users = %v, want %v", c.acl.Users, want)
	}

	rendered, err := dynamicSecurityConfig(c)
	if err != nil {
		t.Fatalf("dynamicSecurityConfig: %v", err)
	}
	var doc struct {
		Clients []struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Salt     string `json:"salt"`
		} `json:"clients"`
		Roles []struct {
			ACLs []struct {
				Topic string `json:"topic"`
			} `json:"acls"`
		} `json:"roles"`
	}
	if err := json.Unmarshal([]byte(rendered), &doc); err != nil {
		t.Fatalf("the dynamic security config is not JSON: %v\n%s", err, rendered)
	}
	var topics []string
	for _, role := range doc.Roles {
		for _, acl := range role.ACLs {
			topics = append(topics, acl.Topic)
		}
	}
	if want := []string{"gobridge/acl/denied"}; !slices.Equal(topics, want) {
		t.Fatalf("rendered denied filters = %v, want %v", topics, want)
	}
	if len(doc.Clients) != 1 || doc.Clients[0].Username != "bridge" {
		t.Fatalf("rendered clients = %+v, want the one user the option was built with", doc.Clients)
	}
	salt, err := base64.StdEncoding.DecodeString(doc.Clients[0].Salt)
	if err != nil {
		t.Fatalf("rendered salt: %v", err)
	}
	hash, err := pbkdf2.Key(sha512.New, "s3cret", salt, passwordIterations, passwordHashBytes)
	if err != nil {
		t.Fatalf("derive the expected password hash: %v", err)
	}
	if base64.StdEncoding.EncodeToString(hash) != doc.Clients[0].Password {
		t.Fatal("the rendered password is not the one the option was built with")
	}
}
