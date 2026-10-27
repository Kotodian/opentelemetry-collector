// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gameexporter

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/exporter/exporterhelper"
	"go.opentelemetry.io/collector/exporter/gameexporter/internal/metadata"
	"go.opentelemetry.io/collector/exporter/xexporter"
)

// NewFactory creates an exporter for prepared game events carried by OTLP logs.
func NewFactory() exporter.Factory {
	return xexporter.NewFactory(metadata.Type, createDefaultConfig,
		xexporter.WithLogs(createLogs, metadata.LogsStability))
}

func createDefaultConfig() component.Config {
	clientConfig := confighttp.NewDefaultClientConfig()
	clientConfig.Timeout = 20 * time.Second
	clientConfig.MaxConnsPerHost = 1
	return &Config{ClientConfig: clientConfig}
}

func createLogs(ctx context.Context, set exporter.Settings, config component.Config) (exporter.Logs, error) {
	cfg := config.(*Config)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	endpoint, err := cfg.eventsURL()
	if err != nil {
		return nil, err
	}
	adapter := &gameExporter{logger: set.Logger, client: &client{
		endpoint: endpoint, appID: cfg.AppID, secret: string(cfg.AppSecret), now: time.Now,
		gate: make(chan struct{}, 1),
	}}
	var transport *http.Client
	var unregister func()
	return exporterhelper.NewLogs(ctx, set, cfg, adapter.pushLogs,
		exporterhelper.WithCapabilities(consumer.Capabilities{MutatesData: false}),
		exporterhelper.WithTimeout(exporterhelper.TimeoutConfig{Timeout: 0}),
		exporterhelper.WithStart(func(ctx context.Context, host component.Host) error {
			var relay configRegistrar
			if cfg.ConfigExtension.Type().String() != "" {
				var ok bool
				relay, ok = host.GetExtensions()[cfg.ConfigExtension].(configRegistrar)
				if !ok {
					return fmt.Errorf("config_extension %q must be enabled and support configuration relay registration", cfg.ConfigExtension)
				}
			}
			clientConfig := cfg.ClientConfig
			// Compression must happen before signing, never in a RoundTripper.
			clientConfig.Compression = ""
			var startErr error
			transport, startErr = clientConfig.ToClient(ctx, host.GetExtensions(), set.TelemetrySettings)
			if startErr != nil {
				return startErr
			}
			transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			adapter.client.http = transport
			if relay != nil {
				unregister, startErr = relay.RegisterConfigHandler(&configHandler{client: adapter.client, timeout: clientConfig.Timeout, gate: make(chan struct{}, 1)})
				if startErr != nil {
					transport.CloseIdleConnections()
					return fmt.Errorf("register configuration relay: %w", startErr)
				}
			}
			return nil
		}),
		exporterhelper.WithShutdown(func(context.Context) error {
			if unregister != nil {
				unregister()
			}
			if transport != nil {
				transport.CloseIdleConnections()
			}
			return nil
		}),
	)
}
