// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package gameexporter

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/exporter/exportertest"
	"go.opentelemetry.io/collector/extension/extensiontest"
	"go.opentelemetry.io/collector/extension/gameconfigextension"
	"go.opentelemetry.io/collector/internal/testutil"
)

type relayHost map[component.ID]component.Component

func (h relayHost) GetExtensions() map[component.ID]component.Component { return h }

func TestFactoryConfigRelayIntegration(t *testing.T) {
	t.Parallel()
	var configCalls, eventCalls atomic.Int32
	var enabled atomic.Bool
	enabled.Store(true)
	platform := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "demo", r.Header.Get("X-App-ID"))
		assert.Empty(t, r.Header.Get("Authorization"))
		assert.Empty(t, r.Header.Get("Cookie"))
		assert.Equal(t, "https://game.example", r.Header.Get("Origin"))
		wire, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}
		encoding := "identity"
		if r.URL.Path == eventsPath {
			encoding = "gzip"
		}
		assert.Equal(t, signature(r.Method, r.URL.Path, "demo", r.Header.Get("X-Timestamp"), encoding, "example-secret", wire), r.Header.Get("X-Signature"))
		switch r.URL.Path {
		case configPath:
			version := configCalls.Add(1)
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Empty(t, wire)
			assert.Empty(t, r.URL.RawQuery)
			w.Header().Set("X-Request-ID", "relay-integration")
			err = json.NewEncoder(w).Encode(map[string]any{
				"config_version": version, "reporting_enabled": enabled.Load(),
				"disabled_events": []string{"debug_perf_sample"}, "refresh_seconds": 300,
			})
			assert.NoError(t, err)
		case eventsPath:
			eventCalls.Add(1)
			reader, readErr := gzip.NewReader(bytes.NewReader(wire))
			if !assert.NoError(t, readErr) {
				return
			}
			defer reader.Close()
			var payload struct{ Events []json.RawMessage }
			if !assert.NoError(t, json.NewDecoder(reader).Decode(&payload)) {
				return
			}
			assert.Len(t, payload.Events, 1)
			_, err = io.WriteString(w, fullACK(1))
			assert.NoError(t, err)
		default:
			t.Errorf("unexpected platform path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(platform.Close)
	extFactory := gameconfigextension.NewFactory()
	extConfig := extFactory.CreateDefaultConfig().(*gameconfigextension.Config)
	extConfig.ServerConfig.NetAddr.Endpoint = testutil.GetAvailableLocalAddress(t)
	ext, err := extFactory.Create(t.Context(), extensiontest.NewNopSettings(extFactory.Type()), extConfig)
	require.NoError(t, err)
	extID := component.NewID(extFactory.Type())
	host := relayHost{extID: ext}
	require.NoError(t, ext.Start(t.Context(), host))
	t.Cleanup(func() { require.NoError(t, ext.Shutdown(t.Context())) })
	cfg := testConfig()
	cfg.ConfigExtension = extID
	cfg.ClientConfig.Endpoint = platform.URL + eventsPath
	cfg.ClientConfig.Headers.Set("Origin", "https://game.example")
	ca := filepath.Join(t.TempDir(), "platform-ca.pem")
	require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: platform.Certificate().Raw}), 0o600))
	cfg.ClientConfig.TLS.CAFile = ca
	factory := NewFactory()
	exp, err := factory.CreateLogs(t.Context(), exportertest.NewNopSettings(factory.Type()), cfg)
	require.NoError(t, err)
	require.NoError(t, exp.Start(t.Context(), host))
	t.Cleanup(func() { require.NoError(t, exp.Shutdown(t.Context())) })
	sdk := &http.Client{Timeout: time.Second}
	t.Cleanup(sdk.CloseIdleConnections)
	for _, wantEnabled := range []bool{true, false, true} {
		enabled.Store(wantEnabled)
		req, reqErr := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+extConfig.ServerConfig.NetAddr.Endpoint+configPath, http.NoBody)
		require.NoError(t, reqErr)
		req.Header.Set("Authorization", "Bearer collector-only")
		req.Header.Set("Cookie", "sdk-cookie=private")
		req.Header.Set("X-Signature", "sdk-must-not-sign-platform-requests")
		response, requestErr := sdk.Do(req)
		require.NoError(t, requestErr)
		var payload map[string]any
		require.NoError(t, json.NewDecoder(response.Body).Decode(&payload))
		require.NoError(t, response.Body.Close())
		assert.Equal(t, http.StatusOK, response.StatusCode)
		assert.Equal(t, wantEnabled, payload["reporting_enabled"])
		assert.Equal(t, "no-store", response.Header.Get("Cache-Control"))
		assert.Equal(t, "relay-integration", response.Header.Get("X-Request-ID"))
	}
	require.NoError(t, exp.ConsumeLogs(t.Context(), testLogs(1)))
	assert.Equal(t, int32(3), configCalls.Load())
	assert.Equal(t, int32(1), eventCalls.Load())
	other, err := factory.CreateLogs(t.Context(), exportertest.NewNopSettings(factory.Type()), cfg)
	require.NoError(t, err)
	require.ErrorContains(t, other.Start(t.Context(), host), "already has an exporter")
	require.NoError(t, other.Shutdown(t.Context()))
	require.NoError(t, exp.Shutdown(t.Context()))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+extConfig.ServerConfig.NetAddr.Endpoint+configPath, http.NoBody)
	require.NoError(t, err)
	response, err := sdk.Do(req)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
}

func TestFactoryRequiresConfigRelayExtension(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		host component.Host
	}{
		{"missing", componenttest.NewNopHost()},
		{"unsupported", relayHost{component.MustNewID("game_config"): nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig()
			cfg.ConfigExtension = component.MustNewID("game_config")
			factory := NewFactory()
			exp, err := factory.CreateLogs(t.Context(), exportertest.NewNopSettings(factory.Type()), cfg)
			require.NoError(t, err)
			require.ErrorContains(t, exp.Start(t.Context(), tc.host), "must be enabled and support")
			require.NoError(t, exp.Shutdown(t.Context()))
		})
	}
}
