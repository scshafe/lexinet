<!-- @canonical tailnet_native_tls_transport -->

# Native Tailnet TLS transport

This domain owns one capability: the lifecycle of Lexinet's embedded Tailscale
node and its native HTTPS listener. `OpenNativeTLS` is the only production
entry point. It returns a `NativeTLSEndpoint` that keeps the fixed
`tsnet.ListenTLS("tcp", ":443")` listener, the matching LocalAPI `WhoIs`
client, and server shutdown together.

The startup order is fixed and fail-closed: validate bounded configuration,
reject ambient enrollment or control-plane inputs, validate the existing
owner-only identity directory, construct a log-suppressed server, wait for
`Up`, obtain `LocalClient`, then open native TLS. `Up` and `ListenTLS` share one
startup budget because upstream `ListenTLS` performs another internal `Up`.
Budget exhaustion closes the server to cancel that hidden wait and joins the
listener call before returning. A partial failure closes the embedded server
exactly once. There is no raw TCP, Funnel, proxy-header identity, certificate
fallback, interactive login, or request-time fallback path in this package.

## Public interface

- `OpenNativeTLS(context.Context, Config) (*NativeTLSEndpoint, error)` opens the
  canonical transport. `Hostname` and an absolute (or `~/`-prefixed)
  `StateDir` are required. The resolved directory must already exist, must not
  be a symlink, must be owned by the effective service user on Unix, and must
  have mode `0700`. Path identity is compared before and after a descriptor
  open to detect replacement during validation. A zero `UpTimeout` means the
  bounded `DefaultUpTimeout`; values above `MaximumUpTimeout` are rejected.
- `ValidateNativeTLSConfiguration(Config) error` performs the same
  configuration, environment, and state-directory preflight without creating
  tsnet. Composition roots call it before migrations or other external work;
  `OpenNativeTLS` repeats it to defend the runtime boundary.
- `NativeTLSEndpoint.Listener()` returns the native TLS listener.
- `NativeTLSEndpoint.WhoIsClient()` returns the identity lookup capability that
  must be called with each HTTP request's exact `RemoteAddr`.
- `NativeTLSEndpoint.Close()` stops the embedded server once and is idempotent.

All failures crossing this domain use `*tailnet.Error`. Its code, message, and
context are safe to serialize. The retained internal cause exists for control
flow and tests only; never unwrap or render external causes into logs,
diagnostics, HTTP responses, or image overlays.

## Trust map

The protected assets are the embedded node identity, HTTPS private-key
material managed by Tailscale, and caller network identity. The boundary
rejects ambient `TS_DEBUG_ENV_FILE`, `TS_AUTHKEY`, OAuth/workload-identity
inputs, `TS_CONTROL_URL`, and `TSNET_FORCE_LOGIN`, so a service process cannot
load a second environment file, silently enroll, change control planes, or
replace persisted identity. Both Tailscale logger callbacks are discarded,
process-wide logtail buffering is disabled, and support-log upload is disabled
before startup.

This package proves transport identity only. It does not decide which tag,
grant, app capability, study operation, or profile a caller may use; the study
authentication/authorization boundary owns those decisions and must validate
every `WhoIs` response. It also does not claim that MagicDNS, HTTPS
certificates, Tailnet policy, node tags, or persisted state are provisioned.
Those are external deployment prerequisites and require real Tailnet evidence.
The deterministic certificate-failure unit test proves only that a
`ListenTLS` error closes this wrapper without a listener or fallback. It is not
evidence of certificate issuance, hostname validation, or a TLS handshake; no
Step 5 evidence gate may substitute that test for the real infrastructure
matrix.

Dependencies are the pinned `tailscale.com/tsnet` implementation and its
LocalAPI response types. The package does not import the HTTP server, study
service, or configuration loader.
