// Copyright (c) 2022 NTT Communications Corporation
//
// This software is released under the MIT License.
// see https://github.com/nttcom/pola/blob/main/LICENSE

package config

import (
	"errors"
	"fmt"
	"net/netip"
	"os"

	"gopkg.in/yaml.v3"
)

type PCEP struct {
	Address string `yaml:"address"`
	Port    string `yaml:"port"`
	// FRRPeers lists peer addresses that must be treated as FRRouting rather than
	// auto-detected, since FRR cannot be distinguished from any other RFC-compliant
	// PCC via its OPEN message.
	FRRPeers []string `yaml:"frrPeers"`
	// NokiaPeers lists peer addresses that must be treated as Nokia SR OS rather
	// than auto-detected, since Nokia cannot be distinguished from any other
	// RFC-compliant PCC via its OPEN message.
	NokiaPeers []string `yaml:"nokiaPeers"`
}

type GRPCServer struct {
	Address string `yaml:"address"`
	Port    string `yaml:"port"`
}

type GRPCClient struct {
	Address string `yaml:"address"`
	Port    string `yaml:"port"`
}

type Log struct {
	Path  string `yaml:"path"`
	Name  string `yaml:"name"`
	Debug bool   `yaml:"debug"`
}

type GoBGP struct {
	GRPCClient GRPCClient `yaml:"grpcClient"`
}

type TED struct {
	Enable bool   `yaml:"enable"`
	ASN    uint32 `yaml:"asn"`
	Source string `yaml:"source"`
}

// defaultIntentPersistencePath is used whenever IntentPersistence.Path is
// unset, whether the whole section was omitted or Path was simply left
// blank.
const defaultIntentPersistencePath = "/var/lib/pola/intents.json"

// IntentPersistence configures durable storage of SR policy intent
// (type/metric) so it survives a polad restart. Enabled by default -
// Enable is a *bool (like Global.TED) specifically so "omitted from the
// config" and "explicitly set to false" are distinguishable; only the
// latter turns it off. Existing polad.yaml files with no intentPersistence
// section at all get the feature on, at defaultIntentPersistencePath.
type IntentPersistence struct {
	Enable *bool  `yaml:"enable"`
	Path   string `yaml:"path"`
}

// Enabled reports whether intent persistence should be active: true unless
// explicitly disabled with `enable: false`.
func (ip IntentPersistence) Enabled() bool {
	return ip.Enable == nil || *ip.Enable
}

// ResolvedPath returns Path, falling back to defaultIntentPersistencePath
// when it's unset.
func (ip IntentPersistence) ResolvedPath() string {
	if ip.Path != "" {
		return ip.Path
	}
	return defaultIntentPersistencePath
}

// defaultNodeExclusionPersistencePath is used whenever
// NodeExclusionPersistence.Path is unset, same fallback rule as
// defaultIntentPersistencePath.
const defaultNodeExclusionPersistencePath = "/var/lib/pola/node-exclusions.json"

// NodeExclusionPersistence configures the global node-exclusion set (avoid
// a router in every dynamically-computed policy server-wide, independent of
// any single policy's own exclude) and its durable storage, so it survives
// a polad restart. Enabled by default, same *bool convention as
// IntentPersistence - see its comment for why.
type NodeExclusionPersistence struct {
	Enable *bool  `yaml:"enable"`
	Path   string `yaml:"path"`
}

// Enabled reports whether the global node-exclusion feature should be
// active: true unless explicitly disabled with `enable: false`.
func (nep NodeExclusionPersistence) Enabled() bool {
	return nep.Enable == nil || *nep.Enable
}

// ResolvedPath returns Path, falling back to
// defaultNodeExclusionPersistencePath when it's unset.
func (nep NodeExclusionPersistence) ResolvedPath() string {
	if nep.Path != "" {
		return nep.Path
	}
	return defaultNodeExclusionPersistencePath
}

type Global struct {
	PCEP                     PCEP                     `yaml:"pcep"`
	GRPCServer               GRPCServer               `yaml:"grpcServer"`
	Log                      Log                      `yaml:"log"`
	TED                      *TED                     `yaml:"ted"`
	GoBGP                    GoBGP                    `yaml:"gobgp"`
	USidMode                 bool                     `yaml:"usidMode"`
	IntentPersistence        IntentPersistence        `yaml:"intentPersistence"`
	NodeExclusionPersistence NodeExclusionPersistence `yaml:"nodeExclusionPersistence"`
}

type Config struct {
	Global Global `yaml:"global"`
}

func ReadConfigFile(configFile string) (Config, error) {
	c := &Config{}

	f, err := os.Open(configFile)
	if err != nil {
		return *c, err
	}
	defer func() {
		if err := f.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to close file \"%s\": %v\n", configFile, err)
		}
	}()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return *c, fmt.Errorf("failed to parse config file %q: %w", configFile, err)
	}
	return *c, nil
}

// Validate checks required configuration fields.
func (c *Config) Validate() error {
	var errs []error

	if c.Global.PCEP.Address == "" {
		errs = append(errs, errors.New("global.pcep.address is required"))
	}
	if c.Global.PCEP.Port == "" {
		errs = append(errs, errors.New("global.pcep.port is required"))
	}
	for _, peer := range c.Global.PCEP.FRRPeers {
		if _, err := netip.ParseAddr(peer); err != nil {
			errs = append(errs, fmt.Errorf("global.pcep.frrPeers contains invalid address %q: %w", peer, err))
		}
	}
	for _, peer := range c.Global.PCEP.NokiaPeers {
		if _, err := netip.ParseAddr(peer); err != nil {
			errs = append(errs, fmt.Errorf("global.pcep.nokiaPeers contains invalid address %q: %w", peer, err))
		}
	}
	if c.Global.GRPCServer.Address == "" {
		errs = append(errs, errors.New("global.grpcServer.address is required"))
	}
	if c.Global.GRPCServer.Port == "" {
		errs = append(errs, errors.New("global.grpcServer.port is required"))
	}
	if c.Global.Log.Path == "" {
		errs = append(errs, errors.New("global.log.path is required"))
	}
	if c.Global.Log.Name == "" {
		errs = append(errs, errors.New("global.log.name is required"))
	}
	if c.Global.TED == nil {
		errs = append(errs, errors.New("global.ted is required"))
	} else if c.Global.TED.Enable {
		if c.Global.TED.Source == "" {
			errs = append(errs, errors.New("global.ted.source is required when global.ted.enable is true"))
		}
		if c.Global.TED.ASN == 0 {
			errs = append(errs, errors.New("global.ted.asn is required when global.ted.enable is true"))
		}
		if c.Global.TED.Source == "gobgp" {
			if c.Global.GoBGP.GRPCClient.Address == "" {
				errs = append(errs, errors.New("global.gobgp.grpcClient.address is required when global.ted.source is gobgp"))
			}
			if c.Global.GoBGP.GRPCClient.Port == "" {
				errs = append(errs, errors.New("global.gobgp.grpcClient.port is required when global.ted.source is gobgp"))
			}
		}
	}

	return errors.Join(errs...)
}
