import XCTest
@testable import LexinetClient

final class ModelDecodingTests: XCTestCase {
    func testDecodesEnrichedSearchFixture() throws {
        let response = try decode(SearchResponse.self, fixture: "search-enriched")

        XCTAssertEqual(response.query, "tui")
        XCTAssertEqual(response.results.count, 1)
        XCTAssertEqual(response.results[0].idea.canonicalName, "TUI")
        XCTAssertEqual(response.results[0].idea.topics.map(\.displayName), ["software", "interfaces"])
        XCTAssertEqual(response.results[0].idea.acronymLinks[0].summary, "Expands to Terminal User Interface")
    }

    func testDecodesIdeaDetailFixture() throws {
        let detail = try decode(IdeaDetail.self, fixture: "idea-detail-enriched")

        XCTAssertEqual(detail.displayName, "Terminal User Interface")
        XCTAssertEqual(detail.primaryDefinition, "A user interface rendered inside a terminal emulator.")
        XCTAssertEqual(detail.names.map(\.displayText), ["Terminal User Interface", "TUI"])
        XCTAssertEqual(detail.topics.map(\.displayName), ["software"])
        XCTAssertEqual(detail.acronymLinks[0].summary, "Expanded by TUI")
    }

    func testDecodesCurrentServiceShapeWithoutEnrichment() throws {
        let detail = try decode(IdeaDetail.self, fixture: "idea-detail-basic")

        XCTAssertEqual(detail.displayName, "Redis")
        XCTAssertEqual(detail.topics, [])
        XCTAssertEqual(detail.acronymLinks, [])
    }

    private func decode<T: Decodable>(_ type: T.Type, fixture: String) throws -> T {
        let url = try XCTUnwrap(Bundle.module.url(forResource: fixture, withExtension: "json"))
        let data = try Data(contentsOf: url)
        return try JSONDecoder().decode(T.self, from: data)
    }
}
