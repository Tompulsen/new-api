package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/system_setting"
	_ "golang.org/x/image/webp"
)

const (
	MaxVideoReferenceBytes = 20 << 20
	VideoReferenceTTL      = time.Hour
	videoReferenceCapacity = 1 << 30
	videoReferenceKey      = "reference-image"
)

var (
	videoReferenceID      = regexp.MustCompile(`^([0-9]{10})-[a-f0-9]{32}$`)
	videoReferenceMutex   sync.Mutex
	videoReferenceCleanup sync.Once
	ErrVideoReferenceGone = errors.New("reference image not found")
)

func videoReferenceDir() string {
	if dir := os.Getenv("VIDEO_REFERENCE_DIR"); dir != "" {
		return dir
	}
	return "video-references"
}

func videoReferenceExpiry(id string) (int64, bool) {
	matches := videoReferenceID.FindStringSubmatch(id)
	if matches == nil {
		return 0, false
	}
	expires, err := strconv.ParseInt(matches[1], 10, 64)
	return expires, err == nil
}

func VerifyVideoReferenceAccess(access, id string, now time.Time) bool {
	expires, valid := videoReferenceExpiry(id)
	return valid && VerifyTaskArtifactAccess(access, "sd-reference:"+id, videoReferenceKey) &&
		now.Unix() < expires
}

// cleanupVideoReferences runs under videoReferenceMutex, including capacity checks.
func cleanupVideoReferences(root *os.Root, now time.Time) (int64, error) {
	dir, err := root.Open(".")
	if err != nil {
		return 0, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return 0, err
	}
	var used int64
	for _, entry := range entries {
		name := entry.Name()
		id := strings.TrimSuffix(name, ".tmp")
		expires, valid := videoReferenceExpiry(id)
		if !valid {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return 0, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if now.Unix() >= expires {
			if err := root.Remove(name); err != nil && !os.IsNotExist(err) {
				return 0, err
			}
			continue
		}
		used += info.Size()
	}
	return used, nil
}

func StartVideoReferenceCleanup(ctx context.Context) {
	videoReferenceCleanup.Do(func() {
		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for {
				videoReferenceMutex.Lock()
				root, err := os.OpenRoot(videoReferenceDir())
				if err == nil {
					_, err = cleanupVideoReferences(root, time.Now())
					root.Close()
				}
				videoReferenceMutex.Unlock()
				if err != nil && !os.IsNotExist(err) {
					common.SysError("temporary video reference cleanup failed")
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	})
}

// StoreVideoReference is called only for SD references after quota reservation.
func StoreVideoReference(ctx context.Context, data []byte) (string, error) {
	if len(data) == 0 || len(data) > MaxVideoReferenceBytes {
		return "", errors.New("reference image must be non-empty and at most 20 MiB")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg" && format != "gif" && format != "webp") ||
		config.Width <= 0 || config.Height <= 0 || config.Width > 64_000_000/config.Height {
		return "", errors.New("reference file must contain a valid PNG, JPEG, GIF or WebP image")
	}
	base := strings.TrimSpace(system_setting.TaskPublicAddress)
	if base == "" {
		base = strings.TrimSpace(system_setting.ServerAddress)
	}
	if ValidateTaskArtifactBaseURL(base) != nil {
		return "", errors.New("configure a public HTTPS address for reference images")
	}
	address, _ := url.Parse(base)
	if address.Scheme != "https" {
		return "", errors.New("configure a public HTTPS address for reference images")
	}
	expires := time.Now().Add(VideoReferenceTTL).Unix()
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", errors.New("could not create reference image ID")
	}
	id := fmt.Sprintf("%d-%x", expires, random)
	access, err := IssueTaskArtifactAccess("sd-reference:"+id, videoReferenceKey)
	if err != nil {
		return "", errors.New("could not sign reference image link")
	}
	videoReferenceMutex.Lock()
	defer videoReferenceMutex.Unlock()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err := os.MkdirAll(videoReferenceDir(), 0700); err != nil {
		return "", errors.New("could not prepare reference image storage")
	}
	root, err := os.OpenRoot(videoReferenceDir())
	if err != nil {
		return "", errors.New("could not open reference image storage")
	}
	defer root.Close()
	used, err := cleanupVideoReferences(root, time.Now())
	if err != nil {
		return "", errors.New("could not prepare reference image storage")
	}
	if used > videoReferenceCapacity-int64(len(data)) {
		return "", errors.New("temporary reference image storage is full")
	}
	file, err := root.OpenFile(id+".tmp", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", errors.New("could not save reference image")
	}
	defer root.Remove(id + ".tmp")
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil || ctx.Err() != nil {
		return "", errors.New("could not save reference image")
	}
	if err := root.Rename(id+".tmp", id); err != nil {
		return "", errors.New("could not save reference image")
	}
	address.Path = strings.TrimRight(address.Path, "/") + "/v1/video-references/" + id
	address.RawPath = ""
	address.RawQuery = url.Values{TaskArtifactAccessQueryParameter: {access}}.Encode()
	return address.String(), nil
}

func OpenVideoReference(id string) (*os.File, string, error) {
	expires, valid := videoReferenceExpiry(id)
	if !valid || time.Now().Unix() >= expires {
		return nil, "", ErrVideoReferenceGone
	}
	root, err := os.OpenRoot(videoReferenceDir())
	if err != nil {
		return nil, "", ErrVideoReferenceGone
	}
	defer root.Close()
	info, err := root.Lstat(id)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > MaxVideoReferenceBytes {
		return nil, "", ErrVideoReferenceGone
	}
	file, err := root.Open(id)
	if err != nil {
		return nil, "", ErrVideoReferenceGone
	}
	var header [512]byte
	n, err := file.Read(header[:])
	if err != nil && err != io.EOF {
		file.Close()
		return nil, "", ErrVideoReferenceGone
	}
	contentType := http.DetectContentType(header[:n])
	switch contentType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		file.Close()
		return nil, "", ErrVideoReferenceGone
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return nil, "", ErrVideoReferenceGone
	}
	return file, contentType, nil
}

// RemoveVideoReference rolls back only a link this gateway signed.
func RemoveVideoReference(link string) error {
	parsed, err := url.Parse(link)
	if err != nil {
		return ErrVideoReferenceGone
	}
	_, id, found := strings.Cut(parsed.Path, "/v1/video-references/")
	if !found || !videoReferenceID.MatchString(id) ||
		!VerifyTaskArtifactAccess(parsed.Query().Get("access"), "sd-reference:"+id, videoReferenceKey) {
		return ErrVideoReferenceGone
	}
	videoReferenceMutex.Lock()
	defer videoReferenceMutex.Unlock()
	root, err := os.OpenRoot(videoReferenceDir())
	if err != nil {
		return err
	}
	defer root.Close()
	err = root.Remove(id)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
