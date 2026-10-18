package studysession

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"lexinet/internal/model"
)

func TestStudyOperationValidAcceptsOnlyFrozenVocabulary(t *testing.T) {
	t.Parallel()
	for _, operation := range []StudyOperation{
		StudyOperationCreateOrResume,
		StudyOperationGet,
		StudyOperationSync,
	} {
		if !operation.Valid() {
			t.Fatalf("valid operation rejected: %q", operation)
		}
	}
	for _, operation := range []StudyOperation{"", "create", "read", "SYNC", " sync"} {
		if operation.Valid() {
			t.Fatalf("invalid operation accepted: %q", operation)
		}
	}
}

func TestOperatorStudyAuthorizerAllowsExactOperatorForEveryOperation(t *testing.T) {
	t.Parallel()
	authorizer := OperatorStudyAuthorizer{}
	for _, operation := range []StudyOperation{StudyOperationCreateOrResume, StudyOperationGet, StudyOperationSync} {
		if err := authorizer.AuthorizeStudy(context.Background(), StudyAuthorizationRequest{
			Principal: model.OperatorStudyPrincipalID(),
			Operation: operation,
			Stage:     StudyAuthorizationStageCapability,
		}); err != nil {
			t.Fatalf("authorize %s: %v", operation, err)
		}
	}
}

func TestOperatorStudyAuthorizerDeniesOtherValidPrincipal(t *testing.T) {
	t.Parallel()
	principal, err := model.NewStudyPrincipalID(model.StudyPrincipalNamespaceFixture, uuid.MustParse("11111111-1111-4111-8111-111111111111"))
	if err != nil {
		t.Fatal(err)
	}
	err = (OperatorStudyAuthorizer{}).AuthorizeStudy(context.Background(), StudyAuthorizationRequest{
		Principal: principal,
		Operation: StudyOperationGet,
		Stage:     StudyAuthorizationStageCapability,
	})
	if !errors.Is(err, &model.StudyError{Code: model.StudyErrorCodeAuthorizationDenied}) {
		t.Fatalf("denial error = %v", err)
	}
}
