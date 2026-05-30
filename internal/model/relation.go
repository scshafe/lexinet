package model

type RelationKind string

const (
	RelationIsA                RelationKind = "is_a"
	RelationPartOf             RelationKind = "part_of"
	RelationDependsOn          RelationKind = "depends_on"
	RelationUsedInDefinitionOf RelationKind = "used_in_definition_of"
	RelationImplements         RelationKind = "implements"
	RelationSupersedes         RelationKind = "supersedes"
	RelationSynonymOf          RelationKind = "synonym_of"
	RelationRelatedTo          RelationKind = "related_to"
)

func ValidRelationKinds() []RelationKind {
	return []RelationKind{
		RelationIsA,
		RelationPartOf,
		RelationDependsOn,
		RelationUsedInDefinitionOf,
		RelationImplements,
		RelationSupersedes,
		RelationSynonymOf,
		RelationRelatedTo,
	}
}
