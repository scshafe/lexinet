package tailnet

import (
	"context"
	"os"

	"tailscale.com/envknob"
	"tailscale.com/logtail"
	"tailscale.com/tsnet"
)

// maximumAuthKeyBytes bounds the single-use key read from stdin; real keys are
// well under 200 bytes.
const maximumAuthKeyBytes = 512

type enrollServer interface {
	Up(context.Context) error
	Close() error
}

type enrollServerFactory func(config normalizedConfig, authKey string) (enrollServer, error)

// Enroll performs the one-time enrollment of the identity directory that
// OpenNativeTLS later requires. It is deliberately not part of the serve path:
// serve never accepts a key, and Enroll never listens. The key is passed only
// as an argument, never read from the environment; the same ambient controls
// OpenNativeTLS rejects are rejected here, so a key cannot arrive by a second
// route. The state directory is created 0700 if absent and must be empty:
// re-enrolling an existing identity is refused rather than overwritten.
func Enroll(ctx context.Context, config Config, authKey string) error {
	return enroll(ctx, config, authKey, os.LookupEnv, newEnrollTSNetServer)
}

func enroll(
	ctx context.Context,
	config Config,
	authKey string,
	lookupEnvironment environmentLookup,
	factory enrollServerFactory,
) error {
	if ctx == nil {
		return configurationError("context")
	}
	if authKey == "" || len(authKey) > maximumAuthKeyBytes {
		return configurationError("auth_key")
	}
	if err := rejectAmbientTailscaleControls(lookupEnvironment); err != nil {
		return err
	}
	normalized, err := normalizeConfig(config)
	if err != nil {
		return err
	}
	if err := prepareEnrollmentDirectory(normalized.stateDir); err != nil {
		return err
	}
	server, err := factory(normalized, authKey)
	if err != nil || server == nil {
		return newError(ErrorCodeServerCreateFailed, "native Tailnet server could not be created", nil, err)
	}
	upContext, cancel := context.WithTimeout(ctx, normalized.upTimeout)
	defer cancel()
	upError := server.Up(upContext)
	closeError := server.Close()
	if upError != nil {
		return newError(ErrorCodeUpFailed, "native Tailnet server did not become ready", nil, upError)
	}
	if closeError != nil {
		return newError(ErrorCodeCloseFailed, "native Tailnet server did not close cleanly", nil, closeError)
	}
	return nil
}

// prepareEnrollmentDirectory creates the directory 0700 when it is absent and
// refuses one that already holds state.
func prepareEnrollmentDirectory(stateDirectory string) error {
	if err := os.Mkdir(stateDirectory, 0o700); err != nil && !os.IsExist(err) {
		return stateDirectoryFilesystemError(err)
	}
	if err := validateStateDirectory(stateDirectory); err != nil {
		return err
	}
	entries, err := os.ReadDir(stateDirectory)
	if err != nil {
		return stateDirectoryFilesystemError(err)
	}
	if len(entries) != 0 {
		return stateDirectoryError("already_enrolled", nil)
	}
	return nil
}

type nativeEnrollServer struct{ server *tsnet.Server }

func newEnrollTSNetServer(config normalizedConfig, authKey string) (enrollServer, error) {
	envknob.SetNoLogsNoSupport()
	logtail.Disable()
	return &nativeEnrollServer{server: &tsnet.Server{
		Hostname: config.hostname,
		Dir:      config.stateDir,
		AuthKey:  authKey,
		UserLogf: discardTailscaleLogf,
		Logf:     discardTailscaleLogf,
	}}, nil
}

func (server *nativeEnrollServer) Up(ctx context.Context) error {
	status, err := server.server.Up(ctx)
	if err != nil {
		return err
	}
	if status == nil {
		return errNativeTSNetStatusUnavailable
	}
	return nil
}

func (server *nativeEnrollServer) Close() error { return server.server.Close() }
