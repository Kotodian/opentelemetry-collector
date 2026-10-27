// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gameexporter

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"go.opentelemetry.io/collector/consumer/consumererror"
	"go.opentelemetry.io/collector/pdata/plog"
)

type deliveryState uint8

const (
	pending deliveryState = iota
	accepted
	rejected
)

type gameExporter struct {
	client *client
	logger *zap.Logger
}

func (e *gameExporter) pushLogs(ctx context.Context, logs plog.Logs) error {
	states := make([]deliveryState, logs.LogRecordCount())
	events := make([]event, 0, len(states))
	var permanentErr error
	index := 0
	for _, rl := range logs.ResourceLogs().All() {
		for _, sl := range rl.ScopeLogs().All() {
			for _, record := range sl.LogRecords().All() {
				ev, err := eventFromBody(record.Body(), index)
				if err != nil {
					states[index] = rejected
					permanentErr = fmt.Errorf("invalid event at log index %d: %w", index, err)
				} else {
					events = append(events, ev)
				}
				index++
			}
		}
	}
	var retryErr error
	for len(events) > 0 {
		end := batchEnd(events)
		batch := events[:end]
		results, err := e.client.send(ctx, batch)
		if err != nil {
			if consumererror.IsPermanent(err) {
				permanentErr = err
				for _, ev := range events {
					states[ev.index] = rejected
				}
			} else {
				retryErr = err
			}
			break
		}
		for _, ev := range batch {
			states[ev.index] = accepted
		}
		for _, item := range results {
			i := batch[*item.Index].index
			if item.Status == "rejected" {
				states[i] = rejected
				permanentErr = fmt.Errorf("platform rejected event (code=%s, field=%s)", diagnostic(item.Code), diagnostic(item.Field))
			} else {
				states[i] = pending
				retryErr = errors.New("platform requested retry for events")
			}
		}
		if retryErr != nil {
			break
		}
		events = events[end:]
	}
	if permanentErr != nil {
		count := 0
		for _, state := range states {
			if state == rejected {
				count++
			}
		}
		// Never log raw bodies, event identifiers, or application secrets.
		e.logger.Warn("Events permanently rejected", zap.Int("rejected_events", count), zap.Error(permanentErr))
	}
	if retryErr != nil {
		return consumererror.NewLogs(retryErr, subset(logs, states, pending))
	}
	if permanentErr != nil {
		return consumererror.NewPermanent(consumererror.NewLogs(permanentErr, subset(logs, states, rejected)))
	}
	return nil
}

func subset(logs plog.Logs, states []deliveryState, wanted deliveryState) plog.Logs {
	out := plog.NewLogs()
	logs.CopyTo(out)
	index := 0
	for _, rl := range out.ResourceLogs().All() {
		for _, sl := range rl.ScopeLogs().All() {
			sl.LogRecords().RemoveIf(func(plog.LogRecord) bool {
				remove := states[index] != wanted
				index++
				return remove
			})
		}
		rl.ScopeLogs().RemoveIf(func(sl plog.ScopeLogs) bool { return sl.LogRecords().Len() == 0 })
	}
	out.ResourceLogs().RemoveIf(func(rl plog.ResourceLogs) bool { return rl.ScopeLogs().Len() == 0 })
	return out
}
