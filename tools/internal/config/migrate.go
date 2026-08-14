package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// PriorSchemaVersion is the schema version this engine can migrate from.
const PriorSchemaVersion = 1

// configV1 is the exact v1 schema. It mirrors the v2 Config but carries
// GitHubApp instead of Publication/Compatibility and omits ForbiddenModules
// from Dependencies. Strict decoding into this struct catches unknown fields,
// duplicates, and multi-document inputs — the same guarantees Decode provides
// for v2.
type configV1 struct {
	Version      int            `yaml:"version"`
	Source       Source         `yaml:"source"`
	Destination  Destination    `yaml:"destination"`
	Packages     Packages       `yaml:"packages"`
	Prune        Prune          `yaml:"prune"`
	Deny         Deny           `yaml:"deny"`
	Closure      Closure        `yaml:"closure"`
	Types        Types          `yaml:"types"`
	Dependencies dependenciesV1 `yaml:"dependencies"`
	Patches      []Patch        `yaml:"patches"`
	Facade       Facade         `yaml:"facade"`
	Release      Release        `yaml:"release"`
	Commit       Commit         `yaml:"commit"`
	Vanity       Vanity         `yaml:"vanity"`
	GitHubApp    gitHubAppV1    `yaml:"githubApp"`
	Determinism  Determinism    `yaml:"determinism"`
}

type dependenciesV1 struct {
	Policy       string               `yaml:"policy"`
	CopyPackages []string             `yaml:"copyPackages"`
	Gates        DependencyGates      `yaml:"gates"`
	Overrides    []DependencyOverride `yaml:"overrides"`
}

type gitHubAppV1 struct {
	AppIDEnv          string `yaml:"appIDEnv"`
	InstallationIDEnv string `yaml:"installationIDEnv"`
	PrivateKeyEnv     string `yaml:"privateKeyEnv"`
	APIBaseURL        string `yaml:"apiBaseURL"`
}

// MigrateV1ToV2 reads a schema v1 profile, validates it strictly, and returns
// migrated v2 bytes. The migration sets publication.mode=manual,
// compatibility.apiserver=external, and forbiddenModules=[].
//
// The migration is deliberate and explicit: an operator upgrades by running
// "soapbox upgrade", which calls this function, reviews the manifest, and
// approves it. No silent migration happens during a normal load.
func MigrateV1ToV2(data []byte) ([]byte, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var v1 configV1
	if err := dec.Decode(&v1); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, &DecodeError{Reason: "document is empty"}
		}
		return nil, &DecodeError{Reason: "malformed document", Err: err}
	}

	// Reject multiple documents.
	var extra yaml.Node
	switch err := dec.Decode(&extra); {
	case err == nil:
		return nil, &DecodeError{Reason: "multiple YAML documents are not supported"}
	case errors.Is(err, io.EOF):
	default:
		return nil, &DecodeError{Reason: "malformed document", Err: err}
	}

	if v1.Version != PriorSchemaVersion {
		return nil, fmt.Errorf("migrate: profile version is %d, want %d for v1-to-v2 migration", v1.Version, PriorSchemaVersion)
	}

	// The v1 schema requires the githubApp section with valid, distinct
	// environment variable names.
	if v1.GitHubApp.AppIDEnv == "" || v1.GitHubApp.InstallationIDEnv == "" || v1.GitHubApp.PrivateKeyEnv == "" || v1.GitHubApp.APIBaseURL == "" {
		return nil, &DecodeError{Reason: "v1 profile requires appIDEnv, installationIDEnv, privateKeyEnv, and apiBaseURL in the githubApp section"}
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"githubApp.appIDEnv", v1.GitHubApp.AppIDEnv},
		{"githubApp.installationIDEnv", v1.GitHubApp.InstallationIDEnv},
		{"githubApp.privateKeyEnv", v1.GitHubApp.PrivateKeyEnv},
	} {
		if err := ValidateEnvName(field.value); err != nil {
			return nil, &DecodeError{Reason: fmt.Sprintf("%s: %v", field.name, err)}
		}
	}
	if v1.GitHubApp.AppIDEnv == v1.GitHubApp.InstallationIDEnv ||
		v1.GitHubApp.AppIDEnv == v1.GitHubApp.PrivateKeyEnv ||
		v1.GitHubApp.InstallationIDEnv == v1.GitHubApp.PrivateKeyEnv {
		return nil, &DecodeError{Reason: "githubApp environment variable names must all be distinct"}
	}
	if v1.GitHubApp.APIBaseURL != "" {
		if err := validateURL(v1.GitHubApp.APIBaseURL, urlRule{allowedHosts: []string{"api.github.com"}}); err != nil {
			return nil, &DecodeError{Reason: fmt.Sprintf("githubApp.apiBaseURL: %v", err)}
		}
	}

	// Construct the v2 Config from the validated v1 data.
	cfg := Config{
		Version:     SchemaVersion,
		Source:      v1.Source,
		Destination: v1.Destination,
		Packages:    v1.Packages,
		Prune:       v1.Prune,
		Deny:        v1.Deny,
		Closure:     v1.Closure,
		Types:       v1.Types,
		Dependencies: Dependencies{
			Policy:           v1.Dependencies.Policy,
			CopyPackages:     v1.Dependencies.CopyPackages,
			ForbiddenModules: []string{},
			Gates:            v1.Dependencies.Gates,
			Overrides:        v1.Dependencies.Overrides,
		},
		Patches: v1.Patches,
		Facade:  v1.Facade,
		Release: v1.Release,
		Commit:  v1.Commit,
		Vanity:  v1.Vanity,
		Publication: Publication{
			Mode: PublicationModeManual,
		},
		Compatibility: Compatibility{
			Apiserver: CompatibilityApiserverExternal,
		},
		Determinism: v1.Determinism,
	}

	cfg.normalize()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("migrated profile: %w", err)
	}

	return encodeYAML(&cfg)
}

// DecodeWithMigration decodes profile bytes, applying v1→v2 migration if
// needed. It returns the migrated bytes alongside the config so the caller can
// write them back.
func DecodeWithMigration(data []byte) (*Config, []byte, error) {
	// Try a quick version probe.
	version, err := probeVersion(data)
	if err != nil {
		// Fall through to Decode which will give a better error.
		cfg, decErr := Decode(data)
		return cfg, data, decErr
	}

	switch version {
	case SchemaVersion:
		cfg, err := Decode(data)
		return cfg, data, err
	case PriorSchemaVersion:
		migrated, err := MigrateV1ToV2(data)
		if err != nil {
			return nil, nil, err
		}
		cfg, err := Decode(migrated)
		if err != nil {
			return nil, nil, fmt.Errorf("migrated profile: %w", err)
		}
		return cfg, migrated, nil
	default:
		cfg, err := Decode(data)
		return cfg, data, err
	}
}

// probeVersion reads just the version field from YAML bytes.
func probeVersion(data []byte) (int, error) {
	var probe struct {
		Version int `yaml:"version"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return 0, err
	}
	return probe.Version, nil
}
