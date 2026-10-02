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
	ok := Certificate{Kind: CertificateKind, Fingerprint: fp, Binding: b, Result: Pass}
	failed := ok
	failed.Result = Fail
	forged := ok
	forged.Binding.Executor = "other"
	write(fp+"-1.json", failed, 0o600)
	write(fp+"-2.json", forged, 0o600)
	write(fp+"-3.json", ok, 0o644)
	if _, _, _, err := Find(store, fp); err == nil {
		t.Fatal("a failed, forged or world-readable certificate was accepted")
	}
	write(fp+"-4.json", ok, 0o600)
	if c, _, _, err := Find(store, fp); err != nil || c.Result != Pass {
		t.Fatalf("a valid certificate: %v", err)
	}
}

func TestResult(t *testing.T) {
	if result(nil) != Inconclusive || result([]Control{{Status: Pass}, {Status: Inconclusive}}) != Inconclusive ||
		result([]Control{{Status: Inconclusive}, {Status: Fail}}) != Fail || result([]Control{{Status: Pass}}) != Pass {
		t.Fatal("result aggregation")
	}
}
