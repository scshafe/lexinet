package studysession

import (
	"sync"

	"lexinet/internal/model"
)

// StudyAdmissionController provides non-queuing global and per-principal
// admission. Callers receive an idempotent release function on success.
type StudyAdmissionController struct {
	mu                sync.Mutex
	globalInFlight    int
	principalInFlight map[string]int
}

func newStudyAdmissionController() *StudyAdmissionController {
	return &StudyAdmissionController{principalInFlight: make(map[string]int)}
}

func (controller *StudyAdmissionController) acquire(principal model.StudyPrincipalID) (func(), bool) {
	if controller == nil || !principal.Valid() {
		return nil, false
	}
	principalKey := principal.String()
	controller.mu.Lock()
	if controller.globalInFlight >= StudyGlobalAdmissionLimit || controller.principalInFlight[principalKey] >= StudyPerPrincipalAdmissionLimit {
		controller.mu.Unlock()
		return nil, false
	}
	controller.globalInFlight++
	controller.principalInFlight[principalKey]++
	controller.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			controller.mu.Lock()
			controller.globalInFlight--
			controller.principalInFlight[principalKey]--
			if controller.principalInFlight[principalKey] == 0 {
				delete(controller.principalInFlight, principalKey)
			}
			controller.mu.Unlock()
		})
	}, true
}
