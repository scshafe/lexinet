import Combine
import Darwin
import Foundation

/// Self-update for the menu-bar app: shows the checkout's commit hash, checks
/// `origin/main` for new commits on demand, and runs the repo-shipped
/// `clients/macos/update.sh` so the button and manual installs share one code
/// path. Button-triggered only — there is no timer.
@MainActor
final class UpdateManager: ObservableObject {
    enum Phase: Equatable {
        case idle
        case checking
        case upToDate
        case updateAvailable(commitsBehind: Int)
        case updating
        case restarting
        case updatedNeedsRelaunch
        case failed(String)
    }

    @Published private(set) var phase: Phase = .idle
    @Published private(set) var versionLabel = "…"

    /// Overridable so a satellite with a differently labeled LaunchAgent still
    /// restarts correctly: `LEXINET_LAUNCHAGENT_LABEL`.
    private let launchAgentLabel: String
    private let repoRoot: URL?

    init() {
        launchAgentLabel = ProcessInfo.processInfo.environment["LEXINET_LAUNCHAGENT_LABEL"] ?? "dev.lexinet.menubar"
        repoRoot = Self.discoverRepoRoot()
    }

    var isBusy: Bool {
        switch phase {
        case .checking, .updating, .restarting:
            return true
        case .idle, .upToDate, .updateAvailable, .updatedNeedsRelaunch, .failed:
            return false
        }
    }

    func refreshVersion() async {
        guard let repoRoot else {
            versionLabel = "unversioned"
            return
        }
        let result = await Self.run("/usr/bin/git", ["rev-parse", "--short", "HEAD"], in: repoRoot, timeout: 10)
        versionLabel = result.succeeded ? result.stdout.trimmingCharacters(in: .whitespacesAndNewlines) : "unversioned"
    }

    func check() async {
        guard !isBusy else { return }
        guard let repoRoot else {
            phase = .failed("No git checkout found for this install — set the repoPath default or LEXINET_REPO_PATH to the lexinet clone.")
            return
        }
        phase = .checking

        let fetch = await Self.run("/usr/bin/git", ["fetch", "origin"], in: repoRoot, timeout: 60)
        guard fetch.succeeded else {
            phase = .failed("Check failed: \(fetch.problem)")
            return
        }

        let count = await Self.run("/usr/bin/git", ["rev-list", "--count", "HEAD..origin/main"], in: repoRoot, timeout: 15)
        guard count.succeeded, let behind = Int(count.stdout.trimmingCharacters(in: .whitespacesAndNewlines)) else {
            phase = .failed("Check failed: \(count.problem)")
            return
        }

        await refreshVersion()
        phase = behind == 0 ? .upToDate : .updateAvailable(commitsBehind: behind)
    }

    func update() async {
        guard case .updateAvailable = phase else { return }
        guard let repoRoot else { return }
        phase = .updating

        let script = repoRoot.appendingPathComponent("clients/macos/update.sh").path
        let result = await Self.run("/bin/bash", [script, "--no-restart"], in: repoRoot, timeout: 900)
        guard result.succeeded else {
            phase = .failed("Update failed — current install untouched. \(result.problem)")
            return
        }

        await refreshVersion()

        // Only restart via launchd when THIS process is the LaunchAgent's; a
        // manually launched instance (source machine, swift run) is told to
        // relaunch by hand instead of stomping an unrelated service.
        if await launchdServicePID() == ProcessInfo.processInfo.processIdentifier {
            phase = .restarting
            let kickstart = Process()
            kickstart.executableURL = URL(fileURLWithPath: "/bin/launchctl")
            kickstart.arguments = ["kickstart", "-k", "gui/\(getuid())/\(launchAgentLabel)"]
            try? kickstart.run()
            // launchd should kill this process within moments; if we survive,
            // say so instead of spinning forever.
            try? await Task.sleep(nanoseconds: 6_000_000_000)
            phase = .failed("Updated, but the restart did not take — quit and relaunch the app manually.")
        } else {
            phase = .updatedNeedsRelaunch
        }
    }

    private func launchdServicePID() async -> Int32? {
        let result = await Self.run("/bin/launchctl", ["print", "gui/\(getuid())/\(launchAgentLabel)"], in: nil, timeout: 10)
        guard result.succeeded else { return nil }
        for line in result.stdout.split(separator: "\n") {
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            if trimmed.hasPrefix("pid = "), let pid = Int32(trimmed.dropFirst("pid = ".count)) {
                return pid
            }
        }
        return nil
    }

    /// The checkout this binary came from: LEXINET_REPO_PATH, then the
    /// `repoPath` user default (for binaries copied out of the checkout, e.g.
    /// ~/Applications installs), then walking up from the executable — which
    /// covers running out of a clone's `.build` tree on both machines.
    nonisolated private static func discoverRepoRoot() -> URL? {
        var candidates: [String] = []
        if let env = ProcessInfo.processInfo.environment["LEXINET_REPO_PATH"], !env.isEmpty {
            candidates.append(env)
        }
        if let stored = UserDefaults.standard.string(forKey: "repoPath"), !stored.isEmpty {
            candidates.append(stored)
        }
        for raw in candidates {
            let url = URL(fileURLWithPath: (raw as NSString).expandingTildeInPath)
            if FileManager.default.fileExists(atPath: url.appendingPathComponent(".git").path) {
                return url
            }
        }

        guard let executable = Bundle.main.executablePath else { return nil }
        var dir = URL(fileURLWithPath: executable).resolvingSymlinksInPath().deletingLastPathComponent()
        for _ in 0..<12 {
            if FileManager.default.fileExists(atPath: dir.appendingPathComponent(".git").path) {
                return dir
            }
            let parent = dir.deletingLastPathComponent()
            if parent.path == dir.path { break }
            dir = parent
        }
        return nil
    }

    struct ProcessOutput {
        let status: Int32
        let stdout: String
        let stderr: String
        let timedOut: Bool

        var succeeded: Bool { status == 0 && !timedOut }

        /// Short human-readable failure: last few meaningful lines.
        var problem: String {
            if timedOut { return "timed out" }
            let lines = (stderr + "\n" + stdout)
                .split(separator: "\n")
                .map { $0.trimmingCharacters(in: .whitespaces) }
                .filter { !$0.isEmpty }
            let tail = lines.suffix(3).joined(separator: " · ")
            return tail.isEmpty ? "exit \(status)" : tail
        }
    }

    nonisolated static func run(
        _ executable: String,
        _ arguments: [String],
        in directory: URL?,
        timeout: TimeInterval
    ) async -> ProcessOutput {
        await withCheckedContinuation { continuation in
            DispatchQueue.global(qos: .userInitiated).async {
                let process = Process()
                process.executableURL = URL(fileURLWithPath: executable)
                process.arguments = arguments
                if let directory { process.currentDirectoryURL = directory }

                // Never hang on interactive prompts from a GUI context; fail
                // fast and surface stderr instead.
                var environment = ProcessInfo.processInfo.environment
                environment["GIT_TERMINAL_PROMPT"] = "0"
                if environment["GIT_SSH_COMMAND"] == nil {
                    environment["GIT_SSH_COMMAND"] = "ssh -oBatchMode=yes"
                }
                process.environment = environment

                let stdoutPipe = Pipe()
                let stderrPipe = Pipe()
                process.standardOutput = stdoutPipe
                process.standardError = stderrPipe
                process.standardInput = FileHandle.nullDevice

                do {
                    try process.run()
                } catch {
                    continuation.resume(returning: ProcessOutput(
                        status: -1, stdout: "",
                        stderr: "failed to launch \(executable): \(error.localizedDescription)",
                        timedOut: false))
                    return
                }

                let timedOut = LockedFlag()
                let timeoutWork = DispatchWorkItem {
                    guard process.isRunning else { return }
                    timedOut.set()
                    process.terminate()
                    let pid = process.processIdentifier
                    DispatchQueue.global().asyncAfter(deadline: .now() + 5) {
                        if process.isRunning { kill(pid, SIGKILL) }
                    }
                }
                DispatchQueue.global().asyncAfter(deadline: .now() + timeout, execute: timeoutWork)

                // Drain both pipes concurrently so a full stderr buffer can
                // never deadlock a long stdout read (or vice versa).
                let drains = DispatchGroup()
                var stdoutData = Data()
                var stderrData = Data()
                drains.enter()
                DispatchQueue.global().async {
                    stdoutData = stdoutPipe.fileHandleForReading.readDataToEndOfFile()
                    drains.leave()
                }
                drains.enter()
                DispatchQueue.global().async {
                    stderrData = stderrPipe.fileHandleForReading.readDataToEndOfFile()
                    drains.leave()
                }

                process.waitUntilExit()
                drains.wait()
                timeoutWork.cancel()

                continuation.resume(returning: ProcessOutput(
                    status: process.terminationStatus,
                    stdout: String(data: stdoutData, encoding: .utf8) ?? "",
                    stderr: String(data: stderrData, encoding: .utf8) ?? "",
                    timedOut: timedOut.get()))
            }
        }
    }

    private final class LockedFlag {
        private var value = false
        private let lock = NSLock()

        func set() {
            lock.lock()
            value = true
            lock.unlock()
        }

        func get() -> Bool {
            lock.lock()
            defer { lock.unlock() }
            return value
        }
    }
}
