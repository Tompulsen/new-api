package service

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"io"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVideoReferenceStorageExpiryAndCleanup(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VIDEO_REFERENCE_DIR", dir)
	previousAddress, previousSecret := system_setting.TaskPublicAddress, common.CryptoSecret
	system_setting.TaskPublicAddress, common.CryptoSecret = "https://gateway.example/prefix", "video-reference-test-secret"
	t.Cleanup(func() { system_setting.TaskPublicAddress, common.CryptoSecret = previousAddress, previousSecret })
	var data bytes.Buffer
	require.NoError(t, png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	before := time.Now()
	link, err := StoreVideoReference(t.Context(), data.Bytes())
	require.NoError(t, err)
	parsed, err := url.Parse(link)
	require.NoError(t, err)
	assert.Equal(t, "gateway.example", parsed.Host)
	assert.True(t, strings.HasPrefix(parsed.Path, "/prefix/v1/video-references/"))
	id := path.Base(parsed.Path)
	access := parsed.Query().Get("access")
	assert.True(t, VerifyVideoReferenceAccess(access, id, before))
	assert.False(t, VerifyVideoReferenceAccess(access+"x", id, before))
	assert.False(t, VerifyVideoReferenceAccess(access, "../"+id, before))
	expires, valid := videoReferenceExpiry(id)
	require.True(t, valid)
	assert.InDelta(t, before.Add(time.Hour).Unix(), expires, 1)
	assert.False(t, VerifyVideoReferenceAccess(access, id, time.Unix(expires, 0)))
	assert.False(t, VerifyVideoReferenceAccess(access, id, time.Unix(expires+1, 0)))
	alteredID := strconv.FormatInt(expires+3600, 10) + id[10:]
	assert.False(t, VerifyVideoReferenceAccess(access, alteredID, before))
	file, mime, err := OpenVideoReference(id)
	require.NoError(t, err)
	actual, err := io.ReadAll(file)
	file.Close()
	require.NoError(t, err)
	assert.Equal(t, data.Bytes(), actual)
	assert.Equal(t, "image/png", mime)
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()
	used, err := cleanupVideoReferences(root, time.Unix(expires, 0))
	require.NoError(t, err)
	assert.Zero(t, used)
	_, _, err = OpenVideoReference(id)
	assert.ErrorIs(t, err, ErrVideoReferenceGone)
}

func TestVideoReferenceRejectsOversizeAndPreservesExactLimit(t *testing.T) {
	t.Setenv("VIDEO_REFERENCE_DIR", t.TempDir())
	previousAddress, previousSecret := system_setting.TaskPublicAddress, common.CryptoSecret
	system_setting.TaskPublicAddress, common.CryptoSecret = "https://gateway.example", "video-reference-limit-secret"
	t.Cleanup(func() { system_setting.TaskPublicAddress, common.CryptoSecret = previousAddress, previousSecret })
	for _, data := range [][]byte{nil, []byte("<svg></svg>"), []byte("not an image"), make([]byte, MaxVideoReferenceBytes+1)} {
		_, err := StoreVideoReference(t.Context(), data)
		require.Error(t, err)
	}
	var pngBytes bytes.Buffer
	require.NoError(t, png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 1, 1))))
	data := make([]byte, MaxVideoReferenceBytes)
	copy(data, pngBytes.Bytes())
	link, err := StoreVideoReference(t.Context(), data)
	require.NoError(t, err)
	assert.NotEmpty(t, link)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = StoreVideoReference(cancelled, pngBytes.Bytes())
	assert.ErrorIs(t, err, context.Canceled)
	system_setting.TaskPublicAddress = "http://localhost:3000"
	_, err = StoreVideoReference(t.Context(), pngBytes.Bytes())
	require.ErrorContains(t, err, "public HTTPS")
}

func TestVideoReferenceCleanupLeavesOtherFilesAndRejectsPaths(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VIDEO_REFERENCE_DIR", dir)
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()
	expired := strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10) + "-" + strings.Repeat("a", 32)
	for _, name := range []string{expired, expired + ".tmp", "unrelated-file"} {
		require.NoError(t, root.WriteFile(name, []byte("data"), 0600))
	}
	_, err = cleanupVideoReferences(root, time.Now())
	require.NoError(t, err)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "unrelated-file", entries[0].Name())
	for _, id := range []string{"../unrelated-file", expired, "unrelated-file", "/absolute"} {
		_, _, err := OpenVideoReference(id)
		assert.ErrorIs(t, err, ErrVideoReferenceGone)
	}
}
