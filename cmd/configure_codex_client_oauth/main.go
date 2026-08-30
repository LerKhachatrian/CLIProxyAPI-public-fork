// Command configure_codex_client_oauth plans or atomically applies the single
// CLIProxy configuration change that enables official Codex client OAuth.
// It never prints configuration contents or credential material.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	codexoauth "github.com/router-for-me/CLIProxyAPI/v7/internal/access/codex_oauth"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"gopkg.in/yaml.v3"
)

type commandOptions struct {
	configPath        string
	backupPath        string
	expectedSource    string
	expectedCandidate string
	enabled           bool
	plan              bool
	apply             bool
}

type commandReceipt struct {
	SchemaVersion    string `json:"schema_version"`
	Mode             string `json:"mode"`
	ConfigPath       string `json:"config_path"`
	SourceSHA256     string `json:"source_sha256"`
	CandidateSHA256  string `json:"candidate_sha256"`
	BackupPath       string `json:"backup_path,omitempty"`
	SourceEnabled    bool   `json:"source_enabled"`
	CandidateEnabled bool   `json:"candidate_enabled"`
	Changed          bool   `json:"changed"`
	Applied          bool   `json:"applied"`
	SecretsEmitted   bool   `json:"secrets_emitted"`
}

const maxConfigBytes int64 = 16 << 20

func main() {
	if errRun := run(os.Args[1:], os.Stdout); errRun != nil {
		_, _ = fmt.Fprintf(os.Stderr, "configure Codex client OAuth: %v\n", errRun)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	options, errOptions := parseOptions(args)
	if errOptions != nil {
		return errOptions
	}
	configPath, errConfigPath := filepath.Abs(options.configPath)
	if errConfigPath != nil {
		return fmt.Errorf("resolve config path: %w", errConfigPath)
	}
	configInfo, errConfigInfo := os.Lstat(configPath)
	if errConfigInfo != nil {
		return fmt.Errorf("stat config: %w", errConfigInfo)
	}
	if !configInfo.Mode().IsRegular() {
		return errors.New("config path must be a regular file")
	}
	if configInfo.Size() > maxConfigBytes {
		return fmt.Errorf("config exceeds the %d-byte safety limit", maxConfigBytes)
	}
	source, errRead := os.ReadFile(configPath)
	if errRead != nil {
		return fmt.Errorf("read config: %w", errRead)
	}
	sourceHash := sha256Hex(source)
	if options.expectedSource != "" && sourceHash != options.expectedSource {
		return fmt.Errorf("source SHA-256 mismatch: expected %s, found %s", options.expectedSource, sourceHash)
	}

	candidate, sourceEnabled, errRender := renderCandidate(source, options.enabled)
	if errRender != nil {
		return errRender
	}
	candidateHash := sha256Hex(candidate)
	if options.expectedCandidate != "" && candidateHash != options.expectedCandidate {
		return fmt.Errorf("candidate SHA-256 mismatch: expected %s, found %s", options.expectedCandidate, candidateHash)
	}

	receipt := commandReceipt{
		SchemaVersion:    "cliproxy.codex-client-oauth-config.v1",
		Mode:             "plan",
		ConfigPath:       configPath,
		SourceSHA256:     sourceHash,
		CandidateSHA256:  candidateHash,
		SourceEnabled:    sourceEnabled,
		CandidateEnabled: options.enabled,
		Changed:          !bytes.Equal(source, candidate),
		SecretsEmitted:   false,
	}
	if options.apply {
		backupPath, errBackupPath := filepath.Abs(options.backupPath)
		if errBackupPath != nil {
			return fmt.Errorf("resolve backup path: %w", errBackupPath)
		}
		if !receipt.Changed {
			return errors.New("refusing no-op apply")
		}
		if errApply := applyCandidate(configPath, backupPath, source, candidate); errApply != nil {
			return errApply
		}
		receipt.Mode = "apply"
		receipt.BackupPath = backupPath
		receipt.Applied = true
	}

	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if errEncode := encoder.Encode(receipt); errEncode != nil {
		if receipt.Applied {
			return fmt.Errorf("configuration was applied but the receipt could not be written: %w", errEncode)
		}
		return fmt.Errorf("write receipt: %w", errEncode)
	}
	return nil
}

func parseOptions(args []string) (commandOptions, error) {
	var options commandOptions
	var value string
	flags := flag.NewFlagSet("configure_codex_client_oauth", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&options.configPath, "config", "", "configuration path")
	flags.StringVar(&options.backupPath, "backup", "", "exact backup path used by apply")
	flags.StringVar(&options.expectedSource, "expected-source-sha256", "", "expected source SHA-256")
	flags.StringVar(&options.expectedCandidate, "expected-candidate-sha256", "", "expected candidate SHA-256")
	flags.StringVar(&value, "value", "", "true or false")
	flags.BoolVar(&options.plan, "plan", false, "render and hash without writing")
	flags.BoolVar(&options.apply, "apply", false, "atomically replace after exact hash checks")
	if errParse := flags.Parse(args); errParse != nil {
		return commandOptions{}, errParse
	}
	if options.plan == options.apply {
		return commandOptions{}, errors.New("choose exactly one mode: --plan or --apply")
	}
	if strings.TrimSpace(options.configPath) == "" {
		return commandOptions{}, errors.New("--config is required")
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "enable", "enabled", "on":
		options.enabled = true
	case "false", "disable", "disabled", "off":
		options.enabled = false
	default:
		return commandOptions{}, errors.New("--value must be true or false")
	}
	options.expectedSource = strings.ToLower(strings.TrimSpace(options.expectedSource))
	options.expectedCandidate = strings.ToLower(strings.TrimSpace(options.expectedCandidate))
	for label, value := range map[string]string{
		"expected source":    options.expectedSource,
		"expected candidate": options.expectedCandidate,
	} {
		if value != "" && !validSHA256(value) {
			return commandOptions{}, fmt.Errorf("%s SHA-256 is invalid", label)
		}
	}
	if options.apply {
		if options.expectedSource == "" || options.expectedCandidate == "" || strings.TrimSpace(options.backupPath) == "" {
			return commandOptions{}, errors.New("--apply requires --backup and both expected SHA-256 values")
		}
	}
	return options, nil
}

func renderCandidate(source []byte, enabled bool) ([]byte, bool, error) {
	var document yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	if errNode := decoder.Decode(&document); errNode != nil {
		return nil, false, errors.New("parse source config: invalid YAML")
	}
	var trailing yaml.Node
	if errTrailing := decoder.Decode(&trailing); errTrailing == nil {
		return nil, false, errors.New("source config must contain exactly one YAML document")
	} else if !errors.Is(errTrailing, io.EOF) {
		return nil, false, errors.New("parse source config: invalid trailing YAML")
	}

	var before config.Config
	if errUnmarshal := document.Decode(&before); errUnmarshal != nil {
		return nil, false, errors.New("parse source config: invalid YAML")
	}
	sourceEnabled := before.Codex.ClientOAuthAccess.Enabled
	if sourceEnabled == enabled {
		return append([]byte(nil), source...), sourceEnabled, nil
	}

	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, false, errors.New("source config must be one YAML mapping document")
	}
	codexNode, errCodex := getOrCreateMapping(document.Content[0], "codex")
	if errCodex != nil {
		return nil, false, errCodex
	}
	accessNode, errAccess := getOrCreateMapping(codexNode, "client-oauth-access")
	if errAccess != nil {
		return nil, false, errAccess
	}
	if errSet := setBoolean(accessNode, "enabled", enabled); errSet != nil {
		return nil, false, errSet
	}

	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if errEncode := encoder.Encode(&document); errEncode != nil {
		_ = encoder.Close()
		return nil, false, fmt.Errorf("render candidate config: %w", errEncode)
	}
	if errClose := encoder.Close(); errClose != nil {
		return nil, false, fmt.Errorf("finish candidate config: %w", errClose)
	}
	candidate := config.NormalizeCommentIndentation(output.Bytes())

	var after config.Config
	if errAfter := yaml.Unmarshal(candidate, &after); errAfter != nil {
		return nil, false, errors.New("validate candidate config: invalid YAML")
	}
	if after.Codex.ClientOAuthAccess.Enabled != enabled {
		return nil, false, errors.New("candidate config did not retain requested value")
	}
	if errListener := codexoauth.ValidateListenerConfig(&after); errListener != nil {
		return nil, false, errListener
	}
	before.Codex.ClientOAuthAccess = after.Codex.ClientOAuthAccess
	if !reflect.DeepEqual(before, after) {
		return nil, false, errors.New("candidate config changed an unrelated parsed setting")
	}
	return candidate, sourceEnabled, nil
}

func getOrCreateMapping(parent *yaml.Node, key string) (*yaml.Node, error) {
	value, found, errFind := mappingValue(parent, key)
	if errFind != nil {
		return nil, errFind
	}
	if found {
		if value.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s must be a YAML mapping", key)
		}
		return value, nil
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	valueNode := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	parent.Content = append(parent.Content, keyNode, valueNode)
	return valueNode, nil
}

func mappingValue(parent *yaml.Node, key string) (*yaml.Node, bool, error) {
	if parent == nil || parent.Kind != yaml.MappingNode || len(parent.Content)%2 != 0 {
		return nil, false, errors.New("invalid YAML mapping structure")
	}
	var match *yaml.Node
	for index := 0; index < len(parent.Content); index += 2 {
		keyNode := parent.Content[index]
		if keyNode.Kind != yaml.ScalarNode || keyNode.Value != key {
			continue
		}
		if match != nil {
			return nil, false, fmt.Errorf("duplicate YAML key: %s", key)
		}
		match = parent.Content[index+1]
	}
	return match, match != nil, nil
}

func setBoolean(parent *yaml.Node, key string, value bool) error {
	node, found, errFind := mappingValue(parent, key)
	if errFind != nil {
		return errFind
	}
	if !found {
		keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
		node = &yaml.Node{}
		parent.Content = append(parent.Content, keyNode, node)
	}
	if node.Kind != 0 && node.Kind != yaml.ScalarNode {
		return fmt.Errorf("%s must be a YAML scalar", key)
	}
	node.Kind = yaml.ScalarNode
	node.Tag = "!!bool"
	node.Value = fmt.Sprintf("%t", value)
	node.Style = 0
	return nil
}

func applyCandidate(configPath, backupPath string, source, candidate []byte) error {
	if errSupported := ensureAtomicReplaceSupported(); errSupported != nil {
		return errSupported
	}
	if strings.EqualFold(filepath.Clean(configPath), filepath.Clean(backupPath)) {
		return errors.New("backup path must differ from config path")
	}
	if !strings.EqualFold(filepath.VolumeName(configPath), filepath.VolumeName(backupPath)) {
		return errors.New("backup path must be on the same volume as config")
	}
	if _, errStat := os.Lstat(backupPath); errStat == nil {
		return errors.New("backup path already exists")
	} else if !os.IsNotExist(errStat) {
		return fmt.Errorf("check backup path: %w", errStat)
	}
	if info, errDirectory := os.Stat(filepath.Dir(backupPath)); errDirectory != nil || !info.IsDir() {
		return errors.New("backup parent directory must already exist")
	}

	info, errInfo := os.Lstat(configPath)
	if errInfo != nil {
		return fmt.Errorf("stat config: %w", errInfo)
	}
	if !info.Mode().IsRegular() {
		return errors.New("config path must be a regular file")
	}
	temporary, errTemp := os.CreateTemp(filepath.Dir(configPath), ".codex-client-oauth-*.tmp")
	if errTemp != nil {
		return fmt.Errorf("create candidate temp file: %w", errTemp)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if errMode := temporary.Chmod(info.Mode()); errMode != nil {
		_ = temporary.Close()
		return fmt.Errorf("set candidate file mode: %w", errMode)
	}
	if _, errWrite := temporary.Write(candidate); errWrite != nil {
		_ = temporary.Close()
		return fmt.Errorf("write candidate config: %w", errWrite)
	}
	if errSync := temporary.Sync(); errSync != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync candidate config: %w", errSync)
	}
	if errClose := temporary.Close(); errClose != nil {
		return fmt.Errorf("close candidate config: %w", errClose)
	}
	writtenCandidate, errReadCandidate := os.ReadFile(temporaryPath)
	if errReadCandidate != nil || sha256Hex(writtenCandidate) != sha256Hex(candidate) {
		return errors.New("candidate temp-file verification failed")
	}

	if errReplace := replaceFileWithBackup(configPath, temporaryPath, backupPath, sha256Hex(source)); errReplace != nil {
		return fmt.Errorf("atomically replace config: %w", errReplace)
	}
	actualConfig, errReadConfig := os.ReadFile(configPath)
	if errReadConfig != nil || sha256Hex(actualConfig) != sha256Hex(candidate) {
		return errors.New("post-replace config verification failed; exact backup is preserved")
	}
	actualBackup, errReadBackup := os.ReadFile(backupPath)
	if errReadBackup != nil || sha256Hex(actualBackup) != sha256Hex(source) {
		return errors.New("post-replace backup verification failed")
	}
	return nil
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func validSHA256(value string) bool {
	decoded, errDecode := hex.DecodeString(value)
	return errDecode == nil && len(decoded) == sha256.Size
}
