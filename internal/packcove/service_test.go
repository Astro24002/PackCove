package packcove

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type harness struct {
	proxy   *httptest.Server
	service *Service
	client  *http.Client
}

func npmArchive(t *testing.T, name, version string) []byte {
	t.Helper()
	var output bytes.Buffer
	compressed := gzip.NewWriter(&output)
	writer := tar.NewWriter(compressed)
	packageJSON, _ := json.Marshal(map[string]string{"name": name, "version": version, "main": "index.js"})
	for fileName, content := range map[string][]byte{
		"package/package.json": packageJSON,
		"package/index.js":     []byte("module.exports = 'fixture';\n"),
	} {
		if err := writer.WriteHeader(&tar.Header{Name: fileName, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func startHarness(t *testing.T, upstream *httptest.Server, dataDir string, staleIfError bool) *harness {
	t.Helper()
	upstreamURL := upstream.URL
	return startConfiguredProxy(t, dataDir, staleIfError, []RepositoryConfig{
		{Name: "maven", Type: "maven", Upstream: upstreamURL + "/maven2/", AllowedDownloadHosts: []string{"127.0.0.1"}},
		{Name: "npm", Type: "npm", Upstream: upstreamURL + "/npm/", AllowedDownloadHosts: []string{"127.0.0.1"}},
		{Name: "pypi", Type: "pypi", Upstream: upstreamURL + "/simple/", AllowedDownloadHosts: []string{"127.0.0.1"}},
	})
}

func startConfiguredProxy(t *testing.T, dataDir string, staleIfError bool, repositories []RepositoryConfig) *harness {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &httptest.Server{Listener: listener, Config: &http.Server{Handler: http.NotFoundHandler()}}
	config, err := ValidateConfig(Config{
		Server:       ServerConfig{Listen: "127.0.0.1:0", PublicBaseURL: "http://" + listener.Addr().String()},
		Storage:      StorageConfig{DataDir: dataDir},
		Cache:        CacheConfig{MetadataTTL: "5m", MutableArtifactTTL: "5m", ImmutableArtifactTTL: "never", StaleIfError: staleIfError},
		Upstream:     UpstreamConfig{ConnectTimeout: "1s", RequestTimeout: "5s", MaxRedirects: 5},
		Repositories: repositories,
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(config, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	proxy.Config.Handler = service
	proxy.Start()
	t.Cleanup(func() {
		proxy.Close()
		if err := service.Close(); err != nil {
			t.Errorf("close service: %v", err)
		}
	})
	return &harness{proxy: proxy, service: service, client: proxy.Client()}
}

func request(t *testing.T, h *harness, method, path string, headers http.Header) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, h.proxy.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	response, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, response.Header, body
}

func TestValidateConfigDefaultsAndErrors(t *testing.T) {
	cfg := Config{
		Server:       ServerConfig{PublicBaseURL: "https://packcove.example.internal"},
		Storage:      StorageConfig{DataDir: t.TempDir()},
		Repositories: []RepositoryConfig{{Name: "central", Type: "maven", Upstream: "https://repo.example/maven", AllowedDownloadHosts: []string{"repo.example"}}},
	}
	validated, err := ValidateConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if validated.MetadataTTL != 5*time.Minute || validated.MutableTTL != 5*time.Minute || validated.ImmutableTTL != 0 {
		t.Fatalf("unexpected default cache TTLs: %#v", validated)
	}
	if validated.Repositories[0].Upstream != "https://repo.example/maven/" {
		t.Fatalf("upstream trailing slash not normalized: %q", validated.Repositories[0].Upstream)
	}

	cfg.Repositories = append(cfg.Repositories, cfg.Repositories[0])
	if _, err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("expected duplicate repository error, got %v", err)
	}
	cfg.Repositories = cfg.Repositories[:1]
	cfg.Repositories[0].AllowedDownloadHosts = []string{"other.example"}
	if _, err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "must include upstream host") {
		t.Fatalf("expected allowed host validation error, got %v", err)
	}
}

func TestSampleConfigLoads(t *testing.T) {
	cfg, err := LoadConfig("../../packcove.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Repositories) != 3 {
		t.Fatalf("sample config contains %d repositories, want 3", len(cfg.Repositories))
	}
}

func TestKubernetesManifestYAMLParses(t *testing.T) {
	file, err := os.Open("../../deploy/kubernetes/packcove.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	decoder := yaml.NewDecoder(file)
	documents := 0
	for {
		var document yaml.Node
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(document.Content) != 0 {
			documents++
		}
	}
	if documents != 5 {
		t.Fatalf("manifest contains %d YAML documents, want 5", documents)
	}
}

func TestMavenCacheRangeHeadAndRestart(t *testing.T) {
	body := []byte("0123456789abcdef")
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/maven2/org/example/demo/1.0/demo-1.0.jar" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/java-archive")
		w.Header().Set("ETag", `"artifact-v1"`)
		_, _ = w.Write(body)
	}))
	defer upstream.Close()
	dataDir := t.TempDir() + "/cache"
	first := startHarness(t, upstream, dataDir, false)
	path := "/repository/maven/org/example/demo/1.0/demo-1.0.jar"
	status, _, got := request(t, first, http.MethodGet, path, nil)
	if status != http.StatusOK || string(got) != string(body) {
		t.Fatalf("cold request = %d %q", status, got)
	}
	status, _, got = request(t, first, http.MethodGet, path, nil)
	if status != http.StatusOK || string(got) != string(body) || calls.Load() != 1 {
		t.Fatalf("hot request = %d %q, upstream calls %d", status, got, calls.Load())
	}
	status, headers, got := request(t, first, http.MethodHead, path, nil)
	if status != http.StatusOK || len(got) != 0 || headers.Get("Content-Length") != fmt.Sprint(len(body)) {
		t.Fatalf("HEAD = %d len=%d content-length=%q", status, len(got), headers.Get("Content-Length"))
	}
	status, headers, got = request(t, first, http.MethodGet, path, http.Header{"Range": []string{"bytes=2-5"}})
	if status != http.StatusPartialContent || string(got) != "2345" || headers.Get("Content-Range") != "bytes 2-5/16" {
		t.Fatalf("range = %d %q %q", status, got, headers.Get("Content-Range"))
	}
	first.proxy.Close()
	if err := first.service.Close(); err != nil {
		t.Fatal(err)
	}
	second := startHarness(t, upstream, dataDir, false)
	status, _, got = request(t, second, http.MethodGet, path, nil)
	if status != http.StatusOK || string(got) != string(body) || calls.Load() != 1 {
		t.Fatalf("post-restart request = %d %q, upstream calls %d", status, got, calls.Load())
	}
}

func TestMavenMetadataConditionalRevalidation(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("If-None-Match") == `"metadata-v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"metadata-v1"`)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, "<metadata><versioning><release>1.0</release></versioning></metadata>")
	}))
	defer upstream.Close()
	h := startHarness(t, upstream, t.TempDir()+"/cache", false)
	path := "/repository/maven/org/example/demo/maven-metadata.xml"
	status, _, first := request(t, h, http.MethodGet, path, nil)
	if status != http.StatusOK {
		t.Fatalf("metadata request = %d", status)
	}
	if _, err := h.service.storage.db.Exec(`UPDATE cache_entries SET fresh_until=1 WHERE repository='maven'`); err != nil {
		t.Fatal(err)
	}
	status, _, second := request(t, h, http.MethodGet, path, nil)
	if status != http.StatusOK || string(first) != string(second) || calls.Load() != 2 {
		t.Fatalf("revalidation = %d %q, upstream calls %d", status, second, calls.Load())
	}
	request(t, h, http.MethodGet, path, nil)
	if calls.Load() != 2 {
		t.Fatalf("fresh metadata caused another upstream request: %d", calls.Load())
	}
}

func TestNPMMetadataTarballMappingAndVariants(t *testing.T) {
	archive := npmArchive(t, "packcove-fixture", "1.0.0")
	sha1sum := sha1.Sum(archive)
	var metadataCalls, tarballCalls atomic.Int64
	var upstreamURL string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/npm/packcove-fixture":
			metadataCalls.Add(1)
			metadata := map[string]any{
				"name": "packcove-fixture", "dist-tags": map[string]string{"latest": "1.0.0"},
				"versions": map[string]any{"1.0.0": map[string]any{
					"name": "packcove-fixture", "version": "1.0.0", "dependencies": map[string]string{},
					"dist": map[string]string{"tarball": upstreamURL + "/npm/packcove-fixture/-/packcove-fixture-1.0.0.tgz", "shasum": hex.EncodeToString(sha1sum[:])},
				}},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(metadata)
		case "/npm/packcove-fixture/-/packcove-fixture-1.0.0.tgz":
			tarballCalls.Add(1)
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	upstreamURL = upstream.URL
	defer upstream.Close()
	h := startHarness(t, upstream, t.TempDir()+"/cache", false)
	status, _, body := request(t, h, http.MethodGet, "/repository/npm/packcove-fixture", http.Header{"Accept": []string{"application/vnd.npm.install-v1+json"}})
	if status != http.StatusOK {
		t.Fatalf("npm metadata = %d %s", status, body)
	}
	var metadata map[string]any
	if err := json.Unmarshal(body, &metadata); err != nil {
		t.Fatal(err)
	}
	versions := metadata["versions"].(map[string]any)
	dist := versions["1.0.0"].(map[string]any)["dist"].(map[string]any)
	tarballURL := dist["tarball"].(string)
	if !strings.HasPrefix(tarballURL, h.proxy.URL+"/repository/npm/_packcove/download/") || dist["shasum"] != hex.EncodeToString(sha1sum[:]) {
		t.Fatalf("npm metadata not safely rewritten: %#v", dist)
	}
	status, _, got := request(t, h, http.MethodGet, strings.TrimPrefix(tarballURL, h.proxy.URL), nil)
	if status != http.StatusOK || string(got) != string(archive) {
		t.Fatalf("tarball request = %d, body %q, bytes %d", status, got, len(got))
	}
	request(t, h, http.MethodGet, strings.TrimPrefix(tarballURL, h.proxy.URL), nil)
	if tarballCalls.Load() != 1 {
		t.Fatalf("tarball not cached, upstream calls %d", tarballCalls.Load())
	}
	otherRepositoryURL := strings.Replace(tarballURL, "/repository/npm/", "/repository/pypi/", 1)
	status, _, _ = request(t, h, http.MethodGet, strings.TrimPrefix(otherRepositoryURL, h.proxy.URL), nil)
	if status != http.StatusNotFound {
		t.Fatalf("download mapping crossed repositories: status %d", status)
	}
	_, _, _ = request(t, h, http.MethodGet, "/repository/npm/packcove-fixture", http.Header{"Accept": []string{"application/json"}})
	if metadataCalls.Load() != 2 {
		t.Fatalf("Accept variants were mixed, upstream metadata calls %d", metadataCalls.Load())
	}
}

func TestScopedNPMPathPreservesEncodedSlash(t *testing.T) {
	var path string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.EscapedPath()
		_, _ = io.WriteString(w, `{"name":"@packcove/fixture","versions":{}}`)
	}))
	defer upstream.Close()
	h := startHarness(t, upstream, t.TempDir()+"/cache", false)
	status, _, _ := request(t, h, http.MethodGet, "/repository/npm/%40packcove%2Ffixture", nil)
	if status != http.StatusOK || path != "/npm/%40packcove%2Ffixture" {
		t.Fatalf("scoped request = %d, upstream path %q", status, path)
	}
}

func TestPyPIMetadataVariantsRewriteLinks(t *testing.T) {
	var htmlCalls, jsonCalls, downloadCalls, metadataDownloads atomic.Int64
	var upstreamURL string
	wheelDigest := sha256.Sum256([]byte("wheel-content"))
	wheelDigestHex := hex.EncodeToString(wheelDigest[:])
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/simple/":
			if strings.Contains(r.Header.Get("Accept"), "json") {
				w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
				_, _ = io.WriteString(w, `{"meta":{"api-version":"1.4"},"projects":[{"name":"Demo_Pkg"}]}`)
				return
			}
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, `<html><body><a href="/simple/Demo_Pkg/">Demo_Pkg</a></body></html>`)
		case "/simple/demo-pkg/":
			if strings.Contains(r.Header.Get("Accept"), "json") {
				jsonCalls.Add(1)
				w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
				_, _ = io.WriteString(w, fmt.Sprintf(`{"meta":{"api-version":"1.4"},"name":"demo-pkg","files":[{"filename":"demo_pkg-1.0-py3-none-any.whl","url":%q,"hashes":{"sha256":%q},"requires-python":">=3.8","yanked":"withdrawn","core-metadata":true}]}`, upstreamURL+"/files/demo_pkg-1.0-py3-none-any.whl", wheelDigestHex))
				return
			}
			htmlCalls.Add(1)
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, fmt.Sprintf(`<html><body><a href="%s/files/demo_pkg-1.0-py3-none-any.whl#sha256=%s" data-requires-python="&gt;=3.8" data-yanked="withdrawn" data-core-metadata="true">demo_pkg-1.0-py3-none-any.whl</a></body></html>`, upstreamURL, wheelDigestHex))
		case "/files/demo_pkg-1.0-py3-none-any.whl":
			downloadCalls.Add(1)
			_, _ = io.WriteString(w, "wheel-content")
		case "/files/demo_pkg-1.0-py3-none-any.whl.metadata":
			metadataDownloads.Add(1)
			_, _ = io.WriteString(w, "Metadata-Version: 2.1\nName: demo-pkg\nVersion: 1.0\n")
		default:
			http.NotFound(w, r)
		}
	}))
	upstreamURL = upstream.URL
	defer upstream.Close()
	h := startHarness(t, upstream, t.TempDir()+"/cache", false)
	status, headers, htmlBody := request(t, h, http.MethodGet, "/repository/pypi/simple/demo_pkg/", http.Header{"Accept": []string{"text/html"}})
	if status != http.StatusOK || headers.Get("Content-Type") == "" || !strings.Contains(string(htmlBody), h.proxy.URL+"/repository/pypi/_packcove/download/") || !strings.Contains(string(htmlBody), "data-yanked=\"withdrawn\"") || !strings.Contains(string(htmlBody), "#sha256="+wheelDigestHex) {
		t.Fatalf("PyPI HTML rewrite missing fields: %d %s", status, htmlBody)
	}
	status, _, jsonBody := request(t, h, http.MethodGet, "/repository/pypi/simple/demo_pkg/", http.Header{"Accept": []string{"application/vnd.pypi.simple.v1+json"}})
	if status != http.StatusOK || !strings.Contains(string(jsonBody), "repository/pypi/_packcove/download") || !strings.Contains(string(jsonBody), "requires-python") || !strings.Contains(string(jsonBody), "withdrawn") {
		t.Fatalf("PyPI JSON rewrite missing fields: %d %s", status, jsonBody)
	}
	if htmlCalls.Load() != 1 || jsonCalls.Load() != 1 {
		t.Fatalf("PyPI Accept variants were not separated: html=%d json=%d", htmlCalls.Load(), jsonCalls.Load())
	}
	hrefStart := strings.Index(string(htmlBody), `href="`)
	if hrefStart < 0 {
		t.Fatalf("PyPI HTML link missing: %s", htmlBody)
	}
	hrefStart += len(`href="`)
	hrefEnd := strings.Index(string(htmlBody[hrefStart:]), `"`)
	if hrefEnd < 0 {
		t.Fatalf("PyPI HTML href not terminated: %s", htmlBody)
	}
	href := string(htmlBody[hrefStart : hrefStart+hrefEnd])
	parsedHref, err := url.Parse(href)
	if err != nil {
		t.Fatal(err)
	}
	status, _, fileBody := request(t, h, http.MethodGet, parsedHref.EscapedPath(), nil)
	if status != http.StatusOK || string(fileBody) != "wheel-content" {
		t.Fatalf("PyPI mapped wheel download = %d %q", status, fileBody)
	}
	metadataPath := strings.TrimSuffix(parsedHref.EscapedPath(), ".whl") + ".whl.metadata"
	status, _, metadataBody := request(t, h, http.MethodGet, metadataPath, nil)
	if status != http.StatusOK || !strings.Contains(string(metadataBody), "Name: demo-pkg") || metadataDownloads.Load() != 1 {
		t.Fatalf("PyPI associated metadata = %d %q, upstream calls %d", status, metadataBody, metadataDownloads.Load())
	}
	status, _, _ = request(t, h, http.MethodGet, "/repository/pypi/simple/", http.Header{"Accept": []string{"text/html"}})
	if status != http.StatusOK {
		t.Fatalf("PyPI root index = %d", status)
	}
	if !strings.Contains(string(htmlBody), "data-requires-python") {
		t.Fatal("PyPI file metadata was not preserved")
	}
	if downloadCalls.Load() != 1 {
		t.Fatalf("wheel was not fetched exactly once: %d", downloadCalls.Load())
	}
}

func TestConcurrentMissesShareOneUpstreamRequest(t *testing.T) {
	var calls atomic.Int64
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		_, _ = io.WriteString(w, "shared complete artifact")
	}))
	defer upstream.Close()
	h := startHarness(t, upstream, t.TempDir()+"/cache", false)
	const clients = 12
	var wg sync.WaitGroup
	errs := make(chan error, clients)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response, err := h.client.Get(h.proxy.URL + "/repository/maven/org/example/demo/1.0/demo.jar")
			if err != nil {
				errs <- err
				return
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				errs <- err
				return
			}
			if response.StatusCode != http.StatusOK || string(body) != "shared complete artifact" {
				errs <- fmt.Errorf("response = %d %q", response.StatusCode, body)
			}
		}()
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request did not start")
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.Load())
	}
}

func TestUpstreamErrorsDoNotCreateCacheEntries(t *testing.T) {
	var truncatedCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/maven2/missing.jar":
			http.NotFound(w, r)
		case "/maven2/broken.jar":
			truncatedCalls.Add(1)
			w.Header().Set("Content-Length", "20")
			_, _ = io.WriteString(w, "short")
		case "/maven2/error.jar":
			http.Error(w, "upstream failure", http.StatusInternalServerError)
		case "/maven2/redirect.jar":
			http.Redirect(w, r, "http://example.com/evil.jar", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	h := startHarness(t, upstream, t.TempDir()+"/cache", false)
	for _, test := range []struct {
		path string
		want int
	}{{"missing.jar", http.StatusNotFound}, {"error.jar", http.StatusBadGateway}, {"redirect.jar", http.StatusBadGateway}, {"broken.jar", http.StatusBadGateway}} {
		for repeat := 0; repeat < 2; repeat++ {
			status, _, _ := request(t, h, http.MethodGet, "/repository/maven/"+test.path, nil)
			if status != test.want {
				t.Errorf("%s attempt %d = %d, want %d", test.path, repeat+1, status, test.want)
			}
		}
	}
	if truncatedCalls.Load() != 2 {
		t.Fatalf("truncated file attempts = %d, want 2", truncatedCalls.Load())
	}
}

func TestStaleIfErrorOnlyServesWhenEnabled(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled_%v", enabled), func(t *testing.T) {
			var fail atomic.Bool
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if fail.Load() {
					http.Error(w, "offline", http.StatusServiceUnavailable)
					return
				}
				_, _ = io.WriteString(w, "<metadata><version>1</version></metadata>")
			}))
			defer upstream.Close()
			h := startHarness(t, upstream, t.TempDir()+"/cache", enabled)
			path := "/repository/maven/org/example/maven-metadata.xml"
			status, _, body := request(t, h, http.MethodGet, path, nil)
			if status != http.StatusOK {
				t.Fatalf("initial metadata = %d", status)
			}
			if _, err := h.service.storage.db.Exec(`UPDATE cache_entries SET fresh_until=1 WHERE repository='maven'`); err != nil {
				t.Fatal(err)
			}
			fail.Store(true)
			status, _, stale := request(t, h, http.MethodGet, path, nil)
			if enabled && (status != http.StatusOK || string(stale) != string(body)) {
				t.Fatalf("stale response = %d %q", status, stale)
			}
			if !enabled && status != http.StatusBadGateway {
				t.Fatalf("stale disabled response = %d", status)
			}
		})
	}
}

func TestOperationsAndReadOnlyMethods(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	h := startHarness(t, upstream, t.TempDir()+"/cache", false)
	status, _, body := request(t, h, http.MethodGet, "/_packcove/health", nil)
	if status != http.StatusOK || !strings.Contains(string(body), `"status":"ok"`) {
		t.Fatalf("health = %d %s", status, body)
	}
	status, _, body = request(t, h, http.MethodGet, "/_packcove/repositories", nil)
	if status != http.StatusOK || !strings.Contains(string(body), `"upstream_status":"unknown"`) || strings.Contains(string(body), "?") {
		t.Fatalf("repositories response leaked or misreported state: %d %s", status, body)
	}
	status, headers, _ := request(t, h, http.MethodPut, "/repository/maven/org/example/demo/1.0/demo.jar", nil)
	if status != http.StatusMethodNotAllowed || headers.Get("Allow") != "GET, HEAD" {
		t.Fatalf("publishing response = %d, Allow %q", status, headers.Get("Allow"))
	}
	status, headers, body = request(t, h, http.MethodGet, "/metrics", nil)
	if status != http.StatusOK || !strings.Contains(headers.Get("Content-Type"), "text/plain") || !strings.Contains(string(body), "packcove_cache_bytes") {
		t.Fatalf("metrics = %d %q %s", status, headers.Get("Content-Type"), body)
	}
	status, _, body = request(t, h, http.MethodGet, "/_packcove/stats", nil)
	if status != http.StatusOK || !strings.Contains(string(body), "uptime_seconds") {
		t.Fatalf("stats = %d %s", status, body)
	}
}

func TestPathTraversalAndUnknownMappingAreRejected(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	h := startHarness(t, upstream, t.TempDir()+"/cache", false)
	for _, path := range []string{"/repository/maven/%2e%2e/secret", "/repository/npm/_packcove/download/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/file.tgz"} {
		status, _, _ := request(t, h, http.MethodGet, path, nil)
		if status != http.StatusBadRequest && status != http.StatusNotFound {
			t.Errorf("%s = %d", path, status)
		}
	}
}

func TestServiceCanBeCreatedAndClosedWithoutRun(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = listener.Close()

	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	config, err := ValidateConfig(Config{
		Server: ServerConfig{PublicBaseURL: "http://localhost:8080"}, Storage: StorageConfig{DataDir: t.TempDir()},
		Repositories: []RepositoryConfig{{Name: "maven", Type: "maven", Upstream: upstream.URL + "/maven2/", AllowedDownloadHosts: []string{"127.0.0.1"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.storage.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTrustedDigestMismatchDoesNotPublishBlob(t *testing.T) {
	store, err := OpenStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	entry := CacheEntry{Repository: "npm", Key: "fixture"}
	_, _, err = store.Store(context.Background(), entry, strings.NewReader("untrusted bytes"), -1, "sha256", strings.Repeat("0", 64))
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("expected integrity failure, got %v", err)
	}
	if _, _, err := store.Get(context.Background(), "npm", "fixture"); !errors.Is(err, errCacheMiss) {
		t.Fatalf("mismatched blob was indexed: %v", err)
	}
	tmpFiles, err := os.ReadDir(store.root + "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if len(tmpFiles) != 0 {
		t.Fatalf("failed write left temporary blobs: %v", tmpFiles)
	}
}
