package studysession

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"lexinet/internal/model"
)

const (
	scopedStudyAuthorizerPrimaryProfileRef   = "learner.primary"
	scopedStudyAuthorizerSecondaryProfileRef = "learner.secondary"
)

func TestScopedStudyAuthorizerAllowsExactConfiguredCreateAndSync(t *testing.T) {
	t.Parallel()
	authorizer, principal := newScopedStudyAuthorizerForTest(
		t,
		[]StudyOperation{StudyOperationCreateOrResume, StudyOperationSync},
		[]string{scopedStudyAuthorizerPrimaryProfileRef},
	)

	for _, operation := range []StudyOperation{StudyOperationCreateOrResume, StudyOperationSync} {
		if err := authorizer.AuthorizeStudy(context.Background(), StudyAuthorizationRequest{
			Principal:  principal,
			Operation:  operation,
			Stage:      StudyAuthorizationStageCapability,
			ProfileRef: scopedStudyAuthorizerSecondaryProfileRef,
		}); err != nil {
			t.Fatalf("authorize %s capability: %v", operation, err)
		}
		if err := authorizer.AuthorizeStudy(context.Background(), StudyAuthorizationRequest{
			Principal:  principal,
			Operation:  operation,
			Stage:      StudyAuthorizationStageResolvedTarget,
			ProfileRef: scopedStudyAuthorizerPrimaryProfileRef,
		}); err != nil {
			t.Fatalf("authorize %s resolved target: %v", operation, err)
		}
	}
}

func TestScopedStudyAuthorizerPreservesOperatorPolicy(t *testing.T) {
	t.Parallel()
	authorizer, _ := newScopedStudyAuthorizerForTest(
		t,
		[]StudyOperation{StudyOperationSync},
		[]string{scopedStudyAuthorizerPrimaryProfileRef},
	)

	for _, operation := range []StudyOperation{StudyOperationCreateOrResume, StudyOperationGet, StudyOperationSync} {
		for _, stage := range []StudyAuthorizationStage{
			StudyAuthorizationStageCapability,
			StudyAuthorizationStageResolvedTarget,
		} {
			if err := authorizer.AuthorizeStudy(context.Background(), StudyAuthorizationRequest{
				Principal:  model.OperatorStudyPrincipalID(),
				Operation:  operation,
				Stage:      stage,
				ProfileRef: "operator-profile-not-in-scoped-grants",
			}); err != nil {
				t.Fatalf("authorize operator %s at %s: %v", operation, stage, err)
			}
		}
	}
}

func TestScopedStudyAuthorizerDeniesUnconfiguredPrincipal(t *testing.T) {
	t.Parallel()
	authorizer, _ := newScopedStudyAuthorizerForTest(
		t,
		[]StudyOperation{StudyOperationSync},
		[]string{scopedStudyAuthorizerPrimaryProfileRef},
	)
	unconfiguredPrincipal := newScopedStudyPrincipalForTest(t)

	err := authorizer.AuthorizeStudy(context.Background(), StudyAuthorizationRequest{
		Principal: unconfiguredPrincipal,
		Operation: StudyOperationSync,
		Stage:     StudyAuthorizationStageCapability,
	})
	assertScopedStudyAuthorizationErrorCode(t, err, model.StudyErrorCodeAuthorizationDenied)
}

func TestScopedStudyAuthorizerDeniesUnauthenticatedPrincipal(t *testing.T) {
	t.Parallel()
	authorizer, _ := newScopedStudyAuthorizerForTest(
		t,
		[]StudyOperation{StudyOperationSync},
		[]string{scopedStudyAuthorizerPrimaryProfileRef},
	)

	err := authorizer.AuthorizeStudy(context.Background(), StudyAuthorizationRequest{
		Operation: StudyOperationSync,
		Stage:     StudyAuthorizationStageCapability,
	})
	assertScopedStudyAuthorizationErrorCode(t, err, model.StudyErrorCodeUnauthorized)
}

func TestScopedStudyAuthorizerDeniesUnconfiguredOperationAtCapability(t *testing.T) {
	t.Parallel()
	authorizer, principal := newScopedStudyAuthorizerForTest(
		t,
		[]StudyOperation{StudyOperationSync},
		[]string{scopedStudyAuthorizerPrimaryProfileRef},
	)

	err := authorizer.AuthorizeStudy(context.Background(), StudyAuthorizationRequest{
		Principal: principal,
		Operation: StudyOperationCreateOrResume,
		Stage:     StudyAuthorizationStageCapability,
	})
	assertScopedStudyAuthorizationErrorCode(t, err, model.StudyErrorCodeAuthorizationDenied)
}

func TestScopedStudyAuthorizerDeniesGetWhenOperationIsAbsent(t *testing.T) {
	t.Parallel()
	authorizer, principal := newScopedStudyAuthorizerForTest(
		t,
		[]StudyOperation{StudyOperationCreateOrResume, StudyOperationSync},
		[]string{scopedStudyAuthorizerPrimaryProfileRef},
	)

	err := authorizer.AuthorizeStudy(context.Background(), StudyAuthorizationRequest{
		Principal: principal,
		Operation: StudyOperationGet,
		Stage:     StudyAuthorizationStageCapability,
	})
	assertScopedStudyAuthorizationErrorCode(t, err, model.StudyErrorCodeAuthorizationDenied)
}

func TestScopedStudyAuthorizerDeniesWrongProfileAtResolvedTarget(t *testing.T) {
	t.Parallel()
	authorizer, principal := newScopedStudyAuthorizerForTest(
		t,
		[]StudyOperation{StudyOperationSync},
		[]string{scopedStudyAuthorizerPrimaryProfileRef},
	)

	err := authorizer.AuthorizeStudy(context.Background(), StudyAuthorizationRequest{
		Principal:  principal,
		Operation:  StudyOperationSync,
		Stage:      StudyAuthorizationStageResolvedTarget,
		ProfileRef: scopedStudyAuthorizerSecondaryProfileRef,
	})
	assertScopedStudyAuthorizationErrorCode(t, err, model.StudyErrorCodeAuthorizationDenied)
}

func TestScopedStudyAuthorizerRequiresExactProfileRefMembership(t *testing.T) {
	t.Parallel()
	authorizer, principal := newScopedStudyAuthorizerForTest(
		t,
		[]StudyOperation{StudyOperationSync},
		[]string{scopedStudyAuthorizerPrimaryProfileRef},
	)

	err := authorizer.AuthorizeStudy(context.Background(), StudyAuthorizationRequest{
		Principal:  principal,
		Operation:  StudyOperationSync,
		Stage:      StudyAuthorizationStageResolvedTarget,
		ProfileRef: "learner.Primary",
	})
	assertScopedStudyAuthorizationErrorCode(t, err, model.StudyErrorCodeAuthorizationDenied)
}

func TestScopedStudyAuthorizerDeniesInvalidOperationAndStage(t *testing.T) {
	t.Parallel()
	authorizer, principal := newScopedStudyAuthorizerForTest(
		t,
		[]StudyOperation{StudyOperationSync},
		[]string{scopedStudyAuthorizerPrimaryProfileRef},
	)

	for _, request := range []StudyAuthorizationRequest{
		{
			Principal: principal,
			Operation: StudyOperation("sync_all"),
			Stage:     StudyAuthorizationStageCapability,
		},
		{
			Principal: principal,
			Operation: StudyOperationSync,
			Stage:     StudyAuthorizationStage("trusted"),
		},
	} {
		err := authorizer.AuthorizeStudy(context.Background(), request)
		assertScopedStudyAuthorizationErrorCode(t, err, model.StudyErrorCodeAuthorizationDenied)
	}
}

func TestNewScopedStudyAuthorizerRejectsInvalidDuplicateEmptyAndOverboundConfiguration(t *testing.T) {
	t.Parallel()
	principal := newScopedStudyPrincipalForTest(t)
	validGrant := ScopedStudyAuthorizationGrant{
		Principal:                 principal,
		AllowedOperations:         []StudyOperation{StudyOperationSync},
		AllowedLearnerProfileRefs: []string{scopedStudyAuthorizerPrimaryProfileRef},
	}

	tooManyOperations := make([]StudyOperation, StudyMaximumScopedAuthorizationOperationsPerGrant+1)
	for index := range tooManyOperations {
		tooManyOperations[index] = StudyOperationSync
	}
	tooManyProfiles := make([]string, StudyMaximumScopedAuthorizationProfileRefsPerGrant+1)
	for index := range tooManyProfiles {
		tooManyProfiles[index] = fmt.Sprintf("learner.profile-%d", index)
	}
	tooManyGrants := make([]ScopedStudyAuthorizationGrant, StudyMaximumScopedAuthorizationGrants+1)
	for index := range tooManyGrants {
		tooManyGrants[index] = ScopedStudyAuthorizationGrant{
			Principal:                 newScopedStudyPrincipalForTest(t),
			AllowedOperations:         []StudyOperation{StudyOperationSync},
			AllowedLearnerProfileRefs: []string{scopedStudyAuthorizerPrimaryProfileRef},
		}
	}

	testCases := []struct {
		name   string
		grants []ScopedStudyAuthorizationGrant
	}{
		{name: "empty grant set"},
		{
			name: "invalid principal",
			grants: []ScopedStudyAuthorizationGrant{{
				AllowedOperations:         []StudyOperation{StudyOperationSync},
				AllowedLearnerProfileRefs: []string{scopedStudyAuthorizerPrimaryProfileRef},
			}},
		},
		{
			name: "operator configured as scoped principal",
			grants: []ScopedStudyAuthorizationGrant{{
				Principal:                 model.OperatorStudyPrincipalID(),
				AllowedOperations:         []StudyOperation{StudyOperationSync},
				AllowedLearnerProfileRefs: []string{scopedStudyAuthorizerPrimaryProfileRef},
			}},
		},
		{
			name: "duplicate principal",
			grants: []ScopedStudyAuthorizationGrant{
				cloneScopedStudyAuthorizationGrantForTest(validGrant),
				cloneScopedStudyAuthorizationGrantForTest(validGrant),
			},
		},
		{
			name: "empty operations",
			grants: []ScopedStudyAuthorizationGrant{{
				Principal:                 principal,
				AllowedLearnerProfileRefs: []string{scopedStudyAuthorizerPrimaryProfileRef},
			}},
		},
		{
			name: "invalid operation",
			grants: []ScopedStudyAuthorizationGrant{{
				Principal:                 principal,
				AllowedOperations:         []StudyOperation{StudyOperation("read_all")},
				AllowedLearnerProfileRefs: []string{scopedStudyAuthorizerPrimaryProfileRef},
			}},
		},
		{
			name: "duplicate operation",
			grants: []ScopedStudyAuthorizationGrant{{
				Principal:                 principal,
				AllowedOperations:         []StudyOperation{StudyOperationSync, StudyOperationSync},
				AllowedLearnerProfileRefs: []string{scopedStudyAuthorizerPrimaryProfileRef},
			}},
		},
		{
			name: "overbound operations",
			grants: []ScopedStudyAuthorizationGrant{{
				Principal:                 principal,
				AllowedOperations:         tooManyOperations,
				AllowedLearnerProfileRefs: []string{scopedStudyAuthorizerPrimaryProfileRef},
			}},
		},
		{
			name: "empty profile refs",
			grants: []ScopedStudyAuthorizationGrant{{
				Principal:         principal,
				AllowedOperations: []StudyOperation{StudyOperationSync},
			}},
		},
		{
			name: "invalid profile ref",
			grants: []ScopedStudyAuthorizationGrant{{
				Principal:                 principal,
				AllowedOperations:         []StudyOperation{StudyOperationSync},
				AllowedLearnerProfileRefs: []string{"learner profile"},
			}},
		},
		{
			name: "duplicate profile ref",
			grants: []ScopedStudyAuthorizationGrant{{
				Principal:         principal,
				AllowedOperations: []StudyOperation{StudyOperationSync},
				AllowedLearnerProfileRefs: []string{
					scopedStudyAuthorizerPrimaryProfileRef,
					scopedStudyAuthorizerPrimaryProfileRef,
				},
			}},
		},
		{
			name: "overbound profile refs",
			grants: []ScopedStudyAuthorizationGrant{{
				Principal:                 principal,
				AllowedOperations:         []StudyOperation{StudyOperationSync},
				AllowedLearnerProfileRefs: tooManyProfiles,
			}},
		},
		{name: "overbound grants", grants: tooManyGrants},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			authorizer, err := NewScopedStudyAuthorizer(testCase.grants)
			if authorizer != nil {
				t.Fatal("invalid configuration returned an authorizer")
			}
			assertScopedStudyAuthorizationErrorCode(t, err, model.StudyErrorCodeSchemaInvalid)
		})
	}
}

func TestScopedStudyAuthorizerDetachesGrantInputs(t *testing.T) {
	t.Parallel()
	principal := newScopedStudyPrincipalForTest(t)
	otherPrincipal := newScopedStudyPrincipalForTest(t)
	operations := []StudyOperation{StudyOperationCreateOrResume, StudyOperationSync}
	profileRefs := []string{scopedStudyAuthorizerPrimaryProfileRef}
	grants := []ScopedStudyAuthorizationGrant{{
		Principal:                 principal,
		AllowedOperations:         operations,
		AllowedLearnerProfileRefs: profileRefs,
	}}
	authorizer, err := NewScopedStudyAuthorizer(grants)
	if err != nil {
		t.Fatal(err)
	}

	operations[0] = StudyOperationGet
	profileRefs[0] = scopedStudyAuthorizerSecondaryProfileRef
	grants[0] = ScopedStudyAuthorizationGrant{
		Principal:                 otherPrincipal,
		AllowedOperations:         []StudyOperation{StudyOperationGet},
		AllowedLearnerProfileRefs: []string{scopedStudyAuthorizerSecondaryProfileRef},
	}

	if err := authorizer.AuthorizeStudy(context.Background(), StudyAuthorizationRequest{
		Principal:  principal,
		Operation:  StudyOperationCreateOrResume,
		Stage:      StudyAuthorizationStageResolvedTarget,
		ProfileRef: scopedStudyAuthorizerPrimaryProfileRef,
	}); err != nil {
		t.Fatalf("detached original grant denied: %v", err)
	}
	for _, request := range []StudyAuthorizationRequest{
		{
			Principal: principal,
			Operation: StudyOperationGet,
			Stage:     StudyAuthorizationStageCapability,
		},
		{
			Principal:  principal,
			Operation:  StudyOperationSync,
			Stage:      StudyAuthorizationStageResolvedTarget,
			ProfileRef: scopedStudyAuthorizerSecondaryProfileRef,
		},
		{
			Principal: otherPrincipal,
			Operation: StudyOperationGet,
			Stage:     StudyAuthorizationStageCapability,
		},
	} {
		err := authorizer.AuthorizeStudy(context.Background(), request)
		assertScopedStudyAuthorizationErrorCode(t, err, model.StudyErrorCodeAuthorizationDenied)
	}
}

func TestScopedStudyAuthorizerCrossProfileSessionDenialStopsAfterNarrowLookup(t *testing.T) {
	t.Parallel()
	authorizer, principal := newScopedStudyAuthorizerForTest(
		t,
		[]StudyOperation{StudyOperationSync},
		[]string{scopedStudyAuthorizerPrimaryProfileRef},
	)
	repository := &recordingStudySessionRepository{
		profile: &model.StudyLearnerProfile{
			ID:  uuid.New(),
			Ref: scopedStudyAuthorizerSecondaryProfileRef,
		},
	}
	service := newStudySessionServiceForTest(t, repository, authorizer, newRecordingStudyAuthoritySource())

	_, err := service.Sync(context.Background(), principal, uuid.New(), validEmptySyncRequestForAuthorizationTest())
	assertScopedStudyAuthorizationErrorCode(t, err, model.StudyErrorCodeAuthorizationDenied)
	if repository.profileCalls != 1 || repository.resolveCalls != 0 || repository.materializeCalls != 0 ||
		repository.committedCalls != 0 || repository.cycleCalls != 0 || repository.adjudicateCalls != 0 {
		t.Fatalf(
			"repository calls resolve=%d profile=%d materialize=%d committed=%d cycle=%d adjudicate=%d",
			repository.resolveCalls,
			repository.profileCalls,
			repository.materializeCalls,
			repository.committedCalls,
			repository.cycleCalls,
			repository.adjudicateCalls,
		)
	}
}

func TestNewScopedStudyAuthorizerErrorsAreStructuredAndRedactSensitiveInput(t *testing.T) {
	t.Parallel()
	const testOnlySensitiveCanary = "TEST-ONLY-BEARER-SECRET-CANARY WITH SPACES"
	_, err := NewScopedStudyAuthorizer([]ScopedStudyAuthorizationGrant{{
		Principal:                 newScopedStudyPrincipalForTest(t),
		AllowedOperations:         []StudyOperation{StudyOperationSync},
		AllowedLearnerProfileRefs: []string{testOnlySensitiveCanary},
	}})
	assertScopedStudyAuthorizationErrorCode(t, err, model.StudyErrorCodeSchemaInvalid)

	var studyError *model.StudyError
	if !errors.As(err, &studyError) || studyError == nil {
		t.Fatalf("error type = %T", err)
	}
	wantContext := map[string]string{
		"operation": studyScopedAuthorizationConfigurationOperation,
		"field":     studyScopedAuthorizationConfigurationField,
	}
	if !reflect.DeepEqual(studyError.Context, wantContext) {
		t.Fatalf("error context = %#v", studyError.Context)
	}
	encoded, marshalErr := json.Marshal(studyError)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	lowerEncoded := strings.ToLower(string(encoded))
	for categoryIndex, forbidden := range []string{
		strings.ToLower(testOnlySensitiveCanary),
		"bearer",
		"secret",
		"token",
		"credential",
	} {
		if strings.Contains(lowerEncoded, forbidden) {
			t.Fatalf("structured error contains forbidden context category %d", categoryIndex)
		}
	}
}

func TestNilScopedStudyAuthorizerFailsClosed(t *testing.T) {
	t.Parallel()
	var authorizer *ScopedStudyAuthorizer
	err := authorizer.AuthorizeStudy(context.Background(), StudyAuthorizationRequest{
		Principal: model.OperatorStudyPrincipalID(),
		Operation: StudyOperationSync,
		Stage:     StudyAuthorizationStageCapability,
	})
	assertScopedStudyAuthorizationErrorCode(t, err, model.StudyErrorCodeInternal)
}

func newScopedStudyAuthorizerForTest(
	t *testing.T,
	operations []StudyOperation,
	profileRefs []string,
) (*ScopedStudyAuthorizer, model.StudyPrincipalID) {
	t.Helper()
	principal := newScopedStudyPrincipalForTest(t)
	authorizer, err := NewScopedStudyAuthorizer([]ScopedStudyAuthorizationGrant{{
		Principal:                 principal,
		AllowedOperations:         append([]StudyOperation(nil), operations...),
		AllowedLearnerProfileRefs: append([]string(nil), profileRefs...),
	}})
	if err != nil {
		t.Fatalf("new scoped study authorizer: %v", err)
	}
	return authorizer, principal
}

func newScopedStudyPrincipalForTest(t *testing.T) model.StudyPrincipalID {
	t.Helper()
	principal, err := model.NewStudyPrincipalID(model.StudyPrincipalNamespaceFixture, uuid.New())
	if err != nil {
		t.Fatalf("new scoped study principal: %v", err)
	}
	return principal
}

func cloneScopedStudyAuthorizationGrantForTest(grant ScopedStudyAuthorizationGrant) ScopedStudyAuthorizationGrant {
	return ScopedStudyAuthorizationGrant{
		Principal:                 grant.Principal,
		AllowedOperations:         append([]StudyOperation(nil), grant.AllowedOperations...),
		AllowedLearnerProfileRefs: append([]string(nil), grant.AllowedLearnerProfileRefs...),
	}
}

func assertScopedStudyAuthorizationErrorCode(t *testing.T, err error, code model.StudyErrorCode) {
	t.Helper()
	if !errors.Is(err, &model.StudyError{Code: code}) {
		t.Fatalf("error = %v, want code %s", err, code)
	}
}
