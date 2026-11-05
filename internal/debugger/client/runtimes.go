package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// Built runtimes on the server: the server's Docker store lists,
// removes, pushes and imports them, and a team server queues builds that its
// builders run and this client follows by polling.

// ListRuntimes returns the server's built runtimes.
func (c *Client) ListRuntimes(ctx context.Context) ([]api.Runtime, error) {
	raw, err := c.do(ctx, http.MethodGet, "/api/v1/runtimes", nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out []api.Runtime
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, c.malformed(err)
	}
	return out, nil
}

// DeleteRuntime removes a runtime's image on the server.
func (c *Client) DeleteRuntime(ctx context.Context, name string) error {
	_, err := c.do(ctx, http.MethodDelete, "/api/v1/runtimes/"+url.PathEscape(name), nil, http.StatusNoContent)
	return err
}

// PushRuntime pushes a runtime as target with the server user's Docker
// credentials.
func (c *Client) PushRuntime(ctx context.Context, name, target string) error {
	_, err := c.do(ctx, http.MethodPost, "/api/v1/runtimes/"+url.PathEscape(name)+"/push", api.RuntimePushRequest{Target: target}, http.StatusNoContent)
	return err
}

// RuntimeStorage reports what runtimes take on the server's Docker disk.
func (c *Client) RuntimeStorage(ctx context.Context) (*api.RuntimeStorage, error) {
	raw, err := c.do(ctx, http.MethodGet, "/api/v1/runtime-storage", nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out api.RuntimeStorage
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, c.malformed(err)
	}
	return &out, nil
}

// PruneRuntimes removes what req chooses from the server's runtime storage.
func (c *Client) PruneRuntimes(ctx context.Context, req api.RuntimePruneRequest) (*api.RuntimePruneResult, error) {
	raw, err := c.do(ctx, http.MethodPost, "/api/v1/runtime-storage/prune", req, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out api.RuntimePruneResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, c.malformed(err)
	}
	return &out, nil
}

// GetRuntime returns one runtime.
func (c *Client) GetRuntime(ctx context.Context, name string) (*api.Runtime, error) {
	raw, err := c.do(ctx, http.MethodGet, "/api/v1/runtimes/"+url.PathEscape(name), nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out api.Runtime
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, c.malformed(err)
	}
	return &out, nil
}

// ImportRuntime accepts an image of the server's Docker store as a runtime.
func (c *Client) ImportRuntime(ctx context.Context, ref string, pull bool) (*api.Runtime, error) {
	raw, err := c.do(ctx, http.MethodPost, "/api/v1/runtimes/import", api.RuntimeImportRequest{Ref: ref, Pull: pull}, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out api.Runtime
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, c.malformed(err)
	}
	return &out, nil
}

// RegisterRuntime records a catalog runtime with the hello read from its
// image.
func (c *Client) RegisterRuntime(ctx context.Context, req api.RuntimeRegisterRequest) (*api.Runtime, error) {
	raw, err := c.do(ctx, http.MethodPost, "/api/v1/runtimes/register", req, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out api.Runtime
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, c.malformed(err)
	}
	return &out, nil
}

// BuildArchive is one module's source for BuildRuntime: an uncompressed tar
// of the module's directory with go.mod at its root.
type BuildArchive struct {
	Module string
	Data   []byte
}

// BuildRuntime queues a build and returns it queued, without waiting. Each
// archive is sent as a file part of a multipart request and replaces the
// request's sources; with none the request is plain JSON.
func (c *Client) BuildRuntime(ctx context.Context, req api.RuntimeBuildRequest, archives []BuildArchive) (*api.RuntimeBuild, error) {
	if len(archives) == 0 {
		raw, err := c.do(ctx, http.MethodPost, "/api/v1/runtime-builds", req, http.StatusAccepted)
		if err != nil {
			return nil, err
		}
		return c.decodeBuild(raw)
	}
	req.Sources = make([]api.RuntimeBuildSource, 0, len(archives))
	for i, a := range archives {
		req.Sources = append(req.Sources, api.RuntimeBuildSource{Module: a.Module, Part: "source-" + strconv.Itoa(i)})
	}
	// The body streams, so a large archive is not copied into a second
	// buffer; a refusal before it is read ends the writer.
	pr, pw := io.Pipe()
	defer pr.Close()
	mw := multipart.NewWriter(pw)
	go func() { pw.CloseWithError(writeBuildUpload(mw, req, archives)) }()
	raw, err := c.exchange(ctx, http.MethodPost, "/api/v1/runtime-builds", pr, mw.FormDataContentType(),
		map[string]string{api.BuildUploadHeader: "1"}, http.StatusAccepted)
	if err != nil {
		return nil, err
	}
	return c.decodeBuild(raw)
}

// writeBuildUpload writes the request part and one file part per archive,
// named as the request's sources name them.
func writeBuildUpload(mw *multipart.Writer, req api.RuntimeBuildRequest, archives []BuildArchive) error {
	head := textproto.MIMEHeader{}
	head.Set("Content-Disposition", fmt.Sprintf("form-data; name=%q", api.BuildRequestPart))
	head.Set("Content-Type", "application/json")
	w, err := mw.CreatePart(head)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(w).Encode(req); err != nil {
		return err
	}
	for i, a := range archives {
		part := req.Sources[i].Part
		head := textproto.MIMEHeader{}
		head.Set("Content-Disposition", fmt.Sprintf("form-data; name=%q; filename=%q", part, part+".tar"))
		head.Set("Content-Type", "application/x-tar")
		if w, err = mw.CreatePart(head); err != nil {
			return err
		}
		if _, err := w.Write(a.Data); err != nil {
			return err
		}
	}
	return mw.Close()
}

// GetRuntimeBuild returns a build with its log lines from logFrom on. It
// does not wait.
func (c *Client) GetRuntimeBuild(ctx context.Context, id string, logFrom int) (*api.RuntimeBuild, error) {
	raw, err := c.do(ctx, http.MethodGet, "/api/v1/runtime-builds/"+url.PathEscape(id)+"?log_from="+strconv.Itoa(logFrom), nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	return c.decodeBuild(raw)
}

// CancelRuntimeBuild asks the server to cancel a build.
func (c *Client) CancelRuntimeBuild(ctx context.Context, id string) (*api.RuntimeBuild, error) {
	raw, err := c.do(ctx, http.MethodPost, "/api/v1/runtime-builds/"+url.PathEscape(id)+"/cancel", nil, http.StatusAccepted)
	if err != nil {
		return nil, err
	}
	return c.decodeBuild(raw)
}

func (c *Client) decodeBuild(raw []byte) (*api.RuntimeBuild, error) {
	var b api.RuntimeBuild
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, c.malformed(err)
	}
	return &b, nil
}

// ListRuntimeBuilds returns a page of the server's retained builds newest
// first, without their logs, of limit builds (0 for the server's default).
// The page's NextCursor, passed back as cursor, continues the list.
func (c *Client) ListRuntimeBuilds(ctx context.Context, limit int, cursor string) (*api.RuntimeBuildPage, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	path := "/api/v1/runtime-builds"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	raw, err := c.do(ctx, http.MethodGet, path, nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out api.RuntimeBuildPage
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, c.malformed(err)
	}
	return &out, nil
}

// CollectorSupport returns the server's support table.
func (c *Client) CollectorSupport(ctx context.Context) (*api.CollectorSupport, error) {
	raw, err := c.do(ctx, http.MethodGet, "/api/v1/collector-versions", nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out api.CollectorSupport
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, c.malformed(err)
	}
	return &out, nil
}

// ComposeManifest writes a manifest from a verified release's catalog.
func (c *Client) ComposeManifest(ctx context.Context, req api.ManifestRequest) (*api.ComposedManifest, error) {
	raw, err := c.do(ctx, http.MethodPost, "/api/v1/manifests", req, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out api.ComposedManifest
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, c.malformed(err)
	}
	return &out, nil
}

// RetargetManifest moves a manifest to another verified release.
func (c *Client) RetargetManifest(ctx context.Context, req api.ManifestRetargetRequest) (*api.RetargetedManifest, error) {
	raw, err := c.do(ctx, http.MethodPost, "/api/v1/manifests/retarget", req, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out api.RetargetedManifest
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, c.malformed(err)
	}
	return &out, nil
}

// CheckUpgrade runs an upgrade check on the server and waits for it; an
// interrupt abandons it, and the server cancels the runs it was waiting
// for. It returns the check's JSON bytes and the decoded check.
func (c *Client) CheckUpgrade(ctx context.Context, req api.UpgradeCheckRequest) ([]byte, *platform.UpgradeCheck, error) {
	raw, err := c.do(ctx, http.MethodPost, "/api/v1/upgrade-checks", req, http.StatusOK)
	if err != nil {
		return nil, nil, err
	}
	// Numbers stay as written, so values render as the server rendered them.
	var out platform.UpgradeCheck
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, nil, c.malformed(err)
	}
	return raw, &out, nil
}

// BisectConfig runs a fleet config bisect on the server and waits for it;
// an interrupt abandons it, and the server cancels the run it was waiting
// for. It returns the bisection's JSON bytes and the decoded bisection.
func (c *Client) BisectConfig(ctx context.Context, req api.ConfigBisectRequest) ([]byte, *platform.Bisection, error) {
	raw, err := c.do(ctx, http.MethodPost, "/api/v1/config-bisects", req, http.StatusOK)
	if err != nil {
		return nil, nil, err
	}
	var out platform.Bisection
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, nil, c.malformed(err)
	}
	return raw, &out, nil
}

// CollectorCatalog returns a verified release's component catalog.
func (c *Client) CollectorCatalog(ctx context.Context, version string) (*platform.CollectorCatalog, error) {
	raw, err := c.do(ctx, http.MethodGet, "/api/v1/collector-versions/"+url.PathEscape(version)+"/catalog", nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out platform.CollectorCatalog
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, c.malformed(err)
	}
	return &out, nil
}

// GetDefaultRuntime returns the runtime of every request that names none.
func (c *Client) GetDefaultRuntime(ctx context.Context) (*api.RuntimeDefault, error) {
	raw, err := c.do(ctx, http.MethodGet, "/api/v1/runtime-default", nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out api.RuntimeDefault
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, c.malformed(err)
	}
	return &out, nil
}

// SetDefaultRuntime makes name, the built-in runtime or a runtime of the
// server's catalog, the default runtime.
func (c *Client) SetDefaultRuntime(ctx context.Context, name string) (*api.RuntimeDefault, error) {
	raw, err := c.do(ctx, http.MethodPut, "/api/v1/runtime-default", api.RuntimeDefaultRequest{Name: name}, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out api.RuntimeDefault
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, c.malformed(err)
	}
	return &out, nil
}
