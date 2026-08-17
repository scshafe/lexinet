package studyauth

import (
	"bytes"
	"encoding/json"
	"net"
	"strings"

	"tailscale.com/tailcfg"

	"lexinet/internal/model"
	"lexinet/internal/studyjson"
	"lexinet/internal/studysession"
)

type applicationCapabilityPermissions struct {
	operations  map[studysession.StudyOperation]struct{}
	profileRefs map[string]struct{}
}

func validApplicationCapabilityName(capability tailcfg.PeerCapability) bool {
	value := string(capability)
	if len(value) == 0 || len(value) > maximumApplicationCapabilityNameBytes {
		return false
	}
	hostname, present := strings.CutSuffix(value, tailnetStudyApplicationCapabilityPath)
	if !present || hostname == "" || !validCapabilityDomainName(hostname) {
		return false
	}
	return hostname == strings.ToLower(hostname)
}

func validCapabilityDomainName(hostname string) bool {
	if len(hostname) == 0 || len(hostname) > 253 || !strings.Contains(hostname, ".") ||
		strings.HasPrefix(hostname, ".") || strings.HasSuffix(hostname, ".") || net.ParseIP(hostname) != nil {
		return false
	}
	for _, label := range strings.Split(hostname, ".") {
		if len(label) == 0 || len(label) > 63 || !asciiLowercaseLetterOrDigit(label[0]) ||
			!asciiLowercaseLetterOrDigit(label[len(label)-1]) {
			return false
		}
		for _, value := range []byte(label) {
			if !asciiLowercaseLetterOrDigit(value) && value != '-' {
				return false
			}
		}
	}
	return true
}

func asciiLowercaseLetterOrDigit(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= '0' && value <= '9'
}

func parseApplicationCapabilityValues(
	values []tailcfg.RawMessage,
	locallyAllowedProfileRefs map[string]struct{},
) ([]applicationCapabilityPermissions, error) {
	if len(values) == 0 || len(values) > maximumApplicationCapabilityValues {
		return nil, newAuthenticationRequestError(model.StudyErrorCodeAuthorizationDenied)
	}

	permissions := make([]applicationCapabilityPermissions, 0, len(values))
	canonicalValues := make(map[string]struct{}, len(values))
	for _, value := range values {
		if len(value) == 0 || len(value) > maximumApplicationCapabilityValueBytes {
			return nil, newAuthenticationRequestError(model.StudyErrorCodeAuthorizationDenied)
		}
		canonical, err := studyjson.CanonicalizeUnique([]byte(value))
		if err != nil {
			return nil, newAuthenticationRequestError(model.StudyErrorCodeAuthorizationDenied)
		}
		canonicalKey := string(canonical)
		if _, duplicate := canonicalValues[canonicalKey]; duplicate {
			return nil, newAuthenticationRequestError(model.StudyErrorCodeAuthorizationDenied)
		}
		canonicalValues[canonicalKey] = struct{}{}

		valuePermissions, err := parseApplicationCapabilityValue(canonical, locallyAllowedProfileRefs)
		if err != nil {
			return nil, err
		}
		if len(valuePermissions.operations) != 0 && len(valuePermissions.profileRefs) != 0 {
			permissions = append(permissions, valuePermissions)
		}
	}
	if len(permissions) == 0 {
		return nil, newAuthenticationRequestError(model.StudyErrorCodeAuthorizationDenied)
	}
	return permissions, nil
}

func parseApplicationCapabilityValue(
	canonical []byte,
	locallyAllowedProfileRefs map[string]struct{},
) (applicationCapabilityPermissions, error) {
	var members map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&members); err != nil || len(members) != 2 {
		return applicationCapabilityPermissions{}, newAuthenticationRequestError(model.StudyErrorCodeAuthorizationDenied)
	}
	operationsJSON, hasOperations := members["operations"]
	profileRefsJSON, hasProfileRefs := members["profile_refs"]
	if !hasOperations || !hasProfileRefs {
		return applicationCapabilityPermissions{}, newAuthenticationRequestError(model.StudyErrorCodeAuthorizationDenied)
	}

	var operations []studysession.StudyOperation
	var profileRefs []string
	if err := json.Unmarshal(operationsJSON, &operations); err != nil ||
		len(operations) == 0 || len(operations) > studysession.StudyMaximumScopedAuthorizationOperationsPerGrant {
		return applicationCapabilityPermissions{}, newAuthenticationRequestError(model.StudyErrorCodeAuthorizationDenied)
	}
	if err := json.Unmarshal(profileRefsJSON, &profileRefs); err != nil ||
		len(profileRefs) == 0 || len(profileRefs) > studysession.StudyMaximumScopedAuthorizationProfileRefsPerGrant {
		return applicationCapabilityPermissions{}, newAuthenticationRequestError(model.StudyErrorCodeAuthorizationDenied)
	}

	permissions := applicationCapabilityPermissions{
		operations:  make(map[studysession.StudyOperation]struct{}, len(operations)),
		profileRefs: make(map[string]struct{}, len(profileRefs)),
	}
	seenOperations := make(map[studysession.StudyOperation]struct{}, len(operations))
	for _, operation := range operations {
		if !operation.Valid() {
			return applicationCapabilityPermissions{}, newAuthenticationRequestError(model.StudyErrorCodeAuthorizationDenied)
		}
		if _, duplicate := seenOperations[operation]; duplicate {
			return applicationCapabilityPermissions{}, newAuthenticationRequestError(model.StudyErrorCodeAuthorizationDenied)
		}
		seenOperations[operation] = struct{}{}
		if locallyAllowedStudyOperation(operation) {
			permissions.operations[operation] = struct{}{}
		}
	}
	seenProfileRefs := make(map[string]struct{}, len(profileRefs))
	for _, profileRef := range profileRefs {
		if !studysession.ValidStudyProfileRef(profileRef) {
			return applicationCapabilityPermissions{}, newAuthenticationRequestError(model.StudyErrorCodeAuthorizationDenied)
		}
		if _, duplicate := seenProfileRefs[profileRef]; duplicate {
			return applicationCapabilityPermissions{}, newAuthenticationRequestError(model.StudyErrorCodeAuthorizationDenied)
		}
		seenProfileRefs[profileRef] = struct{}{}
		if _, locallyAllowed := locallyAllowedProfileRefs[profileRef]; locallyAllowed {
			permissions.profileRefs[profileRef] = struct{}{}
		}
	}
	return permissions, nil
}

func locallyAllowedStudyOperation(operation studysession.StudyOperation) bool {
	return operation == studysession.StudyOperationCreateOrResume || operation == studysession.StudyOperationSync
}
