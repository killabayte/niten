package contract

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func readFixture(t *testing.T, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", rel))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func validateNamed(name string, data []byte) error {
	switch {
	case name == envelopeSchema:
		_, err := ValidateEnvelope(data)
		return err
	case MessageKind(name).Valid():
		return ValidatePayload(MessageKind(name), data)
	default:
		return ValidateRecord(name, data)
	}
}

func TestSchemasCompileAndAreListed(t *testing.T) {
	compileOnce.Do(compile)
	if compileErr != nil {
		t.Fatal(compileErr)
	}
	names := Schemas()
	if !sort.StringsAreSorted(names) {
		t.Fatalf("Schemas() not sorted: %v", names)
	}
	want := 1 + len(MessageKinds) + len(recordNames)
	if len(names) != want {
		t.Fatalf("Schemas() = %d names, want %d: %v", len(names), want, names)
	}
	for _, n := range names {
		if _, ok := compiled[n]; !ok {
			t.Errorf("schema %q listed but not compiled", n)
		}
		if _, err := Raw(n); err != nil {
			t.Errorf("Raw(%q): %v", n, err)
		}
	}
	// Every embedded file except common is validatable.
	entries, _ := files.ReadDir("schemas")
	for _, e := range entries {
		n := strings.TrimSuffix(e.Name(), ".schema.json")
		if n == "common" {
			continue
		}
		if _, ok := compiled[n]; !ok {
			t.Errorf("embedded schema %q is not validatable", n)
		}
	}
}

func TestValidFixtures(t *testing.T) {
	for _, name := range Schemas() {
		t.Run(name, func(t *testing.T) {
			if err := validateNamed(name, readFixture(t, "valid/"+name+".json")); err != nil {
				t.Fatalf("valid fixture rejected: %v", err)
			}
		})
	}
}

func TestInvalidFixturesNameTheField(t *testing.T) {
	var cases []struct {
		Schema, File, Expect string
	}
	if err := json.Unmarshal(readFixture(t, "invalid/cases.json"), &cases); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, c := range cases {
		seen[c.Schema] = true
		t.Run(c.File, func(t *testing.T) {
			err := validateNamed(c.Schema, readFixture(t, "invalid/"+c.File))
			if err == nil {
				t.Fatal("invalid fixture accepted")
			}
			var ve *Error
			if !errors.As(err, &ve) {
				t.Fatalf("error is not a *contract.Error: %v", err)
			}
			if !strings.Contains(err.Error(), c.Expect) {
				t.Fatalf("error does not name %q:\n%v", c.Expect, err)
			}
		})
	}
	for _, name := range Schemas() {
		if !seen[name] {
			t.Errorf("no invalid case for schema %q", name)
		}
	}
}

// Model payloads must not be able to carry coordinator-owned fields.
func TestPayloadsRejectCoordinatorFields(t *testing.T) {
	smuggled := []string{"from", "to", "sender", "attempt_id", "sequence", "accepted", "accepted_at", "status", "message_id", "candidate_id"}
	for _, kind := range MessageKinds {
		base := map[string]any{}
		if err := json.Unmarshal(readFixture(t, "valid/"+string(kind)+".json"), &base); err != nil {
			t.Fatal(err)
		}
		for _, f := range smuggled {
			if _, exists := base[f]; exists {
				t.Fatalf("%s: valid fixture already has coordinator field %q", kind, f)
			}
			m := map[string]any{}
			for k, v := range base {
				m[k] = v
			}
			m[f] = "x"
			data, _ := json.Marshal(m)
			err := ValidatePayload(kind, data)
			if err == nil || !strings.Contains(err.Error(), f) {
				t.Errorf("%s: smuggled %q not rejected by name: %v", kind, f, err)
			}
		}
	}
	// verdict is legal only in review_result
	for _, kind := range MessageKinds {
		if kind == KindReviewResult {
			continue
		}
		m := map[string]any{}
		_ = json.Unmarshal(readFixture(t, "valid/"+string(kind)+".json"), &m)
		m["verdict"] = "approve"
		data, _ := json.Marshal(m)
		if err := ValidatePayload(kind, data); err == nil || !strings.Contains(err.Error(), "verdict") {
			t.Errorf("%s: verdict accepted outside review_result: %v", kind, err)
		}
	}
}

func envelopeFixture(t *testing.T) map[string]any {
	t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal(readFixture(t, "valid/envelope.json"), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEnvelopeRules(t *testing.T) {
	t.Run("valid returns typed envelope", func(t *testing.T) {
		env, err := ValidateEnvelope(readFixture(t, "valid/envelope.json"))
		if err != nil {
			t.Fatal(err)
		}
		if env.Kind != KindCandidateReady || env.From != RoleExecutor || env.Sequence != 3 || env.ReplyTo != nil {
			t.Fatalf("unexpected envelope: %+v", env)
		}
		var cr CandidateReady
		if err := json.Unmarshal(env.Payload, &cr); err != nil || len(cr.Steps) != 1 {
			t.Fatalf("payload not preserved: %v %+v", err, cr)
		}
	})
	t.Run("kind/payload mismatch", func(t *testing.T) {
		m := envelopeFixture(t)
		m["kind"] = "finding"
		m["from"] = "reviewer"
		err := ValidateEnvelope0(t, mustJSON(t, m))
		if err == nil || !strings.Contains(err.Error(), "/payload") {
			t.Fatalf("mismatch not reported under /payload: %v", err)
		}
	})
	t.Run("reviewer cannot send candidate_ready", func(t *testing.T) {
		m := envelopeFixture(t)
		m["from"] = "reviewer"
		err := ValidateEnvelope0(t, mustJSON(t, m))
		if err == nil || !strings.Contains(err.Error(), "/from") {
			t.Fatalf("sender rule not enforced: %v", err)
		}
	})
	t.Run("user cannot forge a model message", func(t *testing.T) {
		m := envelopeFixture(t)
		m["from"] = "user"
		if err := ValidateEnvelope0(t, mustJSON(t, m)); err == nil {
			t.Fatal("user-originated candidate_ready accepted")
		}
	})
	t.Run("to must be coordinator", func(t *testing.T) {
		m := envelopeFixture(t)
		m["to"] = "reviewer"
		err := ValidateEnvelope0(t, mustJSON(t, m))
		if err == nil || !strings.Contains(err.Error(), "/to") {
			t.Fatalf("recipient rule not enforced: %v", err)
		}
	})
	t.Run("unassigned step claimed", func(t *testing.T) {
		m := envelopeFixture(t)
		m["step_ids"] = []string{"S-002"}
		err := ValidateEnvelope0(t, mustJSON(t, m))
		if err == nil || !strings.Contains(err.Error(), "/payload/steps") {
			t.Fatalf("step assignment rule not enforced: %v", err)
		}
	})
	t.Run("unknown kind", func(t *testing.T) {
		m := envelopeFixture(t)
		m["kind"] = "approval"
		err := ValidateEnvelope0(t, mustJSON(t, m))
		if err == nil || !strings.Contains(err.Error(), "kind") {
			t.Fatalf("unknown kind accepted: %v", err)
		}
	})
	t.Run("invalid JSON", func(t *testing.T) {
		if _, err := ValidateEnvelope([]byte("{")); err == nil {
			t.Fatal("invalid JSON accepted")
		}
	})
}

// ValidateEnvelope0 discards the envelope; keeps the subtests readable.
func ValidateEnvelope0(t *testing.T, data []byte) error {
	t.Helper()
	_, err := ValidateEnvelope(data)
	return err
}

func TestUnknownNames(t *testing.T) {
	if err := ValidatePayload("nope", []byte("{}")); err == nil {
		t.Error("unknown kind accepted")
	}
	if err := ValidateRecord("envelope", []byte("{}")); err == nil {
		t.Error("envelope accepted as a record")
	}
	if err := ValidateRecord("finding", []byte("{}")); err == nil {
		t.Error("message kind accepted as a record")
	}
}

func TestErrorNames(t *testing.T) {
	err := ValidatePayload(KindFinding, readFixture(t, "invalid/finding__bad_severity.json"))
	var ve *Error
	if !errors.As(err, &ve) {
		t.Fatal(err)
	}
	if got := ve.Names(); len(got) == 0 || got[0] != "severity" {
		t.Fatalf("Names() = %v", got)
	}
	if ve.Issues[0].Pointer != "/severity" {
		t.Fatalf("pointer = %q", ve.Issues[0].Pointer)
	}
}
