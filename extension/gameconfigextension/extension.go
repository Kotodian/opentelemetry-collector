// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package gameconfigextension

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componentstatus"
	"go.opentelemetry.io/collector/extension"
)

const configPath = "/analytics/v1/config"

type configExtension struct {
	config    *Config
	telemetry component.TelemetrySettings
	server    *http.Server
	done      chan struct{}
	mu        sync.RWMutex
	handler   http.Handler
	stopped   bool
}

var _ extension.Extension = (*configExtension)(nil)

// RegisterConfigHandler binds one application's exporter to this listener.
// Separate extension instances provide separate application routes and credentials.
func (e *configExtension) RegisterConfigHandler(handler http.Handler) (func(), error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped {
		return nil, errors.New("configuration relay is stopped")
	}
	if handler == nil {
		return nil, errors.New("configuration handler is required")
	}
	if e.handler != nil {
		return nil, errors.New("configuration relay already has an exporter; use a separate extension instance per application")
	}
	e.handler = handler
	var once sync.Once
	return func() { once.Do(func() { e.mu.Lock(); defer e.mu.Unlock(); e.handler = nil }) }, nil
}

func (e *configExtension) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path != configPath || r.URL.RawPath != "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, `{"error":"method_not_allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		http.Error(w, `{"error":"query_not_allowed"}`, http.StatusBadRequest)
		return
	}
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		http.Error(w, `{"error":"body_not_allowed"}`, http.StatusBadRequest)
		return
	}
	e.mu.RLock()
	handler := e.handler
	e.mu.RUnlock()
	if handler == nil {
		http.Error(w, `{"error":"exporter_unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	handler.ServeHTTP(w, r)
}

func (e *configExtension) Start(ctx context.Context, host component.Host) error {
	var err error
	e.server, err = e.config.ServerConfig.ToServer(ctx, host.GetExtensions(), e.telemetry, e)
	if err != nil {
		return err
	}
	ln, err := e.config.ServerConfig.ToListener(ctx)
	if err != nil {
		return err
	}
	e.done = make(chan struct{})
	go func() {
		defer close(e.done)
		if serveErr := e.server.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			componentstatus.ReportStatus(host, componentstatus.NewFatalErrorEvent(serveErr))
		}
	}()
	return nil
}

func (e *configExtension) Shutdown(ctx context.Context) error {
	e.mu.Lock()
	e.stopped = true
	e.handler = nil
	e.mu.Unlock()
	if e.server == nil || e.done == nil {
		return nil
	}
	err := e.server.Shutdown(ctx)
	if err != nil {
		err = errors.Join(err, e.server.Close())
	}
	<-e.done
	return err
}
