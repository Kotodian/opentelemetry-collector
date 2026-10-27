// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gameconfigextension

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/extension"
	"go.opentelemetry.io/collector/extension/gameconfigextension/internal/metadata"
)

// NewFactory creates the game configuration relay extension.
func NewFactory() extension.Factory {
	return extension.NewFactory(metadata.Type, createDefaultConfig, create, metadata.ExtensionStability)
}

func createDefaultConfig() component.Config {
	server := confighttp.NewDefaultServerConfig()
	server.NetAddr.Endpoint = "localhost:13134"
	server.ReadTimeout = 5 * time.Second
	server.ReadHeaderTimeout = 5 * time.Second
	server.MaxRequestBodySize = 1
	server.CompressionAlgorithms = []string{""}
	return &Config{ServerConfig: server}
}

func create(_ context.Context, set extension.Settings, config component.Config) (extension.Extension, error) {
	cfg := config.(*Config)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &configExtension{config: cfg, telemetry: set.TelemetrySettings}, nil
}
