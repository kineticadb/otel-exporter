// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package kineticaexporter

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap/confmaptest"
)

const defaultHost = "http://127.0.0.1:9191"

func TestLoadConfig(t *testing.T) {
	t.Parallel()

	cm, err := confmaptest.LoadConf(filepath.Join("testdata", "config.yaml"))
	require.NoError(t, err)

	defaultCfg := CreateDefaultConfig()

	tests := []struct {
		id       component.ID
		expected component.Config
	}{

		{
			id:       component.NewIDWithName("kinetica", ""),
			expected: defaultCfg,
		},
	}

	for _, tt := range tests {
		t.Run(tt.id.String(), func(t *testing.T) {
			factory := NewFactory()
			cfg := factory.CreateDefaultConfig()

			sub, err := cm.Sub(tt.id.String())
			require.NoError(t, err)
			require.NoError(t, component.UnmarshalConfig(sub, cfg))

			assert.NoError(t, component.ValidateConfig(cfg))
			assert.Equal(t, tt.expected, cfg)
		})
	}
}

func withDefaultConfig(fns ...func(*Config)) *Config {
	cfg := CreateDefaultConfig().(*Config)
	for _, fn := range fns {
		fn(cfg)
	}
	return cfg
}

func TestValidateMetricsSettings(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*Config)
		wantErr string
	}{
		{name: "default", modify: func(*Config) {}},
		{name: "v2", modify: func(cfg *Config) { cfg.MetricsSchema = MetricsSchemaV2 }},
		{name: "unset schema", modify: func(cfg *Config) { cfg.MetricsSchema = "" }},
		{name: "no text index options", modify: func(cfg *Config) { cfg.MetricsV2.LabelsAnalyzer = "" }},
		{
			name:    "unknown schema",
			modify:  func(cfg *Config) { cfg.MetricsSchema = "v3" },
			wantErr: "metricsschema",
		},
		{
			name:    "negative cache size",
			modify:  func(cfg *Config) { cfg.MetricsV2.SeriesCacheSize = -1 },
			wantErr: "seriescachesize",
		},
		{
			name:    "unknown analyzer",
			modify:  func(cfg *Config) { cfg.MetricsV2.LabelsAnalyzer = "stemmed" },
			wantErr: "labelsanalyzer",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := withDefaultConfig(tt.modify).Validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				assert.ErrorContains(t, err, tt.wantErr)
			}
		})
	}
}
