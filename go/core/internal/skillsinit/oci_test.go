package skillsinit

import (
	"archive/tar"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_validateOCIManifest_rejectsContentBeforeLayerDownload(t *testing.T) {
	digest := v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("a", 64)}
	tests := map[string]v1.Manifest{
		"compressed content above budget": {
			Config: v1.Descriptor{Digest: digest, Size: maxOCIPullBytes},
			Layers: []v1.Descriptor{{Digest: digest, Size: 1}},
		},
		"external layer URL": {
			Config: v1.Descriptor{Digest: digest, Size: 1},
			Layers: []v1.Descriptor{{Digest: digest, Size: 1, URLs: []string{"https://publisher.example/layer"}}},
		},
		"negative descriptor size": {
			Config: v1.Descriptor{Digest: digest, Size: -1},
		},
	}
	tooManyLayers := v1.Manifest{Config: v1.Descriptor{Digest: digest, Size: 1}}
	tooManyLayers.Layers = make([]v1.Descriptor, maxOCIImageLayers+1)
	tests["too many layers"] = tooManyLayers

	for name, manifest := range tests {
		t.Run(name, func(t *testing.T) {
			require.Error(t, validateOCIManifest(&manifest))
		})
	}
}

func Test_boundedOCITransport_rejectsOversizedManifestWithoutReadingBody(t *testing.T) {
	body := &trackingReadCloser{Reader: strings.NewReader("untrusted")}
	transport := newBoundedOCITransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": []string{"application/vnd.oci.image.manifest.v1+json"}},
			Body:          body,
			ContentLength: maxOCIManifestBytes + 1,
		}, nil
	}))
	request, err := http.NewRequest(http.MethodGet, "https://registry.example/v2/team/skill/manifests/sha256:digest", nil)
	require.NoError(t, err)

	_, err = transport.RoundTrip(request)
	require.ErrorIs(t, err, errOCIResponseTooLarge)
	assert.Zero(t, body.reads, "oversized content-length must fail before reading the response")
	assert.True(t, body.closed, "rejected response body must be closed")
}

func Test_boundedOCIResponseBody_rejectsUnknownLengthManifestAtLimit(t *testing.T) {
	payload := bytes.Repeat([]byte{'x'}, maxOCIManifestBytes+1)
	body := &trackingReadCloser{Reader: bytes.NewReader(payload)}
	transport := newBoundedOCITransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/vnd.oci.image.manifest.v1+json"}},
			Body:       body,
			// A chunked registry response has no trustworthy Content-Length.
			ContentLength: -1,
		}, nil
	}))
	request, err := http.NewRequest(http.MethodGet, "https://registry.example/v2/team/skill/manifests/sha256:digest", nil)
	require.NoError(t, err)
	response, err := transport.RoundTrip(request)
	require.NoError(t, err)

	_, err = io.ReadAll(response.Body)
	require.ErrorIs(t, err, errOCIResponseTooLarge)
	assert.LessOrEqual(t, body.bytesRead, maxOCIManifestBytes+1)
}

func TestFetchOCIAnonymousDoesNotUseAmbientDockerCredentials(t *testing.T) {
	const expectedAuthorization = "Basic dXNlcjpwYXNz"
	var requests atomic.Int64
	var sawCredential atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.Header.Get("Authorization") == expectedAuthorization {
			sawCredential.Store(true)
		}
		response.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		response.Header().Set("WWW-Authenticate", `Basic realm="test-registry"`)
		response.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "https://")
	dockerConfig := t.TempDir()
	config := `{"auths":{"` + host + `":{"auth":"dXNlcjpwYXNz"}}}`
	require.NoError(t, os.WriteFile(filepath.Join(dockerConfig, "config.json"), []byte(config), 0o600))
	t.Setenv("DOCKER_CONFIG", dockerConfig)
	ref := OCIRef{
		Image: host + "/runtime/skills/review@sha256:" + strings.Repeat("a", 64),
		Dest:  filepath.Join(t.TempDir(), "skill"),
	}

	require.Error(t, FetchOCIAnonymous(ref, true))
	assert.Greater(t, requests.Load(), int64(0))
	assert.False(t, sawCredential.Load(), "anonymous external pull used ambient Docker credentials")

	requests.Store(0)
	sawCredential.Store(false)
	require.Error(t, FetchOCI(ref, true))
	assert.Greater(t, requests.Load(), int64(0))
	assert.True(t, sawCredential.Load(), "test registry did not observe the configured control credential")
}

func Test_extractTarWithLimits_rejectsOversizedOrExcessiveArtifacts(t *testing.T) {
	t.Run("extracted bytes", func(t *testing.T) {
		err := extractTarWithLimits(
			tarOf(t, tarEntry{Name: "SKILL.md", Mode: 0o644, Body: []byte("12345")}),
			t.TempDir(), 4, 10,
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "extracted bytes")
	})

	t.Run("entry count", func(t *testing.T) {
		err := extractTarWithLimits(
			tarOf(t,
				tarEntry{Name: "one", Mode: 0o644, Body: []byte("1")},
				tarEntry{Name: "two", Mode: 0o644, Body: []byte("2")},
			),
			t.TempDir(), 10, 1,
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "entries")
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type trackingReadCloser struct {
	io.Reader
	reads     int
	bytesRead int
	closed    bool
}

func (body *trackingReadCloser) Read(buffer []byte) (int, error) {
	body.reads++
	read, err := body.Reader.Read(buffer)
	body.bytesRead += read
	return read, err
}

func (body *trackingReadCloser) Close() error {
	body.closed = true
	return nil
}

// Test_tarEntryToLocal_rejectsEscape covers every shape of tar-entry name
// that the original `tar xf` pipeline would have happily honored: absolute
// paths, ".." traversal, and combinations thereof. A malicious skill image
// is the motivating threat — these names must never produce paths outside dst.
func Test_tarEntryToLocal_rejectsEscape(t *testing.T) {
	cases := []struct {
		name    string
		entry   string
		wantErr bool
	}{
		{"plain file", "file.txt", false},
		{"nested file", "a/b/c.txt", false},
		{"dot-only", ".", false},
		{"leading slash stripped", "/file.txt", false}, // re-rooted under dst, not at /
		{"traversal", "../escape", true},
		{"traversal mid-path", "a/../../escape", true},
		{"absolute escape", "/etc/passwd", false}, // strips leading "/" so result is dst/etc/passwd — under dst, intentional
		{"deep traversal", "../../../etc/passwd", true},
		{"trailing traversal", "a/b/../../..", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tarEntryToLocal(tc.entry)
			if tc.wantErr {
				require.Error(t, err, "tarEntryToLocal(%q) must reject", tc.entry)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// Test_extractTar_rejectsPathTraversalEntry feeds a hand-crafted tar with a
// "../escape" entry. The old shell pipeline would have written outside the
// destination; extractTar must error and not create any file.
func Test_extractTar_rejectsPathTraversalEntry(t *testing.T) {
	dst := t.TempDir()
	buf := tarOf(t, tarEntry{Name: "../escape.txt", Mode: 0o644, Body: []byte("pwned")})
	err := extractTar(buf, dst)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes destination")

	// Sanity: nothing was created either inside dst or as a sibling.
	_, statErr := os.Stat(filepath.Join(filepath.Dir(dst), "escape.txt"))
	require.True(t, os.IsNotExist(statErr), "sibling file must not exist")
}

// Test_extractTar_rejectsAbsoluteSymlink mirrors the OCI test corpus the
// previous container shipped (e.g. distroless's /etc/localtime symlink).
// We refuse rather than risk writing outside the volume.
func Test_extractTar_rejectsAbsoluteSymlink(t *testing.T) {
	dst := t.TempDir()
	buf := tarOf(t, tarEntry{
		Name:     "localtime",
		LinkName: "/etc/passwd",
		Type:     tar.TypeSymlink,
	})
	err := extractTar(buf, dst)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute symlink")
}

// Test_extractTar_rejectsEscapingSymlink covers relative symlinks whose
// resolved target points outside dst.
func Test_extractTar_rejectsEscapingSymlink(t *testing.T) {
	dst := t.TempDir()
	buf := tarOf(t, tarEntry{
		Name:     "link",
		LinkName: "../../etc/passwd",
		Type:     tar.TypeSymlink,
	})
	err := extractTar(buf, dst)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes destination")
}

// Test_extractTar_acceptsBenignSymlink ensures we haven't broken the legitimate
// in-tree symlink case (e.g., a/b -> a/c).
func Test_extractTar_acceptsBenignSymlink(t *testing.T) {
	dst := t.TempDir()
	buf := tarOf(t,
		tarEntry{Name: "target.txt", Mode: 0o644, Body: []byte("hi")},
		tarEntry{Name: "link.txt", LinkName: "target.txt", Type: tar.TypeSymlink},
	)
	require.NoError(t, extractTar(buf, dst))
	got, err := os.Readlink(filepath.Join(dst, "link.txt"))
	require.NoError(t, err)
	assert.Equal(t, "target.txt", got)
}

// Test_extractTar_writesRegularFiles is the smoke test that confirms the
// rewritten extractor still writes normal entries — without this, the negative
// tests above could pass by being unconditionally restrictive.
func Test_extractTar_writesRegularFiles(t *testing.T) {
	dst := t.TempDir()
	buf := tarOf(t,
		tarEntry{Name: "sub/", Mode: 0o755, Type: tar.TypeDir},
		tarEntry{Name: "sub/a.txt", Mode: 0o644, Body: []byte("hello")},
	)
	require.NoError(t, extractTar(buf, dst))
	body, err := os.ReadFile(filepath.Join(dst, "sub", "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(body))
}

// tarEntry is a minimal description of one tar record.
type tarEntry struct {
	Name     string
	Mode     int64
	Body     []byte
	LinkName string
	Type     byte
}

// tarOf assembles a tar stream in memory for use as input to extractTar.
func tarOf(t *testing.T, entries ...tarEntry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for _, e := range entries {
		typ := e.Type
		if typ == 0 {
			if e.LinkName != "" {
				typ = tar.TypeSymlink
			} else if strings.HasSuffix(e.Name, "/") {
				typ = tar.TypeDir
			} else {
				typ = tar.TypeReg
			}
		}
		hdr := &tar.Header{
			Name:     e.Name,
			Mode:     e.Mode,
			Size:     int64(len(e.Body)),
			Typeflag: typ,
			Linkname: e.LinkName,
		}
		if typ != tar.TypeReg {
			hdr.Size = 0
		}
		require.NoError(t, w.WriteHeader(hdr))
		if typ == tar.TypeReg && len(e.Body) > 0 {
			_, err := w.Write(e.Body)
			require.NoError(t, err)
		}
	}
	require.NoError(t, w.Close())
	return &buf
}
