package contract

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

//go:embed schemas/*.schema.json
var files embed.FS

// Record schema names (coordinator-owned records, one file each).
const (
	RecordCandidate   = "candidate_record"
	RecordCheck       = "check_evidence"
	RecordAttestation = "attestation"
	RecordStepGate    = "step_continue"
	RecordHandoff     = "handoff_record"
)

// recordNames lists every record schema; message kinds map to their own files.
var recordNames = []string{RecordCandidate, RecordCheck, RecordAttestation, RecordStepGate, RecordHandoff}

const envelopeSchema = "envelope"

var (
	compileOnce sync.Once
	compiled    map[string]*jsonschema.Schema
	compileErr  error
	printer     = message.NewPrinter(language.English)
)

func compile() {
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	entries, err := fs.ReadDir(files, "schemas")
	if err != nil {
		compileErr = err
		return
	}
	for _, e := range entries {
		data, err := files.ReadFile("schemas/" + e.Name())
		if err != nil {
			compileErr = err
			return
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			compileErr = fmt.Errorf("%s: %w", e.Name(), err)
			return
		}
		if err := c.AddResource(e.Name(), doc); err != nil {
			compileErr = fmt.Errorf("%s: %w", e.Name(), err)
			return
		}
	}
	compiled = map[string]*jsonschema.Schema{}
	for _, name := range Schemas() {
		s, err := c.Compile(name + ".schema.json")
		if err != nil {
			compileErr = fmt.Errorf("compile %s: %w", name, err)
			return
		}
		compiled[name] = s
	}
}

// Schemas lists the names of every validatable schema: the envelope, one per
// message kind and one per record. common.schema.json only holds shared $defs.
func Schemas() []string {
	names := []string{envelopeSchema}
	for _, k := range MessageKinds {
		names = append(names, string(k))
	}
	names = append(names, recordNames...)
	sort.Strings(names)
	return names
}

// Raw returns the embedded schema text, for example to pass to a CLI as a
// structured-output schema.
func Raw(name string) ([]byte, error) { return files.ReadFile("schemas/" + name + ".schema.json") }

// Issue is one validation failure, addressed by JSON pointer into the instance.
type Issue struct {
	Pointer string
	Message string
}

// Error lists every violation of one schema.
type Error struct {
	Schema string
	Issues []Issue
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d violation(s)", e.Schema, len(e.Issues))
	for _, i := range e.Issues {
		fmt.Fprintf(&b, "\n  %s: %s", i.Pointer, i.Message)
	}
	return b.String()
}

// Names returns the field names (last pointer segment) that failed, for tests and reports.
func (e *Error) Names() []string {
	var out []string
	for _, i := range e.Issues {
		segs := strings.Split(strings.TrimPrefix(i.Pointer, "/"), "/")
		out = append(out, segs[len(segs)-1])
	}
	return out
}

func validate(name string, data []byte) error {
	compileOnce.Do(compile)
	if compileErr != nil {
		return compileErr
	}
	s, ok := compiled[name]
	if !ok {
		return fmt.Errorf("unknown schema %q", name)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return &Error{Schema: name, Issues: []Issue{{Pointer: "", Message: "invalid JSON: " + err.Error()}}}
	}
	if err := s.Validate(inst); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			return &Error{Schema: name, Issues: flatten(ve)}
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// flatten collects the leaf causes with their instance pointers.
func flatten(ve *jsonschema.ValidationError) []Issue {
	if len(ve.Causes) == 0 {
		return []Issue{{Pointer: pointer(ve.InstanceLocation), Message: ve.ErrorKind.LocalizedString(printer)}}
	}
	var out []Issue
	for _, c := range ve.Causes {
		out = append(out, flatten(c)...)
	}
	return out
}

func pointer(loc []string) string {
	if len(loc) == 0 {
		return "/"
	}
	var b strings.Builder
	for _, s := range loc {
		b.WriteByte('/')
		b.WriteString(strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1"))
	}
	return b.String()
}

// ValidatePayload checks a model payload against the schema of kind.
func ValidatePayload(kind MessageKind, data []byte) error {
	if !kind.Valid() {
		return fmt.Errorf("unknown message kind %q", kind)
	}
	return validate(string(kind), data)
}

// ValidateRecord checks a coordinator record against the named record schema.
func ValidateRecord(name string, data []byte) error {
	for _, r := range recordNames {
		if r == name {
			return validate(name, data)
		}
	}
	return fmt.Errorf("unknown record %q", name)
}

// ValidateEnvelope checks the envelope shape, then the payload against the
// kind's schema, then the coordinator rules that JSON Schema does not express:
// the sender must be allowed to originate the kind, the recipient is always the
// coordinator, and a candidate_ready may only claim steps the envelope assigned.
func ValidateEnvelope(data []byte) (Envelope, error) {
	var env Envelope
	if err := validate(envelopeSchema, data); err != nil {
		return env, err
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return env, &Error{Schema: envelopeSchema, Issues: []Issue{{Pointer: "/", Message: err.Error()}}}
	}
	if err := ValidatePayload(env.Kind, env.Payload); err != nil {
		var ve *Error
		if errors.As(err, &ve) {
			for i := range ve.Issues {
				ve.Issues[i].Pointer = "/payload" + strings.TrimSuffix(ve.Issues[i].Pointer, "/")
			}
		}
		return env, err
	}
	if err := env.checkRules(); err != nil {
		return env, err
	}
	return env, nil
}

func (env Envelope) checkRules() error {
	var issues []Issue
	if !slicesContains(senders[env.Kind], env.From) {
		issues = append(issues, Issue{Pointer: "/from", Message: fmt.Sprintf("role %q may not send %q", env.From, env.Kind)})
	}
	if env.To != RoleCoordinator {
		issues = append(issues, Issue{Pointer: "/to", Message: "model messages are addressed to the coordinator"})
	}
	if env.Kind == KindCandidateReady {
		var cr CandidateReady
		if err := json.Unmarshal(env.Payload, &cr); err == nil {
			for _, s := range cr.Steps {
				if !slicesContains(env.StepIDs, s) {
					issues = append(issues, Issue{Pointer: "/payload/steps", Message: fmt.Sprintf("step %q was not assigned in step_ids", s)})
				}
			}
		}
	}
	if len(issues) > 0 {
		return &Error{Schema: envelopeSchema, Issues: issues}
	}
	return nil
}

func slicesContains[T comparable](xs []T, x T) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// ValidateValue marshals a Go value and validates it against the named schema.
// It is the round-trip check for the typed structs above.
func ValidateValue(name string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if name == envelopeSchema {
		_, err := ValidateEnvelope(data)
		return err
	}
	if MessageKind(name).Valid() {
		return ValidatePayload(MessageKind(name), data)
	}
	return ValidateRecord(name, data)
}
