// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gameexporter

import (
	"bytes"
	"encoding/json"
	"errors"
)

type summary struct {
	Received  *int `json:"received"`
	Accepted  *int `json:"accepted"`
	Rejected  *int `json:"rejected"`
	Retryable *int `json:"retryable"`
}

type result struct {
	Index   *int            `json:"index"`
	EventID json.RawMessage `json:"event_id"`
	Status  string          `json:"status"`
	Code    string          `json:"code"`
	Field   string          `json:"field"`
}

func parseACK(raw []byte, events []event) ([]result, error) {
	if err := validateJSON(raw); err != nil {
		return nil, err
	}
	var ack struct {
		Summary *summary        `json:"summary"`
		Results json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(raw, &ack); err != nil {
		return nil, errors.New("invalid ACK JSON types")
	}
	if ack.Summary == nil || len(ack.Results) == 0 || bytes.Equal(bytes.TrimSpace(ack.Results), []byte("null")) {
		return nil, errors.New("ACK must contain summary and results array")
	}
	s := ack.Summary
	if s.Received == nil || s.Accepted == nil || s.Rejected == nil || s.Retryable == nil {
		return nil, errors.New("ACK summary is incomplete")
	}
	n := len(events)
	if *s.Received != n || *s.Accepted < 0 || *s.Rejected < 0 || *s.Retryable < 0 || *s.Accepted > n || *s.Rejected > n || *s.Retryable > n || *s.Accepted+*s.Rejected+*s.Retryable != n {
		return nil, errors.New("ACK summary does not match batch")
	}
	var results []result
	if err := json.Unmarshal(ack.Results, &results); err != nil || len(results) != *s.Rejected+*s.Retryable {
		return nil, errors.New("ACK results do not match summary")
	}
	rejected, retryable, previous := 0, 0, -1
	for _, item := range results {
		if item.Index == nil || *item.Index < 0 || *item.Index >= n || *item.Index <= previous {
			return nil, errors.New("ACK indices must be unique, ordered integers within batch")
		}
		previous = *item.Index
		if len(item.EventID) > 0 {
			var id string
			if err := json.Unmarshal(item.EventID, &id); err != nil || id != events[*item.Index].id {
				return nil, errors.New("ACK event_id does not match batch")
			}
		}
		switch item.Status {
		case "rejected":
			rejected++
		case "retryable":
			retryable++
		default:
			return nil, errors.New("unknown ACK status")
		}
	}
	if rejected != *s.Rejected || retryable != *s.Retryable {
		return nil, errors.New("ACK status counts do not match summary")
	}
	return results, nil
}
