package mqttlocal

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// A broker that refuses a SUBSCRIBE.
//
// Mosquitto's acl_file cannot do this: the broker consults it when it accepts
// or delivers a message, never at SUBSCRIBE, so a denied filter is still
// granted. The fixture loads Mosquitto's dynamic security plugin instead, whose
// subscribe ACLs are checked at SUBSCRIBE. Its config is generated into the
// secure-material directory and names no one outside this fixture.

const (
	aclConfigName = "dynamic-security.json"
	aclPluginPath = "/usr/lib/mosquitto_dynamic_security.so" // eclipse-mosquitto image
	aclGroup      = "anonymous"
	aclRole       = "denied-subscriptions"
)

// ACL is the access the broker enforces at SUBSCRIBE time.
type ACL struct {
	// DeniedSubscriptions lists the topic filters a SUBSCRIBE is refused for,
	// matched literally: SUBACK reason code 0x87 (Not authorized) on MQTT 5,
	// 0x80 on MQTT 3.1.1. Every other SUBSCRIBE and every PUBLISH is allowed.
	DeniedSubscriptions []string
	// Users maps each username a client presents to its password. The broker
	// refuses a CONNECT carrying a username it does not know, even though
	// anonymous clients are allowed, so list every one the clients use.
	Users map[string]string
}

// WithACL makes the broker enforce acl for anonymous clients and for the
// listed users. It cannot be combined with WithAuth: list that user in
// ACL.Users instead.
func WithACL(acl ACL) Option {
	return func(c *config) { c.acl = &acl }
}

// dynamicSecurityConfig renders the plugin config for acl: everything allowed
// by default, the denied filters refused for anonymous clients and every user.
func dynamicSecurityConfig(c config) (string, error) {
	if c.username != "" {
		return "", errors.New("mqttlocal: WithACL cannot be combined with WithAuth; list the user in ACL.Users")
	}
	roles := []map[string]any{{"rolename": aclRole}}
	acls := make([]map[string]any, 0, len(c.acl.DeniedSubscriptions))
	for _, filter := range c.acl.DeniedSubscriptions {
		acls = append(acls, map[string]any{"acltype": "subscribeLiteral", "topic": filter, "priority": 0, "allow": false})
	}
	clients := make([]map[string]any, 0, len(c.acl.Users))
	for username, password := range c.acl.Users {
		salt, hash, err := passwordHash(password)
		if err != nil {
			return "", err
		}
		clients = append(clients, map[string]any{
			"username":   username,
			"password":   base64.StdEncoding.EncodeToString(hash),
			"salt":       base64.StdEncoding.EncodeToString(salt),
			"iterations": passwordIterations,
			"roles":      roles,
		})
	}
	doc, err := json.Marshal(map[string]any{
		"defaultACLAccess": map[string]bool{
			"publishClientSend": true, "publishClientReceive": true, "subscribe": true, "unsubscribe": true,
		},
		"clients":        clients,
		"groups":         []map[string]any{{"groupname": aclGroup, "roles": roles, "clients": []string{}}},
		"roles":          []map[string]any{{"rolename": aclRole, "acls": acls}},
		"anonymousGroup": aclGroup,
	})
	if err != nil {
		return "", fmt.Errorf("render the dynamic security config: %w", err)
	}
	return string(doc), nil
}
