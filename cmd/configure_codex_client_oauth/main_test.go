package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRenderCandidateChangesOnlyCodexClientOAuthValue(t *testing.T) {
	source := []byte("# keep this comment\nhost: 127.0.0.1\napi-keys:\n  - synthetic-secret\ncodex:\n  identity-confuse: true\n")
	candidate, sourceEnabled, errRender := renderCandidate(source, true)
	if errRender != nil {
		t.Fatalf("renderCandidate() error = %v", errRender)
	}
	if sourceEnabled {
		t.Fatal("sourceEnabled = true, want false")
	}
	for _, expected := range [][]byte{[]byte("# keep this comment"), []byte("synthetic-secret"), []byte("client-oauth-access:"), []byte("enabled: true")} {
		if !bytes.Contains(candidate, expected) {
			t.Fatalf("candidate omitted %q", expected)
		}
	}
	second, secondEnabled, errSecond := renderCandidate(candidate, true)
	if errSecond != nil {
		t.Fatalf("second renderCandidate() error = %v", errSecond)
	}
	if !secondEnabled || !bytes.Equal(second, candidate) {
		t.Fatal("second render was not byte-identical")
	}
}

func TestRenderCandidateRejectsUnsafeListener(t *testing.T) {
	_, _, errRender := renderCandidate([]byte("host: 0.0.0.0\n"), true)
	if errRender == nil {
		t.Fatal("renderCandidate() enabled OAuth on a non-loopback listener")
	}
}

func TestPlanAndApplyUseExactHashesAndBackup(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("atomic apply is a Windows deployment contract")
	}
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	backupPath := filepath.Join(directory, "config.before.yaml")
	source := []byte("host: 127.0.0.1\napi-keys:\n  - synthetic-secret\n")
	if errWrite := os.WriteFile(configPath, source, 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}

	var planOutput bytes.Buffer
	if errPlan := run([]string{"--config", configPath, "--value", "true", "--plan"}, &planOutput); errPlan != nil {
		t.Fatalf("plan error = %v", errPlan)
	}
	var plan commandReceipt
	if errDecode := json.Unmarshal(planOutput.Bytes(), &plan); errDecode != nil {
		t.Fatalf("decode plan: %v", errDecode)
	}
	if plan.Applied || !plan.Changed || plan.SourceSHA256 != sha256Hex(source) {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	if bytes.Contains(planOutput.Bytes(), []byte("synthetic-secret")) {
		t.Fatal("plan output exposed configuration contents")
	}

	var applyOutput bytes.Buffer
	args := []string{
		"--config", configPath,
		"--backup", backupPath,
		"--value", "true",
		"--expected-source-sha256", plan.SourceSHA256,
		"--expected-candidate-sha256", plan.CandidateSHA256,
		"--apply",
	}
	if errApply := run(args, &applyOutput); errApply != nil {
		t.Fatalf("apply error = %v", errApply)
	}
	var applied commandReceipt
	if errDecode := json.Unmarshal(applyOutput.Bytes(), &applied); errDecode != nil {
		t.Fatalf("decode apply: %v", errDecode)
	}
	if !applied.Applied || applied.Mode != "apply" {
		t.Fatalf("unexpected apply receipt: %+v", applied)
	}
	actualConfig, errConfig := os.ReadFile(configPath)
	if errConfig != nil || sha256Hex(actualConfig) != plan.CandidateSHA256 {
		t.Fatal("applied config hash mismatch")
	}
	actualBackup, errBackup := os.ReadFile(backupPath)
	if errBackup != nil || !bytes.Equal(actualBackup, source) {
		t.Fatal("exact backup mismatch")
	}
}

func TestApplyRejectsDriftBeforeWriting(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("atomic apply is a Windows deployment contract")
	}
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	backupPath := filepath.Join(directory, "config.before.yaml")
	if errWrite := os.WriteFile(configPath, []byte("host: 127.0.0.1\n"), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}
	errApply := run([]string{
		"--config", configPath,
		"--backup", backupPath,
		"--value", "true",
		"--expected-source-sha256", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"--expected-candidate-sha256", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"--apply",
	}, ioDiscard{})
	if errApply == nil {
		t.Fatal("apply accepted a drifted source")
	}
	if _, errBackup := os.Stat(backupPath); !os.IsNotExist(errBackup) {
		t.Fatal("apply created a backup despite source drift")
	}
}

func TestReplaceRejectsDriftAtAtomicBoundary(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("atomic apply is a Windows deployment contract")
	}
	directory := t.TempDir()
	targetPath := filepath.Join(directory, "config.yaml")
	replacementPath := filepath.Join(directory, "candidate.yaml")
	backupPath := filepath.Join(directory, "config.before.yaml")
	current := []byte("host: 127.0.0.1\nversion: changed\n")
	if errWrite := os.WriteFile(targetPath, current, 0o600); errWrite != nil {
		t.Fatalf("write target: %v", errWrite)
	}
	if errWrite := os.WriteFile(replacementPath, []byte("host: 127.0.0.1\nversion: candidate\n"), 0o600); errWrite != nil {
		t.Fatalf("write replacement: %v", errWrite)
	}
	errReplace := replaceFileWithBackup(targetPath, replacementPath, backupPath, sha256Hex([]byte("host: 127.0.0.1\nversion: original\n")))
	if errReplace == nil || !strings.Contains(errReplace.Error(), "atomic boundary") {
		t.Fatalf("replace error = %v, want atomic-boundary drift rejection", errReplace)
	}
	actual, errRead := os.ReadFile(targetPath)
	if errRead != nil || !bytes.Equal(actual, current) {
		t.Fatal("target changed despite atomic-boundary drift")
	}
	if _, errBackup := os.Stat(backupPath); !os.IsNotExist(errBackup) {
		t.Fatal("backup was created despite atomic-boundary drift")
	}
}

func TestMalformedConfigErrorDoesNotExposeContents(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	secret := "synthetic-secret-that-must-not-appear"
	if errWrite := os.WriteFile(configPath, []byte("host: ["+secret+"\n"), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}
	errPlan := run([]string{"--config", configPath, "--value", "true", "--plan"}, ioDiscard{})
	if errPlan == nil {
		t.Fatal("plan accepted malformed config")
	}
	if strings.Contains(errPlan.Error(), secret) {
		t.Fatal("parse error exposed configuration contents")
	}
}

func TestRenderCandidateRejectsMultipleDocuments(t *testing.T) {
	source := []byte("host: 127.0.0.1\n---\nhost: 127.0.0.1\n")
	_, _, errRender := renderCandidate(source, true)
	if errRender == nil || !strings.Contains(errRender.Error(), "exactly one YAML document") {
		t.Fatalf("renderCandidate() error = %v, want multiple-document rejection", errRender)
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(value []byte) (int, error) { return len(value), nil }
