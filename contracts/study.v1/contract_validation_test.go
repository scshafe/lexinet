package studyv1contract_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const openAPIResource = "study.v1.openapi.json"

type contractManifest struct {
	ContractID        string            `json:"contract_id"`
	SchemaVersion     int               `json:"schema_version"`
	OpenAPIPath       string            `json:"openapi_path"`
	RequiredSemantics []string          `json:"required_semantics"`
	Examples          []manifestExample `json:"examples"`
}

type manifestExample struct {
	Path     string `json:"path"`
	Schema   string `json:"schema"`
	Expected string `json:"expected"`
}

func TestStudyV1CanonicalRequestAndResponseSchemasCompile(t *testing.T) {
	directory := contractDirectory(t)
	manifest := loadManifest(t, directory)
	compiler := loadCompiler(t, filepath.Join(directory, manifest.OpenAPIPath))

	for _, component := range []string{
		"CreateSessionRequest",
		"CreateSessionResource",
		"SessionResource",
		"SyncRequest",
		"SyncResource",
		"ErrorEnvelope",
	} {
		if _, err := compiler.Compile(openAPIResource + "#/components/schemas/" + component); err != nil {
			t.Fatalf("compile canonical schema %q: %v", component, err)
		}
	}
}

func TestStudyV1GoldenExamplesValidateAgainstCanonicalSchemas(t *testing.T) {
	directory := contractDirectory(t)
	manifest := loadManifest(t, directory)
	compiler := loadCompiler(t, filepath.Join(directory, manifest.OpenAPIPath))

	for _, example := range manifest.Examples {
		if example.Expected != "valid" {
			continue
		}

		schema, err := compiler.Compile(openAPIResource + "#/components/schemas/" + example.Schema)
		if err != nil {
			t.Fatalf("compile schema %q for %s: %v", example.Schema, example.Path, err)
		}
		instance := loadJSONDocument(t, filepath.Join(directory, example.Path))
		if err := schema.Validate(instance); err != nil {
			t.Fatalf("validate golden example %s: %v", example.Path, err)
		}
	}
}

func TestStudyV1RejectsUnknownRequiredSemantic(t *testing.T) {
	directory := contractDirectory(t)
	manifest := loadManifest(t, directory)
	compiler := loadCompiler(t, filepath.Join(directory, manifest.OpenAPIPath))

	for _, example := range manifest.Examples {
		if example.Expected != "invalid" {
			continue
		}

		schema, err := compiler.Compile(openAPIResource + "#/components/schemas/" + example.Schema)
		if err != nil {
			t.Fatalf("compile schema %q for %s: %v", example.Schema, example.Path, err)
		}
		instance := loadJSONDocument(t, filepath.Join(directory, example.Path))
		if err := schema.Validate(instance); err == nil {
			t.Fatalf("invalid contract example %s unexpectedly validated", example.Path)
		}
		object, ok := instance.(map[string]any)
		if !ok {
			t.Fatalf("invalid contract example %s is not an object", example.Path)
		}
		knownSemantics := make([]any, 0, len(manifest.RequiredSemantics))
		for _, semantic := range manifest.RequiredSemantics {
			knownSemantics = append(knownSemantics, semantic)
		}
		object["required_semantics"] = knownSemantics
		if err := schema.Validate(object); err != nil {
			t.Fatalf("invalid contract example %s has an invalidity other than its unknown required semantic: %v", example.Path, err)
		}
		return
	}

	t.Fatal("manifest does not identify an invalid unknown-required-semantic example")
}

func TestStudyV1RejectsUnknownRequestFields(t *testing.T) {
	directory := contractDirectory(t)
	manifest := loadManifest(t, directory)
	compiler := loadCompiler(t, filepath.Join(directory, manifest.OpenAPIPath))
	schema, err := compiler.Compile(openAPIResource + "#/components/schemas/CreateSessionRequest")
	if err != nil {
		t.Fatalf("compile create request schema: %v", err)
	}
	request := map[string]any{
		"protocol_version":    "study.v1",
		"schema_version":      json.Number("1"),
		"creation_request_id": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		"profile_ref":         "fixture-default",
		"scope": map[string]any{
			"topic_refs": []any{"fixture-systems"},
			"card_kind":  "idea",
		},
		"page": map[string]any{
			"after_position": json.Number("-1"),
			"limit":          json.Number("16"),
		},
		"future_required_behavior": true,
	}
	if err := schema.Validate(request); err == nil {
		t.Fatal("create request with an unknown field unexpectedly validated")
	}
	delete(request, "future_required_behavior")
	if err := schema.Validate(request); err != nil {
		t.Fatalf("create request remains invalid after removing only the unknown field: %v", err)
	}
}

func TestStudyV1AcceptsOptionalResponseAdditions(t *testing.T) {
	directory := contractDirectory(t)
	manifest := loadManifest(t, directory)
	compiler := loadCompiler(t, filepath.Join(directory, manifest.OpenAPIPath))
	schema, err := compiler.Compile(openAPIResource + "#/components/schemas/SessionResource")
	if err != nil {
		t.Fatalf("compile session resource schema: %v", err)
	}
	instance := loadJSONDocument(t, filepath.Join(directory, "examples/session-created.json"))
	object, ok := instance.(map[string]any)
	if !ok {
		t.Fatal("session golden example is not an object")
	}
	object["future_optional_observation"] = map[string]any{"opaque": true}
	if err := schema.Validate(object); err != nil {
		t.Fatalf("optional response addition unexpectedly rejected: %v", err)
	}
}

func TestStudyV1ManifestMatchesCanonicalIdentity(t *testing.T) {
	directory := contractDirectory(t)
	manifest := loadManifest(t, directory)
	document := loadJSONObject(t, filepath.Join(directory, manifest.OpenAPIPath))
	extension := requiredObject(t, document, "x-lexinet-contract")

	if manifest.ContractID != "study.v1" || manifest.SchemaVersion != 1 {
		t.Fatalf("unexpected manifest identity: contract_id=%q schema_version=%d", manifest.ContractID, manifest.SchemaVersion)
	}
	if canonical, _ := extension["canonical"].(string); canonical != manifest.ContractID {
		t.Fatalf("OpenAPI canonical identity %q does not match manifest %q", canonical, manifest.ContractID)
	}

	extensionSemantics := stringSlice(t, extension["required_semantics"], "x-lexinet-contract.required_semantics")
	if !reflect.DeepEqual(extensionSemantics, manifest.RequiredSemantics) {
		t.Fatalf("required semantics diverge between manifest and OpenAPI: manifest=%v openapi=%v", manifest.RequiredSemantics, extensionSemantics)
	}
}

func TestStudyV1UsesRawSuccessResources(t *testing.T) {
	directory := contractDirectory(t)
	manifest := loadManifest(t, directory)
	document := loadJSONObject(t, filepath.Join(directory, manifest.OpenAPIPath))
	paths := requiredObject(t, document, "paths")

	wantSuccessRefs := map[string]string{
		"POST /api/v1/study/sessions 200":                   "#/components/schemas/CreateSessionResource",
		"POST /api/v1/study/sessions 201":                   "#/components/schemas/CreateSessionResource",
		"GET /api/v1/study/sessions/{session_id} 200":       "#/components/schemas/SessionResource",
		"POST /api/v1/study/sessions/{session_id}/sync 200": "#/components/schemas/SyncResource",
	}
	gotSuccessRefs := map[string]string{
		"POST /api/v1/study/sessions 200":                   responseSchemaRef(t, paths, "/api/v1/study/sessions", "post", "200"),
		"POST /api/v1/study/sessions 201":                   responseSchemaRef(t, paths, "/api/v1/study/sessions", "post", "201"),
		"GET /api/v1/study/sessions/{session_id} 200":       responseSchemaRef(t, paths, "/api/v1/study/sessions/{session_id}", "get", "200"),
		"POST /api/v1/study/sessions/{session_id}/sync 200": responseSchemaRef(t, paths, "/api/v1/study/sessions/{session_id}/sync", "post", "200"),
	}
	if !reflect.DeepEqual(gotSuccessRefs, wantSuccessRefs) {
		t.Fatalf("success resources are not raw canonical resources: got=%v want=%v", gotSuccessRefs, wantSuccessRefs)
	}
}

func TestStudyV1PreservesExistingErrorEnvelope(t *testing.T) {
	directory := contractDirectory(t)
	manifest := loadManifest(t, directory)
	document := loadJSONObject(t, filepath.Join(directory, manifest.OpenAPIPath))
	components := requiredObject(t, document, "components")
	schemas := requiredObject(t, components, "schemas")
	errorEnvelope := requiredObject(t, schemas, "ErrorEnvelope")
	wantRequired := []string{"error"}
	if gotRequired := stringSlice(t, errorEnvelope["required"], "ErrorEnvelope.required"); !reflect.DeepEqual(gotRequired, wantRequired) {
		t.Fatalf("error envelope must preserve the top-level error member: got=%v want=%v", gotRequired, wantRequired)
	}
	properties := requiredObject(t, errorEnvelope, "properties")
	if _, exists := properties["error"]; !exists || len(properties) != 1 {
		t.Fatalf("error envelope properties changed: %v", properties)
	}
}

func contractDirectory(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve contract test source path")
	}
	return filepath.Dir(filename)
}

func loadManifest(t *testing.T, directory string) contractManifest {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if err != nil {
		t.Fatalf("read contract manifest: %v", err)
	}

	var manifest contractManifest
	if err := json.Unmarshal(contents, &manifest); err != nil {
		t.Fatalf("decode contract manifest: %v", err)
	}
	return manifest
}

func loadCompiler(t *testing.T, openAPIPath string) *jsonschema.Compiler {
	t.Helper()
	document := loadJSONDocument(t, openAPIPath)
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	if err := compiler.AddResource(openAPIResource, document); err != nil {
		t.Fatalf("register canonical OpenAPI document: %v", err)
	}
	return compiler
}

func loadJSONDocument(t *testing.T, path string) any {
	t.Helper()
	input, err := os.Open(path)
	if err != nil {
		t.Fatalf("open JSON document %s: %v", path, err)
	}
	defer input.Close()

	document, err := jsonschema.UnmarshalJSON(input)
	if err != nil {
		t.Fatalf("decode JSON document %s: %v", path, err)
	}
	return document
}

func loadJSONObject(t *testing.T, path string) map[string]any {
	t.Helper()
	document := loadJSONDocument(t, path)
	object, ok := document.(map[string]any)
	if !ok {
		t.Fatalf("JSON document %s is not an object", path)
	}
	return object
}

func requiredObject(t *testing.T, object map[string]any, key string) map[string]any {
	t.Helper()
	value, exists := object[key]
	if !exists {
		t.Fatalf("missing required object %s", key)
	}
	child, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("required member %s is not an object", key)
	}
	return child
}

func stringSlice(t *testing.T, value any, field string) []string {
	t.Helper()
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("required member %s is not an array", field)
	}
	result := make([]string, 0, len(items))
	for index, item := range items {
		text, ok := item.(string)
		if !ok {
			t.Fatalf("%s[%d] is not a string", field, index)
		}
		result = append(result, text)
	}
	return result
}

func responseSchemaRef(t *testing.T, paths map[string]any, path, method, status string) string {
	t.Helper()
	pathObject := requiredObject(t, paths, path)
	operation := requiredObject(t, pathObject, method)
	responses := requiredObject(t, operation, "responses")
	response := requiredObject(t, responses, status)
	content := requiredObject(t, response, "content")
	mediaType := requiredObject(t, content, "application/json")
	schema := requiredObject(t, mediaType, "schema")
	reference, ok := schema["$ref"].(string)
	if !ok {
		t.Fatalf("response schema %s %s %s has no string $ref: %s", method, path, status, fmt.Sprint(schema["$ref"]))
	}
	return reference
}
