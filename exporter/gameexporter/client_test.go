// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gameexporter

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"go.opentelemetry.io/collector/consumer/consumererror"
)

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

func testAdapter(doer httpDoer) *gameExporter {
	return &gameExporter{logger: zap.NewNop(), client: &client{
		http: doer, endpoint: "https://testing.invalid" + eventsPath,
		appID: "demo", secret: "example-secret", now: time.Now, gate: make(chan struct{}, 1),
	}}
}

func httpResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func fullACK(n int) string {
	return fmt.Sprintf(`{"summary":{"received":%d,"accepted":%d,"rejected":0,"retryable":0},"results":[]}`, n, n)
}

func requestEvents(t *testing.T, req *http.Request) []json.RawMessage {
	t.Helper()
	wire, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.NoError(t, req.Body.Close())
	assert.LessOrEqual(t, len(wire), maxWireBytes)
	assert.Equal(t, http.MethodPost, req.Method)
	assert.Equal(t, eventsPath, req.URL.EscapedPath())
	assert.Empty(t, req.URL.RawQuery)
	assert.Equal(t, "gzip", req.Header.Get("Content-Encoding"))
	assert.Equal(t, "application/json", req.Header.Get("Content-Type"))
	assert.Equal(t, "demo", req.Header.Get("X-App-ID"))
	stamp := req.Header.Get("X-Timestamp")
	seconds, err := strconv.ParseInt(stamp, 10, 64)
	require.NoError(t, err)
	assert.Equal(t, stamp, strconv.FormatInt(seconds, 10))
	hash := sha256.Sum256(wire)
	canonical := fmt.Sprintf("GI-HMAC-SHA256-V1\nPOST\n%s\ndemo\n%s\napplication/json\ngzip\n%x", eventsPath, stamp, hash)
	mac := hmac.New(sha256.New, []byte("example-secret"))
	_, err = mac.Write([]byte(canonical))
	require.NoError(t, err)
	assert.Equal(t, "v1="+hex.EncodeToString(mac.Sum(nil)), req.Header.Get("X-Signature"))
	reader := bytes.NewReader(wire)
	gz, err := gzip.NewReader(reader)
	require.NoError(t, err)
	gz.Multistream(false)
	decoded, err := io.ReadAll(gz)
	require.NoError(t, err)
	require.NoError(t, gz.Close())
	assert.Zero(t, reader.Len(), "a request must contain exactly one gzip stream")
	assert.LessOrEqual(t, len(decoded), maxDecodedBytes)
	var envelope struct {
		Events []json.RawMessage `json:"events"`
	}
	require.NoError(t, json.Unmarshal(decoded, &envelope))
	assert.LessOrEqual(t, len(envelope.Events), maxEvents)
	return envelope.Events
}

func TestSignatureVectors(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "v1=4c1a183922754302fdcfaa46a83ad9007060a014e7566bab80f3a6be7539a82a",
		signature(http.MethodPost, eventsPath, "demo", "1791331200", "identity", "example-secret", []byte(`{"events":[]}`)))
	assert.Equal(t, "v1=dcaaa199cc52a6d261d7d3135b671e0b7e04081c773db677675712f7034d81cd",
		signature(http.MethodGet, "/analytics/v1/config", "demo", "1791331200", "identity", "example-secret", nil))
}

func TestHTTPStatuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status    int
		permanent bool
	}{
		{200, false},
		{201, true},
		{204, true},
		{301, true},
		{302, true},
		{400, true},
		{401, true},
		{403, true},
		{404, true},
		{413, true},
		{415, true},
		{429, false},
		{500, false},
		{502, false},
		{503, false},
		{599, false},
	} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			t.Parallel()
			count := 0
			e := testAdapter(doerFunc(func(req *http.Request) (*http.Response, error) {
				count++
				events := requestEvents(t, req)
				if tc.status == http.StatusOK {
					return httpResponse(tc.status, fullACK(len(events))), nil
				}
				resp := httpResponse(tc.status, `{"error":"server_busy"}`)
				if tc.status == 429 {
					resp.Header.Set("Retry-After", "1")
				}
				return resp, nil
			}))
			err := e.pushLogs(t.Context(), testLogs(1))
			assert.Equal(t, 1, count, "the adapter must not schedule retries")
			if tc.status == http.StatusOK {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, tc.permanent, consumererror.IsPermanent(err))
			assert.Contains(t, err.Error(), "http_status="+strconv.Itoa(tc.status))
			partial, ok := errors.AsType[consumererror.Logs](err)
			require.True(t, ok)
			assert.Equal(t, 1, partial.Data().LogRecordCount())
		})
	}
}

func TestRetryAfter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		delay time.Duration
		valid bool
	}{
		{"1", time.Second, true},
		{"0", 0, true},
		{"", 0, false},
		{"-1", 0, false},
		{"1.5", 0, false},
		{"invalid", 0, false},
		{"99999999999999999999", 0, false},
		{"9223372036", 9223372036 * time.Second, true},
		{"9223372037", 0, false},
		{now.Add(time.Minute).Format(http.TimeFormat), time.Minute, true},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0, true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Parallel()
			delay, valid := retryAfter(tc.value, now)
			assert.Equal(t, tc.valid, valid)
			assert.Equal(t, tc.delay, delay)
		})
	}
}

func TestClockCorrectionAndFreshSignature(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	serverNow := now.Add(10 * time.Minute)
	calls := 0
	e := testAdapter(doerFunc(func(req *http.Request) (*http.Response, error) {
		events := requestEvents(t, req)
		expected := now
		if calls > 0 {
			expected = serverNow
		}
		assert.Equal(t, strconv.FormatInt(expected.Unix(), 10), req.Header.Get("X-Timestamp"))
		calls++
		resp := httpResponse(200, fullACK(len(events)))
		if calls == 1 {
			resp = httpResponse(401, `{"error":"authentication_failed"}`)
		}
		resp.Header.Set("Date", serverNow.Format(http.TimeFormat))
		return resp, nil
	}))
	e.client.now = func() time.Time { return now }
	logs := testLogs(1)
	require.Error(t, e.pushLogs(t.Context(), logs))
	require.NoError(t, e.pushLogs(t.Context(), logs))
	assert.Equal(t, 2, calls)
}

func TestNetworkAndMalformedResponses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		response *http.Response
		err      error
	}{
		{name: "network", err: errors.New("connection reset")},
		{name: "missing_ack", response: httpResponse(200, "{}")},
		{name: "malformed_json", response: httpResponse(200, "{")},
		{name: "too_large", response: httpResponse(200, strings.Repeat(" ", maxResponseBytes+1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := testAdapter(doerFunc(func(*http.Request) (*http.Response, error) { return tc.response, tc.err }))
			err := e.pushLogs(t.Context(), testLogs(3))
			require.Error(t, err)
			assert.False(t, consumererror.IsPermanent(err))
			partial, ok := errors.AsType[consumererror.Logs](err)
			require.True(t, ok)
			assert.Equal(t, 3, partial.Data().LogRecordCount())
		})
	}
}

func TestSingleRequestInFlightAndCancellation(t *testing.T) {
	t.Parallel()
	started, release := make(chan struct{}), make(chan struct{})
	e := testAdapter(doerFunc(func(*http.Request) (*http.Response, error) {
		close(started)
		<-release
		return httpResponse(200, fullACK(1)), nil
	}))
	done := make(chan error, 1)
	go func() { done <- e.pushLogs(t.Context(), testLogs(1)) }()
	<-started
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := e.pushLogs(ctx, testLogs(1))
	close(release)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, <-done)
}
