package studysession

import (
	"testing"

	"github.com/google/uuid"

	"lexinet/internal/model"
)

func TestStudyAdmissionControllerEnforcesPerPrincipalWithoutQueueing(t *testing.T) {
	t.Parallel()
	controller := newStudyAdmissionController()
	principal := fixturePrincipal(t, 1)
	firstRelease, first := controller.acquire(principal)
	secondRelease, second := controller.acquire(principal)
	_, third := controller.acquire(principal)
	if !first || !second || third {
		t.Fatalf("admission outcomes = %v %v %v", first, second, third)
	}
	firstRelease()
	firstRelease()
	thirdRelease, thirdAfterRelease := controller.acquire(principal)
	if !thirdAfterRelease {
		t.Fatal("released admission was not reusable")
	}
	secondRelease()
	thirdRelease()
}

func TestStudyAdmissionControllerEnforcesGlobalBound(t *testing.T) {
	t.Parallel()
	controller := newStudyAdmissionController()
	releases := make([]func(), 0, StudyGlobalAdmissionLimit)
	for index := 0; index < StudyGlobalAdmissionLimit; index++ {
		release, admitted := controller.acquire(fixturePrincipal(t, index+1))
		if !admitted {
			t.Fatalf("global slot %d was rejected", index)
		}
		releases = append(releases, release)
	}
	if _, admitted := controller.acquire(fixturePrincipal(t, 99)); admitted {
		t.Fatal("global overflow was admitted")
	}
	for _, release := range releases {
		release()
	}
}

func fixturePrincipal(t *testing.T, value int) model.StudyPrincipalID {
	t.Helper()
	identity := uuid.NewSHA1(uuid.NameSpaceOID, []byte{byte(value)})
	principal, err := model.NewStudyPrincipalID(model.StudyPrincipalNamespaceFixture, identity)
	if err != nil {
		t.Fatalf("construct principal: %v", err)
	}
	return principal
}
