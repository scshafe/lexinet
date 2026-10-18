package studyauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/netip"
	"reflect"
	"sort"
	"strings"

	"github.com/google/uuid"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"

	"lexinet/internal/model"
	"lexinet/internal/studysession"
)

// WhoIsClient is the complete per-request Tailnet identity dependency. A
// tailscale local.Client satisfies this interface directly.
type WhoIsClient interface {
	WhoIs(context.Context, string) (*apitype.WhoIsResponse, error)
}

// Boundary is the @canonical tailnet_study_authentication boundary. It owns
// startup policy validation, fresh per-request WhoIs authentication, exact
// application-capability validation, opaque principal derivation, and the
// issuer-bound claims consumed by study authorization.
type Boundary struct {
	mode                  AuthenticationMode
	requiredNodeTag       string
	appCapability         tailcfg.PeerCapability
	allowedProfileRefs    map[string]struct{}
	allowedProfileRefList []string
	scopedTokenSubjectID  uuid.UUID
	scopedTokenDigest     [sha256.Size]byte
	scopedTokenReady      bool
}

type authenticationClaimsContextKey struct{}

type authenticationClaims struct {
	issuer      *Boundary
	principal   model.StudyPrincipalID
	authorizers []*studysession.ScopedStudyAuthorizer
}

var _ studysession.StudyAuthorizer = (*Boundary)(nil)

// NewBoundary validates and detaches policy before any listener is opened. It
// returns STUDY_SCHEMA_INVALID with fixed, non-secret context for invalid mode,
// tag, capability, profile ceiling, or scoped-token file configuration. It
// returns only STUDY_SCHEMA_INVALID for expected configuration failures.
func NewBoundary(policy Policy) (*Boundary, error) {
	if !policy.Mode.Valid() {
		return nil, newAuthenticationConfigurationError(authenticationConfigurationFieldMode)
	}
	allowedProfileRefs, allowedProfileRefList, err := validateAllowedProfileRefs(policy.AllowedProfileRefs)
	if err != nil {
		return nil, err
	}

	boundary := &Boundary{
		mode:                  policy.Mode,
		allowedProfileRefs:    allowedProfileRefs,
		allowedProfileRefList: allowedProfileRefList,
	}
	switch policy.Mode {
	case AuthenticationModeTailnetNode:
		if policy.ScopedTokenSubjectID != uuid.Nil || policy.ScopedTokenFile != "" {
			return nil, newAuthenticationConfigurationError(authenticationConfigurationFieldModeSpecificFields)
		}
		if len(policy.RequiredNodeTag) == 0 || len(policy.RequiredNodeTag) > maximumRequiredNodeTagBytes ||
			tailcfg.CheckTag(policy.RequiredNodeTag) != nil {
			return nil, newAuthenticationConfigurationError(authenticationConfigurationFieldRequiredNodeTag)
		}
		if !validApplicationCapabilityName(policy.AppCapability) {
			return nil, newAuthenticationConfigurationError(authenticationConfigurationFieldAppCapability)
		}
		boundary.requiredNodeTag = policy.RequiredNodeTag
		boundary.appCapability = policy.AppCapability
	case AuthenticationModeScopedToken:
		if policy.RequiredNodeTag != "" || policy.AppCapability != "" {
			return nil, newAuthenticationConfigurationError(authenticationConfigurationFieldModeSpecificFields)
		}
		if policy.ScopedTokenSubjectID == uuid.Nil {
			return nil, newAuthenticationConfigurationError(authenticationConfigurationFieldScopedTokenSubject)
		}
		digest, digestErr := readScopedTokenDigest(policy.ScopedTokenFile)
		if digestErr != nil {
			return nil, newAuthenticationConfigurationError(authenticationConfigurationFieldScopedTokenFile)
		}
		boundary.scopedTokenSubjectID = policy.ScopedTokenSubjectID
		boundary.scopedTokenDigest = digest
		boundary.scopedTokenReady = true
	}
	return boundary, nil
}

// Authenticate performs a fresh WhoIs lookup for remoteAddr and returns a
// derived context plus opaque principal. authorizationValues must be empty in
// tailnet-node mode and contain exactly one `Bearer <token>` value in scoped-
// token mode. Failures are fixed structured study errors and never retain or
// expose transport identity, capability JSON, token bytes, or external causes.
// Expected failures use UNAUTHORIZED, STUDY_AUTHORIZATION_DENIED, or retryable
// STUDY_SERVICE_UNAVAILABLE; invalid internal composition uses INTERNAL.
func (boundary *Boundary) Authenticate(
	ctx context.Context,
	remoteAddr string,
	whoIsClient WhoIsClient,
	authorizationValues []string,
) (context.Context, model.StudyPrincipalID, error) {
	if boundary == nil || ctx == nil || !validWhoIsClient(whoIsClient) || !boundary.valid() {
		return nil, model.StudyPrincipalID{}, newAuthenticationRequestError(model.StudyErrorCodeInternal)
	}
	if !validWhoIsRemoteAddress(remoteAddr) {
		return nil, model.StudyPrincipalID{}, newAuthenticationRequestError(model.StudyErrorCodeUnauthorized)
	}
	whoIs, err := whoIsClient.WhoIs(ctx, remoteAddr)
	if err != nil {
		return nil, model.StudyPrincipalID{}, newAuthenticationRequestError(model.StudyErrorCodeServiceUnavailable)
	}
	if whoIs == nil || whoIs.Node == nil || whoIs.Node.Expired || !validStableNodeID(whoIs.Node.StableID) {
		return nil, model.StudyPrincipalID{}, newAuthenticationRequestError(model.StudyErrorCodeUnauthorized)
	}

	var principal model.StudyPrincipalID
	var grants []studysession.ScopedStudyAuthorizationGrant
	switch boundary.mode {
	case AuthenticationModeTailnetNode:
		if len(authorizationValues) != 0 || len(whoIs.Node.Tags) != 1 ||
			whoIs.Node.Tags[0] != boundary.requiredNodeTag {
			return nil, model.StudyPrincipalID{}, newAuthenticationRequestError(model.StudyErrorCodeAuthorizationDenied)
		}
		capabilityValues, present := whoIs.CapMap[boundary.appCapability]
		if !present {
			return nil, model.StudyPrincipalID{}, newAuthenticationRequestError(model.StudyErrorCodeAuthorizationDenied)
		}
		permissionObjects, parseErr := parseApplicationCapabilityValues(capabilityValues, boundary.allowedProfileRefs)
		if parseErr != nil {
			return nil, model.StudyPrincipalID{}, parseErr
		}
		principal, err = deriveTailnetNodePrincipal(whoIs.Node.StableID)
		grants = make([]studysession.ScopedStudyAuthorizationGrant, 0, len(permissionObjects))
		for _, permissions := range permissionObjects {
			grants = append(grants, studysession.ScopedStudyAuthorizationGrant{
				AllowedOperations:         sortedStudyOperations(permissions.operations),
				AllowedLearnerProfileRefs: sortedProfileRefs(permissions.profileRefs),
			})
		}
	case AuthenticationModeScopedToken:
		if !boundary.validScopedTokenAuthorization(authorizationValues) {
			return nil, model.StudyPrincipalID{}, newAuthenticationRequestError(model.StudyErrorCodeUnauthorized)
		}
		principal, err = model.NewStudyPrincipalID(
			model.StudyPrincipalNamespaceScopedToken,
			boundary.scopedTokenSubjectID,
		)
		grants = []studysession.ScopedStudyAuthorizationGrant{{
			AllowedOperations: []studysession.StudyOperation{
				studysession.StudyOperationCreateOrResume,
				studysession.StudyOperationSync,
			},
			AllowedLearnerProfileRefs: append([]string(nil), boundary.allowedProfileRefList...),
		}}
	default:
		return nil, model.StudyPrincipalID{}, newAuthenticationRequestError(model.StudyErrorCodeInternal)
	}
	if err != nil || !principal.Valid() {
		return nil, model.StudyPrincipalID{}, newAuthenticationRequestError(model.StudyErrorCodeInternal)
	}

	requestAuthorizers := make([]*studysession.ScopedStudyAuthorizer, 0, len(grants))
	for _, grant := range grants {
		grant.Principal = principal
		requestAuthorizer, authorizerErr := studysession.NewScopedStudyAuthorizer(
			[]studysession.ScopedStudyAuthorizationGrant{grant},
		)
		if authorizerErr != nil {
			return nil, model.StudyPrincipalID{}, newAuthenticationRequestError(model.StudyErrorCodeInternal)
		}
		requestAuthorizers = append(requestAuthorizers, requestAuthorizer)
	}
	claims := &authenticationClaims{
		issuer:      boundary,
		principal:   principal,
		authorizers: requestAuthorizers,
	}
	return context.WithValue(ctx, authenticationClaimsContextKey{}, claims), principal, nil
}

// PrincipalFromContext returns the opaque authenticated principal only when
// ctx carries complete claims issued by this exact Boundary instance.
func (boundary *Boundary) PrincipalFromContext(ctx context.Context) (model.StudyPrincipalID, bool) {
	claims, ok := boundary.claimsFromContext(ctx)
	if !ok {
		return model.StudyPrincipalID{}, false
	}
	return claims.principal, true
}

// AuthorizeStudy preserves the exact operator policy used by the loopback
// bearer path. Every non-operator request requires claims issued by this exact
// Boundary for the exact request principal, then delegates operation, stage,
// and profile checks to canonical scoped study authorizers created from fresh
// authentication evidence. Tailnet authentication never derives operator.
// Failures use UNAUTHORIZED, STUDY_AUTHORIZATION_DENIED, or INTERNAL.
func (boundary *Boundary) AuthorizeStudy(ctx context.Context, request studysession.StudyAuthorizationRequest) error {
	if boundary == nil || !boundary.valid() {
		return newAuthenticationRequestError(model.StudyErrorCodeInternal)
	}
	if !request.Principal.Valid() {
		return newAuthenticationRequestError(model.StudyErrorCodeUnauthorized)
	}
	if request.Principal == model.OperatorStudyPrincipalID() {
		return (studysession.OperatorStudyAuthorizer{}).AuthorizeStudy(ctx, request)
	}
	claims, ok := boundary.claimsFromContext(ctx)
	if !ok || claims.principal != request.Principal {
		return newAuthenticationRequestError(model.StudyErrorCodeAuthorizationDenied)
	}
	var denied error
	for _, authorizer := range claims.authorizers {
		if err := authorizer.AuthorizeStudy(ctx, request); err == nil {
			return nil
		} else {
			denied = err
		}
	}
	if denied == nil {
		return newAuthenticationRequestError(model.StudyErrorCodeInternal)
	}
	return denied
}

func (boundary *Boundary) claimsFromContext(ctx context.Context) (*authenticationClaims, bool) {
	if boundary == nil || ctx == nil {
		return nil, false
	}
	claims, ok := ctx.Value(authenticationClaimsContextKey{}).(*authenticationClaims)
	if !ok || claims == nil || claims.issuer != boundary || !claims.principal.Valid() || len(claims.authorizers) == 0 {
		return nil, false
	}
	for _, authorizer := range claims.authorizers {
		if authorizer == nil {
			return nil, false
		}
	}
	return claims, true
}

func (boundary *Boundary) valid() bool {
	if boundary == nil || !boundary.mode.Valid() || len(boundary.allowedProfileRefs) == 0 ||
		len(boundary.allowedProfileRefList) != len(boundary.allowedProfileRefs) {
		return false
	}
	switch boundary.mode {
	case AuthenticationModeTailnetNode:
		return boundary.requiredNodeTag != "" && boundary.appCapability != "" &&
			boundary.scopedTokenSubjectID == uuid.Nil && !boundary.scopedTokenReady
	case AuthenticationModeScopedToken:
		return boundary.requiredNodeTag == "" && boundary.appCapability == "" &&
			boundary.scopedTokenSubjectID != uuid.Nil && boundary.scopedTokenReady
	default:
		return false
	}
}

func validateAllowedProfileRefs(profileRefs []string) (map[string]struct{}, []string, error) {
	if len(profileRefs) == 0 || len(profileRefs) > studysession.StudyMaximumScopedAuthorizationProfileRefsPerGrant {
		return nil, nil, newAuthenticationConfigurationError(authenticationConfigurationFieldAllowedProfileRefs)
	}
	allowed := make(map[string]struct{}, len(profileRefs))
	detached := make([]string, 0, len(profileRefs))
	for _, profileRef := range profileRefs {
		if !studysession.ValidStudyProfileRef(profileRef) {
			return nil, nil, newAuthenticationConfigurationError(authenticationConfigurationFieldAllowedProfileRefs)
		}
		if _, duplicate := allowed[profileRef]; duplicate {
			return nil, nil, newAuthenticationConfigurationError(authenticationConfigurationFieldAllowedProfileRefs)
		}
		allowed[profileRef] = struct{}{}
		detached = append(detached, profileRef)
	}
	sort.Strings(detached)
	return allowed, detached, nil
}

func validWhoIsRemoteAddress(value string) bool {
	if len(value) == 0 || len(value) > maximumRemoteAddressBytes || value != strings.TrimSpace(value) {
		return false
	}
	if addressPort, err := netip.ParseAddrPort(value); err == nil {
		return addressPort.Port() != 0
	}
	_, err := netip.ParseAddr(value)
	return err == nil
}

func validWhoIsClient(client WhoIsClient) bool {
	if client == nil {
		return false
	}
	value := reflect.ValueOf(client)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return !value.IsNil()
	default:
		return true
	}
}

func validStableNodeID(stableID tailcfg.StableNodeID) bool {
	return len(stableID) >= 1 && len(stableID) <= maximumStableNodeIDBytes
}

func deriveTailnetNodePrincipal(stableID tailcfg.StableNodeID) (model.StudyPrincipalID, error) {
	domainID := uuid.NewSHA1(uuid.NameSpaceURL, []byte(tailnetNodePrincipalDomainName))
	subjectID := uuid.NewSHA1(domainID, []byte(tailnetStableNodeIDPrincipalSubjectPrefix+string(stableID)))
	return model.NewStudyPrincipalID(model.StudyPrincipalNamespaceTailnetNode, subjectID)
}

func (boundary *Boundary) validScopedTokenAuthorization(values []string) bool {
	if boundary == nil || len(values) != 1 {
		return false
	}
	token, present := strings.CutPrefix(values[0], scopedTokenAuthorizationPrefix)
	if !present || !validScopedTokenString(token) {
		return false
	}
	candidateDigest := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(candidateDigest[:], boundary.scopedTokenDigest[:]) == 1
}

func sortedStudyOperations(values map[studysession.StudyOperation]struct{}) []studysession.StudyOperation {
	result := make([]studysession.StudyOperation, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left] < result[right]
	})
	return result
}

func sortedProfileRefs(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
