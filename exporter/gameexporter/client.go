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
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/collector/consumer/consumererror"
	"go.opentelemetry.io/collector/exporter/exporterhelper"
)

const maxResponseBytes = 1024 * 1024

// httpDoer allows a real HTTP client or a local protocol simulator.
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type client struct {
	http        httpDoer
	endpoint    string
	appID       string
	secret      string
	now         func() time.Time
	clockOffset atomic.Int64
	gate        chan struct{}
}

func signature(method, path, appID, stamp, encoding, secret string, wire []byte) string {
	digest := sha256.Sum256(wire)
	canonical := strings.Join([]string{"GI-HMAC-SHA256-V1", method, path, appID, stamp, "application/json", encoding, hex.EncodeToString(digest[:])}, "\n")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(canonical))
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}

func (c *client) send(ctx context.Context, events []event) ([]result, error) {
	select {
	case c.gate <- struct{}{}:
		defer func() { <-c.gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	raw := batchJSON(events)
	if len(events) == 0 || len(events) > maxEvents || len(raw) > maxDecodedBytes {
		return nil, consumererror.NewPermanent(errors.New("batch exceeds protocol limits"))
	}
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write(raw); err != nil {
		return nil, fmt.Errorf("compress batch: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("finish gzip stream: %w", err)
	}
	wire := buf.Bytes()
	if len(wire) > maxWireBytes {
		return nil, consumererror.NewPermanent(errors.New("compressed batch exceeds 1 MiB"))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(wire))
	if err != nil {
		return nil, consumererror.NewPermanent(fmt.Errorf("create platform request: %w", err))
	}
	stamp := strconv.FormatInt(c.now().Unix()+c.clockOffset.Load(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("X-App-ID", c.appID)
	req.Header.Set("X-Timestamp", stamp)
	req.Header.Set("X-Signature", signature(http.MethodPost, eventsPath, c.appID, stamp, "gzip", c.secret, wire))
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send platform request: %w", err)
	}
	defer resp.Body.Close()
	if date, dateErr := http.ParseTime(resp.Header.Get("Date")); dateErr == nil {
		c.clockOffset.Store(date.Unix() - c.now().Unix())
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if resp.StatusCode != http.StatusOK {
		if err != nil || len(body) > maxResponseBytes {
			body = nil
		}
		statusErr := responseError(resp.StatusCode, body)
		if resp.StatusCode == http.StatusTooManyRequests {
			if delay, ok := retryAfter(resp.Header.Get("Retry-After"), time.Unix(c.now().Unix()+c.clockOffset.Load(), 0)); ok {
				return nil, exporterhelper.NewThrottleRetry(statusErr, delay)
			}
			return nil, statusErr
		}
		if resp.StatusCode >= 500 && resp.StatusCode <= 599 {
			return nil, statusErr
		}
		return nil, consumererror.NewPermanent(statusErr)
	}
	if err != nil {
		return nil, fmt.Errorf("read platform response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return nil, errors.New("platform response exceeds limit")
	}
	results, err := parseACK(body, events)
	if err != nil {
		return nil, fmt.Errorf("invalid platform ACK: %w", err)
	}
	return results, nil
}

var diagnosticPattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`)

func diagnostic(value string) string {
	if diagnosticPattern.MatchString(value) {
		return value
	}
	return "unknown"
}

func responseError(status int, body []byte) error {
	var response struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return fmt.Errorf("platform request failed (http_status=%d)", status)
	}
	return fmt.Errorf("platform request failed (http_status=%d, code=%s)", status, diagnostic(response.Error))
}

func retryAfter(value string, now time.Time) (time.Duration, bool) {
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseUint(value, 10, 63); err == nil {
		const maxSeconds = (1<<63 - 1) / int64(time.Second)
		if seconds > uint64(maxSeconds) {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	date, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	return max(time.Duration(0), date.Sub(now)), true
}
