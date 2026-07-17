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

    public init(baseURL: URL = URL(string: "http://127.0.0.1:17892")!, session: URLSession = .shared) {
        self.baseURL = baseURL
        self.session = session
        self.decoder = JSONDecoder()
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

    private func get<T: Decodable>(_ url: URL) async throws -> T {
        var request = URLRequest(url: url)
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
