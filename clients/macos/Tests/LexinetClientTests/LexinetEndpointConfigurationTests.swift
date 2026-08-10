import XCTest
@testable import LexinetClient

final class LexinetEndpointConfigurationTests: XCTestCase {
    func testEnvironmentEndpointWinsOverConfigFile() throws {
        let homeDirectory = try temporaryHome(
            config: "[server]\nurl = \"https://config.example\"\n"
        )

        let endpoint = try LexinetEndpointConfiguration.resolve(
            environment: ["LEXINET_ENDPOINT": "https://environment.example"],
            userDefaultEndpoint: "https://default.example",
            homeDirectory: homeDirectory
        )

        XCTAssertEqual(endpoint.absoluteString, "https://environment.example")
    }

    func testDefaultConfigPathUsesDotConfigLexinet() {
        let homeDirectory = URL(fileURLWithPath: "/tmp/lexinet-test-home", isDirectory: true)

        let path = LexinetEndpointConfiguration.defaultConfigFileURL(
            homeDirectory: homeDirectory
        ).path

        XCTAssertEqual(path, "/tmp/lexinet-test-home/.config/lexinet/config.toml")
    }

    func testServerURLLoadsFromDefaultConfigFile() throws {
        let homeDirectory = try temporaryHome(
            config: "# client settings\n[server]\nurl = \"https://lexinet.example\" # tailnet\n"
        )

        let endpoint = try LexinetEndpointConfiguration.resolve(
            environment: [:],
            userDefaultEndpoint: nil,
            homeDirectory: homeDirectory
        )

        XCTAssertEqual(endpoint.absoluteString, "https://lexinet.example")
    }

    func testConfigEnvironmentVariableOverridesDefaultConfigPath() throws {
        let homeDirectory = try temporaryHome(
            config: "[server]\nurl = \"https://default-config.example\"\n"
        )
        let overrideFile = homeDirectory.appendingPathComponent("override.toml")
        try "[server]\nurl = 'https://override.example'\n".write(
            to: overrideFile,
            atomically: true,
            encoding: .utf8
        )

        let endpoint = try LexinetEndpointConfiguration.resolve(
            environment: ["LEXINET_CONFIG": overrideFile.path],
            userDefaultEndpoint: nil,
            homeDirectory: homeDirectory
        )

        XCTAssertEqual(endpoint.absoluteString, "https://override.example")
    }

    func testMissingConfigFileUsesUserDefault() throws {
        let homeDirectory = try temporaryHome(config: nil)

        let endpoint = try LexinetEndpointConfiguration.resolve(
            environment: [:],
            userDefaultEndpoint: "https://default.example",
            homeDirectory: homeDirectory
        )

        XCTAssertEqual(endpoint.absoluteString, "https://default.example")
    }

    func testMissingConfigAndUserDefaultUseLoopback() throws {
        let homeDirectory = try temporaryHome(config: nil)

        let endpoint = try LexinetEndpointConfiguration.resolve(
            environment: [:],
            userDefaultEndpoint: nil,
            homeDirectory: homeDirectory
        )

        XCTAssertEqual(endpoint, LexinetEndpointConfiguration.fallbackURL)
    }

    func testInvalidPresentConfigThrowsVisibleConfigurationError() throws {
        let homeDirectory = try temporaryHome(
            config: "[server]\nurl = https://lexinet.example\n"
        )

        XCTAssertThrowsError(
            try LexinetEndpointConfiguration.resolve(
                environment: [:],
                userDefaultEndpoint: "https://default.example",
                homeDirectory: homeDirectory
            )
        ) { error in
            XCTAssertEqual(
                error as? LexinetEndpointConfigurationError,
                .invalidConfig(
                    path: homeDirectory
                        .appendingPathComponent(".config/lexinet/config.toml")
                        .path,
                    line: 2,
                    reason: "server.url must be a quoted string"
                )
            )
        }
    }

    func testInvalidEnvironmentEndpointDoesNotFallBack() throws {
        let homeDirectory = try temporaryHome(
            config: "[server]\nurl = \"https://config.example\"\n"
        )

        XCTAssertThrowsError(
            try LexinetEndpointConfiguration.resolve(
                environment: ["LEXINET_ENDPOINT": "not-a-url"],
                userDefaultEndpoint: nil,
                homeDirectory: homeDirectory
            )
        ) { error in
            XCTAssertEqual(
                error as? LexinetEndpointConfigurationError,
                .invalidEndpoint(source: "LEXINET_ENDPOINT", value: "not-a-url")
            )
        }
    }

    private func temporaryHome(config: String?) throws -> URL {
        let homeDirectory = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        addTeardownBlock {
            try FileManager.default.removeItem(at: homeDirectory)
        }
        try FileManager.default.createDirectory(at: homeDirectory, withIntermediateDirectories: true)

        if let config {
            let configDirectory = homeDirectory.appendingPathComponent(
                ".config/lexinet",
                isDirectory: true
            )
            try FileManager.default.createDirectory(
                at: configDirectory,
                withIntermediateDirectories: true
            )
            try config.write(
                to: configDirectory.appendingPathComponent("config.toml"),
                atomically: true,
                encoding: .utf8
            )
        }

        return homeDirectory
    }
}
