package skillsinit

import (
	"archive/tar"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

const (
	maxOCIManifestBytes = 1 << 20
	maxOCIPullBytes     = 100 << 20
	maxOCIImageLayers   = 256
	maxOCIEntries       = 10_000
)

var errOCIResponseTooLarge = errors.New("OCI response exceeds the configured byte limit")

// FetchOCI pulls the named image, exports its flattened filesystem, and
// extracts it into ref.Dest. It is the in-process replacement for the old
// `krane export | tar xf -` pipeline.
//
// Auth comes from the standard DOCKER_CONFIG mechanism (set by the caller
// after MergeDockerConfigs). Platform follows the host arch — same as the
// old script's case statement on `uname -m`.
func FetchOCI(ref OCIRef, insecure bool) error {
	return fetchOCI(ref, insecure)
}

// FetchOCIAnonymous applies the same bounded OCI materialization while
// explicitly excluding Docker config, credential helpers, and ambient registry
// keychains. External-host portable skills use this credential-free path.
func FetchOCIAnonymous(ref OCIRef, insecure bool) error {
	return fetchOCI(ref, insecure, crane.WithAuth(authn.Anonymous))
}

func fetchOCI(ref OCIRef, insecure bool, authOptions ...crane.Option) error {
	platform, err := hostPlatform()
	if err != nil {
		return err
	}

	baseTransport := remote.DefaultTransport
	if insecure {
		transport, ok := remote.DefaultTransport.(*http.Transport)
		if !ok {
			return fmt.Errorf("configure insecure OCI transport: unsupported default transport %T", remote.DefaultTransport)
		}
		clone := transport.Clone()
		clone.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // explicit test/dev-only caller opt-in
		baseTransport = clone
	}
	bounded := newBoundedOCITransport(baseTransport)
	opts := []crane.Option{crane.WithPlatform(platform), crane.WithTransport(bounded)}
	opts = append(opts, authOptions...)

	img, err := crane.Pull(ref.Image, opts...)
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref.Image, err)
	}
	if err := validateOCIImage(img); err != nil {
		return fmt.Errorf("validate %s before layer download: %w", ref.Image, err)
	}

	parent := filepath.Dir(ref.Dest)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", parent, err)
	}
	staging, err := os.MkdirTemp(parent, "."+filepath.Base(ref.Dest)+"-oci-*")
	if err != nil {
		return fmt.Errorf("create OCI staging directory: %w", err)
	}
	defer os.RemoveAll(staging)
	if err := os.Chmod(staging, 0o755); err != nil {
		return fmt.Errorf("secure OCI staging directory: %w", err)
	}

	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		exportErr := crane.Export(img, pw)
		_ = pw.CloseWithError(exportErr)
		errCh <- exportErr
	}()

	if err := extractTar(pr, staging); err != nil {
		// Abort the export promptly; don't drain potentially large images.
		_ = pr.CloseWithError(err)
		<-errCh
		return fmt.Errorf("extract %s: %w", ref.Image, err)
	}
	if err := <-errCh; err != nil {
		return fmt.Errorf("export %s: %w", ref.Image, err)
	}
	if err := os.RemoveAll(ref.Dest); err != nil {
		return fmt.Errorf("replace %s: %w", ref.Dest, err)
	}
	if err := os.Rename(staging, ref.Dest); err != nil {
		return fmt.Errorf("commit %s: %w", ref.Dest, err)
	}
	return nil
}

type byteBudget struct {
	mu        sync.Mutex
	remaining int64
}

func (b *byteBudget) available() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.remaining
}

func (b *byteBudget) take(bytes int) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	allowed := int64(bytes)
	if allowed > b.remaining {
		allowed = b.remaining
	}
	b.remaining -= allowed
	return int(allowed)
}

type boundedOCITransport struct {
	base   http.RoundTripper
	budget *byteBudget
}

func newBoundedOCITransport(base http.RoundTripper) *boundedOCITransport {
	return &boundedOCITransport{base: base, budget: &byteBudget{remaining: maxOCIPullBytes}}
}

func (t *boundedOCITransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	limit := t.budget.available()
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	if strings.Contains(request.URL.Path, "/manifests/") || strings.Contains(contentType, "manifest") ||
		strings.Contains(contentType, "image.index") || strings.Contains(contentType, "application/json") {
		limit = min(limit, int64(maxOCIManifestBytes))
	}
	if response.ContentLength > limit {
		_ = response.Body.Close()
		return nil, fmt.Errorf("%w: content length %d exceeds %d", errOCIResponseTooLarge, response.ContentLength, limit)
	}
	response.Body = &boundedOCIResponseBody{ReadCloser: response.Body, budget: t.budget, remaining: limit}
	return response, nil
}

type boundedOCIResponseBody struct {
	io.ReadCloser
	budget    *byteBudget
	remaining int64
}

func (b *boundedOCIResponseBody) Read(buffer []byte) (int, error) {
	if b.remaining <= 0 || b.budget.available() <= 0 {
		return 0, errOCIResponseTooLarge
	}
	want := int64(len(buffer))
	if want > b.remaining+1 {
		want = b.remaining + 1
	}
	if available := b.budget.available(); want > available+1 {
		want = available + 1
	}
	read, err := b.ReadCloser.Read(buffer[:want])
	if read == 0 {
		return 0, err
	}
	allowed := read
	if int64(allowed) > b.remaining {
		allowed = int(b.remaining)
	}
	if budgetAllowed := b.budget.take(allowed); budgetAllowed < allowed {
		allowed = budgetAllowed
	}
	b.remaining -= int64(allowed)
	if allowed < read {
		return allowed, errOCIResponseTooLarge
	}
	return allowed, err
}

func validateOCIImage(image v1.Image) error {
	raw, err := image.RawManifest()
	if err != nil {
		return fmt.Errorf("read image manifest: %w", err)
	}
	if len(raw) > maxOCIManifestBytes {
		return fmt.Errorf("image manifest exceeds %d bytes", maxOCIManifestBytes)
	}
	manifest, err := image.Manifest()
	if err != nil {
		return fmt.Errorf("parse image manifest: %w", err)
	}
	return validateOCIManifest(manifest)
}

func validateOCIManifest(manifest *v1.Manifest) error {
	if manifest == nil {
		return fmt.Errorf("image manifest is required")
	}
	if len(manifest.Layers) > maxOCIImageLayers {
		return fmt.Errorf("image has more than %d layers", maxOCIImageLayers)
	}
	total := int64(0)
	descriptors := append([]v1.Descriptor{manifest.Config}, manifest.Layers...)
	for _, descriptor := range descriptors {
		if descriptor.Size < 0 {
			return fmt.Errorf("image descriptor has a negative size")
		}
		if len(descriptor.URLs) != 0 {
			return fmt.Errorf("image descriptor contains an external layer URL")
		}
		if descriptor.Size > maxOCIPullBytes-total {
			return fmt.Errorf("image compressed content exceeds %d bytes", maxOCIPullBytes)
		}
		total += descriptor.Size
	}
	return nil
}

func hostPlatform() (*v1.Platform, error) {
	var arch string
	switch runtime.GOARCH {
	case "amd64":
		arch = "amd64"
	case "arm64":
		arch = "arm64"
	default:
		return nil, fmt.Errorf("unsupported architecture for OCI export: %s", runtime.GOARCH)
	}
	return &v1.Platform{OS: "linux", Architecture: arch}, nil
}

// extractTar writes the tar stream into dst. All filesystem operations go
// through an os.Root anchored at dst, so any path or symlink that would
// resolve outside dst is rejected by the kernel.
func extractTar(r io.Reader, dst string) error {
	return extractTarWithLimits(r, dst, maxOCIPullBytes, maxOCIEntries)
}

func extractTarWithLimits(r io.Reader, dst string, maxBytes, maxEntries int64) error {
	root, err := os.OpenRoot(dst)
	if err != nil {
		return fmt.Errorf("open root %s: %w", dst, err)
	}
	defer root.Close()

	tr := tar.NewReader(r)
	var entries, bytes int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		entries++
		if entries > maxEntries {
			return fmt.Errorf("OCI artifact contains more than %d entries", maxEntries)
		}
		rel, err := tarEntryToLocal(hdr.Name)
		if err != nil {
			return fmt.Errorf("tar entry %q: %w", hdr.Name, err)
		}
		if rel == "" {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(rel, os.FileMode(hdr.Mode)|0o700); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if hdr.Size < 0 || hdr.Size > maxBytes-bytes {
				return fmt.Errorf("OCI artifact exceeds %d extracted bytes", maxBytes)
			}
			bytes += hdr.Size
			if err := root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
				return err
			}
			// OCI layers can overwrite read-only files from earlier layers.
			// Removing first avoids EACCES when O_TRUNC would otherwise fail.
			_ = root.Remove(rel)
			f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.CopyN(f, tr, hdr.Size); err != nil {
				f.Close()
				return err
			}
			f.Close()
		case tar.TypeSymlink:
			if err := root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
				return err
			}
			if err := validateSymlinkTarget(hdr.Name, rel, hdr.Linkname); err != nil {
				return err
			}
			_ = root.Remove(rel)
			if err := root.Symlink(hdr.Linkname, rel); err != nil {
				return err
			}
		default:
			// Skip hardlinks, devices, etc. Not meaningful in a skill bundle.
		}
	}
}

// tarEntryToLocal converts a tar header name (always slash-separated, may
// have a leading "/") into a local OS path that's guaranteed to stay inside
// the root. Returns "" for the no-op "." / "" entries. Delegates the actual
// safety check to filepath.Localize, which rejects ".." segments and any
// path that can't be a relative local path.
func tarEntryToLocal(name string) (string, error) {
	// Strip leading "/" — tar convention for "absolute" entries is to re-root
	// them under the destination, not to escape. Strip trailing "/" too since
	// tar directory entries carry one and filepath.Localize rejects it.
	// filepath.Localize then catches ".." traversal and anything else not
	// locally representable.
	trimmed := strings.Trim(name, "/")
	if trimmed == "" || trimmed == "." {
		return "", nil
	}
	local, err := filepath.Localize(trimmed)
	if err != nil {
		return "", fmt.Errorf("escapes destination: %w", err)
	}
	return local, nil
}

// validateSymlinkTarget rejects symlinks whose target would resolve outside
// the root. os.Root.Symlink itself only creates the link verbatim — it does
// not enforce that the target stays in-root — so we have to check here.
func validateSymlinkTarget(entryName, linkPath, linkTarget string) error {
	if filepath.IsAbs(linkTarget) {
		return fmt.Errorf("tar entry %q has absolute symlink target %q", entryName, linkTarget)
	}
	// Resolve link target relative to the link's *directory*. We use slash-
	// based path.Join + path.Clean because tar names are slash-separated and
	// the result is then validated as a single relative path.
	resolved := path.Join(filepath.ToSlash(filepath.Dir(linkPath)), filepath.ToSlash(linkTarget))
	if resolved == "" || resolved == "." {
		return nil
	}
	if !filepath.IsLocal(filepath.FromSlash(resolved)) {
		return fmt.Errorf("tar entry %q symlink target %q escapes destination", entryName, linkTarget)
	}
	return nil
}
