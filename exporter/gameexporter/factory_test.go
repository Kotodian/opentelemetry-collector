// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gameexporter

import (
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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumererror"
	"go.opentelemetry.io/collector/exporter/exportertest"
)

func TestFactoryHTTPS(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, eventsPath, r.URL.Path)
		assert.Equal(t, "gzip", r.Header.Get("Content-Encoding"))
		assert.Equal(t, "demo", r.Header.Get("X-App-ID"))
		assert.NotEmpty(t, r.Header.Get("X-Signature"))
		reader, err := gzip.NewReader(r.Body)
		if !assert.NoError(t, err) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer reader.Close()
		var payload struct {
			Events []json.RawMessage `json:"events"`
		}
		if !assert.NoError(t, json.NewDecoder(reader).Decode(&payload)) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, err = io.WriteString(w, fullACK(len(payload.Events)))
		assert.NoError(t, err)
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.ClientConfig.Endpoint = server.URL
	ca := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600))
	cfg.ClientConfig.TLS.CAFile = ca
	factory := NewFactory()
	require.Equal(t, "game", factory.Type().String())
	exp, err := factory.CreateLogs(t.Context(), exportertest.NewNopSettings(factory.Type()), cfg)
	require.NoError(t, err)
	require.NoError(t, exp.Start(t.Context(), componenttest.NewNopHost()))
	assert.False(t, exp.Capabilities().MutatesData)
	require.NoError(t, exp.ConsumeLogs(t.Context(), testLogs(3)))
	require.NoError(t, exp.Shutdown(t.Context()))
	assert.Equal(t, int32(1), calls.Load())
}

func TestFactoryDoesNotFollowRedirect(t *testing.T) {
	t.Parallel()
	var redirected atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer target.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.ClientConfig.Endpoint = server.URL
	ca := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600))
	cfg.ClientConfig.TLS.CAFile = ca
	factory := NewFactory()
	exp, err := factory.CreateLogs(t.Context(), exportertest.NewNopSettings(factory.Type()), cfg)
	require.NoError(t, err)
	require.NoError(t, exp.Start(t.Context(), componenttest.NewNopHost()))
	err = exp.ConsumeLogs(t.Context(), testLogs(1))
	require.Error(t, err)
	assert.True(t, consumererror.IsPermanent(err))
	require.NoError(t, exp.Shutdown(t.Context()))
	assert.Zero(t, redirected.Load())
}
