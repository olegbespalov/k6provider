// Package k6provider implements a library for providing custom k6 binaries
// using a k6build service
package k6provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/grafana/k6build"
	"github.com/grafana/k6build/pkg/client"
	"github.com/grafana/k6deps"
)

const (
	k6Module = "k6"
)

var (
	// ErrBinary indicates an error creating local binary
	ErrBinary = errors.New("creating binary")
	// ErrBuild indicates an error building binary
	ErrBuild = errors.New("building binary")
	// ErrDependency is produced by invalid dependencies
	ErrDependency = errors.New("invalid dependency")
	// ErrDownload indicates an error downloading binary
	ErrDownload = errors.New("downloading binary")
)

// Provider defines the interface for providing custom k6 binaries
type Provider interface {
	// GetBinary returns the path to a custom k6 binary that satisfies the given dependencies
	// Dependencies can be obtained using k6deps package
	GetBinary(ctx context.Context, deps k6deps.Dependencies) (string, error)
}

// Config defines the configuration of the Provider.
type Config struct {
	// Platform for the binaries. Defaults to the current platform
	Platform string
	// Client is the HTTP client used for downloading files
	Client *http.Client
	// BinDir path to binary directory. Defaults to the os' tmp dir
	BinDir string
	// BuildServiceURL URL of the k6 build service
	BuildServiceURL string
}

type provider struct {
	client   *http.Client
	bidDir   string
	buildSrv k6build.BuildService
	platform string
}

// NewProvider returns a provider with the given Options
func NewProvider(config Config) (Provider, error) {
	binDir := config.BinDir
	if binDir == "" {
		binDir = os.TempDir()
	}

	httpClient := config.Client
	if httpClient == nil {
		httpClient = http.DefaultClient
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
) (string, error) {
	k6Constrains, buildDeps := buildDeps(deps)

	artifact, err := p.buildSrv.Build(ctx, p.platform, k6Constrains, buildDeps)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrBuild, err)
	}

	artifactDir := filepath.Join(p.bidDir, artifact.ID)
	binPath := filepath.Join(artifactDir, k6Binary)
	_, err = os.Stat(binPath)

	// binary already exists
	if err == nil {
		return binPath, nil
	}

	// other error
	if !os.IsNotExist(err) {
		return "", fmt.Errorf("%w: %w", ErrBinary, err)
	}

	// binary doesn't exists
	err = os.MkdirAll(artifactDir, syscall.S_IRWXU)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrBinary, err)
	}

	target, err := os.OpenFile( //nolint:gosec
		binPath,
		os.O_WRONLY|os.O_CREATE,
		syscall.S_IRUSR|syscall.S_IXUSR|syscall.S_IWUSR,
	)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrBinary, err)
	}

	err = p.download(ctx, artifact.URL, target)
	if err != nil {
		_ = os.RemoveAll(artifactDir)
		return "", err
	}

	_ = target.Close()

	return binPath, nil
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
