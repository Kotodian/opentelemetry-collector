// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gameconfigextension

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/extension/extensiontest"
)

func testExtension() *configExtension {
	return &configExtension{config: createDefaultConfig().(*Config), telemetry: componenttest.NewNopTelemetrySettings()}
}

func TestRelayRegistrationAndRouting(t *testing.T) {
	t.Parallel()
	e := testExtension()
	var calls atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, err := io.WriteString(w, `{"reporting_enabled":false}`)
		assert.NoError(t, err)
	})
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, configPath, http.NoBody))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	unregister, err := e.RegisterConfigHandler(handler)
	require.NoError(t, err)
	_, err = e.RegisterConfigHandler(handler)
	require.Error(t, err, "different applications must not share a listener")
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{http.MethodGet, configPath, "", http.StatusOK},
		{http.MethodPost, configPath, "", http.StatusMethodNotAllowed},
		{http.MethodHead, configPath, "", http.StatusMethodNotAllowed},
		{http.MethodGet, configPath + "?app_id=other", "", http.StatusBadRequest},
		{http.MethodGet, configPath + "?", "", http.StatusBadRequest},
		{http.MethodGet, configPath, "body", http.StatusBadRequest},
		{http.MethodGet, configPath + "/", "", http.StatusNotFound},
		{http.MethodGet, "/other", "", http.StatusNotFound},
	} {
		w = httptest.NewRecorder()
		e.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		assert.Equal(t, tc.status, w.Code)
		assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	}
	assert.Equal(t, int32(1), calls.Load())
	unregister()
	unregister()
	w = httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, configPath, http.NoBody))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	unregister, err = e.RegisterConfigHandler(handler)
	require.NoError(t, err)
	require.NoError(t, e.Shutdown(t.Context()))
	unregister()
	_, err = e.RegisterConfigHandler(handler)
	require.Error(t, err)
}

func TestExtensionConfigAndLifecycle(t *testing.T) {
	t.Parallel()
	cfg := createDefaultConfig().(*Config)
	require.NoError(t, componenttest.CheckConfigStruct(cfg))
	conf := confmap.NewFromStringMap(map[string]any{"endpoint": "localhost:0"})
	require.NoError(t, conf.Unmarshal(cfg))
	require.NoError(t, cfg.Validate())
	factory := NewFactory()
	ext, err := factory.Create(t.Context(), extensiontest.NewNopSettings(factory.Type()), cfg)
	require.NoError(t, err)
	require.NoError(t, ext.Start(t.Context(), componenttest.NewNopHost()))
	require.NoError(t, ext.Shutdown(t.Context()))
	cfg.ServerConfig.NetAddr.Endpoint = ""
	require.Error(t, cfg.Validate())
}
