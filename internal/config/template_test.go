package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTemplateIsAllDefaults(t *testing.T) {
	c, err := Parse(Template())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c, Default()) {
		t.Errorf("the template changes a setting: %+v", c)
	}
}

// uncomment switches on every setting: "# " lines lose their marker, "##" lines go.
func uncomment(b []byte) []byte {
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		trim := strings.TrimLeft(line, " ")
		switch {
		case strings.HasPrefix(trim, "##"):
		case strings.HasPrefix(line, "# "):
			out = append(out, strings.TrimPrefix(line, "# "))
		default:
			out = append(out, line)
		}
	}
	return []byte(strings.Join(out, "\n"))
}

func TestTemplateExamplesAreValid(t *testing.T) {
	c, err := Parse(uncomment(Template()))
	if err != nil {
		t.Fatalf("an example in the template is invalid: %v\n%s", err, uncomment(Template()))
	}
	if p := c.Project("app-group"); len(c.Projects) != 2 || p.Budget.Cap != CapSoft || len(c.Projects["framework-group"].Caches) != 1 ||
		len(c.Projects["framework-group"].Intake) != 3 || c.Desk.BudgetCapMode() != CapSoft {
		t.Errorf("project examples did not apply: %+v", c.Projects)
	}
	if c.Profile("my-app").BranchModel != Promotion || c.Profile("framework").Autonomy.Tag != Agent {
		t.Errorf("examples did not apply: %+v", c.Repos)
	}
}

func TestWriteTemplateNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	wrote, err := WriteTemplate(path)
	if err != nil || !wrote {
		t.Fatalf("first write: %v %v", wrote, err)
	}
	os.WriteFile(path, []byte("defaults:\n  gate: mine\n"), 0o644)
	wrote, err = WriteTemplate(path)
	if err != nil || wrote {
		t.Fatalf("second write: %v %v", wrote, err)
	}
	if b, _ := os.ReadFile(path); string(b) != "defaults:\n  gate: mine\n" {
		t.Errorf("existing file changed: %q", b)
	}
}
