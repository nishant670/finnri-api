package http

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"

	"finnri/internal/config"
)

var openAPIPathParamPattern = regexp.MustCompile(`\{([^}/]+)\}`)

var openAPIHTTPMethods = map[string]string{
	"delete": "DELETE",
	"get":    "GET",
	"patch":  "PATCH",
	"post":   "POST",
	"put":    "PUT",
}

type openAPIDocument struct {
	Paths map[string]map[string]yaml.Node `yaml:"paths"`
}

func TestOpenAPIOperationsAreRegisteredRoutes(t *testing.T) {
	doc := readOpenAPIDocument(t)
	registered := registeredRouteSet(t)

	var missing []string
	for path, operations := range doc.Paths {
		for method := range operations {
			registeredMethod, ok := openAPIHTTPMethods[strings.ToLower(method)]
			if !ok {
				continue
			}

			route := registeredMethod + " " + openAPIPathToGin(path)
			if !registered[route] {
				missing = append(missing, route)
			}
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("OpenAPI documents routes that are not registered:\n%s", strings.Join(missing, "\n"))
	}
}

func TestOpenAPILocalSchemaRefsResolveToValidJSON(t *testing.T) {
	root := projectRoot(t)
	node := readOpenAPINode(t)
	refs := collectLocalRefs(&node)

	if len(refs) == 0 {
		t.Fatal("OpenAPI document does not include any local schema refs")
	}

	for _, ref := range refs {
		path := filepath.Clean(filepath.Join(root, ref))
		if !strings.HasPrefix(path, root+string(os.PathSeparator)) {
			t.Fatalf("local schema ref escapes project root: %s", ref)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("local schema ref %s could not be read: %v", ref, err)
		}
		if !json.Valid(data) {
			t.Fatalf("local schema ref %s is not valid JSON", ref)
		}
	}
}

func readOpenAPIDocument(t *testing.T) openAPIDocument {
	t.Helper()

	var doc openAPIDocument
	data := readOpenAPIYAML(t)
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("failed to parse openapi.yaml: %v", err)
	}
	if len(doc.Paths) == 0 {
		t.Fatal("openapi.yaml does not define any paths")
	}
	return doc
}

func readOpenAPINode(t *testing.T) yaml.Node {
	t.Helper()

	var node yaml.Node
	data := readOpenAPIYAML(t)
	if err := yaml.Unmarshal(data, &node); err != nil {
		t.Fatalf("failed to parse openapi.yaml: %v", err)
	}
	return node
}

func readOpenAPIYAML(t *testing.T) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(projectRoot(t), "openapi.yaml"))
	if err != nil {
		t.Fatalf("failed to read openapi.yaml: %v", err)
	}
	return data
}

func registeredRouteSet(t *testing.T) map[string]bool {
	t.Helper()

	root := projectRoot(t)
	previous, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("failed to change working directory: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Fatalf("failed to restore working directory: %v", err)
		}
	})

	gin.SetMode(gin.TestMode)
	router := NewServer(&config.Config{
		AllowOrigins:       "http://localhost:8081",
		TZDefault:          "Asia/Kolkata",
		OpenAIBaseURL:      "http://127.0.0.1",
		OpenAILlmModel:     "test-llm",
		OpenAIWhisper:      "test-whisper",
		OpenAIMaxTokens:    128,
		ReqTimeoutSec:      1,
		RateLimitRPS:       1000,
		RateLimitBurst:     1000,
		MaxJSONKB:          64,
		MaxUploadMB:        1,
		MaxTranscriptChars: 1000,
	})

	routes := map[string]bool{}
	for _, route := range router.Routes() {
		routes[route.Method+" "+route.Path] = true
	}
	return routes
}

func openAPIPathToGin(path string) string {
	return openAPIPathParamPattern.ReplaceAllString(path, ":$1")
}

func collectLocalRefs(node *yaml.Node) []string {
	refs := map[string]bool{}
	collectLocalRefsInto(node, refs)

	out := make([]string, 0, len(refs))
	for ref := range refs {
		out = append(out, ref)
	}
	sort.Strings(out)
	return out
}

func collectLocalRefsInto(node *yaml.Node, refs map[string]bool) {
	if node == nil {
		return
	}
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			value := node.Content[i+1]
			if key.Value == "$ref" && value.Kind == yaml.ScalarNode && strings.HasPrefix(value.Value, ".") {
				refs[value.Value] = true
			}
		}
	}
	for _, child := range node.Content {
		collectLocalRefsInto(child, refs)
	}
}

func projectRoot(t *testing.T) string {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve test file path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "../.."))
}
