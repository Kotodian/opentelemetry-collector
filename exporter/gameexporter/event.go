// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gameexporter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	"go.opentelemetry.io/collector/pdata/pcommon"
)

const (
	maxEvents        = 200
	maxEventBytes    = 64 * 1024
	batchTargetBytes = 256 * 1024
	maxDecodedBytes  = 4 * 1024 * 1024
	maxWireBytes     = 1024 * 1024
)

type event struct {
	index int
	id    string
	body  json.RawMessage
}

func eventFromBody(body pcommon.Value, index int) (event, error) {
	var raw []byte
	switch body.Type() {
	case pcommon.ValueTypeMap:
		var err error
		raw, err = json.Marshal(body.Map().AsRaw())
		if err != nil {
			return event{}, fmt.Errorf("encode event object: %w", err)
		}
	case pcommon.ValueTypeStr:
		raw = []byte(body.Str())
	default:
		return event{}, errors.New("log body must be an event object or JSON object string")
	}
	if len(raw) > maxEventBytes {
		return event{}, errors.New("event exceeds 64 KiB")
	}
	if err := validateJSON(raw); err != nil {
		return event{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return event{}, errors.New("event must be a JSON object")
	}
	var id string
	if err := json.Unmarshal(fields["event_id"], &id); err != nil || id == "" {
		return event{}, errors.New("event_id must be a nonempty string supplied by the caller")
	}
	return event{index: index, id: id, body: raw}, nil
}

// validateJSON preserves number tokens and detects duplicate keys at every level.
func validateJSON(raw []byte) error {
	if !utf8.Valid(raw) {
		return errors.New("JSON is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := validateValue(dec, 0); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("JSON must contain exactly one document")
	}
	return nil
}

func validateValue(dec *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("JSON nesting exceeds 32 levels")
	}
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		keys := make(map[string]struct{})
		for dec.More() {
			key, keyErr := dec.Token()
			if keyErr != nil {
				return keyErr
			}
			name, isString := key.(string)
			if !isString {
				return errors.New("object key is not a string")
			}
			if _, exists := keys[name]; exists {
				return errors.New("duplicate object key")
			}
			keys[name] = struct{}{}
			if childErr := validateValue(dec, depth+1); childErr != nil {
				return childErr
			}
		}
	case '[':
		for dec.More() {
			if childErr := validateValue(dec, depth+1); childErr != nil {
				return childErr
			}
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	_, err = dec.Token()
	return err
}

func batchEnd(events []event) int {
	size := len(`{"events":[]}`)
	end := 0
	for end < len(events) && end < maxEvents {
		next := len(events[end].body)
		if end > 0 {
			next++
		}
		if size+next > batchTargetBytes {
			break
		}
		size += next
		end++
	}
	return end
}

func batchJSON(events []event) []byte {
	var buf bytes.Buffer
	buf.WriteString(`{"events":[`)
	for i, ev := range events {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(ev.body)
	}
	buf.WriteString(`]}`)
	return buf.Bytes()
}
