import AppKit
import SwiftUI
import LexinetClient

@main
struct LexinetMenuBarApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var appDelegate

    var body: some Scene {
        Settings {
            EmptyView()
        }
    }
}

final class AppDelegate: NSObject, NSApplicationDelegate {
    private var statusItem: NSStatusItem?
    private let popover = NSPopover()

    func applicationDidFinishLaunching(_ notification: Notification) {
        NSApp.setActivationPolicy(.accessory)

        let statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        statusItem.button?.image = NSImage(systemSymbolName: "text.magnifyingglass", accessibilityDescription: "lexinet")
        statusItem.button?.title = " Lexinet"
        statusItem.button?.target = self
        statusItem.button?.action = #selector(togglePopover)
        self.statusItem = statusItem

        popover.behavior = .transient
        popover.contentSize = NSSize(width: 420, height: 520)
        popover.contentViewController = NSHostingController(rootView: LookupView())
    }

    @objc private func togglePopover() {
        guard let button = statusItem?.button else { return }
        if popover.isShown {
            popover.performClose(nil)
        } else {
            popover.show(relativeTo: button.bounds, of: button, preferredEdge: .minY)
            popover.contentViewController?.view.window?.makeKey()
        }
    }
}

@MainActor
final class LookupViewModel: ObservableObject {
    enum State: Equatable {
        case idle
        case loading
        case unavailable(String)
        case failed(String)
        case loaded(results: [SearchResult], selected: IdeaDetail?)
    }

    @Published var query = ""
    @Published private(set) var state: State = .idle

    private let api: LexinetAPI

    init(api: LexinetAPI = LexinetAPI(baseURL: LookupViewModel.configuredEndpoint())) {
        self.api = api
    }

    /// The service endpoint, overridable for a shared/tailnet lexicon: the LEXINET_ENDPOINT
    /// environment variable wins, then the "endpoint" user default (`defaults write
    /// dev.lexinet.menubar endpoint http://host:17892`), then the local default.
    nonisolated static func configuredEndpoint() -> URL {
        let fallback = URL(string: "http://127.0.0.1:17892")!
        if let raw = ProcessInfo.processInfo.environment["LEXINET_ENDPOINT"],
           let url = URL(string: raw.trimmingCharacters(in: .whitespacesAndNewlines)), url.scheme != nil {
            return url
        }
        if let raw = UserDefaults.standard.string(forKey: "endpoint"),
           let url = URL(string: raw.trimmingCharacters(in: .whitespacesAndNewlines)), url.scheme != nil {
            return url
        }
        return fallback
    }

    func search() async {
        let trimmed = query.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else {
            state = .idle
            return
        }

        state = .loading
        do {
            let response = try await api.search(query: trimmed)
            let selected = try await firstDetail(from: response.results)
            state = .loaded(results: response.results, selected: selected)
        } catch let error as LexinetClientError {
            apply(error)
        } catch {
            state = .failed(error.localizedDescription)
        }
    }

    func select(_ result: SearchResult) async {
        do {
            let detail = try await api.ideaDetail(ref: result.idea.slug ?? result.idea.id)
            if case .loaded(let results, _) = state {
                state = .loaded(results: results, selected: detail)
            }
        } catch let error as LexinetClientError {
            apply(error)
        } catch {
            state = .failed(error.localizedDescription)
        }
    }

    private func firstDetail(from results: [SearchResult]) async throws -> IdeaDetail? {
        guard let first = results.first else { return nil }
        return try await api.ideaDetail(ref: first.idea.slug ?? first.idea.id)
    }

    private func apply(_ error: LexinetClientError) {
        switch error {
        case .serviceUnavailable(let message):
            state = .unavailable(message)
        case .nonSuccess(_, let message):
            state = .failed(message)
        case .invalidResponse:
            state = .failed(error.localizedDescription)
        }
    }
}

struct LookupView: View {
    @StateObject private var viewModel = LookupViewModel()

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            header
            searchBar
            content
            Spacer(minLength: 0)
        }
        .padding(18)
        .frame(width: 420, height: 520)
    }

    private var header: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text("lexinet")
                .font(.system(size: 26, weight: .semibold, design: .rounded))
            Text("Idea lookup from the menu bar")
                .font(.callout)
                .foregroundStyle(.secondary)
        }
    }

    private var searchBar: some View {
        HStack {
            TextField("Search ideas", text: $viewModel.query)
                .textFieldStyle(.roundedBorder)
                .onSubmit { Task { await viewModel.search() } }
            Button("Search") {
                Task { await viewModel.search() }
            }
            .keyboardShortcut(.return, modifiers: [])
        }
    }

    @ViewBuilder
    private var content: some View {
        switch viewModel.state {
        case .idle:
            hint("Type a term to search canonical names, aliases, definitions, examples, and notes.")
        case .loading:
            ProgressView("Searching...")
                .frame(maxWidth: .infinity, alignment: .center)
        case .unavailable(let message):
            serviceUnavailable(message)
        case .failed(let message):
            errorState(message)
        case .loaded(let results, let selected):
            if results.isEmpty {
                hint("No matching ideas found.")
            } else {
                resultsAndDetail(results: results, selected: selected)
            }
        }
    }

    private func resultsAndDetail(results: [SearchResult], selected: IdeaDetail?) -> some View {
        HStack(alignment: .top, spacing: 12) {
            List(results) { result in
                Button {
                    Task { await viewModel.select(result) }
                } label: {
                    VStack(alignment: .leading, spacing: 3) {
                        Text(result.idea.canonicalName ?? result.idea.slug ?? result.idea.id)
                            .font(.headline)
                        if let summary = result.idea.summary {
                            Text(summary)
                                .lineLimit(2)
                                .font(.caption)
                                .foregroundStyle(.secondary)
                        }
                    }
                }
                .buttonStyle(.plain)
            }
            .frame(width: 150)

            Divider()

            ScrollView {
                if let selected {
                    detail(selected)
                } else {
                    hint("Select a result to see names, definition, topics, and acronym links.")
                }
            }
        }
    }

    private func detail(_ idea: IdeaDetail) -> some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(idea.displayName)
                .font(.title3.bold())
            if let definition = idea.primaryDefinition {
                section("Definition") {
                    Text(definition)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }
            if !idea.names.isEmpty {
                section("Names") {
                    FlowText(idea.names.map { "\($0.displayText) (\($0.kind))" })
                }
            }
            if !idea.topics.isEmpty {
                section("Topics") {
                    FlowText(idea.topics.map(\.displayName))
                }
            }
            if !idea.acronymLinks.isEmpty {
                section("Acronym Links") {
                    VStack(alignment: .leading, spacing: 5) {
                        ForEach(Array(idea.acronymLinks.enumerated()), id: \.offset) { _, link in
                            Text(link.summary)
                        }
                    }
                }
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func section<Content: View>(_ title: String, @ViewBuilder content: () -> Content) -> some View {
        VStack(alignment: .leading, spacing: 5) {
            Text(title.uppercased())
                .font(.caption.weight(.semibold))
                .foregroundStyle(.secondary)
            content()
        }
    }

    private func hint(_ message: String) -> some View {
        Text(message)
            .foregroundStyle(.secondary)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.top, 8)
    }

    private func serviceUnavailable(_ message: String) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("Service unavailable")
                .font(.headline)
            Text(message)
            Text("Start hint: run `lexinet serve --tailnet off`, then search again.")
                .font(.callout)
                .foregroundStyle(.secondary)
        }
        .padding(12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Color.orange.opacity(0.12), in: RoundedRectangle(cornerRadius: 10))
    }

    private func errorState(_ message: String) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("Lookup failed")
                .font(.headline)
            Text(message)
            Text("The client kept running; adjust the query or check the service response.")
                .font(.callout)
                .foregroundStyle(.secondary)
        }
        .padding(12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Color.red.opacity(0.10), in: RoundedRectangle(cornerRadius: 10))
    }
}

struct FlowText: View {
    let values: [String]

    init(_ values: [String]) {
        self.values = values
    }

    var body: some View {
        Text(values.joined(separator: "  |  "))
            .font(.callout)
            .fixedSize(horizontal: false, vertical: true)
    }
}
