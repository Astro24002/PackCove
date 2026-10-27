package packcove

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type Service struct {
	cfg       *RuntimeConfig
	logger    *slog.Logger
	storage   *Storage
	metrics   *Metrics
	startedAt time.Time
	rootCtx   context.Context
	rootStop  context.CancelFunc
	clients   map[string]*http.Client
	statusMu  sync.RWMutex
	statuses  map[string]upstreamStatus
	flightMu  sync.Mutex
	flights   map[string]*flightCall
	closeOnce sync.Once
}

type upstreamStatus struct {
	Status string
	At     time.Time
}

type flightCall struct {
	done   chan struct{}
	result fetchOutcome
	err    error
}

type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func NewService(cfg *RuntimeConfig, logger *slog.Logger) (*Service, error) {
	if cfg == nil {
		return nil, errors.New("configuration is required")
	}
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))
	}
	store, err := OpenStorage(cfg.Storage.DataDir)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	svc := &Service{
		cfg: cfg, logger: logger, storage: store, metrics: NewMetrics(), startedAt: time.Now().UTC(),
		rootCtx: ctx, rootStop: cancel, clients: make(map[string]*http.Client),
		statuses: make(map[string]upstreamStatus), flights: make(map[string]*flightCall),
	}
	for _, repo := range cfg.Repositories {
		hosts := make(map[string]bool, len(repo.AllowedDownloadHosts))
		for _, host := range repo.AllowedDownloadHosts {
			hosts[strings.ToLower(host)] = true
		}
		transport := &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: cfg.ConnectTimeout, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   cfg.ConnectTimeout,
			ResponseHeaderTimeout: cfg.RequestTimeout,
			DisableCompression:    true,
		}
		maxRedirects := cfg.Upstream.MaxRedirects
		svc.clients[repo.Name] = &http.Client{
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) > maxRedirects {
					return fmt.Errorf("too many upstream redirects")
				}
				if !hosts[strings.ToLower(req.URL.Hostname())] {
					return fmt.Errorf("redirect host %q is not allowed", req.URL.Hostname())
				}
				if len(via) > 0 {
					req.Header.Del("Authorization")
					req.Header.Del("Cookie")
				}
				return nil
			},
		}
	}
	return svc, nil
}

func (s *Service) Handler() http.Handler { return s }

func (s *Service) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.rootStop()
		err = s.storage.Close()
	})
	return err
}

func (s *Service) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.cfg.Server.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.Server.Listen, err)
	}
	server := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	s.logger.Info("PackCove started", "listen", listener.Addr().String(), "repositories", len(s.cfg.Repositories))
	select {
	case <-ctx.Done():
		s.rootStop()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return err
		}
		if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	recorder := &responseRecorder{ResponseWriter: w}
	defer func() {
		if recorder.status == 0 {
			recorder.status = http.StatusOK
		}
		s.metrics.ObserveRequest(s.requestMetricLabel(r.URL.Path), time.Since(started).Seconds())
		s.logger.Info("http request", "method", r.Method, "path", r.URL.Path, "status", recorder.status, "bytes", recorder.bytes, "duration_seconds", time.Since(started).Seconds())
	}()
	if r.URL.Path == "/_packcove/health" {
		s.handleHealth(recorder, r)
		return
	}
	if r.URL.Path == "/_packcove/repositories" {
		s.handleRepositories(recorder, r)
		return
	}
	if r.URL.Path == "/_packcove/stats" {
		s.handleStats(recorder, r)
		return
	}
	if r.URL.Path == "/metrics" {
		s.handleMetrics(recorder, r)
		return
	}
	s.handleRepository(recorder, r)
}

func (w *responseRecorder) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseRecorder) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	w.bytes += int64(n)
	return n, err
}

func (w *responseRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Service) methodAllowed(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	for _, method := range methods {
		if r.Method == method {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(methods, ", "))
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

func (s *Service) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !s.methodAllowed(w, r, http.MethodGet) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.storage.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "degraded", "storage": map[string]string{"status": "unavailable", "path": s.cfg.Storage.DataDir}, "started_at": s.startedAt,
		})
		return
	}
	if _, err := os.Stat(s.cfg.Storage.DataDir); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "degraded", "storage": map[string]string{"status": "unavailable", "path": s.cfg.Storage.DataDir}, "started_at": s.startedAt,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "storage": map[string]string{"status": "ok", "path": s.cfg.Storage.DataDir}, "started_at": s.startedAt,
	})
}

type repositoryView struct {
	Name           string     `json:"name"`
	Type           string     `json:"type"`
	URL            string     `json:"url"`
	Upstream       string     `json:"upstream"`
	UpstreamStatus string     `json:"upstream_status"`
	LastObservedAt *time.Time `json:"last_observed_at"`
}

func (s *Service) handleRepositories(w http.ResponseWriter, r *http.Request) {
	if !s.methodAllowed(w, r, http.MethodGet) {
		return
	}
	views := make([]repositoryView, 0, len(s.cfg.Repositories))
	for _, repo := range s.cfg.Repositories {
		status := "unknown"
		var observed *time.Time
		s.statusMu.RLock()
		if value, ok := s.statuses[repo.Name]; ok {
			status = value.Status
			at := value.At
			observed = &at
		}
		s.statusMu.RUnlock()
		views = append(views, repositoryView{
			Name: repo.Name, Type: repo.Type,
			URL:      publicRepositoryURL(s.cfg.PublicURL, repo.Name, repo.Type),
			Upstream: sanitizeUpstream(repo.Upstream), UpstreamStatus: status, LastObservedAt: observed,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"repositories": views})
}

func (s *Service) handleStats(w http.ResponseWriter, r *http.Request) {
	if !s.methodAllowed(w, r, http.MethodGet) {
		return
	}
	bytes, err := s.storage.TotalBytes(r.Context())
	if err != nil {
		s.metrics.Error("_service", "storage")
		http.Error(w, "cache index unavailable", http.StatusInternalServerError)
		return
	}
	freeBytes, err := s.storage.FreeBytes()
	if err != nil {
		http.Error(w, "cache filesystem unavailable", http.StatusInternalServerError)
		return
	}
	names := s.repositoryNames()
	writeJSON(w, http.StatusOK, map[string]any{
		"uptime_seconds": time.Since(s.startedAt).Seconds(), "cache_bytes": bytes, "storage_free_bytes": freeBytes,
		"repositories": s.metrics.Snapshot(names),
	})
}

func (s *Service) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.methodAllowed(w, r, http.MethodGet) {
		return
	}
	bytes, err := s.storage.TotalBytes(r.Context())
	if err != nil {
		http.Error(w, "cache index unavailable", http.StatusInternalServerError)
		return
	}
	freeBytes, err := s.storage.FreeBytes()
	if err != nil {
		http.Error(w, "cache filesystem unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(s.metrics.WritePrometheus(bytes, freeBytes, s.metricRepositoryNames())))
}

func (s *Service) handleRepository(w *responseRecorder, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "publishing is not supported", http.StatusMethodNotAllowed)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/repository/") {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/repository/")
	parts := strings.SplitN(rest, "/", 2)
	if parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	repo, ok := s.repository(parts[0])
	if !ok {
		http.NotFound(w, r)
		return
	}
	rel := ""
	if len(parts) == 2 {
		rel = parts[1]
	}
	if !validResourcePath(rel) {
		s.metrics.Error(repo.Name, "invalid_path")
		http.Error(w, "invalid repository path", http.StatusBadRequest)
		return
	}
	s.proxyRepository(w, r, repo, rel)
}

func (s *Service) repository(name string) (RepositoryConfig, bool) {
	for _, repo := range s.cfg.Repositories {
		if repo.Name == name {
			return repo, true
		}
	}
	return RepositoryConfig{}, false
}

func (s *Service) repositoryNames() []string {
	result := make([]string, 0, len(s.cfg.Repositories))
	for _, repo := range s.cfg.Repositories {
		result = append(result, repo.Name)
	}
	return result
}

func (s *Service) metricRepositoryNames() []string {
	result := s.repositoryNames()
	return append(result, "_service", "unknown")
}

func (s *Service) requestMetricLabel(path string) string {
	name := repositoryFromPath(path)
	if name == "_service" {
		return name
	}
	if _, ok := s.repository(name); !ok {
		return "unknown"
	}
	return name
}

func (s *Service) observeUpstream(repository string, statusCode int) {
	status := "available"
	if statusCode >= 500 {
		status = "unavailable"
	}
	s.statusMu.Lock()
	s.statuses[repository] = upstreamStatus{Status: status, At: time.Now().UTC()}
	s.statusMu.Unlock()
}

func (s *Service) doFlight(ctx context.Context, key string, fn func() (fetchOutcome, error)) (fetchOutcome, error, bool) {
	s.flightMu.Lock()
	if call := s.flights[key]; call != nil {
		s.flightMu.Unlock()
		select {
		case <-ctx.Done():
			return fetchOutcome{}, ctx.Err(), true
		case <-call.done:
			return call.result, call.err, true
		}
	}
	call := &flightCall{done: make(chan struct{})}
	s.flights[key] = call
	s.flightMu.Unlock()
	call.result, call.err = fn()
	s.flightMu.Lock()
	delete(s.flights, key)
	close(call.done)
	s.flightMu.Unlock()
	return call.result, call.err, false
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func repositoryFromPath(path string) string {
	if !strings.HasPrefix(path, "/repository/") {
		return "_service"
	}
	name := strings.SplitN(strings.TrimPrefix(path, "/repository/"), "/", 2)[0]
	if name == "" {
		return "_service"
	}
	return name
}
