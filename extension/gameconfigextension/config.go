// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gameconfigextension

import (
	"errors"

	"go.opentelemetry.io/collector/config/confighttp"
)

// Config configures the SDK-facing listener, TLS, authentication, and CORS.
type Config struct {
	ServerConfig confighttp.ServerConfig `mapstructure:",squash"`
}

// Validate checks the SDK-facing listener configuration.
func (cfg *Config) Validate() error {
	if cfg.ServerConfig.NetAddr.Endpoint == "" {
		return errors.New("endpoint is required")
	}
	return nil
}
