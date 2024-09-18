// Package k6provider implements a library for providing custom k6 binaries
// using a k6build service
package k6provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/grafana/k6build"
	"github.com/grafana/k6build/pkg/client"
	"github.com/grafana/k6deps"
)

const (
	k6Binary = "k6"
	k6Module = "k6"
)

var (
	// ErrBinary indicates an error creating local binary
	ErrBinary = errors.New("creating binary")
	// ErrBuild indicates an error building binary
	ErrBuild = errors.New("building binary")
	// ErrConfig is produced by invalid configuration
	ErrConfig = errors.New("invalid configuration")
	// ErrDependency is produced by invalid dependencies
	ErrDependency = errors.New("invalid dependency")
	// ErrDownload indicates an error downloading binary
	ErrDownload = errors.New("downloading binary")
)

// K6Binary defines the attributes of a k6 binary
type K6Binary struct {
	// Path to the binary
	Path string
	// Dependencies as a map of name: version
	// e.g. {"k6": "v0.50.0", "k6/x/kubernetes": "v0.9.0"}
	Dependencies map[string]string
	// Checksum of the binary
	Checksum string
}

// UnmarshalDeps returns the dependencies as a list of name:version pairs separated by ";"
func (b K6Binary) UnmarshalDeps() string {
	buffer := &bytes.Buffer{}
	for dep, version := range b.Dependencies {
		buffer.WriteString(fmt.Sprintf("%s:%q;", dep, version))
	}
	return buffer.String()
}

// Provider defines the interface for providing custom k6 binaries
// from a k6build service
type Provider interface {
	// GetBinary returns the a custom k6 binary that satisfies the given dependencies
	// Dependencies can be obtained using k6deps package
	GetBinary(ctx context.Context, deps k6deps.Dependencies) (K6Binary, error)
}

// Config defines the configuration of the Provider.
type Config struct {
	// Platform for the binaries. Defaults to the current platform
	Platform string
	// BinDir path to binary directory. Defaults to the os' tmp dir
	BinDir string
	// BuildServiceURL URL of the k6 build service
	BuildServiceURL string
	// DownloadProxyURL URL to proxy for downloading binaries
	DownloadProxyURL string
}

type provider struct {
	client   *http.Client
	bidDir   string
	buildSrv k6build.BuildService
	platform string
}

// NewDefaultProvider returns a Provider with default settings
// Expects the K6_BUILD_SERVICE_URL environment variable to be set
// with the URL to the k6build service
func NewDefaultProvider() (Provider, error) {
	return NewProvider(Config{})
}

// NewProvider returns a Provider with the given Options
// If BuildServiceURL is not set, it will use the K6_BUILD_SERVICE_URL environment variable
// If DownloadProxyURL is not set, it will use the K6_DOWNLOAD_PROXY environment variable
func NewProvider(config Config) (Provider, error) {
	binDir := config.BinDir
	if binDir == "" {
		binDir = filepath.Join(os.TempDir(), "k6provider", "cache")
	}

	httpClient := http.DefaultClient

	proxyURL := config.DownloadProxyURL
	if proxyURL == "" {
		proxyURL = os.Getenv("K6_DOWNLOAD_PROXY")
	}
	if proxyURL != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrConfig, err)
		}
		proxy := http.ProxyURL(parsed)
		transport := &http.Transport{Proxy: proxy}
		httpClient = &http.Client{Transport: transport}
	}

	buildSrvURL := config.BuildServiceURL
	if buildSrvURL == "" {
		buildSrvURL = os.Getenv("K6_BUILD_SERVICE_URL")
	}

	buildSrv, err := client.NewBuildServiceClient(
		client.BuildServiceClientConfig{
			URL: buildSrvURL,
		},
	)
	if err != nil {
		return nil, err
	}

	platform := config.Platform
	if platform == "" {
		platform = fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return &provider{
		client:   httpClient,
		bidDir:   binDir,
		buildSrv: buildSrv,
		platform: platform,
	}, nil
}

func (p *provider) GetBinary(
	ctx context.Context,
	deps k6deps.Dependencies,
) (K6Binary, error) {
	k6Constrains, buildDeps := buildDeps(deps)

	artifact, err := p.buildSrv.Build(ctx, p.platform, k6Constrains, buildDeps)
	if err != nil {
		return K6Binary{}, fmt.Errorf("%w: %w", ErrBuild, err)
	}

	artifactDir := filepath.Join(p.bidDir, artifact.ID)
	binPath := filepath.Join(artifactDir, k6Binary)
	_, err = os.Stat(binPath)

	// binary already exists
	if err == nil {
		return K6Binary{
			Path:         binPath,
			Dependencies: artifact.Dependencies,
			Checksum:     artifact.Checksum,
		}, nil
	}

	// other error
	if !os.IsNotExist(err) {
		return K6Binary{}, fmt.Errorf("%w: %w", ErrBinary, err)
	}

	// binary doesn't exists
	err = os.MkdirAll(artifactDir, syscall.S_IRWXU)
	if err != nil {
		return K6Binary{}, fmt.Errorf("%w: %w", ErrBinary, err)
	}

	target, err := os.OpenFile( //nolint:gosec
		binPath,
		os.O_WRONLY|os.O_CREATE,
		syscall.S_IRUSR|syscall.S_IXUSR|syscall.S_IWUSR,
	)
	if err != nil {
		return K6Binary{}, fmt.Errorf("%w: %w", ErrBinary, err)
	}

	err = p.download(ctx, artifact.URL, target)
	if err != nil {
		_ = os.RemoveAll(artifactDir)
		return K6Binary{}, err
	}

	_ = target.Close()

	return K6Binary{
		Path:         binPath,
		Dependencies: artifact.Dependencies,
		Checksum:     artifact.Checksum,
	}, nil
}

func (p *provider) download(ctx context.Context, from string, dest io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, from, nil)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrDownload, err)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrDownload, err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s", ErrDownload, from)
	}

	defer resp.Body.Close() //nolint:errcheck

	_, err = io.Copy(dest, resp.Body)

	return err
}

func buildDeps(deps k6deps.Dependencies) (string, []k6build.Dependency) {
	bdeps := make([]k6build.Dependency, 0, len(deps))
	k6constraint := "*"

	for _, dep := range deps {
		if dep.Name == k6Module {
			k6constraint = dep.GetConstraints().String()
			continue
		}

		bdeps = append(
			bdeps,
			k6build.Dependency{
				Name:        dep.Name,
				Constraints: dep.GetConstraints().String(),
			},
		)
	}

	return k6constraint, bdeps
}
