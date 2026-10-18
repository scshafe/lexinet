package tailnet

import "fmt"

// ErrorCode is a stable machine-readable native Tailnet transport failure code.
type ErrorCode string

const (
	ErrorCodeConfigurationInvalid         ErrorCode = "TAILNET_CONFIGURATION_INVALID"
	ErrorCodeAmbientConfigurationRejected ErrorCode = "TAILNET_AMBIENT_CONFIGURATION_REJECTED"
	ErrorCodeStateDirectoryInvalid        ErrorCode = "TAILNET_STATE_DIRECTORY_INVALID"
	ErrorCodeServerCreateFailed           ErrorCode = "TAILNET_SERVER_CREATE_FAILED"
	ErrorCodeUpFailed                     ErrorCode = "TAILNET_UP_FAILED"
	ErrorCodeLocalClientFailed            ErrorCode = "TAILNET_LOCAL_CLIENT_FAILED"
	ErrorCodeNativeTLSListenFailed        ErrorCode = "TAILNET_NATIVE_TLS_LISTEN_FAILED"
	ErrorCodeCloseFailed                  ErrorCode = "TAILNET_CLOSE_FAILED"
)

// Error reports an allowlisted code, message, and context without rendering
// raw Tailscale, filesystem, environment, or certificate error text.
type Error struct {
	Code    ErrorCode         `json:"code"`
	Message string            `json:"message"`
	Context map[string]string `json:"context,omitempty"`
	cause   error
}

func (err *Error) Error() string {
	if err == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s: %s", err.Code, err.Message)
}

// Unwrap preserves the internal failure chain for programmatic inspection.
// Callers must never render the unwrapped external error into diagnostics.
func (err *Error) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.cause
}

func (err *Error) Is(target error) bool {
	tailnetTarget, ok := target.(*Error)
	return ok && err != nil && tailnetTarget != nil && tailnetTarget.Code == err.Code
}

func newError(code ErrorCode, message string, context map[string]string, cause error) *Error {
	return &Error{
		Code:    code,
		Message: message,
		Context: cloneStringMap(context),
		cause:   cause,
	}
}

func cloneStringMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	clone := make(map[string]string, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}
