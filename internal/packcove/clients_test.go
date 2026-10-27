package packcove

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func clientTestsEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("PACKCOVE_RUN_CLIENT_TESTS") != "1" {
		t.Skip("set PACKCOVE_RUN_CLIENT_TESTS=1 to run package manager client tests")
	}
}

func TestNPMClientInstall(t *testing.T) {
	clientTestsEnabled(t)
	npmPath, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("npm is not installed")
	}
	archive := npmArchive(t, "packcove-fixture", "1.0.0")
	integrity := sha512.Sum512(archive)
	integrityText := "sha512-" + base64.StdEncoding.EncodeToString(integrity[:])
	var metadataCalls, tarballCalls atomic.Int64
	var upstreamURL string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/npm/packcove-fixture":
			metadataCalls.Add(1)
			metadata := map[string]any{
				"name": "packcove-fixture", "dist-tags": map[string]string{"latest": "1.0.0"},
				"versions": map[string]any{"1.0.0": map[string]any{
					"name": "packcove-fixture", "version": "1.0.0", "main": "index.js", "dependencies": map[string]string{},
					"dist": map[string]string{"tarball": upstreamURL + "/npm/packcove-fixture/-/packcove-fixture-1.0.0.tgz", "integrity": integrityText},
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
	for client := 0; client < 2; client++ {
		clientRoot := filepath.Join(t.TempDir(), fmt.Sprintf("npm-client-%d", client))
		command := exec.Command(npmPath, "install", "--ignore-scripts", "--no-audit", "--no-fund", "--registry", h.proxy.URL+"/repository/npm/", "--prefix", clientRoot, "packcove-fixture@1.0.0")
		command.Env = append(os.Environ(), "npm_config_cache="+filepath.Join(clientRoot, ".npm-cache"), "npm_config_update_notifier=false")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("npm client %d failed: %v\n%s", client, err, output)
		}
		nodePath, err := exec.LookPath("node")
		if err != nil {
			t.Skip("node is not installed")
		}
		verify := exec.Command(nodePath, "-e", "if (require(process.argv[1]) !== 'fixture') process.exit(1)", filepath.Join(clientRoot, "node_modules", "packcove-fixture"))
		if output, err := verify.CombinedOutput(); err != nil {
			t.Fatalf("installed npm package could not be loaded: %v\n%s", err, output)
		}
	}
	if tarballCalls.Load() != 1 {
		t.Fatalf("two isolated npm caches fetched tarball %d times, want 1", tarballCalls.Load())
	}
	if metadataCalls.Load() < 1 {
		t.Fatal("npm did not request package metadata")
	}
}

func TestPyPIClientInstall(t *testing.T) {
	clientTestsEnabled(t)
	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("Docker is not installed")
	}
	wheel := pythonWheel(t)
	wheelHash := sha256.Sum256(wheel)
	wheelHashHex := hex.EncodeToString(wheelHash[:])
	var indexCalls, fileCalls atomic.Int64
	var upstreamURL string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/simple/packcove-fixture/" {
			indexCalls.Add(1)
			if strings.Contains(r.Header.Get("Accept"), "json") {
				w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"meta": map[string]string{"api-version": "1.4"}, "name": "packcove-fixture",
					"files": []map[string]any{{
						"filename": "packcove_fixture-1.0.0-py3-none-any.whl",
						"url":      upstreamURL + "/files/packcove_fixture-1.0.0-py3-none-any.whl",
						"hashes":   map[string]string{"sha256": wheelHashHex}, "requires-python": ">=3.8", "yanked": false,
					}},
				})
				return
			}
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprintf(w, `<html><body><a href="%s/files/packcove_fixture-1.0.0-py3-none-any.whl#sha256=%s" data-requires-python="&gt;=3.8">packcove_fixture-1.0.0-py3-none-any.whl</a></body></html>`, upstreamURL, wheelHashHex)
			return
		}
		if r.URL.Path == "/files/packcove_fixture-1.0.0-py3-none-any.whl" {
			fileCalls.Add(1)
			_, _ = w.Write(wheel)
			return
		}
		http.NotFound(w, r)
	}))
	upstreamURL = upstream.URL
	defer upstream.Close()
	h := startHarness(t, upstream, t.TempDir()+"/cache", false)
	clientRoot := t.TempDir()
	registry := h.proxy.URL + "/repository/pypi/simple/"
	for client := 0; client < 2; client++ {
		installDir := filepath.Join(clientRoot, fmt.Sprintf("installed-%d", client))
		command := exec.Command(dockerPath, "run", "--rm", "--network", "host", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "-e", "HOME=/tmp", "-v", clientRoot+":/fixture", "python:3.12-slim",
			"python", "-m", "pip", "install", "--disable-pip-version-check", "--no-cache-dir", "--no-deps", "--target", "/fixture/"+filepath.Base(installDir), "--index-url", registry, "packcove-fixture==1.0.0")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("pip client %d failed: %v\n%s", client, err, output)
		}
		verify := exec.Command(dockerPath, "run", "--rm", "-v", clientRoot+":/fixture", "python:3.12-slim", "python", "-c",
			"import sys; sys.path.insert(0, '/fixture/"+filepath.Base(installDir)+"'); import packcove_fixture; assert packcove_fixture.VALUE == 'fixture'")
		if output, err := verify.CombinedOutput(); err != nil {
			t.Fatalf("installed PyPI package could not be imported: %v\n%s", err, output)
		}
	}
	if fileCalls.Load() != 1 {
		t.Fatalf("two isolated pip clients fetched the wheel %d times, want 1", fileCalls.Load())
	}
	if indexCalls.Load() < 1 {
		t.Fatal("pip did not request a project index")
	}
}

func TestMavenClientInstall(t *testing.T) {
	clientTestsEnabled(t)
	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("Docker is not installed")
	}
	proxyListener, err := netListen()
	if err != nil {
		t.Fatal(err)
	}
	defer proxyListener.Close()
	config, err := ValidateConfig(Config{
		Server:       ServerConfig{Listen: "127.0.0.1:0", PublicBaseURL: "http://" + proxyListener.Addr().String()},
		Storage:      StorageConfig{DataDir: t.TempDir() + "/cache"},
		Repositories: []RepositoryConfig{{Name: "maven", Type: "maven", Upstream: "https://maven-central.storage-download.googleapis.com/maven2/", AllowedDownloadHosts: []string{"maven-central.storage-download.googleapis.com"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(config, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	proxy := &httptest.Server{Listener: proxyListener, Config: &http.Server{Handler: service}}
	proxy.Start()
	client := proxy.Client()
	t.Cleanup(func() {
		proxy.Close()
		_ = service.Close()
	})
	testRoot := t.TempDir()
	settings := fmt.Sprintf(`<settings><mirrors><mirror><id>packcove-central</id><mirrorOf>central</mirrorOf><url>%s/repository/maven/</url></mirror></mirrors></settings>`, proxy.URL)
	pom := `<project xmlns="http://maven.apache.org/POM/4.0.0"><modelVersion>4.0.0</modelVersion><groupId>org.packcove.test</groupId><artifactId>fixture</artifactId><version>1.0</version></project>`
	if err := os.WriteFile(filepath.Join(testRoot, "settings.xml"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(testRoot, "pom.xml"), []byte(pom), 0o600); err != nil {
		t.Fatal(err)
	}
	readUpstreamRequests := func() uint64 {
		t.Helper()
		response, err := client.Get(proxy.URL + "/_packcove/stats")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var stats struct {
			Repositories map[string]struct {
				Upstream map[string]uint64 `json:"upstream_requests"`
			} `json:"repositories"`
		}
		if err := json.NewDecoder(response.Body).Decode(&stats); err != nil {
			t.Fatal(err)
		}
		var total uint64
		for _, count := range stats.Repositories["maven"].Upstream {
			total += count
		}
		return total
	}
	defer func() {
		uid := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
		cleanup := exec.Command(dockerPath, "run", "--rm", "-v", testRoot+":/fixture", "--entrypoint", "/usr/bin/chown", "maven:3.9.11-eclipse-temurin-17", "-R", uid, "/fixture")
		if output, err := cleanup.CombinedOutput(); err != nil {
			t.Errorf("restore test directory ownership: %v\n%s", err, output)
		}
	}()
	for index, repository := range []string{"m2-a", "m2-b"} {
		before := readUpstreamRequests()
		command := exec.Command(dockerPath, "run", "--rm", "--network", "host", "-v", testRoot+":/fixture", "maven:3.9.11-eclipse-temurin-17",
			"mvn", "-B", "-s", "/fixture/settings.xml", "-f", "/fixture/pom.xml", "-Dmaven.repo.local=/fixture/"+repository,
			"org.apache.maven.plugins:maven-dependency-plugin:3.6.1:get", "-Dartifact=org.apache.commons:commons-lang3:3.14.0", "-Dtransitive=false")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("Maven client with %s failed: %v\n%s", repository, err, output)
		}
		after := readUpstreamRequests()
		if index == 0 && after <= before {
			t.Fatal("cold Maven install did not reach the configured upstream")
		}
		if index == 1 && after != before {
			t.Fatalf("hot Maven install made %d extra upstream requests", after-before)
		}
	}
	if _, err := os.Stat(filepath.Join(testRoot, "m2-b/org/apache/commons/commons-lang3/3.14.0/commons-lang3-3.14.0.jar")); err != nil {
		t.Fatalf("Maven did not resolve the dependency into its isolated local cache: %v", err)
	}
}

func pythonWheel(t *testing.T) []byte {
	t.Helper()
	files := map[string][]byte{
		"packcove_fixture/__init__.py":              []byte("VALUE = 'fixture'\n"),
		"packcove_fixture-1.0.0.dist-info/METADATA": []byte("Metadata-Version: 2.1\nName: packcove-fixture\nVersion: 1.0.0\n\n"),
		"packcove_fixture-1.0.0.dist-info/WHEEL":    []byte("Wheel-Version: 1.0\nGenerator: PackCove test\nRoot-Is-Purelib: true\nTag: py3-none-any\n\n"),
	}
	var record strings.Builder
	for name, content := range files {
		checksum := sha256.Sum256(content)
		encoded := base64.RawURLEncoding.EncodeToString(checksum[:])
		fmt.Fprintf(&record, "%s,sha256=%s,%d\n", name, encoded, len(content))
	}
	record.WriteString("packcove_fixture-1.0.0.dist-info/RECORD,,\n")
	files["packcove_fixture-1.0.0.dist-info/RECORD"] = []byte(record.String())
	var output bytes.Buffer
	archive := zip.NewWriter(&output)
	for name, content := range files {
		writer, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func netListen() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }
