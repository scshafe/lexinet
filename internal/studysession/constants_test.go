package studysession

import "testing"

func TestRequiredStudySemanticsReturnsDetachedCanonicalOrder(t *testing.T) {
	first := RequiredStudySemantics()
	second := RequiredStudySemantics()
	if len(first) != len(studyRequiredSemantics) || len(second) != len(studyRequiredSemantics) {
		t.Fatal("STUDY_REQUIRED_SEMANTICS.CARDINALITY_INVALID")
	}
	for index := range studyRequiredSemantics {
		if first[index] != studyRequiredSemantics[index] || second[index] != studyRequiredSemantics[index] {
			t.Fatal("STUDY_REQUIRED_SEMANTICS.ORDER_INVALID")
		}
	}
	first[0] = "mutated"
	if second[0] != studyRequiredSemantics[0] || RequiredStudySemantics()[0] != studyRequiredSemantics[0] {
		t.Fatal("STUDY_REQUIRED_SEMANTICS.CALLER_ALIAS_MUTATED_AUTHORITY")
	}
}
