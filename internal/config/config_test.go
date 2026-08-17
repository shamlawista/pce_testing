// Copyright (c) 2022 NTT Communications Corporation
//
// This software is released under the MIT License.
// see https://github.com/nttcom/pola/blob/main/LICENSE

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "polad.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}
	return path
}

const validConfig = `
global:
  pcep:
    address: "127.0.0.1"
    port: 4189
  grpcServer:
    address: "127.0.0.1"
    port: 50052
  log:
    path: "/var/log/pola/"
    name: "polad.log"
  ted:
    enable: true
    source: "gobgp"
    asn: 65000
  gobgp:
    grpcClient:
      address: "127.0.0.1"
      port: 50051
  usidMode: true
`

func TestReadConfigFile_Valid(t *testing.T) {
	path := writeConfig(t, validConfig)

	c, err := ReadConfigFile(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Global.GRPCServer.Address != "127.0.0.1" || c.Global.GRPCServer.Port != "50052" {
		t.Errorf("unexpected GRPCServer: %+v", c.Global.GRPCServer)
	}
	if c.Global.TED == nil || !c.Global.TED.Enable || c.Global.TED.ASN != 65000 {
		t.Errorf("unexpected TED: %+v", c.Global.TED)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("unexpected validation error: %v", err)
	}
}

// Regression test: v1.3.0 configs used kebab-case keys (grpc-server, usid-mode,
// grpc-client), which were renamed to camelCase. These must now fail loudly
// instead of silently decoding to empty values.
func TestReadConfigFile_LegacyKebabCaseKeysRejected(t *testing.T) {
	legacyConfig := `
global:
  pcep:
    address: "127.0.0.1"
    port: 4189
  grpc-server:
    address: "127.0.0.1"
    port: 50052
  log:
    path: "/var/log/pola/"
    name: "polad.log"
  usid-mode: true
`
	path := writeConfig(t, legacyConfig)

	_, err := ReadConfigFile(path)
	if err == nil {
		t.Fatal("expected error for legacy kebab-case config, got nil")
	}
	if !strings.Contains(err.Error(), "grpc-server") {
		t.Errorf("expected error to mention the offending key \"grpc-server\", got: %v", err)
	}
}

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		wantErr bool
	}{
		{
			name:   "valid, TED enabled",
			config: validConfig,
		},
		{
			name: "valid, TED disabled without source/asn",
			config: `
global:
  pcep:
    address: "127.0.0.1"
    port: 4189
  grpcServer:
    address: "127.0.0.1"
    port: 50052
  log:
    path: "/var/log/pola/"
    name: "polad.log"
  ted:
    enable: false
`,
		},
		{
			name: "missing grpcServer.address",
			config: `
global:
  pcep:
    address: "127.0.0.1"
    port: 4189
  grpcServer:
    port: 50052
  log:
    path: "/var/log/pola/"
    name: "polad.log"
  ted:
    enable: false
`,
			wantErr: true,
		},
		{
			name: "missing grpcServer section entirely",
			config: `
global:
  pcep:
    address: "127.0.0.1"
    port: 4189
  log:
    path: "/var/log/pola/"
    name: "polad.log"
  ted:
    enable: false
`,
			wantErr: true,
		},
		{
			name: "missing ted section",
			config: `
global:
  pcep:
    address: "127.0.0.1"
    port: 4189
  grpcServer:
    address: "127.0.0.1"
    port: 50052
  log:
    path: "/var/log/pola/"
    name: "polad.log"
`,
			wantErr: true,
		},
		{
			name: "ted enabled without asn",
			config: `
global:
  pcep:
    address: "127.0.0.1"
    port: 4189
  grpcServer:
    address: "127.0.0.1"
    port: 50052
  log:
    path: "/var/log/pola/"
    name: "polad.log"
  ted:
    enable: true
    source: "gobgp"
`,
			wantErr: true,
		},
		{
			name: "valid frrPeers",
			config: `
global:
  pcep:
    address: "127.0.0.1"
    port: 4189
    frrPeers:
      - "10.0.0.1"
      - "10.0.0.2"
  grpcServer:
    address: "127.0.0.1"
    port: 50052
  log:
    path: "/var/log/pola/"
    name: "polad.log"
  ted:
    enable: false
`,
		},
		{
			name: "invalid frrPeers entry",
			config: `
global:
  pcep:
    address: "127.0.0.1"
    port: 4189
    frrPeers:
      - "not-an-address"
  grpcServer:
    address: "127.0.0.1"
    port: 50052
  log:
    path: "/var/log/pola/"
    name: "polad.log"
  ted:
    enable: false
`,
			wantErr: true,
		},
		{
			name: "valid nokiaPeers",
			config: `
global:
  pcep:
    address: "127.0.0.1"
    port: 4189
    nokiaPeers:
      - "213.119.192.12"
  grpcServer:
    address: "127.0.0.1"
    port: 50052
  log:
    path: "/var/log/pola/"
    name: "polad.log"
  ted:
    enable: false
`,
		},
		{
			name: "invalid nokiaPeers entry",
			config: `
global:
  pcep:
    address: "127.0.0.1"
    port: 4189
    nokiaPeers:
      - "not-an-address"
  grpcServer:
    address: "127.0.0.1"
    port: 50052
  log:
    path: "/var/log/pola/"
    name: "polad.log"
  ted:
    enable: false
`,
			wantErr: true,
		},
		{
			name: "intentPersistence enabled without path",
			config: `
global:
  pcep:
    address: "127.0.0.1"
    port: 4189
  grpcServer:
    address: "127.0.0.1"
    port: 50052
  log:
    path: "/var/log/pola/"
    name: "polad.log"
  ted:
    enable: false
  intentPersistence:
    enable: true
`,
			wantErr: true,
		},
		{
			name: "intentPersistence enabled with path",
			config: `
global:
  pcep:
    address: "127.0.0.1"
    port: 4189
  grpcServer:
    address: "127.0.0.1"
    port: 50052
  log:
    path: "/var/log/pola/"
    name: "polad.log"
  ted:
    enable: false
  intentPersistence:
    enable: true
    path: "/var/lib/pola/intents.json"
`,
		},
		{
			name: "intentPersistence disabled without path",
			config: `
global:
  pcep:
    address: "127.0.0.1"
    port: 4189
  grpcServer:
    address: "127.0.0.1"
    port: 50052
  log:
    path: "/var/log/pola/"
    name: "polad.log"
  ted:
    enable: false
  intentPersistence:
    enable: false
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, tt.config)
			c, err := ReadConfigFile(path)
			if err != nil {
				t.Fatalf("unexpected read error: %v", err)
			}
			err = c.Validate()
			if tt.wantErr && err == nil {
				t.Error("expected validation error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected validation error: %v", err)
			}
		})
	}
}

// TestConfig_Validate_LegacyConfigWithoutIntentPersistence_StillValid pins
// the compatibility guarantee that motivated making IntentPersistence a
// plain (non-pointer) struct with Enable defaulting false, unlike TED:
// validConfig has no intentPersistence section at all and must keep
// validating with zero changes, exactly as every existing polad.yaml does.
func TestConfig_Validate_LegacyConfigWithoutIntentPersistence_StillValid(t *testing.T) {
	path := writeConfig(t, validConfig)

	c, err := ReadConfigFile(path)
	if err != nil {
		t.Fatalf("unexpected read error: %v", err)
	}
	if c.Global.IntentPersistence.Enable {
		t.Error("expected IntentPersistence.Enable to default to false when the section is omitted")
	}
	if err := c.Validate(); err != nil {
		t.Errorf("unexpected validation error for a config with no intentPersistence section: %v", err)
	}
}

func TestReadConfigFile_IntentPersistenceSection_ParsesExpectedFields(t *testing.T) {
	config := `
global:
  pcep:
    address: "127.0.0.1"
    port: 4189
  grpcServer:
    address: "127.0.0.1"
    port: 50052
  log:
    path: "/var/log/pola/"
    name: "polad.log"
  ted:
    enable: false
  intentPersistence:
    enable: true
    path: "/var/lib/pola/intents.json"
`
	path := writeConfig(t, config)

	c, err := ReadConfigFile(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !c.Global.IntentPersistence.Enable {
		t.Error("expected IntentPersistence.Enable to be true")
	}
	if c.Global.IntentPersistence.Path != "/var/lib/pola/intents.json" {
		t.Errorf("unexpected IntentPersistence.Path: %q", c.Global.IntentPersistence.Path)
	}
}

// yaml.v3 decodes an unquoted integer scalar into a string field without
// error; pin this behavior since Port is typed as string.
func TestReadConfigFile_UnquotedIntegerPort(t *testing.T) {
	path := writeConfig(t, validConfig)

	c, err := ReadConfigFile(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Global.GRPCServer.Port != "50052" {
		t.Errorf("expected Port %q, got %q", "50052", c.Global.GRPCServer.Port)
	}
}
