import XCTest
#if canImport(FoundationNetworking)
import FoundationNetworking
#endif
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

    func testDecodesPendingSubmissionFixture() throws {
        let submission = try decode(Submission.self, fixture: "submission-pending")

        XCTAssertEqual(submission.id, "11111111-1111-1111-1111-111111111111")
        XCTAssertEqual(submission.term, "RAG")
        XCTAssertEqual(submission.context, "Used in agent architecture notes.")
        XCTAssertEqual(submission.status, .pending)
        XCTAssertNil(submission.ideaID)
        XCTAssertNil(submission.note)
        XCTAssertNil(submission.completedAt)
    }

    func testDecodesTerminalSubmissionFixtureWithOptionalFields() throws {
        let response = try decode(ListSubmissionsResponse.self, fixture: "submissions-list")

        XCTAssertEqual(response.total, 2)
        XCTAssertEqual(response.submissions.map(\.status), [.enriched, .failed])
        XCTAssertEqual(response.submissions[0].ideaID, "22222222-2222-2222-2222-222222222222")
        XCTAssertEqual(response.submissions[0].note, "Linked to the existing concept.")
        XCTAssertNotNil(response.submissions[0].completedAt)
        XCTAssertNil(response.submissions[1].context)
        XCTAssertEqual(response.submissions[1].note, "Insufficient details.")
    }

    func testCreateSubmissionPostsJSONAndDecodesResponse() async throws {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [MockURLProtocol.self]
        let session = URLSession(configuration: configuration)
        let api = LexinetAPI(baseURL: URL(string: "http://lexinet.test")!, session: session)

        MockURLProtocol.handler = { request in
            XCTAssertEqual(request.httpMethod, "POST")
            XCTAssertEqual(request.url?.path, "/submissions")
            XCTAssertEqual(request.value(forHTTPHeaderField: "Accept"), "application/json")
            XCTAssertEqual(request.value(forHTTPHeaderField: "Content-Type"), "application/json")

            let body = try XCTUnwrap(Self.bodyData(from: request))
            let json = try XCTUnwrap(JSONSerialization.jsonObject(with: body) as? [String: String])
            XCTAssertEqual(json["term"], "RAG")
            XCTAssertEqual(json["context"], "Used in agent architecture notes.")

            let response = HTTPURLResponse(
                url: try XCTUnwrap(request.url),
                statusCode: 201,
                httpVersion: nil,
                headerFields: ["Content-Type": "application/json"]
            )!
            let data = try Data(contentsOf: try XCTUnwrap(Bundle.module.url(forResource: "submission-pending", withExtension: "json")))
            return (response, data)
        }
        defer { MockURLProtocol.handler = nil }

        let submission = try await api.createSubmission(term: " RAG ", context: " Used in agent architecture notes. ")

        XCTAssertEqual(submission.term, "RAG")
        XCTAssertEqual(submission.context, "Used in agent architecture notes.")
        XCTAssertEqual(submission.status, .pending)
    }

    private func decode<T: Decodable>(_ type: T.Type, fixture: String) throws -> T {
        let url = try XCTUnwrap(Bundle.module.url(forResource: fixture, withExtension: "json"))
        let data = try Data(contentsOf: url)
        return try JSONDecoder().decode(T.self, from: data)
    }

    private static func bodyData(from request: URLRequest) -> Data? {
        if let body = request.httpBody {
            return body
        }
        guard let stream = request.httpBodyStream else {
            return nil
        }
        stream.open()
        defer { stream.close() }

        var data = Data()
        let bufferSize = 1024
        let buffer = UnsafeMutablePointer<UInt8>.allocate(capacity: bufferSize)
        defer { buffer.deallocate() }
        while stream.hasBytesAvailable {
            let read = stream.read(buffer, maxLength: bufferSize)
            if read <= 0 {
                break
            }
            data.append(buffer, count: read)
        }
        return data
    }
}

private final class MockURLProtocol: URLProtocol {
    static var handler: ((URLRequest) throws -> (HTTPURLResponse, Data))?

    override class func canInit(with request: URLRequest) -> Bool {
        true
    }

    override class func canonicalRequest(for request: URLRequest) -> URLRequest {
        request
    }

    override func startLoading() {
        guard let handler = Self.handler else {
            client?.urlProtocol(self, didFailWithError: LexinetClientError.invalidResponse)
            return
        }

        do {
            let (response, data) = try handler(request)
            client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
            client?.urlProtocol(self, didLoad: data)
            client?.urlProtocolDidFinishLoading(self)
        } catch {
            client?.urlProtocol(self, didFailWithError: error)
        }
    }

    override func stopLoading() {}
}
