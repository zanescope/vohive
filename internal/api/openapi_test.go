package api

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOpenAPIVoHiveYAMLValid(t *testing.T) {
	data, err := os.ReadFile("openapi.vohive.yaml")
	if err != nil {
		t.Fatalf("read openapi.vohive.yaml: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("openapi.vohive.yaml is invalid YAML: %v", err)
	}
	if doc["openapi"] == "" {
		t.Fatalf("openapi.vohive.yaml missing openapi version")
	}
}

func TestOpenAPISIMNoteContract(t *testing.T) {
	data, err := os.ReadFile("openapi.vohive.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths      map[string]map[string]any `yaml:"paths"`
		Components struct {
			Schemas map[string]struct {
				Properties map[string]any `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.Paths["/cards/{iccid}/note"]["patch"]; !ok {
		t.Fatal("OpenAPI missing PATCH /cards/{iccid}/note")
	}
	for _, schemaName := range []string{"DeviceMgmtOverviewLiteItem", "EsimProfile"} {
		schema, ok := doc.Components.Schemas[schemaName]
		if !ok {
			t.Fatalf("OpenAPI missing schema %s", schemaName)
		}
		if _, ok := schema.Properties["sim_note"]; !ok {
			t.Fatalf("OpenAPI schema %s missing sim_note", schemaName)
		}
	}
}
