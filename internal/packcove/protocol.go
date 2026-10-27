package packcove

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"
)

const maxMetadataBytes = 64 << 20

var pypiNameSeparators = regexp.MustCompile(`[-_.]+`)

func metadataRequest(repo RepositoryConfig, rel string) bool {
	switch repo.Type {
	case "maven":
		return strings.Contains(rel, "maven-metadata.xml")
	case "npm":
		return !strings.HasSuffix(strings.ToLower(rel), ".tgz") && !strings.Contains(strings.ToLower(rel), ".tar.")
	case "pypi":
		return strings.HasPrefix(rel, "simple/") || rel == "simple" || rel == ""
	default:
		return false
	}
}

func ttlFor(repo RepositoryConfig, rel string, cfg *RuntimeConfig) time.Duration {
	if metadataRequest(repo, rel) {
		return cfg.MetadataTTL
	}
	if repo.Type == "maven" && strings.Contains(strings.ToUpper(rel), "SNAPSHOT") {
		return cfg.MutableTTL
	}
	return cfg.ImmutableTTL
}

func expiry(now time.Time, ttl time.Duration) time.Time {
	if ttl <= 0 {
		return time.Time{}
	}
	return now.Add(ttl)
}

func pypiUpstreamPath(rel string) (string, bool) {
	if rel == "simple" {
		return "", true
	}
	if rel == "simple/" {
		return "", false
	}
	if !strings.HasPrefix(rel, "simple/") {
		return rel, false
	}
	name := strings.TrimPrefix(rel, "simple/")
	if name == "" {
		return "", false
	}
	parts := strings.Split(name, "/")
	parts[0] = pypiNameSeparators.ReplaceAllString(strings.ToLower(parts[0]), "-")
	return strings.Join(parts, "/"), !strings.HasSuffix(name, "/")
}

func requestUpstreamURL(repo RepositoryConfig, rel, rawRel, query string) (*url.URL, bool, error) {
	upstream, err := url.Parse(repo.Upstream)
	if err != nil {
		return nil, false, err
	}
	redirect := false
	if repo.Type == "pypi" {
		rel, redirect = pypiUpstreamPath(rel)
		rawRel = ""
	}
	basePath := strings.TrimSuffix(upstream.Path, "/")
	baseEscapedPath := strings.TrimSuffix(upstream.EscapedPath(), "/")
	if rel != "" {
		upstream.Path = basePath + "/" + strings.TrimLeft(rel, "/")
	} else {
		upstream.Path = basePath + "/"
	}
	if rawRel != "" && rel != "" {
		rawRel = strings.TrimLeft(rawRel, "/")
		upstream.RawPath = baseEscapedPath + "/" + rawRel
		if unescaped, err := url.PathUnescape(upstream.RawPath); err != nil || unescaped != upstream.Path {
			upstream.RawPath = ""
		}
	} else {
		upstream.RawPath = ""
	}
	upstream.RawQuery = query
	return upstream, redirect, nil
}

func publicRepositoryURL(base *url.URL, name, repositoryType string) string {
	entry := ""
	if repositoryType == "pypi" {
		entry = "simple/"
	}
	return strings.TrimRight(base.String(), "/") + "/repository/" + name + "/" + entry
}

func sanitizeUpstream(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func mergeVary(existing, value string) string {
	for _, candidate := range strings.Split(existing, ",") {
		if strings.EqualFold(strings.TrimSpace(candidate), value) {
			return existing
		}
	}
	if strings.TrimSpace(existing) == "" {
		return value
	}
	return existing + ", " + value
}

func rewriteMetadata(ctx context.Context, storage *Storage, cfg *RuntimeConfig, repo RepositoryConfig, target *url.URL, contentType, accept string, body []byte) ([]byte, string, error) {
	switch repo.Type {
	case "npm":
		return rewriteNPMMetadata(ctx, storage, cfg, repo, body)
	case "pypi":
		if strings.Contains(strings.ToLower(contentType), "json") || strings.Contains(strings.ToLower(accept), "json") {
			return rewritePyPIJSON(ctx, storage, cfg, repo, target, body)
		}
		return rewritePyPIHTML(ctx, storage, cfg, repo, target, body)
	default:
		return body, contentType, nil
	}
}

func rewriteNPMMetadata(ctx context.Context, storage *Storage, cfg *RuntimeConfig, repo RepositoryConfig, body []byte) ([]byte, string, error) {
	var document any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return nil, "", fmt.Errorf("parse npm package metadata: %w", err)
	}
	var visit func(any) error
	visit = func(value any) error {
		switch typed := value.(type) {
		case map[string]any:
			if dist, ok := typed["dist"].(map[string]any); ok {
				if raw, ok := dist["tarball"].(string); ok {
					algorithm, digest, err := npmDigest(dist)
					if err != nil {
						return fmt.Errorf("parse npm integrity: %w", err)
					}
					rewritten, err := rewriteDownloadURLWithDigest(ctx, storage, cfg, repo, raw, algorithm, digest, "", "")
					if err != nil {
						return fmt.Errorf("rewrite npm tarball URL: %w", err)
					}
					dist["tarball"] = rewritten
				}
			}
			for _, child := range typed {
				if err := visit(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range typed {
				if err := visit(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(document); err != nil {
		return nil, "", err
	}
	data, err := json.Marshal(document)
	return data, "application/json; charset=utf-8", err
}

func rewritePyPIJSON(ctx context.Context, storage *Storage, cfg *RuntimeConfig, repo RepositoryConfig, target *url.URL, body []byte) ([]byte, string, error) {
	var document map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return nil, "", fmt.Errorf("parse PyPI JSON simple response: %w", err)
	}
	files, ok := document["files"].([]any)
	if ok {
		for _, item := range files {
			file, ok := item.(map[string]any)
			if !ok {
				continue
			}
			raw, ok := file["url"].(string)
			if !ok {
				continue
			}
			reference, err := url.Parse(raw)
			if err != nil {
				return nil, "", fmt.Errorf("parse PyPI file URL: %w", err)
			}
			resolved := target.ResolveReference(reference)
			algorithm, digest, err := simpleFileDigest(file["hashes"])
			if err != nil {
				return nil, "", fmt.Errorf("parse PyPI file hash: %w", err)
			}
			metadataValue, hasMetadata := file["core-metadata"]
			if !hasMetadata {
				metadataValue, hasMetadata = file["dist-info-metadata"]
			}
			metadataAlgorithm, metadataDigest, err := simpleFileDigest(metadataValue)
			if err != nil {
				return nil, "", fmt.Errorf("parse PyPI metadata hash: %w", err)
			}
			rewritten, err := rewriteDownloadURLWithDigest(ctx, storage, cfg, repo, resolved.String(), algorithm, digest, metadataAlgorithm, metadataDigest)
			if err != nil {
				return nil, "", fmt.Errorf("rewrite PyPI file URL: %w", err)
			}
			file["url"] = rewritten
		}
	}
	data, err := json.Marshal(document)
	return data, "application/vnd.pypi.simple.v1+json; charset=utf-8", err
}

func rewritePyPIHTML(ctx context.Context, storage *Storage, cfg *RuntimeConfig, repo RepositoryConfig, target *url.URL, body []byte) ([]byte, string, error) {
	document, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("parse PyPI HTML simple response: %w", err)
	}
	upstreamBase, _ := url.Parse(repo.Upstream)
	var rewriteErr error
	var visit func(*html.Node)
	visit = func(node *html.Node) {
		if rewriteErr != nil {
			return
		}
		if node.Type == html.ElementNode && strings.EqualFold(node.Data, "a") {
			for i := range node.Attr {
				if !strings.EqualFold(node.Attr[i].Key, "href") {
					continue
				}
				expectedAlgorithm, expectedDigest, err := digestFromFragment(node.Attr[i].Val)
				if err != nil {
					rewriteErr = err
					return
				}
				attributes := make(map[string]string, len(node.Attr))
				for _, attr := range node.Attr {
					attributes[strings.ToLower(attr.Key)] = attr.Val
				}
				metadataAlgorithm, metadataDigest, err := digestAttribute(attributes["data-core-metadata"])
				if err != nil {
					metadataAlgorithm, metadataDigest, err = digestAttribute(attributes["data-dist-info-metadata"])
				}
				if err != nil {
					rewriteErr = err
					return
				}
				rewritten, err := rewritePyPIHref(ctx, storage, cfg, repo, upstreamBase, target, node.Attr[i].Val, expectedAlgorithm, expectedDigest, metadataAlgorithm, metadataDigest)
				if err != nil {
					rewriteErr = err
					return
				}
				node.Attr[i].Val = rewritten
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(document)
	if rewriteErr != nil {
		return nil, "", rewriteErr
	}
	var output bytes.Buffer
	if err := html.Render(&output, document); err != nil {
		return nil, "", fmt.Errorf("serialize PyPI HTML simple response: %w", err)
	}
	return output.Bytes(), "text/html; charset=utf-8", nil
}

func rewritePyPIHref(ctx context.Context, storage *Storage, cfg *RuntimeConfig, repo RepositoryConfig, upstreamBase, target *url.URL, raw, expectedAlgorithm, expectedDigest, metadataAlgorithm, metadataDigest string) (string, error) {
	reference, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse PyPI file URL: %w", err)
	}
	resolved := target.ResolveReference(reference)
	fragment := resolved.Fragment
	resolved.Fragment = ""
	if sameOrigin(resolved, upstreamBase) && strings.HasPrefix(resolved.Path, upstreamBase.Path) {
		localPath := strings.TrimPrefix(resolved.Path, strings.TrimSuffix(upstreamBase.Path, "/"))
		local := strings.TrimRight(cfg.Server.PublicBaseURL, "/") + "/repository/" + repo.Name + "/simple/" + strings.TrimLeft(localPath, "/")
		if resolved.RawQuery != "" {
			local += "?" + resolved.RawQuery
		}
		if fragment != "" {
			local += "#" + fragment
		}
		return local, nil
	}
	rewritten, err := rewriteDownloadURLWithDigest(ctx, storage, cfg, repo, resolved.String(), expectedAlgorithm, expectedDigest, metadataAlgorithm, metadataDigest)
	if err != nil {
		return "", err
	}
	if fragment != "" {
		rewritten += "#" + fragment
	}
	return rewritten, nil
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func rewriteDownloadURLWithDigest(ctx context.Context, storage *Storage, cfg *RuntimeConfig, repo RepositoryConfig, raw, algorithm, digest, metadataAlgorithm, metadataDigest string) (string, error) {
	download, err := url.Parse(raw)
	if err != nil || download.Host == "" || (download.Scheme != "http" && download.Scheme != "https") || download.User != nil {
		return "", fmt.Errorf("download URL is not an absolute HTTP(S) URL")
	}
	allowed := false
	for _, host := range repo.AllowedDownloadHosts {
		if strings.EqualFold(host, download.Hostname()) {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", fmt.Errorf("download host %q is not allowed for repository %q", download.Hostname(), repo.Name)
	}
	fragment := download.Fragment
	download.Fragment = ""
	if algorithm == "" && fragment != "" {
		algorithm, digest, err = digestFromFragment("#" + fragment)
		if err != nil {
			return "", err
		}
	}
	filename := path.Base(download.Path)
	if filename == "." || filename == "/" || filename == "" {
		filename = "artifact"
	}
	id, err := storage.MapDownload(ctx, repo.Name, download.String(), filename, algorithm, digest, metadataAlgorithm, metadataDigest)
	if err != nil {
		return "", err
	}
	public := strings.TrimRight(cfg.Server.PublicBaseURL, "/") + "/repository/" + repo.Name + "/_packcove/download/" + id + "/" + url.PathEscape(filename)
	if fragment != "" {
		public += "#" + fragment
	}
	return public, nil
}

func readMetadata(response io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(response, maxMetadataBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxMetadataBytes {
		return nil, fmt.Errorf("metadata response exceeds %d bytes", maxMetadataBytes)
	}
	return body, nil
}

func npmDigest(dist map[string]any) (string, string, error) {
	if raw, ok := dist["integrity"].(string); ok && strings.TrimSpace(raw) != "" {
		priority := map[string]int{"sha1": 1, "sha256": 2, "sha384": 3, "sha512": 4}
		bestAlgorithm, bestDigest := "", ""
		for _, token := range strings.Fields(raw) {
			parts := strings.SplitN(token, "-", 2)
			if len(parts) != 2 {
				continue
			}
			algorithm := strings.ToLower(parts[0])
			if priority[algorithm] == 0 || priority[algorithm] <= priority[bestAlgorithm] {
				continue
			}
			decoded, err := base64.StdEncoding.DecodeString(parts[1])
			if err != nil {
				decoded, err = base64.RawStdEncoding.DecodeString(parts[1])
			}
			if err != nil {
				return "", "", fmt.Errorf("invalid %s SRI digest", algorithm)
			}
			bestAlgorithm, bestDigest = algorithm, hex.EncodeToString(decoded)
		}
		if bestAlgorithm != "" {
			if _, _, err := normalizeDigest(bestAlgorithm, bestDigest); err != nil {
				return "", "", err
			}
			return bestAlgorithm, bestDigest, nil
		}
	}
	if raw, ok := dist["shasum"].(string); ok && raw != "" {
		return normalizeDigest("sha1", raw)
	}
	return "", "", nil
}

func simpleFileDigest(value any) (string, string, error) {
	hashes, ok := value.(map[string]any)
	if !ok {
		return "", "", nil
	}
	for _, algorithm := range []string{"sha512", "sha384", "sha256", "sha1"} {
		if raw, ok := hashes[algorithm].(string); ok && raw != "" {
			return normalizeDigest(algorithm, raw)
		}
	}
	return "", "", nil
}

func digestAttribute(value string) (string, string, error) {
	if value == "" || strings.EqualFold(value, "true") || strings.EqualFold(value, "false") {
		return "", "", nil
	}
	return parseDigest(value)
}

func digestFromFragment(rawURL string) (string, string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", "", err
	}
	if parsed.Fragment == "" {
		return "", "", nil
	}
	return parseDigest(parsed.Fragment)
}

func parseDigest(raw string) (string, string, error) {
	raw = strings.TrimPrefix(raw, "#")
	for _, item := range strings.Split(raw, "&") {
		parts := strings.SplitN(item, "=", 2)
		if len(parts) != 2 {
			continue
		}
		algorithm := strings.ToLower(parts[0])
		if algorithm != "sha1" && algorithm != "sha256" && algorithm != "sha384" && algorithm != "sha512" {
			continue
		}
		return normalizeDigest(algorithm, parts[1])
	}
	return "", "", nil
}

func normalizeDigest(algorithm, digest string) (string, string, error) {
	length := map[string]int{"sha1": 20, "sha256": 32, "sha384": 48, "sha512": 64}[strings.ToLower(algorithm)]
	decoded, err := hex.DecodeString(digest)
	if length == 0 || err != nil || len(decoded) != length {
		return "", "", fmt.Errorf("invalid %s hex digest", algorithm)
	}
	return strings.ToLower(algorithm), strings.ToLower(digest), nil
}
