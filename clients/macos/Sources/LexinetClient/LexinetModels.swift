import Foundation

public struct SearchResponse: Decodable, Equatable {
    public let query: String
    public let total: Int
    public let results: [SearchResult]
}

public struct SearchResult: Decodable, Equatable, Identifiable {
    public let idea: IdeaSummary
    public let score: Double?
    public let matchedVia: String?
    public let matchedField: String?
    public let matchedName: String?
    public let matchType: String?
    public let headline: String?

    public var id: String { idea.id }

    enum CodingKeys: String, CodingKey {
        case idea
        case score
        case matchedVia = "matched_via"
        case matchedField = "matched_field"
        case matchedName = "matched_name"
        case matchType = "match_type"
        case headline
    }
}

public struct IdeaSummary: Decodable, Equatable, Identifiable {
    public let id: String
    public let slug: String?
    public let canonicalName: String?
    public let summary: String?
    public let topics: [Topic]
    public let acronymLinks: [AcronymLink]

    enum CodingKeys: String, CodingKey {
        case id
        case slug
        case canonicalName = "canonical_name"
        case summary
        case topics
        case acronymLinks = "acronym_links"
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        id = try container.decode(String.self, forKey: .id)
        slug = try container.decodeIfPresent(String.self, forKey: .slug)
        canonicalName = try container.decodeIfPresent(String.self, forKey: .canonicalName)
        summary = try container.decodeIfPresent(String.self, forKey: .summary)
        topics = try container.decodeIfPresent([Topic].self, forKey: .topics) ?? []
        acronymLinks = try container.decodeIfPresent([AcronymLink].self, forKey: .acronymLinks) ?? []
    }
}

public struct IdeaDetail: Decodable, Equatable, Identifiable {
    public let id: String
    public let slug: String?
    public let canonicalName: String?
    public let summary: String?
    public let names: [IdeaName]
    public let definitions: [Definition]
    public let topics: [Topic]
    public let acronymLinks: [AcronymLink]

    public var displayName: String {
        if let canonicalName, !canonicalName.isEmpty {
            return canonicalName
        }
        if let canonical = names.first(where: { $0.kind == "canonical" }) {
            return canonical.displayText
        }
        if let first = names.first {
            return first.displayText
        }
        return slug ?? id
    }

    public var primaryDefinition: String? {
        definitions.first(where: { $0.isPrimary })?.body ?? definitions.first?.body ?? summary
    }

    enum CodingKeys: String, CodingKey {
        case id
        case slug
        case canonicalName = "canonical_name"
        case summary
        case names
        case definitions
        case topics
        case acronymLinks = "acronym_links"
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        id = try container.decode(String.self, forKey: .id)
        slug = try container.decodeIfPresent(String.self, forKey: .slug)
        canonicalName = try container.decodeIfPresent(String.self, forKey: .canonicalName)
        summary = try container.decodeIfPresent(String.self, forKey: .summary)
        names = try container.decodeIfPresent([IdeaName].self, forKey: .names) ?? []
        definitions = try container.decodeIfPresent([Definition].self, forKey: .definitions) ?? []
        topics = try container.decodeIfPresent([Topic].self, forKey: .topics) ?? []
        acronymLinks = try container.decodeIfPresent([AcronymLink].self, forKey: .acronymLinks) ?? []
    }
}

public struct IdeaName: Decodable, Equatable, Identifiable {
    public let id: String
    public let text: String
    public let display: String?
    public let kind: String
    public let expansion: String?

    public var displayText: String { display ?? text }
}

public struct Definition: Decodable, Equatable, Identifiable {
    public let id: String
    public let body: String
    public let isPrimary: Bool

    enum CodingKeys: String, CodingKey {
        case id
        case body
        case isPrimary = "is_primary"
    }
}

public struct Topic: Decodable, Equatable, Identifiable {
    public let id: String?
    public let slug: String?
    public let name: String

    public var displayName: String { name.isEmpty ? (slug ?? "topic") : name }

    enum CodingKeys: String, CodingKey {
        case id
        case slug
        case name
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        id = try container.decodeIfPresent(String.self, forKey: .id)
        slug = try container.decodeIfPresent(String.self, forKey: .slug)
        name = try container.decodeIfPresent(String.self, forKey: .name) ?? slug ?? ""
    }
}

public struct AcronymLink: Decodable, Equatable, Identifiable {
    public let id: String?
    public let acronymIdeaID: String?
    public let expansionIdeaID: String?
    public let acronymCanonicalName: String?
    public let expansionCanonicalName: String?
    public let role: String?

    public var summary: String {
        let acronym = acronymCanonicalName ?? acronymIdeaID ?? "acronym"
        let expansion = expansionCanonicalName ?? expansionIdeaID ?? "expansion"
        if role == "expansion" {
            return "Expanded by \(acronym)"
        }
        if role == "acronym" {
            return "Expands to \(expansion)"
        }
        return "\(acronym) -> \(expansion)"
    }

    enum CodingKeys: String, CodingKey {
        case id
        case acronymIdeaID = "acronym_idea_id"
        case expansionIdeaID = "expansion_idea_id"
        case acronymCanonicalName = "acronym_canonical_name"
        case expansionCanonicalName = "expansion_canonical_name"
        case role
    }
}

public enum SubmissionStatus: String, Decodable, Equatable {
    case pending
    case enriched
    case failed
}

public struct Submission: Decodable, Equatable, Identifiable {
    public let id: String
    public let term: String
    public let context: String?
    public let status: SubmissionStatus
    public let ideaID: String?
    public let note: String?
    public let completedAt: Date?
    public let createdAt: Date
    public let updatedAt: Date

    enum CodingKeys: String, CodingKey {
        case id
        case term
        case context
        case status
        case ideaID = "idea_id"
        case note
        case completedAt = "completed_at"
        case createdAt = "created_at"
        case updatedAt = "updated_at"
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        id = try container.decode(String.self, forKey: .id)
        term = try container.decode(String.self, forKey: .term)
        context = try container.decodeIfPresent(String.self, forKey: .context)
        status = try container.decode(SubmissionStatus.self, forKey: .status)
        ideaID = try container.decodeIfPresent(String.self, forKey: .ideaID)
        note = try container.decodeIfPresent(String.self, forKey: .note)
        completedAt = try Self.decodeDateIfPresent(container, forKey: .completedAt)
        createdAt = try Self.decodeDate(container, forKey: .createdAt)
        updatedAt = try Self.decodeDate(container, forKey: .updatedAt)
    }

    private static func decodeDate(_ container: KeyedDecodingContainer<CodingKeys>, forKey key: CodingKeys) throws -> Date {
        let raw = try container.decode(String.self, forKey: key)
        if let date = parseGoDate(raw) {
            return date
        }
        throw DecodingError.dataCorruptedError(forKey: key, in: container, debugDescription: "Invalid Go time value: \(raw)")
    }

    private static func decodeDateIfPresent(_ container: KeyedDecodingContainer<CodingKeys>, forKey key: CodingKeys) throws -> Date? {
        guard let raw = try container.decodeIfPresent(String.self, forKey: key) else {
            return nil
        }
        if let date = parseGoDate(raw) {
            return date
        }
        throw DecodingError.dataCorruptedError(forKey: key, in: container, debugDescription: "Invalid Go time value: \(raw)")
    }

    private static func parseGoDate(_ raw: String) -> Date? {
        let fractional = ISO8601DateFormatter()
        fractional.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let date = fractional.date(from: raw) {
            return date
        }

        let standard = ISO8601DateFormatter()
        standard.formatOptions = [.withInternetDateTime]
        return standard.date(from: raw)
    }
}

public struct ListSubmissionsResponse: Decodable, Equatable {
    public let total: Int
    public let limit: Int
    public let offset: Int
    public let submissions: [Submission]
}
