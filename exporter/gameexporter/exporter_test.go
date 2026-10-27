// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gameexporter

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/collector/consumer/consumererror"
	"go.opentelemetry.io/collector/pdata/plog"
)

func testLogs(n int) plog.Logs {
	out := plog.NewLogs()
	rl := out.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr("service.name", "example")
	sl := rl.ScopeLogs().AppendEmpty()
	sl.Scope().SetName("prepared-game-events")
	for i := range n {
		raw := fmt.Sprintf(`{"event_id":"event-%d","event_name":"app_launch","schema_version":1,"event_time":1791331200000,"device_id":"install-1","session_id":"session-1","account_type":"guest","app_version":"1.0.0","channel":"official","platform":"linux","market_region":"cn","properties":{}}`, i)
		sl.LogRecords().AppendEmpty().Body().SetStr(raw)
	}
	return out
}

func recordIDs(t *testing.T, logs plog.Logs) []string {
	t.Helper()
	var ids []string
	for _, rl := range logs.ResourceLogs().All() {
		for _, sl := range rl.ScopeLogs().All() {
			for _, record := range sl.LogRecords().All() {
				ev, err := eventFromBody(record.Body(), 0)
				require.NoError(t, err)
				ids = append(ids, ev.id)
			}
		}
	}
	return ids
}

func TestCountAndByteSplitting(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 1, 200, 201, 450} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			t.Parallel()
			calls := 0
			e := testAdapter(doerFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				events := requestEvents(t, req)
				require.NotEmpty(t, events)
				return httpResponse(200, fullACK(len(events))), nil
			}))
			logs := testLogs(n)
			before, err := (&plog.JSONMarshaler{}).MarshalLogs(logs)
			require.NoError(t, err)
			require.NoError(t, e.pushLogs(t.Context(), logs))
			assert.Equal(t, (n+maxEvents-1)/maxEvents, calls)
			after, err := (&plog.JSONMarshaler{}).MarshalLogs(logs)
			require.NoError(t, err)
			assert.JSONEq(t, string(before), string(after))
		})
	}
	logs := testLogs(20)
	for _, r := range logs.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().All() {
		raw := strings.Replace(r.Body().Str(), `"properties":{}`, `"properties":{"padding":"`+strings.Repeat("x", 30000)+`"}`, 1)
		r.Body().SetStr(raw)
	}
	calls, total := 0, 0
	e := testAdapter(doerFunc(func(req *http.Request) (*http.Response, error) {
		events := requestEvents(t, req)
		assert.LessOrEqual(t, len(batchJSON(rawEvents(events))), batchTargetBytes)
		calls++
		total += len(events)
		return httpResponse(200, fullACK(len(events))), nil
	}))
	require.NoError(t, e.pushLogs(t.Context(), logs))
	assert.Greater(t, calls, 1)
	assert.Equal(t, 20, total)
}

func rawEvents(raw []json.RawMessage) []event {
	out := make([]event, len(raw))
	for i, b := range raw {
		out[i].body = b
	}
	return out
}

func TestPartialACKRetriesOnlyUnconfirmedEvents(t *testing.T) {
	t.Parallel()
	calls := 0
	e := testAdapter(doerFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		events := requestEvents(t, req)
		if calls == 1 {
			require.Len(t, events, 200)
			return httpResponse(200, `{"summary":{"received":200,"accepted":198,"rejected":1,"retryable":1},"results":[{"index":1,"event_id":"event-1","status":"rejected","code":"invalid_region","field":"market_region"},{"index":2,"event_id":"event-2","status":"retryable","code":"delivery_unknown"}]}`), nil
		}
		require.Len(t, events, 4)
		return httpResponse(200, fullACK(4)), nil
	}))
	logs := testLogs(203)
	err := e.pushLogs(t.Context(), logs)
	require.Error(t, err)
	assert.False(t, consumererror.IsPermanent(err))
	partial, ok := errors.AsType[consumererror.Logs](err)
	require.True(t, ok)
	assert.Equal(t, []string{"event-2", "event-200", "event-201", "event-202"}, recordIDs(t, partial.Data()))
	assert.Equal(t, "example", partial.Data().ResourceLogs().At(0).Resource().Attributes().AsRaw()["service.name"])
	assert.Equal(t, 203, logs.LogRecordCount())
	require.NoError(t, e.pushLogs(t.Context(), partial.Data()))
	assert.Equal(t, 2, calls)
}

func TestFailedLaterBatchRetainsOnlyUnconfirmed(t *testing.T) {
	t.Parallel()
	calls := 0
	e := testAdapter(doerFunc(func(req *http.Request) (*http.Response, error) {
		events := requestEvents(t, req)
		calls++
		if calls == 2 {
			return httpResponse(503, `{"error":"draining"}`), nil
		}
		return httpResponse(200, fullACK(len(events))), nil
	}))
	err := e.pushLogs(t.Context(), testLogs(450))
	partial, ok := errors.AsType[consumererror.Logs](err)
	require.True(t, ok)
	assert.Equal(t, 250, partial.Data().LogRecordCount())
	assert.Equal(t, "event-200", recordIDs(t, partial.Data())[0])
	assert.Equal(t, 2, calls)
}

func TestRejectedAndInvalidEventsDoNotBlockValidEvents(t *testing.T) {
	t.Parallel()
	for _, localInvalid := range []bool{true, false} {
		t.Run(strconv.FormatBool(localInvalid), func(t *testing.T) {
			t.Parallel()
			logs := testLogs(3)
			if localInvalid {
				logs.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(1).Body().SetStr("{}")
			}
			e := testAdapter(doerFunc(func(req *http.Request) (*http.Response, error) {
				events := requestEvents(t, req)
				if localInvalid {
					require.Len(t, events, 2)
					return httpResponse(200, fullACK(2)), nil
				}
				return httpResponse(200, `{"summary":{"received":3,"accepted":2,"rejected":1,"retryable":0},"results":[{"index":1,"event_id":"event-1","status":"rejected","code":"required","field":"market_region"}]}`), nil
			}))
			err := e.pushLogs(t.Context(), logs)
			require.Error(t, err)
			assert.True(t, consumererror.IsPermanent(err))
			partial, ok := errors.AsType[consumererror.Logs](err)
			require.True(t, ok)
			assert.Equal(t, 1, partial.Data().LogRecordCount())
		})
	}
}
