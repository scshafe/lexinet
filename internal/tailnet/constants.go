package tailnet

import "time"

const (
	NativeTLSNetwork = "tcp"
	NativeTLSAddress = ":443"

	DefaultUpTimeout = 30 * time.Second
	MaximumUpTimeout = 2 * time.Minute

	maximumHostnameBytes     = 63
	maximumStateDirPathBytes = 4096
)

var ambientTailscaleEnrollmentControls = [...]string{
	"TS_DEBUG_ENV_FILE",
	"TS_AUTHKEY",
	"TS_AUTH_KEY",
	"TS_CLIENT_SECRET",
	"TS_CLIENT_ID",
	"TS_ID_TOKEN",
	"TS_AUDIENCE",
	"TS_CONTROL_URL",
	"TSNET_FORCE_LOGIN",
}
