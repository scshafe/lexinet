package studysession

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"time"

	"github.com/google/uuid"

	"lexinet/internal/model"
	"lexinet/internal/study"
)

const studySeedRandomByteCount = 32
const studyMaximumUUIDCollisionRetries = 16

// StudyAuthoritySource is the sole service authority for wall time,
// cryptographic scheduling seeds, session IDs, and lazy session-item IDs.
// AllocateStudyItemIDs is intentionally the materializer's bounded allocator.
type StudyAuthoritySource interface {
	study.StudyItemIDAllocator
	Now() time.Time
	NewStudySessionID() (uuid.UUID, error)
	NewStudySeed() (string, error)
}

// SystemStudyAuthoritySource binds UTC wall time and crypto/rand. It contains
// no request state and is safe for concurrent use.
type SystemStudyAuthoritySource struct {
	random io.Reader
}

// NewSystemStudyAuthoritySource returns the production authority source.
func NewSystemStudyAuthoritySource() *SystemStudyAuthoritySource {
	return &SystemStudyAuthoritySource{random: rand.Reader}
}

// Now returns the current UTC service time.
func (*SystemStudyAuthoritySource) Now() time.Time {
	return time.Now().UTC()
}

// NewStudySessionID returns one cryptographically random UUID.
func (source *SystemStudyAuthoritySource) NewStudySessionID() (uuid.UUID, error) {
	if source == nil || source.random == nil {
		return uuid.Nil, newStudySessionError(model.StudyErrorCodeInternal)
	}
	identity, err := uuid.NewRandomFromReader(source.random)
	if err != nil {
		return uuid.Nil, newStudySessionError(model.StudyErrorCodeInternal)
	}
	return identity, nil
}

// NewStudySeed returns a bounded URL-safe encoding of 256 random bits.
func (source *SystemStudyAuthoritySource) NewStudySeed() (string, error) {
	if source == nil || source.random == nil {
		return "", newStudySessionError(model.StudyErrorCodeInternal)
	}
	randomBytes := make([]byte, studySeedRandomByteCount)
	if _, err := io.ReadFull(source.random, randomBytes); err != nil {
		return "", newStudySessionError(model.StudyErrorCodeInternal)
	}
	return base64.RawURLEncoding.EncodeToString(randomBytes), nil
}

// AllocateStudyItemIDs returns exactly requestedCount distinct random UUIDs.
// The materializer owns the count bound and independently validates output.
func (source *SystemStudyAuthoritySource) AllocateStudyItemIDs(ctx context.Context, requestedCount int) ([]uuid.UUID, error) {
	if source == nil || source.random == nil || ctx == nil || requestedCount < 0 || requestedCount > study.StudyMaximumSchedulingCards {
		return nil, newStudySessionError(model.StudyErrorCodeSchemaInvalid)
	}
	identities := make([]uuid.UUID, requestedCount)
	seen := make(map[uuid.UUID]struct{}, requestedCount)
	for index := range identities {
		allocated := false
		for attempt := 0; attempt < studyMaximumUUIDCollisionRetries; attempt++ {
			if err := ctx.Err(); err != nil {
				return nil, sanitizeStudySessionError(err)
			}
			identity, err := uuid.NewRandomFromReader(source.random)
			if err != nil {
				return nil, newStudySessionError(model.StudyErrorCodeInternal)
			}
			if _, duplicate := seen[identity]; duplicate {
				continue
			}
			seen[identity] = struct{}{}
			identities[index] = identity
			allocated = true
			break
		}
		if !allocated {
			return nil, newStudySessionError(model.StudyErrorCodeInternal)
		}
	}
	return identities, nil
}
