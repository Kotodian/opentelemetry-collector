// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gameexporter

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/collector/pdata/pcommon"
)

func TestPreparedEventPreservesNumberTokensAndContext(t *testing.T) {
	t.Parallel()
	raw := `{"event_id":"id","event_time":1791331200000,"schema_version":1,"properties":{"big":123456789012345678901234567890,"decimal":0.123456789012345678901234567890}}`
	ev, err := eventFromBody(pcommon.NewValueStr(raw), 0)
	require.NoError(t, err)
	assert.Equal(t, raw, string(ev.body))
	assert.Equal(t, "id", ev.id)
	value := pcommon.NewValueMap()
	require.NoError(t, value.Map().FromRaw(map[string]any{"event_id": "map-event", "properties": map[string]any{"int": int64(9007199254740993)}}))
	ev, err = eventFromBody(value, 1)
	require.NoError(t, err)
	assert.Contains(t, string(ev.body), "9007199254740993")
	assert.Equal(t, 1, ev.index)
}

func TestInvalidPreparedEvents(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"missing_id": "{}", "null": "null", "array": "[]", "id_type": `{"event_id":123}`,
		"duplicate_top":    `{"event_id":"a","event_id":"b"}`,
		"duplicate_nested": `{"event_id":"a","properties":{"x":1,"x":2}}`,
		"trailing":         `{"event_id":"a"}{}`,
		"invalid_utf8":     string([]byte{0xff}),
		"oversized":        fmt.Sprintf(`{"event_id":"a","properties":{"p":%q}}`, strings.Repeat("x", maxEventBytes)),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := eventFromBody(pcommon.NewValueStr(raw), 0)
			require.Error(t, err)
		})
	}
	_, err := eventFromBody(pcommon.NewValueInt(1), 0)
	require.Error(t, err)
}

func TestEventByteLimit(t *testing.T) {
	t.Parallel()
	prefix := `{"event_id":"a","properties":{"p":"`
	suffix := `"}}`
	raw := prefix + strings.Repeat("x", maxEventBytes-len(prefix)-len(suffix)) + suffix
	ev, err := eventFromBody(pcommon.NewValueStr(raw), 0)
	require.NoError(t, err)
	assert.Len(t, ev.body, maxEventBytes)
	_, err = eventFromBody(pcommon.NewValueStr(raw+" "), 0)
	require.Error(t, err)
	var obj map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(ev.body, &obj))
}
