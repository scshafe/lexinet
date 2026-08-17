package studysession

import (
	"bytes"
	"context"
	"testing"
)

func TestSystemStudyAuthoritySourceAllocatesRequestedDistinctIdentities(t *testing.T) {
	t.Parallel()
	randomBytes := append(bytes.Repeat([]byte{0x42}, 16), bytes.Repeat([]byte{0x43}, 16)...)
	source := &SystemStudyAuthoritySource{random: bytes.NewReader(randomBytes)}
	identities, err := source.AllocateStudyItemIDs(context.Background(), 2)
	if err != nil {
		t.Fatalf("allocate identities: %v", err)
	}
	if len(identities) != 2 || identities[0] == identities[1] {
		t.Fatalf("allocated identities = %v", identities)
	}
}

func TestSystemStudyAuthoritySourceBoundsCollisionRetries(t *testing.T) {
	t.Parallel()
	source := &SystemStudyAuthoritySource{
		random: bytes.NewReader(bytes.Repeat([]byte{0x42}, 16*(studyMaximumUUIDCollisionRetries+1))),
	}
	if _, err := source.AllocateStudyItemIDs(context.Background(), 2); err == nil {
		t.Fatal("colliding entropy produced duplicate identities or retried forever")
	}
}

func TestSystemStudyAuthoritySourceRejectsUnavailableRandomness(t *testing.T) {
	t.Parallel()
	source := &SystemStudyAuthoritySource{random: bytes.NewReader(nil)}
	if _, err := source.NewStudySeed(); err == nil {
		t.Fatal("empty random source produced a seed")
	}
}
