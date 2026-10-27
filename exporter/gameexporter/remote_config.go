// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gameexporter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const configPath = "/analytics/v1/config"

// configRegistrar is implemented by an SDK-facing Collector extension.
type configRegistrar interface {
	RegisterConfigHandler(http.Handler) (func(), error)
}

type configHandler struct {
	client  *client
	timeout time.Duration
	gate    chan struct{}
}

func (h *configHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()
	select {
	case h.gate <- struct{}{}:
		defer func() { <-h.gate }()
	case <-ctx.Done():
		http.Error(w, `{"error":"config_unavailable"}`, http.StatusGatewayTimeout)
		return
	}
	response, err := h.client.fetchConfig(ctx)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		http.Error(w, `{"error":"config_unavailable"}`, status)
		return
	}
	for _, name := range []string{"Date", "X-Request-ID", "Retry-After"} {
		if value := response.header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.WriteHeader(response.status)
	// The SDK may disconnect after the platform request has completed.
	if _, writeErr := w.Write(response.body); writeErr != nil {
		return
	}
}

type configResponse struct {
	status int
	header http.Header
	body   []byte
}

func (c *client) fetchConfig(ctx context.Context) (configResponse, error) {
	target, err := url.Parse(c.endpoint)
	if err != nil {
		return configResponse{}, fmt.Errorf("parse platform endpoint: %w", err)
	}
	target.Path = configPath
	target.RawPath = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), http.NoBody)
	if err != nil {
		return configResponse{}, fmt.Errorf("create configuration request: %w", err)
	}
	stamp := strconv.FormatInt(c.now().Unix()+c.clockOffset.Load(), 10)
	req.Header.Set("X-App-ID", c.appID)
	req.Header.Set("X-Timestamp", stamp)
	req.Header.Set("X-Signature", signature(http.MethodGet, configPath, c.appID, stamp, "identity", c.secret, nil))
	response, err := c.http.Do(req)
	if err != nil {
		return configResponse{}, fmt.Errorf("fetch platform configuration: %w", err)
	}
	defer response.Body.Close()
	if date, dateErr := http.ParseTime(response.Header.Get("Date")); dateErr == nil {
		c.clockOffset.Store(date.Unix() - c.now().Unix())
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return configResponse{}, fmt.Errorf("read platform configuration: %w", err)
	}
	if len(body) > maxResponseBytes {
		return configResponse{}, errors.New("configuration response exceeds limit")
	}
	if response.StatusCode == http.StatusOK {
		if err := validateRemoteConfig(body); err != nil {
			return configResponse{}, err
		}
	} else if response.StatusCode < 400 || response.StatusCode > 599 {
		return configResponse{}, errors.New("unexpected platform configuration status")
	}
	return configResponse{status: response.StatusCode, header: response.Header, body: body}, nil
}

func validateRemoteConfig(body []byte) error {
	if err := validateJSON(body); err != nil {
		return fmt.Errorf("invalid platform configuration JSON: %w", err)
	}
	var config struct {
		Version  *int64     `json:"config_version"`
		Enabled  *bool      `json:"reporting_enabled"`
		Disabled *[]*string `json:"disabled_events"`
		Refresh  *int       `json:"refresh_seconds"`
	}
	if err := json.Unmarshal(body, &config); err != nil {
		return fmt.Errorf("invalid platform configuration types: %w", err)
	}
	if config.Version == nil || *config.Version < 0 || config.Enabled == nil || config.Disabled == nil || config.Refresh == nil || *config.Refresh < 300 || *config.Refresh > 86400 {
		return errors.New("invalid platform configuration fields")
	}
	for _, name := range *config.Disabled {
		if name == nil {
			return errors.New("disabled_events must contain strings")
		}
	}
	return nil
}
