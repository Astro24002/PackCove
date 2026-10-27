package packcove

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

var mappingIDPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type fetchOutcome struct {
	Entry  CacheEntry
	Path   string
	Result string
	Source string
	Stale  bool
}

type expectedDigest struct {
	Algorithm string
	Value     string
}

type gatewayError struct {
	Status  int
	Reason  string
	Message string
}

func (e *gatewayError) Error() string { return e.Message }

type upstreamStatusError struct {
	Status      int
	ContentType string
	Body        []byte
}

func (e *upstreamStatusError) Error() string {
	return fmt.Sprintf("upstream returned HTTP %d", e.Status)
}

func (s *Service) proxyRepository(w *responseRecorder, r *http.Request, repo RepositoryConfig, rel string) {
	var target *url.URL
	var err error
	digest := expectedDigest{}
	mappedDownload := strings.HasPrefix(rel, "_packcove/download/")
	isMetadata := false
	if mappedDownload {
		target, digest, err = s.resolveDownload(r.Context(), repo, rel)
		if err != nil {
			s.writeProxyError(w, repo.Name, err)
			return
		}
		rel = "_packcove/download/" + strings.SplitN(strings.TrimPrefix(rel, "_packcove/download/"), "/", 2)[0]
	} else {
		rawPath := strings.TrimPrefix(r.URL.EscapedPath(), "/repository/"+repo.Name+"/")
		var redirect bool
		target, redirect, err = requestUpstreamURL(repo, rel, rawPath, r.URL.RawQuery)
		if err != nil {
			s.writeProxyError(w, repo.Name, &gatewayError{Status: http.StatusBadRequest, Reason: "invalid_path", Message: "invalid repository path"})
			return
		}
		if redirect {
			location := r.URL.Path + "/"
			if r.URL.RawQuery != "" {
				location += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, location, http.StatusPermanentRedirect)
			return
		}
		isMetadata = metadataRequest(repo, rel)
	}
	metadata := isMetadata
	accept := r.Header.Get("Accept")
	keyInput := target.String()
	keyInput += "\nDigest:" + digest.Algorithm + ":" + digest.Value
	if metadata && (repo.Type == "npm" || repo.Type == "pypi") {
		keyInput += "\nAccept:" + accept
	}
	cacheHash := sha256.Sum256([]byte(keyInput))
	cacheKey := hex.EncodeToString(cacheHash[:])
	entry, filePath, cacheErr := s.storage.Get(r.Context(), repo.Name, cacheKey)
	if cacheErr != nil && !errors.Is(cacheErr, errCacheMiss) {
		s.writeProxyError(w, repo.Name, &gatewayError{Status: http.StatusInternalServerError, Reason: "storage", Message: "cache index unavailable"})
		return
	}
	if cacheErr == nil && (entry.FreshUntil.IsZero() || time.Now().Before(entry.FreshUntil)) {
		if r.Method == http.MethodGet && !isMetadata {
			s.metrics.Artifact(repo.Name, "hit")
		}
		s.serveCached(w, r, entry, filePath, "cache", repo.Name)
		return
	}
	wasCached := cacheErr == nil
	flightKey := repo.Name + ":" + cacheKey
	result, fetchErr, joined := s.doFlight(r.Context(), flightKey, func() (fetchOutcome, error) {
		current, currentPath, currentErr := s.storage.Get(s.rootCtx, repo.Name, cacheKey)
		if currentErr != nil && !errors.Is(currentErr, errCacheMiss) {
			return fetchOutcome{}, &gatewayError{Status: http.StatusInternalServerError, Reason: "storage", Message: "cache index unavailable"}
		}
		if currentErr == nil && (current.FreshUntil.IsZero() || time.Now().Before(current.FreshUntil)) {
			return fetchOutcome{Entry: current, Path: currentPath, Result: "hit", Source: "cache"}, nil
		}
		var stale *CacheEntry
		if currentErr == nil {
			stale = &current
		}
		fetchCtx, cancel := context.WithTimeout(s.rootCtx, s.cfg.RequestTimeout)
		defer cancel()
		return s.fetchAndStore(fetchCtx, repo, rel, target, cacheKey, accept, metadata, stale, digest)
	})
	if fetchErr != nil {
		s.writeProxyError(w, repo.Name, fetchErr)
		return
	}
	if result.Result == "" {
		result.Result = "miss"
	}
	if joined && !wasCached && result.Result == "hit" {
		result.Result = "miss"
	}
	if r.Method == http.MethodGet && !isMetadata {
		s.metrics.Artifact(repo.Name, result.Result)
	}
	s.serveCached(w, r, result.Entry, result.Path, result.Source, repo.Name)
	if result.Stale {
		s.logger.Warn("served stale metadata after upstream failure", "repository", repo.Name, "path", r.URL.Path)
	}
}

func (s *Service) resolveDownload(ctx context.Context, repo RepositoryConfig, rel string) (*url.URL, expectedDigest, error) {
	parts := strings.Split(rel, "/")
	if len(parts) != 4 || parts[0] != "_packcove" || parts[1] != "download" || !mappingIDPattern.MatchString(parts[2]) || parts[3] == "" {
		return nil, expectedDigest{}, &gatewayError{Status: http.StatusNotFound, Reason: "not_found", Message: "download mapping not found"}
	}
	mapping, err := s.storage.ResolveDownload(ctx, parts[2])
	if errors.Is(err, errCacheMiss) || err == nil && mapping.Repository != repo.Name {
		return nil, expectedDigest{}, &gatewayError{Status: http.StatusNotFound, Reason: "not_found", Message: "download mapping not found"}
	}
	if err != nil {
		return nil, expectedDigest{}, &gatewayError{Status: http.StatusInternalServerError, Reason: "storage", Message: "download mapping unavailable"}
	}
	digest := expectedDigest{Algorithm: mapping.ExpectedAlgorithm, Value: mapping.ExpectedDigest}
	suffix := ""
	switch parts[3] {
	case mapping.Filename:
	case mapping.Filename + ".metadata":
		suffix = ".metadata"
		digest = expectedDigest{Algorithm: mapping.MetadataAlgorithm, Value: mapping.MetadataDigest}
	case mapping.Filename + ".asc":
		suffix = ".asc"
		digest = expectedDigest{}
	default:
		return nil, expectedDigest{}, &gatewayError{Status: http.StatusNotFound, Reason: "not_found", Message: "download mapping not found"}
	}
	target, err := url.Parse(mapping.UpstreamURL)
	if err != nil {
		return nil, expectedDigest{}, &gatewayError{Status: http.StatusInternalServerError, Reason: "storage", Message: "download mapping is invalid"}
	}
	if suffix != "" {
		target.Path += suffix
		target.RawPath = ""
	}
	allowed := false
	for _, host := range repo.AllowedDownloadHosts {
		if strings.EqualFold(host, target.Hostname()) {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, expectedDigest{}, &gatewayError{Status: http.StatusForbidden, Reason: "redirect_denied", Message: "download host is not allowed"}
	}
	return target, digest, nil
}

func (s *Service) fetchAndStore(ctx context.Context, repo RepositoryConfig, rel string, target *url.URL, key, accept string, metadata bool, stale *CacheEntry, digest expectedDigest) (fetchOutcome, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return fetchOutcome{}, &gatewayError{Status: http.StatusBadRequest, Reason: "invalid_path", Message: "invalid upstream URL"}
	}
	if accept != "" {
		request.Header.Set("Accept", accept)
	}
	request.Header.Set("Accept-Encoding", "identity")
	if stale != nil {
		if stale.ETag != "" {
			request.Header.Set("If-None-Match", stale.ETag)
		}
		if stale.LastModified != "" {
			request.Header.Set("If-Modified-Since", stale.LastModified)
		}
	}
	started := time.Now()
	response, err := s.clients[repo.Name].Do(request)
	s.metrics.ObserveUpstream(repo.Name, time.Since(started).Seconds())
	if err != nil {
		s.metrics.Upstream(repo.Name, "error")
		s.observeUpstream(repo.Name, http.StatusServiceUnavailable)
		reason, status, message := "upstream", http.StatusBadGateway, "upstream request failed"
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			reason, status, message = "upstream_timeout", http.StatusGatewayTimeout, "upstream request timed out"
		}
		if stale != nil && metadata && s.cfg.Cache.StaleIfError {
			entry, filePath, getErr := s.storage.Get(context.Background(), stale.Repository, stale.Key)
			if getErr == nil {
				return fetchOutcome{Entry: entry, Path: filePath, Result: "revalidated", Source: "cache", Stale: true}, nil
			}
		}
		return fetchOutcome{}, &gatewayError{Status: status, Reason: reason, Message: message}
	}
	defer response.Body.Close()
	s.observeUpstream(repo.Name, response.StatusCode)
	if response.StatusCode == http.StatusNotModified && stale != nil {
		ttl := ttlFor(repo, rel, s.cfg)
		entry, err := s.storage.Revalidate(ctx, *stale, ttl)
		if err != nil {
			return fetchOutcome{}, &gatewayError{Status: http.StatusInternalServerError, Reason: "storage", Message: "could not refresh cache index"}
		}
		entry, filePath, err := s.storage.Get(ctx, repo.Name, key)
		if err != nil {
			return fetchOutcome{}, &gatewayError{Status: http.StatusBadGateway, Reason: "cache_corrupt", Message: "validated cache file is missing"}
		}
		s.metrics.Upstream(repo.Name, "revalidated")
		return fetchOutcome{Entry: entry, Path: filePath, Result: "revalidated", Source: "cache"}, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone {
			s.metrics.Upstream(repo.Name, "not_found")
		} else {
			s.metrics.Upstream(repo.Name, "error")
		}
		if stale != nil && metadata && s.cfg.Cache.StaleIfError && response.StatusCode >= 500 {
			entry, filePath, getErr := s.storage.Get(context.Background(), stale.Repository, stale.Key)
			if getErr == nil {
				return fetchOutcome{Entry: entry, Path: filePath, Result: "revalidated", Source: "cache", Stale: true}, nil
			}
		}
		return fetchOutcome{}, &upstreamStatusError{Status: response.StatusCode, ContentType: response.Header.Get("Content-Type"), Body: body}
	}
	var body io.Reader = response.Body
	contentType := response.Header.Get("Content-Type")
	contentLength := response.ContentLength
	etag := response.Header.Get("ETag")
	lastModified := response.Header.Get("Last-Modified")
	contentEncoding := response.Header.Get("Content-Encoding")
	vary := response.Header.Get("Vary")
	if metadata && (repo.Type == "npm" || repo.Type == "pypi") {
		if contentEncoding != "" && !strings.EqualFold(contentEncoding, "identity") {
			return fetchOutcome{}, &gatewayError{Status: http.StatusBadGateway, Reason: "invalid_metadata", Message: "upstream returned encoded metadata that cannot be rewritten"}
		}
		original, err := readMetadata(response.Body)
		if err != nil {
			return fetchOutcome{}, &gatewayError{Status: http.StatusBadGateway, Reason: "upstream", Message: "could not read upstream metadata"}
		}
		rewritten, newContentType, err := rewriteMetadata(ctx, s.storage, s.cfg, repo, target, contentType, accept, original)
		if err != nil {
			return fetchOutcome{}, &gatewayError{Status: http.StatusBadGateway, Reason: "invalid_metadata", Message: "upstream metadata contains invalid or disallowed links"}
		}
		body = bytes.NewReader(rewritten)
		contentType = newContentType
		contentLength = int64(len(rewritten))
		etag = ""
		lastModified = ""
		contentEncoding = ""
		vary = mergeVary(vary, "Accept")
	}
	s.metrics.Upstream(repo.Name, "success")
	now := time.Now().UTC()
	cacheEntry := CacheEntry{
		Repository: repo.Name, Key: key, ContentType: contentType,
		ContentEncoding: contentEncoding, Vary: vary,
		CacheControl: response.Header.Get("Cache-Control"), ETag: etag,
		LastModified: lastModified, ValidatedAt: now, FreshUntil: expiry(now, ttlFor(repo, rel, s.cfg)),
		UpstreamURL: target.String(),
	}
	entry, filePath, err := s.storage.Store(ctx, cacheEntry, body, contentLength, digest.Algorithm, digest.Value)
	if err != nil {
		reason := "storage"
		if strings.Contains(err.Error(), "length mismatch") {
			reason = "upstream"
		} else if strings.Contains(err.Error(), "digest mismatch") {
			reason = "integrity"
		}
		return fetchOutcome{}, &gatewayError{Status: http.StatusBadGateway, Reason: reason, Message: "upstream response could not be stored completely"}
	}
	result := "miss"
	if stale != nil {
		result = "revalidated"
	}
	return fetchOutcome{Entry: entry, Path: filePath, Result: result, Source: "upstream"}, nil
}

func (s *Service) serveCached(w *responseRecorder, r *http.Request, entry CacheEntry, filePath, source, repository string) {
	file, err := os.Open(filePath)
	if err != nil {
		s.writeProxyError(w, repository, &gatewayError{Status: http.StatusInternalServerError, Reason: "storage", Message: "cached file is unavailable"})
		return
	}
	defer file.Close()
	if entry.ContentType != "" {
		w.Header().Set("Content-Type", entry.ContentType)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	if entry.Vary != "" {
		w.Header().Set("Vary", entry.Vary)
	}
	if entry.ContentEncoding != "" {
		w.Header().Set("Content-Encoding", entry.ContentEncoding)
	}
	if entry.CacheControl != "" {
		w.Header().Set("Cache-Control", entry.CacheControl)
	}
	if entry.ETag != "" {
		w.Header().Set("ETag", entry.ETag)
		for _, candidate := range strings.Split(r.Header.Get("If-None-Match"), ",") {
			candidate = strings.TrimSpace(candidate)
			if candidate == "*" || candidate == entry.ETag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
	}
	var modTime time.Time
	if entry.LastModified != "" {
		if parsed, err := time.Parse(http.TimeFormat, entry.LastModified); err == nil {
			modTime = parsed
			w.Header().Set("Last-Modified", entry.LastModified)
		}
	}
	if modTime.IsZero() {
		modTime = entry.ValidatedAt
	}
	before := w.bytes
	http.ServeContent(w, r, path.Base(entry.UpstreamURL), modTime, file)
	s.metrics.Bytes(repository, source, w.bytes-before)
}

func (s *Service) writeProxyError(w *responseRecorder, repository string, err error) {
	var gateway *gatewayError
	if errors.As(err, &gateway) {
		s.metrics.Error(repository, gateway.Reason)
		s.logger.Warn("repository request failed", "repository", repository, "reason", gateway.Reason, "error", gateway.Message)
		http.Error(w, gateway.Message, gateway.Status)
		return
	}
	var upstream *upstreamStatusError
	if errors.As(err, &upstream) {
		status := upstream.Status
		if status >= 500 {
			status = http.StatusBadGateway
		}
		reason := "upstream"
		if upstream.Status == http.StatusNotFound || upstream.Status == http.StatusGone {
			reason = "not_found"
		}
		s.metrics.Error(repository, reason)
		if upstream.ContentType != "" {
			w.Header().Set("Content-Type", upstream.ContentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write(upstream.Body)
		return
	}
	s.metrics.Error(repository, "upstream")
	http.Error(w, "repository request failed", http.StatusBadGateway)
}

func validResourcePath(rel string) bool {
	if strings.ContainsAny(rel, "\\\x00\r\n") {
		return false
	}
	for _, segment := range strings.Split(rel, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}
