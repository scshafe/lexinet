package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"lexinet/internal/model"
)

func newIdeaCommand(opts *Options) *cobra.Command {
	cmd := &cobra.Command{Use: "idea", Short: "Manage ideas"}
	cmd.AddCommand(newIdeaAddCommand(opts), newIdeaGetCommand(opts), newIdeaListCommand(opts), newIdeaEditCommand(opts), newIdeaRemoveCommand(opts), newIdeaMergeCommand(opts))
	return cmd
}

func newIdeaAddCommand(opts *Options) *cobra.Command {
	var slug, summary, domain string
	var aliases, definitions, examples, notes []string
	cmd := &cobra.Command{
		Use:   "add <canonical-name>",
		Short: "Create an idea",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			input := model.CreateIdeaInput{
				Slug:    stringPtrIf(slug),
				Summary: stringPtrIf(summary),
				Domain:  stringPtrIf(domain),
				Names: []model.CreateNameInput{{
					Text: args[0],
					Kind: model.NameKindCanonical,
				}},
			}
			for _, alias := range aliases {
				input.Names = append(input.Names, model.CreateNameInput{Text: alias, Kind: model.NameKindAlias})
			}
			for _, value := range definitions {
				body, err := bodyFromReader(value, cmd.InOrStdin())
				if err != nil {
					return err
				}
				input.Definitions = append(input.Definitions, model.CreateDefinitionInput{Body: body})
			}
			for _, value := range examples {
				body, err := bodyFromReader(value, cmd.InOrStdin())
				if err != nil {
					return err
				}
				input.Examples = append(input.Examples, model.CreateExampleInput{Body: body})
			}
			for _, value := range notes {
				body, err := bodyFromReader(value, cmd.InOrStdin())
				if err != nil {
					return err
				}
				input.Notes = append(input.Notes, model.CreateNoteInput{Body: body})
			}
			var out json.RawMessage
			if err := clientDo(cmd.Context(), opts, http.MethodPost, "/ideas", input, &out); err != nil {
				return err
			}
			return outputRaw(cmd, opts, out)
		},
	}
	cmd.Flags().StringVar(&slug, "slug", "", "stable idea slug")
	cmd.Flags().StringVar(&summary, "summary", "", "one-line summary")
	cmd.Flags().StringVar(&domain, "domain", "", "idea domain")
	cmd.Flags().StringArrayVar(&aliases, "alias", nil, "alias name (repeatable)")
	cmd.Flags().StringArrayVar(&definitions, "definition", nil, "definition body or - for stdin (repeatable)")
	cmd.Flags().StringArrayVar(&examples, "example", nil, "example body or - for stdin (repeatable)")
	cmd.Flags().StringArrayVar(&notes, "note", nil, "note body or - for stdin (repeatable)")
	return cmd
}

func newIdeaGetCommand(opts *Options) *cobra.Command {
	var expands []string
	var noRelations bool
	cmd := &cobra.Command{
		Use:   "get <ref>",
		Short: "Fetch an idea",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(expands) == 0 {
				expands = []string{"definitions", "examples", "notes", "relationships"}
			}
			if noRelations {
				expands = removeValue(expands, "relationships")
			}
			q := url.Values{}
			if len(expands) > 0 {
				q.Set("expand", strings.Join(expands, ","))
			}
			return clientGetRaw(cmd, opts, apiPath("/ideas/{}", args[0]), q)
		},
	}
	cmd.Flags().StringArrayVar(&expands, "expand", nil, "expand definitions, examples, notes, relationships (repeatable or comma-separated)")
	cmd.Flags().BoolVar(&noRelations, "no-relations", false, "exclude relationship expansion")
	return cmd
}

func newIdeaListCommand(opts *Options) *cobra.Command {
	var q, slug string
	var limit, offset int
	var jsonl bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List ideas",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			query := url.Values{}
			setQuery(query, "q", q)
			setQuery(query, "slug", slug)
			setQueryInt(query, "limit", limit)
			setQueryInt(query, "offset", offset)
			var out json.RawMessage
			if err := clientDo(cmd.Context(), opts, http.MethodGet, pathWithQuery("/ideas", query), nil, &out); err != nil {
				return err
			}
			if jsonl {
				return outputJSONLField(cmd, out, "ideas")
			}
			return outputRaw(cmd, opts, out)
		},
	}
	cmd.Flags().StringVar(&q, "q", "", "filter by query")
	cmd.Flags().StringVar(&slug, "slug", "", "filter by slug")
	cmd.Flags().IntVar(&limit, "limit", 20, "result limit")
	cmd.Flags().IntVar(&offset, "offset", 0, "result offset")
	cmd.Flags().BoolVar(&jsonl, "jsonl", false, "emit ideas as JSON Lines")
	return cmd
}

func newIdeaEditCommand(opts *Options) *cobra.Command {
	var slug, summary, domain, status string
	cmd := &cobra.Command{
		Use:   "edit <ref>",
		Short: "Update idea metadata",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			patch := map[string]any{}
			if cmd.Flags().Changed("slug") {
				patch["slug"] = slug
			}
			if cmd.Flags().Changed("summary") {
				patch["summary"] = summary
			}
			if cmd.Flags().Changed("domain") {
				patch["domain"] = domain
			}
			if cmd.Flags().Changed("status") {
				ideaStatus := model.IdeaStatus(status)
				if !ideaStatus.Valid() {
					return usageError("invalid status %q", status)
				}
				patch["status"] = status
			}
			if len(patch) == 0 {
				return usageError("at least one field flag is required")
			}
			var out json.RawMessage
			if err := clientDo(cmd.Context(), opts, http.MethodPatch, apiPath("/ideas/{}", args[0]), patch, &out); err != nil {
				return err
			}
			return outputRaw(cmd, opts, out)
		},
	}
	cmd.Flags().StringVar(&slug, "slug", "", "new slug")
	cmd.Flags().StringVar(&summary, "summary", "", "new summary")
	cmd.Flags().StringVar(&domain, "domain", "", "new domain")
	cmd.Flags().StringVar(&status, "status", "", "new status")
	return cmd
}

func newIdeaRemoveCommand(opts *Options) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:     "rm <ref>",
		Aliases: []string{"remove", "delete"},
		Short:   "Delete an idea",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireDestructiveConfirmation(yes, "idea rm requires --yes when stdin is not a TTY"); err != nil {
				return err
			}
			if err := clientDo(cmd.Context(), opts, http.MethodDelete, apiPath("/ideas/{}", args[0]), nil, nil); err != nil {
				return err
			}
			return outputValue(cmd, opts, map[string]any{"ok": true, "ref": args[0]})
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "confirm deletion")
	return cmd
}

func newIdeaMergeCommand(opts *Options) *cobra.Command {
	var into string
	var yes bool
	cmd := &cobra.Command{
		Use:   "merge <ref> --into <ref>",
		Short: "Merge one idea into another",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(into) == "" {
				return usageError("--into is required")
			}
			if err := requireDestructiveConfirmation(yes, "idea merge requires --yes when stdin is not a TTY"); err != nil {
				return err
			}
			var out json.RawMessage
			if err := clientDo(cmd.Context(), opts, http.MethodPost, apiPath("/ideas/{}/merge", args[0]), map[string]string{"into": into}, &out); err != nil {
				return err
			}
			return outputRaw(cmd, opts, out)
		},
	}
	cmd.Flags().StringVar(&into, "into", "", "surviving idea ref")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "confirm merge")
	return cmd
}

func newNameCommand(opts *Options) *cobra.Command {
	cmd := &cobra.Command{Use: "name", Short: "Manage idea names"}
	cmd.AddCommand(newNameAddCommand(opts), newNameRemoveCommand(opts), newNamePromoteCommand(opts), newNameListCommand(opts))
	return cmd
}

func newNameAddCommand(opts *Options) *cobra.Command {
	var kind, display, expansion, lang, note string
	var sortKey float64
	cmd := &cobra.Command{
		Use:   "add <idea-ref> <text>",
		Short: "Add a name",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			nameKind := model.NameKind(kind)
			if !nameKind.Valid() {
				return usageError("invalid name kind %q (valid: %s)", kind, joinNameKinds())
			}
			input := model.CreateNameInput{Text: args[1], Kind: nameKind, Display: stringPtrIf(display), Expansion: stringPtrIf(expansion), LangTag: stringPtrIf(lang), DeprecationNote: stringPtrIf(note), SortKey: sortKey}
			var out json.RawMessage
			if err := clientDo(cmd.Context(), opts, http.MethodPost, apiPath("/ideas/{}/names", args[0]), input, &out); err != nil {
				return err
			}
			return outputRaw(cmd, opts, out)
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "alias", "name kind")
	cmd.Flags().StringVar(&display, "display", "", "display form")
	cmd.Flags().StringVar(&expansion, "expansion", "", "acronym expansion")
	cmd.Flags().StringVar(&lang, "lang", "", "localized language tag")
	cmd.Flags().StringVar(&note, "note", "", "deprecation note")
	cmd.Flags().Float64Var(&sortKey, "sort", 0, "sort key")
	return cmd
}

func newNameRemoveCommand(opts *Options) *cobra.Command {
	return deleteByIDCommand(opts, "rm <id>", "Remove a name", "/names/{}")
}

func newNamePromoteCommand(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "promote <idea-ref> <text>",
		Short: "Promote a name to canonical",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var out json.RawMessage
			if err := clientDo(cmd.Context(), opts, http.MethodPost, apiPath("/ideas/{}/names/promote", args[0]), map[string]string{"text": args[1]}, &out); err != nil {
				return err
			}
			return outputRaw(cmd, opts, out)
		},
	}
}

func newNameListCommand(opts *Options) *cobra.Command {
	var kind string
	cmd := &cobra.Command{
		Use:   "list <idea-ref>",
		Short: "List names for an idea",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var response struct {
				Names []model.Name `json:"names"`
			}
			if err := clientDo(cmd.Context(), opts, http.MethodGet, apiPath("/ideas/{}/names", args[0]), nil, &response); err != nil {
				return err
			}
			if kind != "" {
				nameKind := model.NameKind(kind)
				if !nameKind.Valid() {
					return usageError("invalid name kind %q (valid: %s)", kind, joinNameKinds())
				}
				filtered := response.Names[:0]
				for _, name := range response.Names {
					if name.Kind == nameKind {
						filtered = append(filtered, name)
					}
				}
				response.Names = filtered
			}
			return outputValue(cmd, opts, response)
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "", "filter by name kind")
	return cmd
}

func newBodyCommand(opts *Options, group string) *cobra.Command {
	plural := group + "s"
	if group == "def" {
		plural = "definitions"
	}
	cmd := &cobra.Command{Use: group, Short: "Manage " + plural}
	cmd.AddCommand(newBodyAddCommand(opts, group, plural), deleteByIDCommand(opts, "rm <id>", "Remove a "+group, "/"+plural+"/{}"), newBodyListCommand(opts, group, plural))
	return cmd
}

func newBodyAddCommand(opts *Options, group, plural string) *cobra.Command {
	var register, source, lang, caption, kind string
	var primary bool
	var sortKey float64
	cmd := &cobra.Command{
		Use:   "add <idea-ref> <text|->",
		Short: "Add a " + group,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := bodyFromReader(args[1], cmd.InOrStdin())
			if err != nil {
				return err
			}
			var input any
			switch group {
			case "def":
				input = model.CreateDefinitionInput{Body: body, Register: stringPtrIf(register), Source: stringPtrIf(source), IsPrimary: primary, SortKey: sortKey}
			case "example":
				input = model.CreateExampleInput{Body: body, Lang: stringPtrIf(lang), Caption: stringPtrIf(caption), SortKey: sortKey}
			case "note":
				input = model.CreateNoteInput{Body: body, Kind: stringPtrIf(kind), SortKey: sortKey}
			}
			var out json.RawMessage
			if err := clientDo(cmd.Context(), opts, http.MethodPost, apiPath("/ideas/{}/"+plural, args[0]), input, &out); err != nil {
				return err
			}
			return outputRaw(cmd, opts, out)
		},
	}
	cmd.Flags().Float64Var(&sortKey, "sort", 0, "sort key")
	switch group {
	case "def":
		cmd.Flags().StringVar(&register, "register", "", "definition register")
		cmd.Flags().StringVar(&register, "kind", "", "alias for --register")
		cmd.Flags().StringVar(&source, "source", "", "definition source")
		cmd.Flags().BoolVar(&primary, "primary", false, "mark as primary definition")
	case "example":
		cmd.Flags().StringVar(&lang, "lang", "", "example language")
		cmd.Flags().StringVar(&caption, "caption", "", "example caption")
	case "note":
		cmd.Flags().StringVar(&kind, "kind", "", "note kind")
	}
	return cmd
}

func newBodyListCommand(opts *Options, group, plural string) *cobra.Command {
	return &cobra.Command{
		Use:   "list <idea-ref>",
		Short: "List " + plural,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return clientGetRaw(cmd, opts, apiPath("/ideas/{}/"+plural, args[0]), nil)
		},
	}
}

func newLinkCommand(opts *Options) *cobra.Command {
	cmd := &cobra.Command{Use: "link", Short: "Manage relationship links"}
	cmd.AddCommand(newLinkAddCommand(opts), deleteByIDCommand(opts, "rm <id>", "Remove a relationship", "/relationships/{}"), newLinkListCommand(opts))
	return cmd
}

func newLinkAddCommand(opts *Options) *cobra.Command {
	var note string
	cmd := &cobra.Command{
		Use:   "add <from-ref> <relation> <to-ref>",
		Short: "Create or update a relationship",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind := model.RelationKind(args[1])
			if _, err := model.ParseRelationKind(kind); err != nil {
				return usageError("invalid relation %q (valid: %s)", args[1], joinRelationKinds())
			}
			input := model.CreateRelationshipInput{From: args[0], Kind: kind, To: args[2], Note: stringPtrIf(note)}
			var out json.RawMessage
			if err := clientDo(cmd.Context(), opts, http.MethodPost, "/relationships", input, &out); err != nil {
				return err
			}
			return outputRaw(cmd, opts, out)
		},
	}
	cmd.Flags().StringVar(&note, "note", "", "relationship note")
	return cmd
}

func newLinkListCommand(opts *Options) *cobra.Command {
	var relation, direction string
	cmd := &cobra.Command{
		Use:   "list <ref>",
		Short: "List relationships for an idea",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query, err := relationListQuery(args[0], relation, direction)
			if err != nil {
				return err
			}
			return clientGetRaw(cmd, opts, "/relationships", query)
		},
	}
	cmd.Flags().StringVar(&relation, "relation", "", "relationship kind")
	cmd.Flags().StringVar(&direction, "direction", "both", "out, in, or both")
	return cmd
}

func newTraverseCommand(opts *Options) *cobra.Command {
	var relations []string
	var direction, format string
	var depth int
	var jsonl bool
	cmd := &cobra.Command{
		Use:   "traverse <ref>",
		Short: "Traverse relationship graph",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if depth > model.MaxCLITraversalDepth {
				return usageError("--depth must be <= %d", model.MaxCLITraversalDepth)
			}
			if format != "tree" && format != "edges" && format != "ids" {
				return usageError("invalid --format %q", format)
			}
			query := url.Values{}
			setQueryInt(query, "depth", depth)
			setQuery(query, "direction", direction)
			for _, relation := range splitCSV(relations) {
				if _, err := model.ParseRelationKind(model.RelationKind(relation)); err != nil {
					return usageError("invalid relation %q (valid: %s)", relation, joinRelationKinds())
				}
				query.Add("kind", relation)
			}
			var out json.RawMessage
			if err := clientDo(cmd.Context(), opts, http.MethodGet, pathWithQuery(apiPath("/ideas/{}/graph", args[0]), query), nil, &out); err != nil {
				return err
			}
			if jsonl {
				return outputJSONLField(cmd, out, "relationships")
			}
			return outputRaw(cmd, opts, out)
		},
	}
	cmd.Flags().StringArrayVar(&relations, "relation", nil, "relationship kind (repeatable or comma-separated)")
	cmd.Flags().StringVar(&direction, "direction", "both", "out, in, or both")
	cmd.Flags().IntVar(&depth, "depth", model.DefaultTraversalDepth, "traversal depth")
	cmd.Flags().StringVar(&format, "format", "edges", "tree, edges, or ids")
	cmd.Flags().BoolVar(&jsonl, "jsonl", false, "emit relationships as JSON Lines")
	return cmd
}

func newSearchCommand(opts *Options) *cobra.Command {
	var fuzzy, jsonl bool
	var scope, nameKind string
	var threshold, minScore float64
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "search <q>",
		Short: "Search ideas",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := url.Values{}
			query.Set("q", args[0])
			if scope != "" {
				query.Set("scope", scope)
			}
			if fuzzy {
				query.Set("fuzzy", "true")
			}
			if threshold != 0 {
				query.Set("threshold", fmt.Sprintf("%g", threshold))
			}
			if minScore != 0 {
				query.Set("threshold", fmt.Sprintf("%g", minScore))
			}
			if nameKind != "" {
				if !model.NameKind(nameKind).Valid() {
					return usageError("invalid name kind %q (valid: %s)", nameKind, joinNameKinds())
				}
				query.Add("kind", nameKind)
			}
			setQueryInt(query, "limit", limit)
			setQueryInt(query, "offset", offset)
			var out json.RawMessage
			if err := clientDo(cmd.Context(), opts, http.MethodGet, pathWithQuery("/search", query), nil, &out); err != nil {
				return err
			}
			if jsonl {
				return outputJSONLField(cmd, out, "results")
			}
			return outputRaw(cmd, opts, out)
		},
	}
	cmd.Flags().BoolVar(&fuzzy, "fuzzy", false, "enable fuzzy matching")
	cmd.Flags().StringVar(&scope, "scope", "", "names or all")
	cmd.Flags().StringVar(&nameKind, "name-kind", "", "filter by name kind")
	cmd.Flags().Float64Var(&threshold, "threshold", 0, "fuzzy threshold")
	cmd.Flags().Float64Var(&minScore, "min-score", 0, "alias for --threshold")
	cmd.Flags().IntVar(&limit, "limit", 20, "result limit")
	cmd.Flags().IntVar(&offset, "offset", 0, "result offset")
	cmd.Flags().BoolVar(&jsonl, "jsonl", false, "emit results as JSON Lines")
	return cmd
}

func deleteByIDCommand(opts *Options, use, short, pattern string) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireDestructiveConfirmation(yes, strings.Split(use, " ")[0]+" requires --yes when stdin is not a TTY"); err != nil {
				return err
			}
			if err := clientDo(cmd.Context(), opts, http.MethodDelete, apiPath(pattern, args[0]), nil, nil); err != nil {
				return err
			}
			return outputValue(cmd, opts, map[string]any{"ok": true, "id": args[0]})
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "confirm deletion")
	return cmd
}

func clientGetRaw(cmd *cobra.Command, opts *Options, path string, query url.Values) error {
	var out json.RawMessage
	if err := clientDo(cmd.Context(), opts, http.MethodGet, pathWithQuery(path, query), nil, &out); err != nil {
		return err
	}
	return outputRaw(cmd, opts, out)
}

func clientDo(ctx context.Context, opts *Options, method, path string, in, out any) error {
	endpoint, token, err := endpointAndToken(opts)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return doJSON(ctx, method, endpoint, path, token, in, out)
}

func outputRaw(cmd *cobra.Command, opts *Options, raw json.RawMessage) error {
	if opts.Quiet {
		if id := rawID(raw); id != "" {
			fmt.Fprintln(cmd.OutOrStdout(), id)
			return nil
		}
	}
	if opts.Compact {
		_, err := cmd.OutOrStdout().Write(append(bytesClone(raw), '\n'))
		return err
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	return writeJSON(cmd.OutOrStdout(), value)
}

func outputValue(cmd *cobra.Command, opts *Options, value any) error {
	if opts.Quiet {
		if id := valueID(value); id != "" {
			fmt.Fprintln(cmd.OutOrStdout(), id)
			return nil
		}
	}
	if opts.Compact {
		enc := json.NewEncoder(cmd.OutOrStdout())
		return enc.Encode(value)
	}
	return writeJSON(cmd.OutOrStdout(), value)
}

func outputJSONLField(cmd *cobra.Command, raw json.RawMessage, field string) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return err
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(obj[field], &rows); err != nil {
		return err
	}
	for _, row := range rows {
		if _, err := cmd.OutOrStdout().Write(append(bytesClone(row), '\n')); err != nil {
			return err
		}
	}
	return nil
}

func relationListQuery(ref, relation, direction string) (url.Values, error) {
	query := url.Values{"idea": []string{ref}}
	if relation != "" {
		if _, err := model.ParseRelationKind(model.RelationKind(relation)); err != nil {
			return nil, usageError("invalid relation %q (valid: %s)", relation, joinRelationKinds())
		}
		query.Set("kind", relation)
	}
	if direction != "" {
		if _, err := model.NormalizeRelationDirection(model.RelationDirection(direction), model.RelationDirectionBoth); err != nil {
			return nil, usageError("invalid direction %q", direction)
		}
		query.Set("direction", direction)
	}
	return query, nil
}

func requireDestructiveConfirmation(yes bool, message string) error {
	if yes || stdinIsTerminal() {
		return nil
	}
	return usageError(message)
}

func usageError(format string, args ...any) error {
	return &ExitError{Code: ExitUsage, Err: fmt.Errorf(format, args...)}
}

func stringPtrIf(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
}

func pathWithQuery(path string, query url.Values) string {
	if len(query) == 0 {
		return path
	}
	return path + "?" + query.Encode()
}

func setQuery(query url.Values, key, value string) {
	if value != "" {
		query.Set(key, value)
	}
}

func setQueryInt(query url.Values, key string, value int) {
	if value != 0 {
		query.Set(key, fmt.Sprintf("%d", value))
	}
}

func splitCSV(values []string) []string {
	var out []string
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

func removeValue(values []string, remove string) []string {
	parts := splitCSV(values)
	out := parts[:0]
	for _, value := range parts {
		if value != remove {
			out = append(out, value)
		}
	}
	return out
}

func joinRelationKinds() string {
	values := model.AcceptedRelationLabels()
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, string(value))
	}
	return strings.Join(parts, ", ")
}

func joinNameKinds() string {
	values := model.ValidNameKinds()
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, string(value))
	}
	return strings.Join(parts, ", ")
}

func rawID(raw json.RawMessage) string {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	if id, ok := obj["id"].(string); ok {
		return id
	}
	return ""
}

func valueID(value any) string {
	if obj, ok := value.(map[string]any); ok {
		if id, ok := obj["id"].(string); ok {
			return id
		}
	}
	return ""
}

func bytesClone(in []byte) []byte {
	out := make([]byte, len(in))
	copy(out, in)
	return out
}
