// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gameexporter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validConfig = `{"config_version":12,"reporting_enabled":false,"disabled_events":["debug_perf_sample"],"refresh_seconds":43200}`

func TestRemoteConfigSignedGET(t *testing.T) {
	t.Parallel()
	now := time.Unix(1791331200, 0).UTC()
	e := testAdapter(doerFunc(func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, http.MethodGet, req.Method)
		assert.Equal(t, "https://testing.invalid"+configPath, req.URL.String())
		assert.Equal(t, http.NoBody, req.Body)
		assert.Empty(t, req.Header.Get("Content-Encoding"))
		assert.Equal(t, "demo", req.Header.Get("X-App-ID"))
		assert.Equal(t, "1791331200", req.Header.Get("X-Timestamp"))
		assert.Equal(t, "v1=dcaaa199cc52a6d261d7d3135b671e0b7e04081c773db677675712f7034d81cd", req.Header.Get("X-Signature"))
		resp := httpResponse(http.StatusOK, validConfig)
		resp.Header.Set("Date", now.Add(10*time.Minute).Format(http.TimeFormat))
		return resp, nil
	}))
	e.client.now = func() time.Time { return now }
	response, err := e.client.fetchConfig(t.Context())
	require.NoError(t, err)
	assert.Equal(t, validConfig, string(response.body))
	assert.Equal(t, int64(600), e.client.clockOffset.Load())
}

func TestConfigHandlerForwardsResponseAndReplacesCredentials(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			calls := 0
			body := validConfig
			if status != http.StatusOK {
				body = `{"request_id":"request-1","error":"server_busy"}`
			}
			e := testAdapter(doerFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				assert.Equal(t, "demo", req.Header.Get("X-App-ID"))
				assert.NotEqual(t, "sdk-signature", req.Header.Get("X-Signature"))
				assert.Empty(t, req.Header.Get("Authorization"))
				assert.Empty(t, req.Header.Get("Cookie"))
				assert.Empty(t, req.Header.Get("Origin"), "client-controlled headers must not reach platform")
				resp := httpResponse(status, body)
				resp.Header.Set("Retry-After", "1")
				resp.Header.Set("Date", "Tue, 27 Oct 2026 12:00:00 GMT")
				resp.Header.Set("X-Request-ID", "request-1")
				resp.Header.Set("Set-Cookie", "platform-cookie=private")
				resp.Header.Set("X-Signature", "upstream-private-signature")
				return resp, nil
			}))
			handler := &configHandler{client: e.client, timeout: time.Second, gate: make(chan struct{}, 1)}
			req := httptest.NewRequest(http.MethodGet, configPath, http.NoBody)
			req.Header.Set("X-App-ID", "sdk-app")
			req.Header.Set("X-Signature", "sdk-signature")
			req.Header.Set("Authorization", "Bearer sdk-token")
			req.Header.Set("Origin", "https://other.invalid")
			req.Header.Set("Cookie", "sdk-cookie=secret")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			assert.Equal(t, status, w.Code)
			assert.Equal(t, body, w.Body.String())
			assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
			assert.Equal(t, "1", w.Header().Get("Retry-After"))
			assert.Equal(t, "request-1", w.Header().Get("X-Request-ID"))
			assert.Empty(t, w.Header().Get("Set-Cookie"))
			assert.Empty(t, w.Header().Get("X-Signature"))
			assert.Equal(t, 1, calls)
		})
	}
}

func TestRemoteConfigValidation(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"missing": "{}", "null": "null", "malformed": "{", "trailing": validConfig + "{}",
		"missing_version":    `{"reporting_enabled":true,"disabled_events":[],"refresh_seconds":43200}`,
		"null_enabled":       `{"config_version":0,"reporting_enabled":null,"disabled_events":[],"refresh_seconds":43200}`,
		"null_disabled":      `{"config_version":0,"reporting_enabled":true,"disabled_events":null,"refresh_seconds":43200}`,
		"negative_version":   `{"config_version":-1,"reporting_enabled":true,"disabled_events":[],"refresh_seconds":43200}`,
		"fractional_version": `{"config_version":1.0,"reporting_enabled":true,"disabled_events":[],"refresh_seconds":43200}`,
		"refresh_low":        `{"config_version":0,"reporting_enabled":true,"disabled_events":[],"refresh_seconds":299}`,
		"refresh_high":       `{"config_version":0,"reporting_enabled":true,"disabled_events":[],"refresh_seconds":86401}`,
		"duplicate_keys":     `{"config_version":0,"reporting_enabled":true,"reporting_enabled":false,"disabled_events":[],"refresh_seconds":43200}`,
		"null_event":         `{"config_version":0,"reporting_enabled":true,"disabled_events":[null],"refresh_seconds":43200}`,
		"wrong_event_type":   `{"config_version":0,"reporting_enabled":true,"disabled_events":[123],"refresh_seconds":43200}`,
	} {
		t.Run(name, func(t *testing.T) { t.Parallel(); require.Error(t, validateRemoteConfig([]byte(body))) })
	}
	for _, refresh := range []int{300, 43200, 86400} {
		body := strings.ReplaceAll(validConfig, "43200", strconv.Itoa(refresh))
		require.NoError(t, validateRemoteConfig([]byte(body)))
	}
	require.NoError(t, validateRemoteConfig([]byte(`{"config_version":0,"reporting_enabled":true,"disabled_events":[],"refresh_seconds":43200}`)))
}

func TestConfigRelayFailureDoesNotFabricateAnEnabledConfig(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		response *http.Response
		err      error
	}{
		{name: "invalid_config", response: httpResponse(http.StatusOK, "{}")},
		{name: "oversized", response: httpResponse(http.StatusOK, strings.Repeat(" ", maxResponseBytes+1))},
		{name: "redirect", response: httpResponse(http.StatusFound, "")},
		{name: "empty_success", response: httpResponse(http.StatusNoContent, "")},
		{name: "network", err: errors.New("private upstream error")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := testAdapter(doerFunc(func(*http.Request) (*http.Response, error) { return tc.response, tc.err }))
			handler := &configHandler{client: e.client, timeout: time.Second, gate: make(chan struct{}, 1)}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, configPath, http.NoBody))
			assert.Equal(t, http.StatusBadGateway, w.Code)
			assert.NotContains(t, w.Body.String(), "private upstream")
			assert.NotContains(t, w.Body.String(), "reporting_enabled")
		})
	}
}

func TestConfigRelayPropagatesCancellation(t *testing.T) {
	t.Parallel()
	observed := make(chan error, 1)
	e := testAdapter(doerFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		observed <- req.Context().Err()
		return nil, req.Context().Err()
	}))
	handler := &configHandler{client: e.client, timeout: 20 * time.Millisecond, gate: make(chan struct{}, 1)}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, configPath, http.NoBody))
	assert.Equal(t, http.StatusGatewayTimeout, w.Code)
	assert.ErrorIs(t, <-observed, context.DeadlineExceeded)
}

func TestConfigRelayBusyRequestCanBeCancelled(t *testing.T) {
	t.Parallel()
	e := testAdapter(doerFunc(func(*http.Request) (*http.Response, error) {
		t.Error("must not call platform while busy")
		return nil, io.EOF
	}))
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	handler := &configHandler{client: e.client, timeout: time.Second, gate: gate}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequestWithContext(ctx, http.MethodGet, configPath, http.NoBody))
	assert.Equal(t, http.StatusGatewayTimeout, w.Code)
}

func TestConfigRelayCancelsInFlightPlatformRequest(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var observed error
	e := testAdapter(doerFunc(func(req *http.Request) (*http.Response, error) {
		cancel()
		<-req.Context().Done()
		observed = req.Context().Err()
		return nil, observed
	}))
	handler := &configHandler{client: e.client, timeout: time.Second, gate: make(chan struct{}, 1)}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequestWithContext(ctx, http.MethodGet, configPath, http.NoBody))
	require.ErrorIs(t, observed, context.Canceled)
	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.NotContains(t, w.Body.String(), "reporting_enabled")
}
