package studysession

import (
	"bytes"
	"encoding/json"
	"sort"

	"github.com/google/uuid"

	"lexinet/internal/model"
	"lexinet/internal/study"
)

// BuildStudySessionPage returns the longest deterministic issued-item prefix
// within both the requested count and the typed 983,040-byte page budget.
func BuildStudySessionPage(
	items []model.StudySessionItem,
	request StudyPageRequest,
) (StudySessionPageResource, model.StudySessionPageArtifact, error) {
	if err := validateStudyPageRequest(request); err != nil {
		return StudySessionPageResource{}, model.StudySessionPageArtifact{}, err
	}
	validatedItems, err := validateAndCloneOrderedSessionItems(items)
	if err != nil {
		return StudySessionPageResource{}, model.StudySessionPageArtifact{}, err
	}

	firstIndex := sort.Search(len(validatedItems), func(index int) bool {
		return validatedItems[index].Position > request.AfterPosition
	})
	page := StudySessionPageResource{
		AfterPosition:     request.AfterPosition,
		NextAfterPosition: request.AfterPosition,
		Complete:          firstIndex == len(validatedItems),
		Items:             make([]StudyCardProjectionResource, 0, request.Limit),
	}
	for index := firstIndex; index < len(validatedItems) && len(page.Items) < request.Limit; index++ {
		page.Items = append(page.Items, studySessionItemResource(validatedItems[index]))
		page.NextAfterPosition = validatedItems[index].Position
		page.Complete = index == len(validatedItems)-1
		candidateJSON, candidateErr := encodeStudySessionPageResource(page)
		if candidateErr != nil {
			return StudySessionPageResource{}, model.StudySessionPageArtifact{}, candidateErr
		}
		if len(candidateJSON) > StudyMaximumPageJSONBytes {
			page.Items = page.Items[:len(page.Items)-1]
			if len(page.Items) == 0 {
				return StudySessionPageResource{}, model.StudySessionPageArtifact{}, newStudySessionError(model.StudyErrorCodeInternal)
			}
			page.NextAfterPosition = page.Items[len(page.Items)-1].Position
			page.Complete = false
			break
		}
	}
	if len(page.Items) == request.Limit && page.NextAfterPosition < validatedItems[len(validatedItems)-1].Position {
		page.Complete = false
	}

	encodedJSON, err := encodeStudySessionPageResource(page)
	if err != nil {
		return StudySessionPageResource{}, model.StudySessionPageArtifact{}, err
	}
	if len(encodedJSON) > StudyMaximumPageJSONBytes {
		return StudySessionPageResource{}, model.StudySessionPageArtifact{}, newStudySessionError(model.StudyErrorCodeInternal)
	}
	artifact := model.StudySessionPageArtifact{
		Request: model.StudySessionPageRequest{
			AfterPosition: request.AfterPosition,
			Limit:         request.Limit,
		},
		CanonicalJSON:   append(json.RawMessage(nil), encodedJSON...),
		CanonicalSHA256: model.SHA256Hex(encodedJSON),
	}
	if err := model.ValidateStudySessionPageArtifact(artifact); err != nil {
		return StudySessionPageResource{}, model.StudySessionPageArtifact{}, newStudySessionError(model.StudyErrorCodeInternal)
	}
	return page, artifact, nil
}

func decodeStudySessionPageArtifact(
	artifact model.StudySessionPageArtifact,
	request StudyPageRequest,
	items []model.StudySessionItem,
) (StudySessionPageResource, error) {
	if artifact.Request.AfterPosition != request.AfterPosition || artifact.Request.Limit != request.Limit ||
		len(artifact.CanonicalJSON) == 0 || len(artifact.CanonicalJSON) > StudyMaximumPageJSONBytes ||
		!model.IsStudySHA256(artifact.CanonicalSHA256) || model.SHA256Hex(artifact.CanonicalJSON) != artifact.CanonicalSHA256 {
		return StudySessionPageResource{}, newStudySessionError(model.StudyErrorCodeInternal)
	}
	pageDocument, err := model.DecodeExactStudySessionPageDocument(artifact.CanonicalJSON)
	if err != nil {
		return StudySessionPageResource{}, newStudySessionError(model.StudyErrorCodeInternal)
	}
	page := studySessionPageResourceFromDocument(pageDocument)
	if err := validatePersistedStudyPage(page, request, items); err != nil {
		return StudySessionPageResource{}, err
	}
	return page, nil
}

func validatePersistedStudyPage(
	page StudySessionPageResource,
	request StudyPageRequest,
	items []model.StudySessionItem,
) error {
	validatedItems, err := validateAndCloneOrderedSessionItems(items)
	if err != nil {
		return err
	}
	if page.AfterPosition != request.AfterPosition || page.NextAfterPosition < page.AfterPosition ||
		len(page.Items) > request.Limit || len(page.Items) > StudyMaximumPageItems {
		return newStudySessionError(model.StudyErrorCodeInternal)
	}
	firstIndex := sort.Search(len(validatedItems), func(index int) bool {
		return validatedItems[index].Position > request.AfterPosition
	})
	if len(page.Items) == 0 {
		if !page.Complete || firstIndex != len(validatedItems) || page.NextAfterPosition != request.AfterPosition {
			return newStudySessionError(model.StudyErrorCodeInternal)
		}
		return nil
	}
	if firstIndex+len(page.Items) > len(validatedItems) {
		return newStudySessionError(model.StudyErrorCodeInternal)
	}
	for offset := range page.Items {
		if !sameImmutableStudyCardProjection(page.Items[offset], validatedItems[firstIndex+offset]) {
			return newStudySessionError(model.StudyErrorCodeInternal)
		}
	}
	expectedNext := page.Items[len(page.Items)-1].Position
	expectedComplete := firstIndex+len(page.Items) == len(validatedItems)
	if page.NextAfterPosition != expectedNext || page.Complete != expectedComplete {
		return newStudySessionError(model.StudyErrorCodeInternal)
	}
	return nil
}

func validateAndCloneOrderedSessionItems(items []model.StudySessionItem) ([]model.StudySessionItem, error) {
	if len(items) > study.StudyMaximumSchedulingCards {
		return nil, newStudySessionError(model.StudyErrorCodeInternal)
	}
	clone := make([]model.StudySessionItem, len(items))
	seenIDs := make(map[uuid.UUID]struct{}, len(items))
	for index, item := range items {
		if err := model.ValidateStudySessionItem(item); err != nil || item.Position != index {
			return nil, newStudySessionError(model.StudyErrorCodeInternal)
		}
		if _, duplicate := seenIDs[item.ID]; duplicate {
			return nil, newStudySessionError(model.StudyErrorCodeInternal)
		}
		seenIDs[item.ID] = struct{}{}
		clone[index] = cloneStudySessionItem(item)
	}
	return clone, nil
}

func studySessionItemResource(item model.StudySessionItem) StudyCardProjectionResource {
	return StudyCardProjectionResource{
		SessionItemID:    item.ID,
		Position:         item.Position,
		CardKind:         item.CardKind,
		CardIdentity:     item.CardIdentity,
		LiveSourceIdeaID: cloneUUIDPointer(item.LiveSourceIdeaID),
		SourceRevision: StudySourceRevisionResource{
			SourceKind:      item.HistoricalSourceKind,
			SourceIdentity:  item.HistoricalSourceIdentity,
			ContentRevision: item.SourceContentRevision,
			ContentSHA256:   item.SourceContentSHA256,
		},
		Front: StudyCardFrontResource{CanonicalName: item.FrontCanonicalName},
		Back: StudyCardBackResource{
			Definition: item.BackDefinition,
			Example:    cloneStringPointer(item.BackExample),
		},
		SelectionReason: StudySelectionReasonResource{
			RatingRank:                      item.SelectionReason.RatingRank,
			LatestPresentedIngestionOrdinal: cloneInt64Pointer(item.SelectionReason.LatestPresentedIngestionOrdinal),
			TieBreakerSHA256:                item.SelectionReason.TieBreakerSHA256,
			BoundaryAdjustment:              item.SelectionReason.BoundaryAdjustment,
		},
	}
}

func sameImmutableStudyCardProjection(resource StudyCardProjectionResource, item model.StudySessionItem) bool {
	expected := studySessionItemResource(item)
	// LiveSourceIdeaID is intentionally mutable. A stored sync page preserves
	// the historical value even after a later merge or hard deletion.
	expected.LiveSourceIdeaID = resource.LiveSourceIdeaID
	left, leftErr := json.Marshal(resource)
	right, rightErr := json.Marshal(expected)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}

func encodeStudySessionPageResource(page StudySessionPageResource) ([]byte, error) {
	encoded, err := model.EncodeStudySessionPageDocument(studySessionPageDocument(page))
	if err != nil {
		return nil, newStudySessionError(model.StudyErrorCodeInternal)
	}
	return encoded, nil
}

func marshalBoundedStudyResponse(value any) (json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, newStudySessionError(model.StudyErrorCodeInternal)
	}
	if len(encoded) > StudyMaximumResponseBodyBytes {
		return nil, newStudySessionError(model.StudyErrorCodeInternal)
	}
	return append(json.RawMessage(nil), encoded...), nil
}

func studySessionPageDocument(page StudySessionPageResource) model.StudySessionPageDocument {
	document := model.StudySessionPageDocument{
		AfterPosition:     page.AfterPosition,
		NextAfterPosition: page.NextAfterPosition,
		Complete:          page.Complete,
		Items:             make([]model.StudySessionPageCardProjection, len(page.Items)),
	}
	for index, item := range page.Items {
		document.Items[index] = model.StudySessionPageCardProjection{
			SessionItemID:    item.SessionItemID,
			Position:         item.Position,
			CardKind:         item.CardKind,
			CardIdentity:     item.CardIdentity,
			LiveSourceIdeaID: cloneUUIDPointer(item.LiveSourceIdeaID),
			SourceRevision: model.StudySessionPageSourceRevision{
				SourceKind:      item.SourceRevision.SourceKind,
				SourceIdentity:  item.SourceRevision.SourceIdentity,
				ContentRevision: item.SourceRevision.ContentRevision,
				ContentSHA256:   item.SourceRevision.ContentSHA256,
			},
			Front: model.StudySessionPageCardFront{CanonicalName: item.Front.CanonicalName},
			Back: model.StudySessionPageCardBack{
				Definition: item.Back.Definition,
				Example:    cloneStringPointer(item.Back.Example),
			},
			SelectionReason: model.StudySessionPageSelectionReason{
				RatingRank:                      item.SelectionReason.RatingRank,
				LatestPresentedIngestionOrdinal: cloneInt64Pointer(item.SelectionReason.LatestPresentedIngestionOrdinal),
				TieBreakerSHA256:                item.SelectionReason.TieBreakerSHA256,
				BoundaryAdjustment:              item.SelectionReason.BoundaryAdjustment,
			},
		}
	}
	return document
}

func studySessionPageResourceFromDocument(page model.StudySessionPageDocument) StudySessionPageResource {
	resource := StudySessionPageResource{
		AfterPosition:     page.AfterPosition,
		NextAfterPosition: page.NextAfterPosition,
		Complete:          page.Complete,
		Items:             make([]StudyCardProjectionResource, len(page.Items)),
	}
	for index, item := range page.Items {
		resource.Items[index] = StudyCardProjectionResource{
			SessionItemID:    item.SessionItemID,
			Position:         item.Position,
			CardKind:         item.CardKind,
			CardIdentity:     item.CardIdentity,
			LiveSourceIdeaID: cloneUUIDPointer(item.LiveSourceIdeaID),
			SourceRevision: StudySourceRevisionResource{
				SourceKind:      item.SourceRevision.SourceKind,
				SourceIdentity:  item.SourceRevision.SourceIdentity,
				ContentRevision: item.SourceRevision.ContentRevision,
				ContentSHA256:   item.SourceRevision.ContentSHA256,
			},
			Front: StudyCardFrontResource{CanonicalName: item.Front.CanonicalName},
			Back: StudyCardBackResource{
				Definition: item.Back.Definition,
				Example:    cloneStringPointer(item.Back.Example),
			},
			SelectionReason: StudySelectionReasonResource{
				RatingRank:                      item.SelectionReason.RatingRank,
				LatestPresentedIngestionOrdinal: cloneInt64Pointer(item.SelectionReason.LatestPresentedIngestionOrdinal),
				TieBreakerSHA256:                item.SelectionReason.TieBreakerSHA256,
				BoundaryAdjustment:              item.SelectionReason.BoundaryAdjustment,
			},
		}
	}
	return resource
}

func cloneStudySessionItem(item model.StudySessionItem) model.StudySessionItem {
	clone := item
	clone.LiveSourceIdeaID = cloneUUIDPointer(item.LiveSourceIdeaID)
	clone.BackExample = cloneStringPointer(item.BackExample)
	clone.SelectionReason.LatestPresentedIngestionOrdinal = cloneInt64Pointer(item.SelectionReason.LatestPresentedIngestionOrdinal)
	return clone
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
