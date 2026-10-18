package studypostgres

import (
	"reflect"
	"testing"
)

func TestOwnedContainerRunArgumentsUseBoundedPostgresDataTmpfs(t *testing.T) {
	const containerName = "lexinet-study-test-command-shape"
	want := []string{
		"run",
		"--detach",
		"--name", containerName,
		"--env", "POSTGRES_HOST_AUTH_METHOD=trust",
		"--publish", "127.0.0.1::5432",
		"--mount", "type=tmpfs,destination=/var/lib/postgresql/data,tmpfs-size=536870912,tmpfs-mode=0700",
		"postgres:16-alpine",
	}

	if got := ownedContainerRunArguments(containerName); !reflect.DeepEqual(got, want) {
		t.Fatalf("STUDY_TEST_POSTGRES.RUN_ARGUMENTS_UNEXPECTED: got=%q want=%q", got, want)
	}
}

func TestOwnedContainerCleanupArgumentsRemoveAttachedVolumes(t *testing.T) {
	const containerName = "lexinet-study-test-command-shape"
	want := []string{"rm", "--force", "--volumes", containerName}

	if got := ownedContainerCleanupArguments(containerName); !reflect.DeepEqual(got, want) {
		t.Fatalf("STUDY_TEST_POSTGRES.CLEANUP_ARGUMENTS_UNEXPECTED: got=%q want=%q", got, want)
	}
}
