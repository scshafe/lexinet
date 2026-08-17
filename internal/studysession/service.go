package studysession

import (
	"bytes"
	"context"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"lexinet/internal/model"
	"lexinet/internal/study"
)

// StudySessionService owns every study.v1 composition decision. Handlers call
// one method and never coordinate repository operations themselves.
//
// @canonical study_session_service -- all create-or-resume, get, and sync
// operations must cross this service.
type StudySessionService struct {
	repository   StudySessionRepository
	authorizer   StudyAuthorizer
	authority    StudyAuthoritySource
	materializer *study.CycleMaterializer
	shadowRunner *study.StudyShadowRunner
	admission    *StudyAdmissionController
}

// NewStudySessionService constructs the canonical service with the reviewed
// scheduler, lazy item-ID allocator, and fixed four-global/two-principal
// non-queuing admission policy.
func NewStudySessionService(
	repository StudySessionRepository,
	authorizer StudyAuthorizer,
	scheduler study.StudyScheduler,
	authority StudyAuthoritySource,
	shadowRunner *study.StudyShadowRunner,
) (*StudySessionService, error) {
	if repository == nil || authorizer == nil || scheduler == nil || authority == nil {
		return nil, newStudySessionError(model.StudyErrorCodeSchemaInvalid)
	}
	materializer, err := study.NewCanonicalCycleMaterializer(scheduler, authority)
	if err != nil {
		return nil, sanitizeStudySessionError(err)
	}
	return &StudySessionService{
		repository:   repository,
		authorizer:   authorizer,
		authority:    authority,
		materializer: materializer,
		shadowRunner: shadowRunner,
		admission:    newStudyAdmissionController(),
	}, nil
}

// CreateOrResume authorizes, normalizes, materializes, independently reads
// back, pages, and returns one authoritative create-or-resume resource.
func (service *StudySessionService) CreateOrResume(
	ctx context.Context,
	principal model.StudyPrincipalID,
	request StudyCreateSessionRequest,
) (*StudyCreateSessionResult, error) {
	operationContext, release, err := service.beginOperation(ctx, StudyAuthorizationRequest{
		Principal: principal,
		Operation: StudyOperationCreateOrResume,
		Stage:     StudyAuthorizationStageCapability,
	})
	if err != nil {
		return nil, err
	}
	defer release()
	defer operationContext.cancel()
	if err := service.authorizer.AuthorizeStudy(operationContext.ctx, StudyAuthorizationRequest{
		Principal:  principal,
		Operation:  StudyOperationCreateOrResume,
		Stage:      StudyAuthorizationStageResolvedTarget,
		ProfileRef: request.ProfileRef,
	}); err != nil {
		return nil, sanitizeStudySessionError(err)
	}

	if err := validateCreateStudyRequestBeforeResolution(request); err != nil {
		return nil, err
	}
	resolvedScope, err := service.repository.ResolveStudyScope(
		operationContext.ctx,
		append([]string(nil), request.Scope.TopicRefs...),
		request.Scope.CardKind,
	)
	if err != nil {
		return nil, sanitizeStudySessionError(err)
	}
	normalizedRequestSHA256, err := normalizeCreateStudyRequest(request, resolvedScope)
	if err != nil {
		return nil, err
	}

	sessionID, err := service.authority.NewStudySessionID()
	if err != nil || sessionID == uuid.Nil {
		return nil, newStudySessionError(model.StudyErrorCodeInternal)
	}
	seed, err := service.authority.NewStudySeed()
	if err != nil || !boundedStudyServiceText(seed, 128) {
		return nil, newStudySessionError(model.StudyErrorCodeInternal)
	}
	plannedAt, err := service.authoritativeTime()
	if err != nil {
		return nil, err
	}

	var predecessorSessionID *uuid.UUID
	var predecessorFlushReceiptID uuid.UUID
	if request.Predecessor != nil {
		predecessorSessionID = cloneUUIDPointer(&request.Predecessor.SessionID)
		predecessorFlushReceiptID = request.Predecessor.FlushReceiptID
	}
	materialized, err := service.repository.MaterializeStudyCycle(
		operationContext.ctx,
		service.materializer,
		StudyCycleMaterializationCommand{
			ProfileRef: request.ProfileRef,
			Scope:      resolvedScope,
			Materialization: study.CycleMaterializationInput{
				SessionID:                     sessionID,
				Principal:                     principal,
				CreationRequestID:             request.CreationRequestID,
				NormalizedRequestSHA256:       normalizedRequestSHA256,
				PlannedAt:                     plannedAt,
				Seed:                          seed,
				PredecessorSessionID:          predecessorSessionID,
				PredecessorFlushReceiptID:     predecessorFlushReceiptID,
				LifecycleReasonCode:           studyLifecycleReasonCreated,
				PredecessorTerminalReasonCode: studyPredecessorTerminalReasonCompleted,
			},
		},
	)
	if err != nil {
		return nil, sanitizeStudySessionError(err)
	}
	if materialized == nil {
		return nil, newStudySessionError(model.StudyErrorCodeInternal)
	}

	committed, err := service.repository.ReadCommittedStudyCycle(
		operationContext.ctx,
		StudyCommittedCycleReadCommand{
			Principal:         principal,
			ProfileRef:        request.ProfileRef,
			SessionID:         materialized.Persistence.Session.ID,
			CreationRequestID: request.CreationRequestID,
		},
	)
	if err != nil {
		return nil, sanitizeStudySessionError(err)
	}
	if committed == nil {
		return nil, newStudySessionError(model.StudyErrorCodeInternal)
	}
	validatedCommitted, err := service.materializer.ValidateCommittedCycleAuthority(
		materialized.Persistence,
		*committed,
	)
	if err != nil || validatedCommitted == nil {
		return nil, newStudySessionError(model.StudyErrorCodeInternal)
	}
	if validatedCommitted.Receipt.NormalizedRequestSHA256 != normalizedRequestSHA256 ||
		validatedCommitted.Receipt.CreationRequestID != request.CreationRequestID {
		return nil, newStudySessionError(model.StudyErrorCodeInternal)
	}

	page, _, err := BuildStudySessionPage(validatedCommitted.Items, request.Page)
	if err != nil {
		return nil, err
	}
	generatedAt, err := service.authoritativeTime()
	if err != nil {
		return nil, err
	}
	resource, err := buildStudySessionResource(
		request.ProfileRef,
		validatedCommitted.Session,
		&validatedCommitted.Receipt,
		page,
		generatedAt,
	)
	if err != nil {
		return nil, err
	}
	responseJSON, err := marshalBoundedStudyResponse(resource)
	if err != nil {
		return nil, err
	}

	if materialized.Persistence.Created &&
		validatedCommitted.Session.Lifecycle == model.StudySessionLifecycleActive &&
		len(validatedCommitted.Items) > 0 &&
		materialized.SchedulingSnapshot != nil && materialized.AuthoritativePlan != nil &&
		service.shadowRunner != nil {
		// The runner receives private copies and converts every adapter outcome,
		// including panic/timeout/busy, into observation-only evidence.
		service.shadowRunner.Run(operationContext.ctx, *materialized.SchedulingSnapshot, *materialized.AuthoritativePlan)
	}

	return &StudyCreateSessionResult{
		Resource:     resource,
		ResponseJSON: responseJSON,
		Created:      materialized.Persistence.Created,
	}, nil
}

// Get authorizes and returns one deterministic bounded page from a consistent
// session/profile/item snapshot. CreationReceipt is always null.
func (service *StudySessionService) Get(
	ctx context.Context,
	principal model.StudyPrincipalID,
	sessionID uuid.UUID,
	pageRequest StudyPageRequest,
) (*StudyGetSessionResult, error) {
	operationContext, release, authorizedProfile, err := service.beginSessionOperation(ctx, principal, StudyOperationGet, sessionID)
	if err != nil {
		return nil, err
	}
	defer release()
	defer operationContext.cancel()
	if sessionID == uuid.Nil {
		return nil, newStudySessionError(model.StudyErrorCodeSchemaInvalid)
	}
	if err := validateStudyPageRequest(pageRequest); err != nil {
		return nil, err
	}
	authority, err := service.repository.ReadStudySessionCycle(operationContext.ctx, sessionID)
	if err != nil {
		return nil, sanitizeStudySessionError(err)
	}
	if err := validateStudySessionCycleAuthority(authority, sessionID); err != nil {
		return nil, err
	}
	if authority.Profile.ID != authorizedProfile.ID || authority.Profile.Ref != authorizedProfile.Ref {
		return nil, newStudySessionError(model.StudyErrorCodeStateStale)
	}
	page, _, err := BuildStudySessionPage(authority.Items, pageRequest)
	if err != nil {
		return nil, err
	}
	generatedAt, err := service.authoritativeTime()
	if err != nil {
		return nil, err
	}
	resource, err := buildStudySessionResource(
		authority.Profile.Ref,
		authority.Session,
		nil,
		page,
		generatedAt,
	)
	if err != nil {
		return nil, err
	}
	responseJSON, err := marshalBoundedStudyResponse(resource)
	if err != nil {
		return nil, err
	}
	return &StudyGetSessionResult{Resource: resource, ResponseJSON: responseJSON}, nil
}

// Sync authorizes, normalizes, binds an immutable requested page to the flush
// receipt, and returns stored artifacts beside the live session envelope.
func (service *StudySessionService) Sync(
	ctx context.Context,
	principal model.StudyPrincipalID,
	sessionID uuid.UUID,
	request StudySyncRequest,
) (*StudySyncResult, error) {
	operationContext, release, authorizedProfile, err := service.beginSessionOperation(ctx, principal, StudyOperationSync, sessionID)
	if err != nil {
		return nil, err
	}
	defer release()
	defer operationContext.cancel()

	normalizedRequestSHA256, events, err := normalizeSyncStudyRequest(sessionID, request)
	if err != nil {
		return nil, err
	}
	authority, err := service.repository.ReadStudySessionCycle(operationContext.ctx, sessionID)
	if err != nil {
		return nil, sanitizeStudySessionError(err)
	}
	if err := validateStudySessionCycleAuthority(authority, sessionID); err != nil {
		return nil, err
	}
	if authority.Profile.ID != authorizedProfile.ID || authority.Profile.Ref != authorizedProfile.Ref {
		return nil, newStudySessionError(model.StudyErrorCodeStateStale)
	}
	for _, event := range events {
		if event.ProfileRef != authority.Profile.Ref {
			return nil, newStudySessionError(model.StudyErrorCodeSchemaInvalid)
		}
	}
	_, candidatePageArtifact, err := BuildStudySessionPage(authority.Items, request.Page)
	if err != nil {
		return nil, err
	}
	adjudicated, err := service.repository.AdjudicateStudyEvents(operationContext.ctx, StudyEventSyncCommand{
		Principal:               principal,
		ProfileRef:              authority.Profile.Ref,
		SessionID:               sessionID,
		SyncRequestID:           request.SyncID,
		NormalizedRequestSHA256: normalizedRequestSHA256,
		ClientStreamID:          request.ClientStreamID,
		ExpectedStateVersion:    request.ExpectedStateVersion,
		Events:                  events,
		PageArtifact:            candidatePageArtifact,
	})
	if err != nil {
		return nil, sanitizeStudySessionError(err)
	}
	if err := validateStudyEventSyncAuthority(
		adjudicated,
		principal,
		*authority,
		sessionID,
		request,
		normalizedRequestSHA256,
		events,
		candidatePageArtifact,
	); err != nil {
		return nil, err
	}
	persistedPage, err := decodeStudySessionPageArtifact(adjudicated.PageArtifact, request.Page, authority.Items)
	if err != nil {
		return nil, err
	}
	generatedAt, err := service.authoritativeTime()
	if err != nil {
		return nil, err
	}
	sessionResource, err := buildStudySessionResource(
		authority.Profile.Ref,
		adjudicated.Session,
		nil,
		persistedPage,
		generatedAt,
	)
	if err != nil {
		return nil, err
	}
	resource := StudySyncResource{
		ProtocolVersion:            model.StudyProtocolVersion,
		SchemaVersion:              model.StudySchemaVersion,
		RequiredSemantics:          RequiredStudySemantics(),
		SyncID:                     request.SyncID,
		NormalizedRequestSHA256:    normalizedRequestSHA256,
		ClientStreamID:             request.ClientStreamID,
		AdjudicatedThroughSequence: adjudicated.FlushReceipt.AcknowledgedThroughSequence,
		ProfileIngestionHighWater:  adjudicated.FlushReceipt.ProfileIngestionHighWater,
		Receipts:                   studyEventReceiptResources(adjudicated.Receipts),
		FlushReceipt:               studyFlushReceiptResource(adjudicated.FlushReceipt),
		Session:                    sessionResource,
		GeneratedAt:                generatedAt,
	}
	responseJSON, err := marshalBoundedStudyResponse(resource)
	if err != nil {
		return nil, err
	}
	return &StudySyncResult{Resource: resource, ResponseJSON: responseJSON}, nil
}

type studyOperationContext struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func (service *StudySessionService) beginOperation(
	ctx context.Context,
	authorization StudyAuthorizationRequest,
) (studyOperationContext, func(), error) {
	if service == nil || service.repository == nil || service.authorizer == nil ||
		service.authority == nil || service.materializer == nil || service.admission == nil || ctx == nil {
		return studyOperationContext{}, nil, newStudySessionError(model.StudyErrorCodeInternal)
	}
	if !authorization.Principal.Valid() {
		return studyOperationContext{}, nil, newStudySessionError(model.StudyErrorCodeUnauthorized)
	}
	operationCtx, cancel := context.WithTimeout(ctx, StudyAuthoritativeOperationTimeout)
	if err := service.authorizer.AuthorizeStudy(operationCtx, authorization); err != nil {
		cancel()
		return studyOperationContext{}, nil, sanitizeStudySessionError(err)
	}
	release, admitted := service.admission.acquire(authorization.Principal)
	if !admitted {
		cancel()
		return studyOperationContext{}, nil, newStudySessionError(model.StudyErrorCodeRateLimited)
	}
	return studyOperationContext{ctx: operationCtx, cancel: cancel}, release, nil
}

func (service *StudySessionService) beginSessionOperation(
	ctx context.Context,
	principal model.StudyPrincipalID,
	operation StudyOperation,
	sessionID uuid.UUID,
) (studyOperationContext, func(), *model.StudyLearnerProfile, error) {
	if service == nil || service.repository == nil || service.authorizer == nil ||
		service.authority == nil || service.materializer == nil || service.admission == nil || ctx == nil {
		return studyOperationContext{}, nil, nil, newStudySessionError(model.StudyErrorCodeInternal)
	}
	if !principal.Valid() {
		return studyOperationContext{}, nil, nil, newStudySessionError(model.StudyErrorCodeUnauthorized)
	}
	if !operation.valid() || sessionID == uuid.Nil {
		return studyOperationContext{}, nil, nil, newStudySessionError(model.StudyErrorCodeSchemaInvalid)
	}
	operationCtx, cancel := context.WithTimeout(ctx, StudyAuthoritativeOperationTimeout)
	if err := service.authorizer.AuthorizeStudy(operationCtx, StudyAuthorizationRequest{
		Principal: principal,
		Operation: operation,
		Stage:     StudyAuthorizationStageCapability,
		SessionID: sessionID,
	}); err != nil {
		cancel()
		return studyOperationContext{}, nil, nil, sanitizeStudySessionError(err)
	}
	release, admitted := service.admission.acquire(principal)
	if !admitted {
		cancel()
		return studyOperationContext{}, nil, nil, newStudySessionError(model.StudyErrorCodeRateLimited)
	}
	fail := func(operationErr error) (studyOperationContext, func(), *model.StudyLearnerProfile, error) {
		cancel()
		release()
		return studyOperationContext{}, nil, nil, operationErr
	}
	profile, err := service.repository.GetStudyLearnerProfileBySessionID(operationCtx, sessionID)
	if err != nil {
		return fail(sanitizeStudySessionError(err))
	}
	if profile == nil || profile.ID == uuid.Nil || !validStudyReference(profile.Ref) {
		return fail(newStudySessionError(model.StudyErrorCodeInternal))
	}
	if err := service.authorizer.AuthorizeStudy(operationCtx, StudyAuthorizationRequest{
		Principal:  principal,
		Operation:  operation,
		Stage:      StudyAuthorizationStageResolvedTarget,
		ProfileRef: profile.Ref,
		SessionID:  sessionID,
	}); err != nil {
		return fail(sanitizeStudySessionError(err))
	}
	profileClone := *profile
	return studyOperationContext{ctx: operationCtx, cancel: cancel}, release, &profileClone, nil
}

func (service *StudySessionService) authoritativeTime() (time.Time, error) {
	value := service.authority.Now().UTC().Truncate(time.Microsecond)
	if value.IsZero() {
		return time.Time{}, newStudySessionError(model.StudyErrorCodeInternal)
	}
	return value, nil
}

func buildStudySessionResource(
	profileRef string,
	session model.StudySession,
	receipt *model.StudyCreationReceipt,
	page StudySessionPageResource,
	generatedAt time.Time,
) (StudySessionResource, error) {
	if !validStudyReference(profileRef) || generatedAt.IsZero() || session.ID == uuid.Nil ||
		!session.Lifecycle.Valid() || !model.IsStudySHA256(session.CorpusSHA256) ||
		session.StateVersion < 1 || session.ProfileIngestionHighWater < 0 {
		return StudySessionResource{}, newStudySessionError(model.StudyErrorCodeInternal)
	}
	if err := validateResolvedStudyScope(session.Scope.Scope.CardKind, session.Scope); err != nil {
		return StudySessionResource{}, newStudySessionError(model.StudyErrorCodeInternal)
	}
	if session.Lifecycle == model.StudySessionLifecycleWaitingForCorpus {
		if session.Scheduler != nil || len(page.Items) != 0 || !page.Complete {
			return StudySessionResource{}, newStudySessionError(model.StudyErrorCodeInternal)
		}
	} else if session.Lifecycle == model.StudySessionLifecycleActive && session.Scheduler == nil {
		return StudySessionResource{}, newStudySessionError(model.StudyErrorCodeInternal)
	}

	var receiptResource *StudyCreationReceiptResource
	if receipt != nil {
		if receipt.CreationRequestID == uuid.Nil || !model.IsStudySHA256(receipt.NormalizedRequestSHA256) ||
			!receipt.Disposition.Valid() || receipt.CanonicalStudySessionID != session.ID {
			return StudySessionResource{}, newStudySessionError(model.StudyErrorCodeInternal)
		}
		receiptResource = &StudyCreationReceiptResource{
			CreationRequestID:       receipt.CreationRequestID,
			NormalizedRequestSHA256: receipt.NormalizedRequestSHA256,
			Disposition:             receipt.Disposition,
			CanonicalSessionID:      receipt.CanonicalStudySessionID,
		}
	}
	return StudySessionResource{
		ProtocolVersion:   model.StudyProtocolVersion,
		SchemaVersion:     model.StudySchemaVersion,
		RequiredSemantics: RequiredStudySemantics(),
		SessionID:         session.ID,
		ProfileRef:        profileRef,
		Scope: StudyScopeResource{
			TopicRefs: append([]string(nil), session.Scope.Scope.TopicRefs...),
			CardKind:  session.Scope.Scope.CardKind,
		},
		Lifecycle:                 session.Lifecycle,
		CreationReceipt:           receiptResource,
		PredecessorSessionID:      cloneUUIDPointer(session.PredecessorStudySessionID),
		SuccessorSessionID:        cloneUUIDPointer(session.SuccessorStudySessionID),
		CorpusSHA256:              session.CorpusSHA256,
		ProfileIngestionHighWater: session.ProfileIngestionHighWater,
		StateVersion:              session.StateVersion,
		Scheduler:                 studySchedulerResource(session.Scheduler),
		Page:                      page,
		GeneratedAt:               generatedAt.UTC(),
	}, nil
}

func validateStudySessionCycleAuthority(authority *StudySessionCycleAuthority, expectedSessionID uuid.UUID) error {
	if authority == nil || expectedSessionID == uuid.Nil || authority.Session.ID != expectedSessionID ||
		authority.Profile.ID == uuid.Nil || authority.Session.LearnerProfileID != authority.Profile.ID ||
		!validStudyReference(authority.Profile.Ref) || authority.Profile.ProfileIngestionHighWater < 0 ||
		authority.Profile.ProfileIngestionHighWater != authority.Session.ProfileIngestionHighWater {
		return newStudySessionError(model.StudyErrorCodeInternal)
	}
	validated, err := study.ValidateCommittedSessionAuthority(authority.Session, authority.Items)
	if err != nil || validated == nil {
		return newStudySessionError(model.StudyErrorCodeInternal)
	}
	authority.Session = validated.Session
	authority.Items = cloneStudySessionItems(validated.Items)
	return nil
}

func validateStudyEventSyncAuthority(
	authority *StudyEventSyncAuthority,
	principal model.StudyPrincipalID,
	previous StudySessionCycleAuthority,
	sessionID uuid.UUID,
	request StudySyncRequest,
	normalizedRequestSHA256 string,
	events []model.StudyEvent,
	candidatePageArtifact model.StudySessionPageArtifact,
) error {
	profile := previous.Profile
	if authority == nil || !principal.Valid() || authority.Session.ID != sessionID ||
		authority.Session.LearnerProfileID != profile.ID || len(authority.Receipts) != len(events) ||
		len(authority.Receipts) > model.StudyMaximumEvents {
		return newStudySessionError(model.StudyErrorCodeInternal)
	}
	flush := authority.FlushReceipt
	if flush.ID == uuid.Nil || flush.StudySessionID != sessionID || flush.SyncRequestID != request.SyncID ||
		flush.ClientStreamID != request.ClientStreamID || flush.LearnerProfileID != profile.ID ||
		flush.StudyPrincipalID.String() != principal.String() || !flush.SessionPageArtifactRequired ||
		flush.NormalizedSyncRequestSHA256 != normalizedRequestSHA256 || flush.AcknowledgedThroughSequence < 0 ||
		flush.ProfileIngestionHighWater < 0 || flush.StudySessionStateVersion < 1 || flush.IssuedAt.IsZero() ||
		flush.StudySessionStateVersion > authority.Session.StateVersion ||
		flush.ProfileIngestionHighWater > authority.Session.ProfileIngestionHighWater ||
		authority.PageArtifact.Request.AfterPosition != request.Page.AfterPosition ||
		authority.PageArtifact.Request.Limit != request.Page.Limit {
		return newStudySessionError(model.StudyErrorCodeInternal)
	}
	if !authority.Replayed &&
		(previous.Session.StateVersion != request.ExpectedStateVersion ||
			flush.StudySessionStateVersion != authority.Session.StateVersion ||
			flush.ProfileIngestionHighWater != authority.Session.ProfileIngestionHighWater ||
			!sameStudySessionPageArtifact(authority.PageArtifact, candidatePageArtifact)) {
		return newStudySessionError(model.StudyErrorCodeInternal)
	}
	if err := validateStudyFlushReceiptWindow(flush, events); err != nil {
		return err
	}
	expectedFlushStateVersion := request.ExpectedStateVersion
	if flush.FirstClientSequence != nil {
		if expectedFlushStateVersion == math.MaxInt64 {
			return newStudySessionError(model.StudyErrorCodeInternal)
		}
		expectedFlushStateVersion++
	}
	if flush.StudySessionStateVersion != expectedFlushStateVersion {
		return newStudySessionError(model.StudyErrorCodeInternal)
	}
	acceptedSuffix := make([]model.StudyEventReceipt, 0, len(authority.Receipts))
	for index, receipt := range authority.Receipts {
		event := events[index]
		_, payloadSHA256, err := model.NormalizeStudyEventPayload(event)
		if err != nil {
			return newStudySessionError(model.StudyErrorCodeInternal)
		}
		if receipt.EventID == uuid.Nil || receipt.ClientSequence < 1 || !model.IsStudySHA256(receipt.PayloadSHA256) ||
			!model.IsStudySHA256(receipt.BoundPayloadSHA256) || receipt.CreatedAt.IsZero() ||
			!receipt.Disposition.Valid() || !receipt.Code.ValidFor(receipt.Disposition) ||
			receipt.ClientStreamID != request.ClientStreamID || receipt.ClientSequence != event.ClientSequence ||
			receipt.EventID != event.ID || receipt.LearnerProfileID != profile.ID ||
			receipt.StudySessionID != sessionID || receipt.StudySessionItemID != event.StudySessionItemID ||
			receipt.CardIdentity != event.CardIdentity || receipt.ContentSHA256 != event.ContentSHA256 ||
			receipt.PayloadSHA256 != payloadSHA256 || receipt.OriginatingStudyFlushReceiptID == uuid.Nil ||
			receipt.EventIDBindingMatchesInput != (receipt.BoundPayloadSHA256 == payloadSHA256) ||
			receipt.CreatedAt.After(flush.IssuedAt) {
			return newStudySessionError(model.StudyErrorCodeInternal)
		}
		inSuffix := sequenceInStudyFlushSuffix(flush, receipt.ClientSequence)
		if (receipt.OriginatingStudyFlushReceiptID == flush.ID) != inSuffix {
			return newStudySessionError(model.StudyErrorCodeInternal)
		}
		switch receipt.Disposition {
		case model.StudyEventReceiptDispositionAccepted, model.StudyEventReceiptDispositionSemanticDuplicate:
			if receipt.CanonicalStudyEventID == nil || *receipt.CanonicalStudyEventID == uuid.Nil ||
				receipt.CanonicalProfileIngestionOrdinal == nil || *receipt.CanonicalProfileIngestionOrdinal < 1 ||
				*receipt.CanonicalProfileIngestionOrdinal > flush.ProfileIngestionHighWater ||
				*receipt.CanonicalProfileIngestionOrdinal > authority.Session.ProfileIngestionHighWater ||
				receipt.TerminalFailureClientSequence != nil || !receipt.EventIDBindingMatchesInput {
				return newStudySessionError(model.StudyErrorCodeInternal)
			}
			if receipt.Disposition == model.StudyEventReceiptDispositionAccepted &&
				*receipt.CanonicalStudyEventID != receipt.EventID {
				return newStudySessionError(model.StudyErrorCodeInternal)
			}
			if inSuffix && receipt.Disposition == model.StudyEventReceiptDispositionAccepted {
				acceptedSuffix = append(acceptedSuffix, receipt)
			}
		case model.StudyEventReceiptDispositionTerminalRejected:
			if receipt.CanonicalStudyEventID != nil || receipt.CanonicalProfileIngestionOrdinal != nil ||
				receipt.TerminalFailureClientSequence == nil || *receipt.TerminalFailureClientSequence < 1 {
				return newStudySessionError(model.StudyErrorCodeInternal)
			}
		default:
			return newStudySessionError(model.StudyErrorCodeInternal)
		}
	}
	if err := validateStudyFlushSuffixCanonicalOrdinals(
		flush,
		authority.Receipts,
		acceptedSuffix,
		previous.Profile.ProfileIngestionHighWater,
		authority.Replayed,
	); err != nil {
		return err
	}
	if err := validateStudyFlushSuffixReceiptGroup(flush, authority.Receipts); err != nil {
		return err
	}
	validatedSession, err := study.ValidateCommittedSessionAdvance(previous.Session, authority.Session, previous.Items)
	if err != nil || validatedSession == nil {
		return newStudySessionError(model.StudyErrorCodeInternal)
	}
	authority.Session = validatedSession.Session
	return nil
}

func sameStudySessionPageArtifact(left, right model.StudySessionPageArtifact) bool {
	return left.Request == right.Request &&
		left.CanonicalSHA256 == right.CanonicalSHA256 &&
		bytes.Equal(left.CanonicalJSON, right.CanonicalJSON)
}

func sequenceInStudyFlushSuffix(flush model.StudyFlushReceipt, sequence int64) bool {
	return flush.FirstClientSequence != nil && flush.LastClientSequence != nil &&
		sequence >= *flush.FirstClientSequence && sequence <= *flush.LastClientSequence
}

func validateStudyFlushSuffixReceiptGroup(
	flush model.StudyFlushReceipt,
	receipts []model.StudyEventReceipt,
) error {
	if flush.FirstClientSequence == nil {
		return nil
	}
	var suffix []model.StudyEventReceipt
	for _, receipt := range receipts {
		if sequenceInStudyFlushSuffix(flush, receipt.ClientSequence) {
			suffix = append(suffix, receipt)
		}
	}
	if len(suffix) != int(*flush.LastClientSequence-*flush.FirstClientSequence+1) {
		return newStudySessionError(model.StudyErrorCodeInternal)
	}
	first := suffix[0]
	if first.Disposition != model.StudyEventReceiptDispositionTerminalRejected {
		for _, receipt := range suffix {
			if receipt.Disposition == model.StudyEventReceiptDispositionTerminalRejected {
				return newStudySessionError(model.StudyErrorCodeInternal)
			}
		}
		return nil
	}
	if first.TerminalFailureClientSequence == nil ||
		*first.TerminalFailureClientSequence < *flush.FirstClientSequence ||
		*first.TerminalFailureClientSequence > *flush.LastClientSequence {
		return newStudySessionError(model.StudyErrorCodeInternal)
	}
	causeSeen := false
	for _, receipt := range suffix {
		if receipt.Disposition != model.StudyEventReceiptDispositionTerminalRejected ||
			receipt.Code != first.Code || receipt.TerminalFailureClientSequence == nil ||
			*receipt.TerminalFailureClientSequence != *first.TerminalFailureClientSequence {
			return newStudySessionError(model.StudyErrorCodeInternal)
		}
		if receipt.ClientSequence == *first.TerminalFailureClientSequence {
			if (receipt.Code == model.StudyEventReceiptCodeIDConflict) == receipt.EventIDBindingMatchesInput {
				return newStudySessionError(model.StudyErrorCodeInternal)
			}
			causeSeen = true
		}
	}
	if !causeSeen {
		return newStudySessionError(model.StudyErrorCodeInternal)
	}
	return nil
}

func validateStudyFlushReceiptWindow(flush model.StudyFlushReceipt, events []model.StudyEvent) error {
	if len(events) == 0 {
		if flush.RequestFirstClientSequence != nil || flush.RequestLastClientSequence != nil ||
			flush.FirstClientSequence != nil || flush.LastClientSequence != nil {
			return newStudySessionError(model.StudyErrorCodeInternal)
		}
		return nil
	}
	firstRequestSequence := events[0].ClientSequence
	lastRequestSequence := events[len(events)-1].ClientSequence
	if flush.RequestFirstClientSequence == nil || *flush.RequestFirstClientSequence != firstRequestSequence ||
		flush.RequestLastClientSequence == nil || *flush.RequestLastClientSequence != lastRequestSequence ||
		flush.AcknowledgedThroughSequence < lastRequestSequence ||
		(flush.FirstClientSequence == nil) != (flush.LastClientSequence == nil) {
		return newStudySessionError(model.StudyErrorCodeInternal)
	}
	if flush.FirstClientSequence != nil {
		if *flush.FirstClientSequence < firstRequestSequence || *flush.FirstClientSequence > lastRequestSequence ||
			*flush.LastClientSequence < *flush.FirstClientSequence || *flush.LastClientSequence != lastRequestSequence ||
			flush.AcknowledgedThroughSequence != *flush.LastClientSequence {
			return newStudySessionError(model.StudyErrorCodeInternal)
		}
	}
	return nil
}

func validateStudyFlushSuffixCanonicalOrdinals(
	flush model.StudyFlushReceipt,
	receipts []model.StudyEventReceipt,
	accepted []model.StudyEventReceipt,
	previousProfileIngestionHighWater int64,
	replayed bool,
) error {
	if len(accepted) == 0 {
		return nil
	}
	firstOrdinal := *accepted[0].CanonicalProfileIngestionOrdinal
	if !replayed && firstOrdinal <= previousProfileIngestionHighWater {
		return newStudySessionError(model.StudyErrorCodeInternal)
	}
	for index, receipt := range accepted {
		if *receipt.CanonicalProfileIngestionOrdinal != firstOrdinal+int64(index) {
			return newStudySessionError(model.StudyErrorCodeInternal)
		}
	}
	if *accepted[len(accepted)-1].CanonicalProfileIngestionOrdinal != flush.ProfileIngestionHighWater {
		return newStudySessionError(model.StudyErrorCodeInternal)
	}
	acceptedByOrdinal := make(map[int64]uuid.UUID, len(accepted))
	for _, receipt := range accepted {
		acceptedByOrdinal[*receipt.CanonicalProfileIngestionOrdinal] = *receipt.CanonicalStudyEventID
	}
	for _, receipt := range receipts {
		if !sequenceInStudyFlushSuffix(flush, receipt.ClientSequence) ||
			receipt.Disposition != model.StudyEventReceiptDispositionSemanticDuplicate ||
			receipt.CanonicalProfileIngestionOrdinal == nil || *receipt.CanonicalProfileIngestionOrdinal < firstOrdinal {
			continue
		}
		canonicalID, exists := acceptedByOrdinal[*receipt.CanonicalProfileIngestionOrdinal]
		if !exists || receipt.CanonicalStudyEventID == nil || canonicalID != *receipt.CanonicalStudyEventID {
			return newStudySessionError(model.StudyErrorCodeInternal)
		}
		acceptedEarlier := false
		for _, candidate := range accepted {
			if candidate.ClientSequence >= receipt.ClientSequence {
				break
			}
			if *candidate.CanonicalProfileIngestionOrdinal == *receipt.CanonicalProfileIngestionOrdinal &&
				*candidate.CanonicalStudyEventID == *receipt.CanonicalStudyEventID {
				acceptedEarlier = true
				break
			}
		}
		if !acceptedEarlier {
			return newStudySessionError(model.StudyErrorCodeInternal)
		}
	}
	return nil
}

func studySchedulerResource(scheduler *model.StudySchedulerProvenance) *StudySchedulerResource {
	if scheduler == nil {
		return nil
	}
	return &StudySchedulerResource{
		Name:                           scheduler.Name,
		Version:                        scheduler.Version,
		ConfigurationSHA256:            scheduler.ConfigurationSHA256,
		InputProfileIngestionHighWater: scheduler.InputProfileIngestionHighWater,
		PlannedAt:                      scheduler.PlannedAt.UTC(),
		Seed:                           scheduler.Seed,
	}
}

func studyEventReceiptResources(receipts []model.StudyEventReceipt) []StudyEventReceiptResource {
	resources := make([]StudyEventReceiptResource, len(receipts))
	for index, receipt := range receipts {
		resources[index] = StudyEventReceiptResource{
			ClientSequence:          receipt.ClientSequence,
			EventID:                 receipt.EventID,
			PayloadSHA256:           receipt.PayloadSHA256,
			Disposition:             receipt.Disposition,
			Code:                    receipt.Code,
			CanonicalEventID:        cloneUUIDPointer(receipt.CanonicalStudyEventID),
			ProfileIngestionOrdinal: cloneInt64Pointer(receipt.CanonicalProfileIngestionOrdinal),
		}
	}
	return resources
}

func studyFlushReceiptResource(receipt model.StudyFlushReceipt) StudyFlushReceiptResource {
	return StudyFlushReceiptResource{
		ReceiptID:                   receipt.ID,
		SessionID:                   receipt.StudySessionID,
		SyncID:                      receipt.SyncRequestID,
		ClientStreamID:              receipt.ClientStreamID,
		AcknowledgedThroughSequence: receipt.AcknowledgedThroughSequence,
		ProfileIngestionHighWater:   receipt.ProfileIngestionHighWater,
		StateVersion:                receipt.StudySessionStateVersion,
		IssuedAt:                    receipt.IssuedAt.UTC(),
	}
}

func boundedStudyServiceText(value string, maximumRunes int) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != "" && utf8.RuneCountInString(value) <= maximumRunes
}
