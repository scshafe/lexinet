import Foundation

public enum LexinetEndpointConfigurationError: LocalizedError, Equatable {
    case invalidEndpoint(source: String, value: String)
    case invalidConfig(path: String, line: Int, reason: String)
    case unreadableConfig(path: String, reason: String)

    public var errorDescription: String? {
        switch self {
        case .invalidEndpoint(let source, let value):
            return "Invalid lexinet endpoint in \(source): \(value.debugDescription). Expected an http or https URL."
        case .invalidConfig(let path, let line, let reason):
            return "Invalid lexinet config at \(path):\(line): \(reason)"
        case .unreadableConfig(let path, let reason):
            return "Unable to read lexinet config at \(path): \(reason)"
        }
    }
}

/// @canonical macos_endpoint_configuration -- all macOS client endpoint resolution uses this type.
public struct LexinetEndpointConfiguration {
    public static let endpointEnvironmentVariable = "LEXINET_ENDPOINT"
    public static let configEnvironmentVariable = "LEXINET_CONFIG"
    public static let fallbackURL = URL(string: "http://127.0.0.1:17892")!

    /// Returns the standard menu-bar config path under a user's home directory.
    ///
    /// - Parameter homeDirectory: An absolute user home-directory URL.
    /// - Returns: `<homeDirectory>/.config/lexinet/config.toml` as a file URL.
    public static func defaultConfigFileURL(
        homeDirectory: URL = FileManager.default.homeDirectoryForCurrentUser
    ) -> URL {
        homeDirectory
            .appendingPathComponent(".config", isDirectory: true)
            .appendingPathComponent("lexinet", isDirectory: true)
            .appendingPathComponent("config.toml", isDirectory: false)
    }

    /// Resolves the service URL from environment, config file, user default, then loopback.
    ///
    /// A missing config file means no file override. A present but unreadable or invalid config
    /// throws `LexinetEndpointConfigurationError` so the app can expose the failure visibly.
    ///
    /// - Parameters:
    ///   - environment: Environment values containing optional `LEXINET_ENDPOINT` or
    ///     `LEXINET_CONFIG` overrides.
    ///   - userDefaultEndpoint: The legacy `dev.lexinet.menubar` endpoint value, when present.
    ///   - homeDirectory: The home directory used to construct the default config path.
    /// - Returns: A validated HTTP or HTTPS service URL.
    /// - Throws: `LexinetEndpointConfigurationError` when an explicit endpoint or a present
    ///   config file cannot be read or validated.
    public static func resolve(
        environment: [String: String] = ProcessInfo.processInfo.environment,
        userDefaultEndpoint: String? = UserDefaults.standard.string(forKey: "endpoint"),
        homeDirectory: URL = FileManager.default.homeDirectoryForCurrentUser
    ) throws -> URL {
        if let rawEndpoint = environment[endpointEnvironmentVariable] {
            return try validatedURL(rawEndpoint, source: endpointEnvironmentVariable)
        }

        let configFileURL = try resolvedConfigFileURL(
            environment: environment,
            homeDirectory: homeDirectory
        )
        if let rawEndpoint = try serverURLFromConfigIfPresent(at: configFileURL) {
            return try validatedURL(
                rawEndpoint,
                source: "[server].url in \(configFileURL.path)"
            )
        }

        if let userDefaultEndpoint {
            return try validatedURL(userDefaultEndpoint, source: "dev.lexinet.menubar endpoint")
        }

        return fallbackURL
    }

    private static func resolvedConfigFileURL(
        environment: [String: String],
        homeDirectory: URL
    ) throws -> URL {
        guard let configuredPath = environment[configEnvironmentVariable] else {
            return defaultConfigFileURL(homeDirectory: homeDirectory)
        }

        let trimmedPath = configuredPath.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmedPath.isEmpty else {
            throw LexinetEndpointConfigurationError.invalidConfig(
                path: configEnvironmentVariable,
                line: 1,
                reason: "the config path is empty"
            )
        }

        let expandedPath = NSString(string: trimmedPath).expandingTildeInPath
        return URL(fileURLWithPath: expandedPath, isDirectory: false)
    }

    private static func serverURLFromConfigIfPresent(at configFileURL: URL) throws -> String? {
        let contents: String
        do {
            contents = try String(contentsOf: configFileURL, encoding: .utf8)
        } catch let error as CocoaError where error.code == .fileReadNoSuchFile {
            return nil
        } catch {
            throw LexinetEndpointConfigurationError.unreadableConfig(
                path: configFileURL.path,
                reason: error.localizedDescription
            )
        }

        return try parseServerURL(contents, path: configFileURL.path)
    }

    private static func parseServerURL(_ contents: String, path: String) throws -> String? {
        var currentSection: String?
        var parsedURL: String?

        for (zeroBasedLineNumber, rawLine) in contents.split(
            separator: "\n",
            omittingEmptySubsequences: false
        ).enumerated() {
            let lineNumber = zeroBasedLineNumber + 1
            let line = contentBeforeComment(in: String(rawLine))
                .trimmingCharacters(in: .whitespacesAndNewlines)
            guard !line.isEmpty else { continue }

            if line.hasPrefix("[") && line.hasSuffix("]") {
                currentSection = String(line.dropFirst().dropLast())
                    .trimmingCharacters(in: .whitespacesAndNewlines)
                continue
            }

            guard currentSection == "server" else { continue }
            guard let equalsIndex = line.firstIndex(of: "=") else {
                if line == "url" || line.hasPrefix("url ") {
                    throw LexinetEndpointConfigurationError.invalidConfig(
                        path: path,
                        line: lineNumber,
                        reason: "server.url must use `url = \"https://host\"`"
                    )
                }
                continue
            }

            let key = line[..<equalsIndex].trimmingCharacters(in: .whitespacesAndNewlines)
            guard key == "url" else { continue }
            guard parsedURL == nil else {
                throw LexinetEndpointConfigurationError.invalidConfig(
                    path: path,
                    line: lineNumber,
                    reason: "server.url is defined more than once"
                )
            }

            let rawValue = line[line.index(after: equalsIndex)...]
                .trimmingCharacters(in: .whitespacesAndNewlines)
            parsedURL = try parseTOMLString(rawValue, path: path, line: lineNumber)
        }

        return parsedURL
    }

    private static func parseTOMLString(_ rawValue: String, path: String, line: Int) throws -> String {
        if rawValue.first == "\"", rawValue.last == "\"" {
            do {
                return try JSONDecoder().decode(String.self, from: Data(rawValue.utf8))
            } catch {
                throw LexinetEndpointConfigurationError.invalidConfig(
                    path: path,
                    line: line,
                    reason: "server.url is not a valid quoted string"
                )
            }
        }

        if rawValue.first == "'", rawValue.last == "'", rawValue.count >= 2 {
            return String(rawValue.dropFirst().dropLast())
        }

        throw LexinetEndpointConfigurationError.invalidConfig(
            path: path,
            line: line,
            reason: "server.url must be a quoted string"
        )
    }

    private static func contentBeforeComment(in rawLine: String) -> String {
        var quote: Character?
        var escaped = false

        for index in rawLine.indices {
            let character = rawLine[index]
            if escaped {
                escaped = false
                continue
            }
            if character == "\\", quote == "\"" {
                escaped = true
                continue
            }
            if character == "\"" || character == "'" {
                if quote == character {
                    quote = nil
                } else if quote == nil {
                    quote = character
                }
                continue
            }
            if character == "#", quote == nil {
                return String(rawLine[..<index])
            }
        }

        return rawLine
    }

    private static func validatedURL(_ rawValue: String, source: String) throws -> URL {
        let trimmedValue = rawValue.trimmingCharacters(in: .whitespacesAndNewlines)
        if let url = URL(string: trimmedValue),
           let scheme = url.scheme?.lowercased(),
           (scheme == "http" || scheme == "https"),
           url.host != nil {
            return url
        }

        throw LexinetEndpointConfigurationError.invalidEndpoint(
            source: source,
            value: rawValue
        )
    }
}
