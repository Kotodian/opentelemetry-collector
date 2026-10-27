// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gameexporter

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/config/configopaque"
)

const eventsPath = "/analytics/v1/events"

var appIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// Config configures HTTP transport and platform application credentials.
type Config struct {
	ClientConfig confighttp.ClientConfig `mapstructure:",squash"`
	AppID        string                  `mapstructure:"app_id"`
	AppSecret    configopaque.String     `mapstructure:"app_secret"`
	// ConfigExtension identifies the SDK-facing configuration relay. Empty disables it.
	ConfigExtension component.ID `mapstructure:"config_extension"`
}

var _ component.Config = (*Config)(nil)

// Validate rejects configuration that would change the signed wire protocol.
func (cfg *Config) Validate() error {
	if _, err := cfg.eventsURL(); err != nil {
		return err
	}
	if !appIDPattern.MatchString(cfg.AppID) {
		return errors.New("app_id must match [A-Za-z0-9_.-]{1,128}")
	}
	if cfg.AppSecret == "" {
		return errors.New("app_secret is required")
	}
	if cfg.ClientConfig.Timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	if cfg.ClientConfig.Compression != "" && cfg.ClientConfig.Compression != "gzip" && cfg.ClientConfig.Compression != "none" {
		return errors.New("only gzip compression is supported; the exporter compresses before signing")
	}
	if cfg.ClientConfig.Auth.HasValue() || len(cfg.ClientConfig.Middlewares) > 0 {
		return errors.New("auth and middlewares cannot override platform signing")
	}
	for name := range cfg.ClientConfig.Headers.Iter {
		if !strings.EqualFold(name, "Origin") {
			return fmt.Errorf("only the %s header may be configured", http.CanonicalHeaderKey("Origin"))
		}
	}
	return cfg.ClientConfig.Validate()
}

func (cfg *Config) eventsURL() (string, error) {
	u, err := url.Parse(cfg.ClientConfig.Endpoint)
	if err != nil {
		return "", errors.New("endpoint must be a valid HTTPS URL")
	}
	if u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" {
		return "", errors.New("endpoint must use HTTPS without credentials, query, or fragment")
	}
	if u.Path != "" && u.Path != "/" && u.Path != eventsPath {
		return "", errors.New("endpoint path must be empty, /, or /analytics/v1/events")
	}
	u.Path = eventsPath
	return u.String(), nil
}
