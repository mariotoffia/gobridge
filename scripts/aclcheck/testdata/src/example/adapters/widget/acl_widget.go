// Package widget is an aclcheck analysistest fixture adapter package
// (its path contains /adapters/). acl_widget.go is the ACL file: it may
// import the SDK but must not re-export SDK types under an exported name.
package widget

import sdk "github.com/aws/aws-sdk-go-v2/service/fakesvc"

// Leaked re-exports an SDK type under an exported alias — flagged.
type Leaked = sdk.Message // want `exported type alias "Leaked" re-exports SDK type from "github.com/aws/aws-sdk-go-v2/service/fakesvc"`

// Wrapped is an exported defined type whose source is an SDK type —
// flagged.
type Wrapped sdk.Client // want `exported type "Wrapped" re-exports SDK type`

// LeakedSlice exports a slice of an SDK type — also a leak.
type LeakedSlice []sdk.Message // want `exported type "LeakedSlice" re-exports SDK type`

// Allowed re-exports the SDK type but is explicitly annotated, so the
// export is intentional and permitted.
//
//aclcheck:allow-export
type Allowed = sdk.Message

// internalAlias is unexported, so it never leaks past the package.
type internalAlias = sdk.Message

// Good is the correct ACL pattern: an SDK value behind an unexported
// field, exposed only through a domain-typed accessor.
type Good struct {
	msg sdk.Message
}

// Body crosses the boundary as a plain string, not an SDK type.
func (g Good) Body() string { return g.msg.Body }

// NewGood builds a Good from raw bytes at the SDK boundary.
func NewGood(body string) Good { return Good{msg: sdk.Message{Body: body}} }

// Raw is a method on an exported receiver that returns an SDK type —
// flagged: a non-ACL caller can name the SDK type through it.
func (g Good) Raw() sdk.Message { return g.msg } // want `exported function "Raw" exposes SDK type from "github.com/aws/aws-sdk-go-v2/service/fakesvc"`

// RawAllowed returns the SDK type but is explicitly annotated, so the
// export is intentional and permitted.
//
//aclcheck:allow-export
func (g Good) RawAllowed() sdk.Message { return g.msg }

// NewClient is an exported package-level func taking an SDK type — also
// flagged on its parameter.
func NewClient(c sdk.Client) Good { return Good{} } // want `exported function "NewClient" exposes SDK type from "github.com/aws/aws-sdk-go-v2/service/fakesvc"`

// raw is unexported, so it never leaks past the package even though it
// returns an SDK type.
func (g Good) raw() sdk.Message { return g.msg }

var _ = internalAlias{}
