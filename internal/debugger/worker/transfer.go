package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/lineahq/linea/internal/debugger/engine"
)

// transferTimeout bounds one presigned download or upload.
const transferTimeout = 5 * time.Minute

// transferClient moves payloads to and from presigned URLs. It follows no
// proxy settings a URL's signature would not survive and sends no
// credentials of its own: the URL carries them.
var transferClient = &http.Client{Timeout: transferTimeout}

// Digest is the "sha256:<hex>" digest of raw, as bundles and staged results
// are named.
func Digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// fetchBundle downloads a bundle from its presigned URL and checks its size
// and digest.
func fetchBundle(ctx context.Context, url, digest string, size int64) ([]byte, error) {
	if size <= 0 || size > engine.MaxBundleBytes {
		return nil, fmt.Errorf("%w: bundle of %d bytes", engine.ErrBundle, size)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: bundle URL: %w", engine.ErrInvalidRequest, err)
	}
	resp, err := transferClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: download the bundle: %w", engine.ErrRuntimeUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: download the bundle: object storage answered %s", engine.ErrRuntimeUnavailable, resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, size+1))
	if err != nil {
		return nil, fmt.Errorf("%w: download the bundle: %w", engine.ErrRuntimeUnavailable, err)
	}
	if int64(len(raw)) != size || Digest(raw) != digest {
		return nil, fmt.Errorf("%w: the downloaded bundle is not the one assigned", engine.ErrBundle)
	}
	return raw, nil
}

// uploadResult writes a result to its presigned URL.
func uploadResult(ctx context.Context, url string, raw []byte) error {
	ctx, cancel := context.WithTimeout(ctx, transferTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("result URL: %w", err)
	}
	req.ContentLength = int64(len(raw))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := transferClient.Do(req)
	if err != nil {
		return fmt.Errorf("upload the result: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("upload the result: object storage answered %s", resp.Status)
	}
	return nil
}
