// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gameexporter

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestACKIntegrity(t *testing.T) {
	t.Parallel()
	events := []event{{id: "a"}, {id: "b"}, {id: "c"}}
	valid := `{"summary":{"received":3,"accepted":1,"rejected":1,"retryable":1},"results":[{"index":1,"event_id":"b","status":"rejected"},{"index":2,"event_id":"c","status":"retryable"}]}`
	results, err := parseACK([]byte(valid), events)
	require.NoError(t, err)
	require.Len(t, results, 2)
	_, err = parseACK([]byte(fullACK(3)), events)
	require.NoError(t, err)
	cases := map[string]string{
		"missing_summary":   `{"results":[]}`,
		"missing_results":   `{"summary":{"received":3,"accepted":3,"rejected":0,"retryable":0}}`,
		"null_results":      `{"summary":{"received":3,"accepted":3,"rejected":0,"retryable":0},"results":null}`,
		"null_summary":      `{"summary":null,"results":[]}`,
		"missing_counter":   `{"summary":{"received":3,"accepted":3,"rejected":0},"results":[]}`,
		"empty":             `{}`,
		"wrong_received":    `{"summary":{"received":2,"accepted":2,"rejected":0,"retryable":0},"results":[]}`,
		"negative":          `{"summary":{"received":3,"accepted":4,"rejected":-1,"retryable":0},"results":[]}`,
		"float":             `{"summary":{"received":3.0,"accepted":3,"rejected":0,"retryable":0},"results":[]}`,
		"duplicate_keys":    `{"summary":{"received":3,"received":3,"accepted":3,"rejected":0,"retryable":0},"results":[]}`,
		"trailing_document": fullACK(3) + "{}",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) { t.Parallel(); _, err := parseACK([]byte(body), events); require.Error(t, err) })
	}
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"wrong_sum", func(a map[string]any) { a["summary"].(map[string]any)["accepted"] = 0 }},
		{"wrong_count", func(a map[string]any) { a["results"] = []any{} }},
		{"wrong_status_count", func(a map[string]any) { a["results"].([]any)[0].(map[string]any)["status"] = "retryable" }},
		{"unknown_status", func(a map[string]any) { a["results"].([]any)[0].(map[string]any)["status"] = "accepted" }},
		{"missing_index", func(a map[string]any) { delete(a["results"].([]any)[0].(map[string]any), "index") }},
		{"negative_index", func(a map[string]any) { a["results"].([]any)[0].(map[string]any)["index"] = -1 }},
		{"out_of_range", func(a map[string]any) { a["results"].([]any)[1].(map[string]any)["index"] = 3 }},
		{"fractional_index", func(a map[string]any) { a["results"].([]any)[0].(map[string]any)["index"] = 1.5 }},
		{"duplicate_index", func(a map[string]any) { a["results"].([]any)[1].(map[string]any)["index"] = 1 }},
		{"wrong_id", func(a map[string]any) { a["results"].([]any)[0].(map[string]any)["event_id"] = "wrong" }},
		{"null_id", func(a map[string]any) { a["results"].([]any)[0].(map[string]any)["event_id"] = nil }},
		{"non_string_id", func(a map[string]any) { a["results"].([]any)[0].(map[string]any)["event_id"] = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var a map[string]any
			require.NoError(t, json.Unmarshal([]byte(valid), &a))
			tc.mutate(a)
			raw, err := json.Marshal(a)
			require.NoError(t, err)
			_, err = parseACK(raw, events)
			require.Error(t, err)
		})
	}
}
