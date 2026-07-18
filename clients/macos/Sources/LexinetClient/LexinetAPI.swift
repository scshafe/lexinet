import Foundation
#if canImport(FoundationNetworking)
import FoundationNetworking
#endif

public enum LexinetClientError: Error, Equatable, LocalizedError {
    case serviceUnavailable(String)
    case nonSuccess(statusCode: Int, message: String)
    case invalidResponse

    public var errorDescription: String? {
        switch self {
        case .serviceUnavailable(let message):
            return message
        case .nonSuccess(let statusCode, let message):
            return "lexinet returned HTTP \(statusCode): \(message)"
        case .invalidResponse:
            return "lexinet returned a response the client could not understand"
        }
    }
}

public struct LexinetAPI {
    public let baseURL: URL
    private let session: URLSession
    private let decoder: JSONDecoder
    private let encoder: JSONEncoder

    public init(baseURL: URL = URL(string: "http://127.0.0.1:17892")!, session: URLSession = .shared) {
        self.baseURL = baseURL
        self.session = session
        self.decoder = JSONDecoder()
        self.encoder = JSONEncoder()
    }

    public func search(query: String, limit: Int = 8) async throws -> SearchResponse {
        var components = URLComponents(url: baseURL.appendingPathComponent("search"), resolvingAgainstBaseURL: false)!
        components.queryItems = [
            URLQueryItem(name: "q", value: query),
            URLQueryItem(name: "limit", value: String(limit)),
            URLQueryItem(name: "scope", value: "all")
        ]
        guard let url = components.url else {
            throw LexinetClientError.invalidResponse
        }
        return try await get(url)
    }

    public func ideaDetail(ref: String) async throws -> IdeaDetail {
        var components = URLComponents(url: baseURL.appendingPathComponent("ideas").appendingPathComponent(ref), resolvingAgainstBaseURL: false)!
        components.queryItems = [URLQueryItem(name: "expand", value: "definitions,relationships")]
        guard let url = components.url else {
            throw LexinetClientError.invalidResponse
        }
        return try await get(url)
    }

    public func createSubmission(term: String, context: String?) async throws -> Submission {
        struct CreateSubmissionRequest: Encodable {
            let term: String
            let context: String?
        }

        let trimmedContext = context?.trimmingCharacters(in: .whitespacesAndNewlines)
        let request = CreateSubmissionRequest(
            term: term.trimmingCharacters(in: .whitespacesAndNewlines),
            context: trimmedContext?.isEmpty == true ? nil : trimmedContext
        )
        let url = baseURL.appendingPathComponent("submissions")
        return try await post(url, body: request)
    }

    private func get<T: Decodable>(_ url: URL) async throws -> T {
        try await send(URLRequest(url: url))
    }

    private func post<T: Decodable, Body: Encodable>(_ url: URL, body: Body) async throws -> T {
        var request = URLRequest(url: url)
        request.httpMethod = "POST"
        request.httpBody = try encoder.encode(body)
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        return try await send(request)
    }

    private func send<T: Decodable>(_ originalRequest: URLRequest) async throws -> T {
        var request = originalRequest
        request.setValue("application/json", forHTTPHeaderField: "Accept")

        let data: Data
        let response: URLResponse
        do {
            (data, response) = try await session.data(for: request)
        } catch {
            throw LexinetClientError.serviceUnavailable("lexinet service is unavailable. Start it with: lexinet serve --tailnet off")
        }

        guard let httpResponse = response as? HTTPURLResponse else {
            throw LexinetClientError.invalidResponse
        }

        guard (200..<300).contains(httpResponse.statusCode) else {
            let message = Self.errorMessage(from: data) ?? HTTPURLResponse.localizedString(forStatusCode: httpResponse.statusCode)
            throw LexinetClientError.nonSuccess(statusCode: httpResponse.statusCode, message: message)
        }

        do {
            return try decoder.decode(T.self, from: data)
        } catch {
            throw LexinetClientError.invalidResponse
        }
    }

    private static func errorMessage(from data: Data) -> String? {
        struct ErrorEnvelope: Decodable {
            struct APIError: Decodable {
                let message: String
            }
            let error: APIError
        }

        return try? JSONDecoder().decode(ErrorEnvelope.self, from: data).error.message
    }
}
