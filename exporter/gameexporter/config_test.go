// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gameexporter

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/confmap"
)

func testConfig() *Config {
	cfg := createDefaultConfig().(*Config)
	cfg.ClientConfig.Endpoint = "https://analysis-dev.youwo.com:8000"
	cfg.AppID = "demo"
	cfg.AppSecret = "example-secret"
	return cfg
}

func TestConfig(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	require.NoError(t, componenttest.CheckConfigStruct(cfg))
	require.NoError(t, cfg.Validate())
	for _, tc := range []struct {
		name   string
		change func(*Config)
	}{
		{"empty_endpoint", func(c *Config) { c.ClientConfig.Endpoint = "" }},
		{"http", func(c *Config) { c.ClientConfig.Endpoint = "http://localhost" }},
		{"query", func(c *Config) { c.ClientConfig.Endpoint += "?x=y" }},
		{"empty_query", func(c *Config) { c.ClientConfig.Endpoint += "?" }},
		{"fragment", func(c *Config) { c.ClientConfig.Endpoint += "#x" }},
		{"wrong_path", func(c *Config) { c.ClientConfig.Endpoint += "/v1/logs" }},
		{"trailing_slash", func(c *Config) { c.ClientConfig.Endpoint += eventsPath + "/" }},
		{"userinfo", func(c *Config) { c.ClientConfig.Endpoint = "https://user:pass@testing.invalid" }},
		{"encoded_path", func(c *Config) { c.ClientConfig.Endpoint = "https://testing.invalid/analytics/v1/%65vents" }},
		{"bad_app_id", func(c *Config) { c.AppID = "bad id" }},
		{"empty_secret", func(c *Config) { c.AppSecret = "" }},
		{"no_timeout", func(c *Config) { c.ClientConfig.Timeout = 0 }},
		{"compression", func(c *Config) { c.ClientConfig.Compression = "zstd" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig()
			tc.change(cfg)
			require.Error(t, cfg.Validate())
		})
	}
	for _, endpoint := range []string{"https://testing.invalid", "https://testing.invalid/", "https://testing.invalid" + eventsPath} {
		cfg := testConfig()
		cfg.ClientConfig.Endpoint = endpoint
		require.NoError(t, cfg.Validate())
		target, err := cfg.eventsURL()
		require.NoError(t, err)
		assert.Equal(t, "https://testing.invalid"+eventsPath, target)
	}
}

func TestConfigUnmarshal(t *testing.T) {
	t.Parallel()
	cfg := createDefaultConfig().(*Config)
	conf := confmap.NewFromStringMap(map[string]any{
		"endpoint": "https://testing.invalid", "app_id": "demo", "app_secret": "example-secret", "timeout": "20s", "compression": "gzip", "config_extension": "game_config/mobile",
	})
	require.NoError(t, conf.Unmarshal(cfg))
	require.NoError(t, cfg.Validate())
	assert.Equal(t, "game_config/mobile", cfg.ConfigExtension.String())
	assert.Equal(t, "demo", cfg.AppID)
	assert.Equal(t, "example-secret", string(cfg.AppSecret))
}
