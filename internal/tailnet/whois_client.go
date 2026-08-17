package tailnet

import (
	"context"

	"tailscale.com/client/tailscale/apitype"
)

// WhoIsClient is the complete caller-identity capability exposed by the
// embedded Tailnet node. Authorization remains outside this transport domain.
type WhoIsClient interface {
	WhoIs(context.Context, string) (*apitype.WhoIsResponse, error)
}
