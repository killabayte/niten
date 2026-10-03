package probe

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Every field of the binding changes the fingerprint.
func TestFingerprintCoversTheBinding(t *testing.T) {
	base := Binding{ProbeVersion: 1, Topology: Topology, Claude: Tool{SHA256: "a"}, Codex: Tool{SHA256: "b"}, Executor: "e", Reviewer: "r",
		SettingsTemplateSHA256: "s", ManagedPolicy: map[string]string{"m": "absent"}, AdapterSHA256: "x", OSBuild: "25G83"}
	f := base.Fingerprint()
	for name, mut := range map[string]func(*Binding){
		"claude binary": func(b *Binding) { b.Claude.SHA256 = "c" },
		"codex version": func(b *Binding) { b.Codex.Version = "2" },
		"executor":      func(b *Binding) { b.Executor = "e2" },
		"template":      func(b *Binding) { b.SettingsTemplateSHA256 = "t" },
		"managed":       func(b *Binding) { b.ManagedPolicy = map[string]string{"m": "x"} },
		"strip env":     func(b *Binding) { b.StripEnv = []string{"X"} },
		"adapter":       func(b *Binding) { b.AdapterSHA256 = "y" },
		"os build":      func(b *Binding) { b.OSBuild = "25H1" },
		"topology":      func(b *Binding) { b.Topology = "p0b" },
	} {
		b := base
		b.ManagedPolicy = map[string]string{"m": "absent"}
		mut(&b)
		if b.Fingerprint() == f {
			t.Errorf("%s does not change the fingerprint", name)
		}
	}
}

// Find accepts only a passing, private certificate whose binding reproduces
// its fingerprint.
func TestFindRefusesForgedCertificates(t *testing.T) {
	store := t.TempDir()
	b := Binding{ProbeVersion: 1, Topology: Topology, Executor: "e"}
	fp := b.Fingerprint()
	write := func(name string, c Certificate, mode os.FileMode) {
		os.MkdirAll(CertificateDir(store), 0o700)
		raw, _ := json.Marshal(c)
		os.WriteFile(filepath.Join(CertificateDir(store), name), raw, mode)
	}
	ok := Certificate{Kind: CertificateKind, Fingerprint: fp, Binding: b, Result: Pass, Invocations: RequiredInvocations, Controls: completeControls()}
	failed := ok
	failed.Result = Fail
	forged := ok
	forged.Binding.Executor = "other"
	partial := ok
	partial.Controls = ok.Controls[:len(ok.Controls)-1] // a role's control missing
	oneCall := ok
	oneCall.Invocations = 1
	write(fp+"-1.json", failed, 0o600)
	write(fp+"-2.json", forged, 0o600)
	write(fp+"-3.json", ok, 0o644)      // world-readable
	write(fp+"-4.json", partial, 0o600) // Result says pass but a control is missing
	write(fp+"-5.json", oneCall, 0o600) // only one invocation
	if _, _, _, err := Find(store, fp); err == nil {
		t.Fatal("a failed, forged, world-readable, partial or one-invocation certificate was accepted")
	}
	write(fp+"-6.json", ok, 0o600)
	if c, _, _, err := Find(store, fp); err != nil || c.Result != Pass {
		t.Fatalf("a valid complete certificate was rejected: %v", err)
	}
}

// A complete set of passing controls with two invocations passes; anything
// missing, failing or short of two invocations does not.
func completeControls() []Control {
	var cs []Control
	for role, names := range requiredControls {
		for _, n := range names {
			cs = append(cs, Control{Role: role, Name: n, Status: Pass})
		}
	}
	return cs
}

func TestAggregate(t *testing.T) {
	full := completeControls()
	if aggregate(full, 2) != Pass {
		t.Fatal("a complete passing set is not pass")
	}
	if aggregate(full, 1) != Inconclusive {
		t.Fatal("one invocation is accepted")
	}
	if aggregate(full[:len(full)-1], 2) != Inconclusive {
		t.Fatal("a missing control is accepted")
	}
	failed := append(completeControls(), Control{Role: "executor", Name: "shell negative", Status: Fail})
	if aggregate(failed, 2) != Fail {
		t.Fatal("a failure is not a failure")
	}
	inc := completeControls()
	inc[0].Status = Inconclusive
	if aggregate(inc, 2) != Inconclusive {
		t.Fatal("an inconclusive control is accepted")
	}
	dup := append(completeControls(), Control{Role: "executor", Name: "shell negative", Status: Pass})
	if aggregate(dup, 2) != Inconclusive {
		t.Fatal("a duplicated control is accepted")
	}
}
