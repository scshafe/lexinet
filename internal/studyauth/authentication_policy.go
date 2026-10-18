package studyauth

import (
	"github.com/google/uuid"
	"tailscale.com/tailcfg"
)

// AuthenticationMode selects exactly one non-fallback Tailnet study
// authentication mechanism.
type AuthenticationMode string

const (
	AuthenticationModeTailnetNode AuthenticationMode = "tailnet_node"
	AuthenticationModeScopedToken AuthenticationMode = "scoped_token"
)

// Valid reports whether mode is one supported Tailnet study authentication
// mechanism. The two valid modes are mutually exclusive at startup.
func (mode AuthenticationMode) Valid() bool {
	return mode == AuthenticationModeTailnetNode || mode == AuthenticationModeScopedToken
}

// Policy is the complete startup input for the Tailnet study authentication
// boundary. AllowedProfileRefs must contain one to 32 exact study.v1 profile
// references. Tailnet-node mode requires RequiredNodeTag and AppCapability and
// forbids scoped-token fields. Scoped-token mode requires an independently
// provisioned subject UUID and absolute owner-only token file and forbids the
// node-mode fields.
type Policy struct {
	Mode                 AuthenticationMode
	RequiredNodeTag      string
	AppCapability        tailcfg.PeerCapability
	AllowedProfileRefs   []string
	ScopedTokenSubjectID uuid.UUID
	ScopedTokenFile      string
}
