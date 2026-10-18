package studysession

// ValidStudyProfileRef is the @canonical study_profile_reference_validation
// entry point. It reports whether value matches the exact bounded study.v1
// learner-profile reference grammar used by request validation.
func ValidStudyProfileRef(value string) bool {
	return validStudyReference(value)
}
