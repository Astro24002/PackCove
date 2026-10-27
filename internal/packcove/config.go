package packcove

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"os"
)

type Config struct {
	Server       ServerConfig       `yaml:"server"`
	Storage      StorageConfig      `yaml:"storage"`
	Cache        CacheConfig        `yaml:"cache"`
	Upstream     UpstreamConfig     `yaml:"upstream"`
	Repositories []RepositoryConfig `yaml:"repositories"`
}

type ServerConfig struct {
	Listen        string `yaml:"listen"`
	PublicBaseURL string `yaml:"public_base_url"`
}

type StorageConfig struct {
	DataDir string `yaml:"data_dir"`
}

type CacheConfig struct {
	MetadataTTL          string `yaml:"metadata_ttl"`
	MutableArtifactTTL   string `yaml:"mutable_artifact_ttl"`
	ImmutableArtifactTTL string `yaml:"immutable_artifact_ttl"`
	StaleIfError         bool   `yaml:"stale_if_error"`
}

type UpstreamConfig struct {
	ConnectTimeout string `yaml:"connect_timeout"`
	RequestTimeout string `yaml:"request_timeout"`
	MaxRedirects   int    `yaml:"max_redirects"`
}

type RepositoryConfig struct {
	Name                 string   `yaml:"name"`
	Type                 string   `yaml:"type"`
	Upstream             string   `yaml:"upstream"`
	AllowedDownloadHosts []string `yaml:"allowed_download_hosts"`
}

type RuntimeConfig struct {
	Config
	PublicURL      *url.URL
	MetadataTTL    time.Duration
	MutableTTL     time.Duration
	ImmutableTTL   time.Duration
	ConnectTimeout time.Duration
	RequestTimeout time.Duration
}

var repositoryNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func LoadConfig(path string) (*RuntimeConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	return ValidateConfig(cfg)
}

func ValidateConfig(cfg Config) (*RuntimeConfig, error) {
	if cfg.Server.Listen == "" {
		cfg.Server.Listen = "0.0.0.0:8080"
	}
	if cfg.Storage.DataDir == "" {
		return nil, fmt.Errorf("storage.data_dir is required")
	}
	if cfg.Server.PublicBaseURL == "" {
		return nil, fmt.Errorf("server.public_base_url is required")
	}
	publicURL, err := url.Parse(cfg.Server.PublicBaseURL)
	if err != nil || publicURL.Host == "" || (publicURL.Scheme != "http" && publicURL.Scheme != "https") || publicURL.User != nil || publicURL.RawQuery != "" || publicURL.Fragment != "" {
		return nil, fmt.Errorf("server.public_base_url must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if cfg.Cache.MetadataTTL == "" {
		cfg.Cache.MetadataTTL = "5m"
	}
	if cfg.Cache.MutableArtifactTTL == "" {
		cfg.Cache.MutableArtifactTTL = "5m"
	}
	if cfg.Cache.ImmutableArtifactTTL == "" {
		cfg.Cache.ImmutableArtifactTTL = "never"
	}
	if cfg.Upstream.ConnectTimeout == "" {
		cfg.Upstream.ConnectTimeout = "10s"
	}
	if cfg.Upstream.RequestTimeout == "" {
		cfg.Upstream.RequestTimeout = "5m"
	}
	if cfg.Upstream.MaxRedirects == 0 {
		cfg.Upstream.MaxRedirects = 5
	}
	if cfg.Upstream.MaxRedirects < 0 || cfg.Upstream.MaxRedirects > 20 {
		return nil, fmt.Errorf("upstream.max_redirects must be between 1 and 20")
	}
	metadataTTL, err := time.ParseDuration(cfg.Cache.MetadataTTL)
	if err != nil || metadataTTL <= 0 {
		return nil, fmt.Errorf("cache.metadata_ttl must be a positive duration")
	}
	mutableTTL, err := time.ParseDuration(cfg.Cache.MutableArtifactTTL)
	if err != nil || mutableTTL <= 0 {
		return nil, fmt.Errorf("cache.mutable_artifact_ttl must be a positive duration")
	}
	var immutableTTL time.Duration
	if !strings.EqualFold(cfg.Cache.ImmutableArtifactTTL, "never") {
		immutableTTL, err = time.ParseDuration(cfg.Cache.ImmutableArtifactTTL)
		if err != nil || immutableTTL <= 0 {
			return nil, fmt.Errorf("cache.immutable_artifact_ttl must be a positive duration or never")
		}
	}
	connectTimeout, err := time.ParseDuration(cfg.Upstream.ConnectTimeout)
	if err != nil || connectTimeout <= 0 {
		return nil, fmt.Errorf("upstream.connect_timeout must be a positive duration")
	}
	requestTimeout, err := time.ParseDuration(cfg.Upstream.RequestTimeout)
	if err != nil || requestTimeout <= 0 {
		return nil, fmt.Errorf("upstream.request_timeout must be a positive duration")
	}
	if len(cfg.Repositories) == 0 {
		return nil, fmt.Errorf("repositories must contain at least one repository")
	}
	seen := make(map[string]bool, len(cfg.Repositories))
	for i := range cfg.Repositories {
		repo := &cfg.Repositories[i]
		if !repositoryNamePattern.MatchString(repo.Name) {
			return nil, fmt.Errorf("repositories[%d].name must match %s", i, repositoryNamePattern.String())
		}
		if seen[repo.Name] {
			return nil, fmt.Errorf("repositories[%d].name duplicates %q", i, repo.Name)
		}
		seen[repo.Name] = true
		if repo.Type != "maven" && repo.Type != "npm" && repo.Type != "pypi" {
			return nil, fmt.Errorf("repositories[%d].type must be maven, npm, or pypi", i)
		}
		upstream, parseErr := url.Parse(repo.Upstream)
		if parseErr != nil || upstream.Host == "" || (upstream.Scheme != "http" && upstream.Scheme != "https") || upstream.User != nil || upstream.RawQuery != "" || upstream.Fragment != "" {
			return nil, fmt.Errorf("repositories[%d].upstream must be an absolute HTTP(S) URL without credentials, query, or fragment", i)
		}
		if !strings.HasSuffix(upstream.Path, "/") {
			upstream.Path += "/"
		}
		repo.Upstream = upstream.String()
		hosts := make(map[string]bool)
		for _, host := range repo.AllowedDownloadHosts {
			host = strings.ToLower(strings.TrimSpace(host))
			if host == "" || strings.ContainsAny(host, "/:@") {
				return nil, fmt.Errorf("repositories[%d].allowed_download_hosts contains invalid host %q", i, host)
			}
			hosts[host] = true
		}
		if !hosts[strings.ToLower(upstream.Hostname())] {
			return nil, fmt.Errorf("repositories[%d].allowed_download_hosts must include upstream host %q", i, upstream.Hostname())
		}
		repo.AllowedDownloadHosts = repo.AllowedDownloadHosts[:0]
		for host := range hosts {
			repo.AllowedDownloadHosts = append(repo.AllowedDownloadHosts, host)
		}
	}
	return &RuntimeConfig{
		Config: cfg, PublicURL: publicURL, MetadataTTL: metadataTTL,
		MutableTTL: mutableTTL, ImmutableTTL: immutableTTL,
		ConnectTimeout: connectTimeout, RequestTimeout: requestTimeout,
	}, nil
}
