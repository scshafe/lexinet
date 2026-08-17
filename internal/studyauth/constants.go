package studyauth

const (
	tailnetStudyAuthenticationConfigurationOperation = "configure_tailnet_study_authentication"
	tailnetStudyAuthenticationRequestOperation       = "authenticate_tailnet_study_request"

	tailnetStudyApplicationCapabilityPath = "/cap/lexinet-study-v1"

	maximumRequiredNodeTagBytes               = 64
	maximumApplicationCapabilityNameBytes     = 320
	maximumStableNodeIDBytes                  = 256
	maximumRemoteAddressBytes                 = 128
	maximumApplicationCapabilityValues        = 16
	maximumApplicationCapabilityValueBytes    = 8_192
	maximumScopedTokenBytes                   = 256
	minimumScopedTokenBytes                   = 32
	maximumScopedTokenFileBytes               = maximumScopedTokenBytes + 1
	scopedTokenAuthorizationPrefix            = "Bearer "
	tailnetNodePrincipalDomainName            = "urn:lexinet:study-principal:tailnet-node:v1"
	tailnetStableNodeIDPrincipalSubjectPrefix = "tailscale.com/control/stable-node-id/v1\x00"
)

const (
	authenticationConfigurationFieldMode               = "mode"
	authenticationConfigurationFieldModeSpecificFields = "mode_specific_fields"
	authenticationConfigurationFieldRequiredNodeTag    = "required_node_tag"
	authenticationConfigurationFieldAppCapability      = "app_capability"
	authenticationConfigurationFieldAllowedProfileRefs = "allowed_profile_refs"
	authenticationConfigurationFieldScopedTokenSubject = "scoped_token_subject_id"
	authenticationConfigurationFieldScopedTokenFile    = "scoped_token_file"
)
