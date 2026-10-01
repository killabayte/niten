package contract

import (
	"bytes"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// A bundled schema accepts and rejects exactly what the embedded one does.
func TestBundleIsSelfContainedAndEquivalent(t *testing.T) {
	for _, kind := range MessageKinds {
		b, err := Bundle(string(kind))
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		c := jsonschema.NewCompiler()
		c.AssertFormat()
		if err := c.AddResource("bundle.json", doc); err != nil {
			t.Fatal(err)
		}
		s, err := c.Compile("bundle.json")
		if err != nil {
			t.Fatalf("%s: compile the bundle alone: %v", kind, err)
		}
		valid := readFixture(t, "valid/"+string(kind)+".json")
		inst, _ := jsonschema.UnmarshalJSON(bytes.NewReader(valid))
		if err := s.Validate(inst); err != nil {
			t.Fatalf("%s: the bundle rejects the valid fixture: %v", kind, err)
		}
	}
	b, _ := Bundle(string(KindCandidateReady))
	s := compileBundle(t, b)
	inst, _ := jsonschema.UnmarshalJSON(bytes.NewReader(readFixture(t, "invalid/candidate_ready__no_steps.json")))
	if s.Validate(inst) == nil {
		t.Fatal("the bundle accepts an invalid payload")
	}
}

func compileBundle(t *testing.T, b []byte) *jsonschema.Schema {
	t.Helper()
	doc, _ := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	c := jsonschema.NewCompiler()
	c.AddResource("bundle.json", doc)
	s, err := c.Compile("bundle.json")
	if err != nil {
		t.Fatal(err)
	}
	return s
}
