package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/lineahq/linea/build/versions"
	"github.com/lineahq/linea/internal/debugger/container"
	"github.com/lineahq/linea/internal/debugger/worker"
)

const (
	// maxManifestBytes bounds a downloaded manifest.
	maxManifestBytes = 8 << 20
	// maxSourceBytes bounds a downloaded module archive: its unpacked bytes
	// (container.MaxModuleBytes) and the tar headers around them.
	maxSourceBytes = 2 * container.MaxModuleBytes
	// maxErrorBytes bounds the error a build result carries; the log has
	// the rest.
	maxErrorBytes = 8 << 10
)

// builder runs the builds the server gives it.
type builder struct {
	// build compiles a runtime (container.Native.Build).
	build func(ctx context.Context, req container.BuildRequest) (*container.NativeRuntime, error)
	// source is the worker source built into this binary.
	source fs.FS
	// table is the support table that holds the verified releases'
	// dependency sets.
	table  *versions.Table
	client *http.Client
	// export pushes images of built binaries when a job asks for it, with
	// the credentials in the registryAuth file (none when it is empty).
	export       *container.ImageExport
	registryAuth string
}

// newTransferClient moves inputs and outputs through presigned URLs. It has
// no overall timeout: a build's deadline bounds each transfer through its
// context.
func newTransferClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 2 * time.Minute
	return &http.Client{Transport: t}
}

// run is the worker.BuildFunc: it downloads and checks the job's inputs,
// builds, and uploads the binary. Everything it logs or returns is redacted.
func (b *builder) run(ctx context.Context, job worker.BuildJob, log func(string)) worker.BuildResult {
	redact := redactor(job)
	say := func(line string) { log(redact(line)) }
	fail := func(err error) worker.BuildResult {
		msg := redact(err.Error())
		if len(msg) > maxErrorBytes {
			msg = msg[:maxErrorBytes] + "..."
		}
		return worker.BuildResult{Error: msg}
	}

	say("downloading the manifest")
	manifest, err := b.download(ctx, job.Manifest, maxManifestBytes)
	if err != nil {
		return fail(fmt.Errorf("the manifest: %w", err))
	}
	sources := make([]container.ModuleSource, 0, len(job.Sources))
	for _, in := range job.Sources {
		say("downloading the source of " + in.Module)
		archive, err := b.download(ctx, in, maxSourceBytes)
		if err != nil {
			return fail(fmt.Errorf("the source of %s: %w", in.Module, err))
		}
		files, err := container.ModuleArchive(in.Module, archive)
		if err != nil {
			return fail(fmt.Errorf("the source of %s: %w", in.Module, err))
		}
		sources = append(sources, container.ModuleSource{Module: in.Module, Files: files, Origin: "upload " + in.Digest})
	}

	var set *container.DependencySet
	if job.Verified {
		release := b.table.Release(job.CollectorVersion)
		if release == nil {
			return fail(fmt.Errorf("the server verifies Collector %s but this builder's support table does not list it: update the builder", job.CollectorVersion))
		}
		set = &container.DependencySet{GoMod: release.GoMod, GoSum: release.GoSum}
	}

	rt, err := b.build(ctx, container.BuildRequest{Name: job.Name, Manifest: manifest, Source: b.source, Set: set, Sources: sources, Log: say})
	if rt != nil {
		defer rt.Close()
	}
	if err != nil {
		return fail(err)
	}
	if rt.Size > job.BinaryLimit {
		return fail(fmt.Errorf("the built binary is %d bytes, the limit is %d", rt.Size, job.BinaryLimit))
	}
	say(fmt.Sprintf("uploading the binary (%d bytes)", rt.Size))
	if err := b.upload(ctx, job.BinaryURL, rt.Path, rt.Size); err != nil {
		return fail(fmt.Errorf("uploading the binary: %w", err))
	}
	hello := rt.Hello
	result := worker.BuildResult{BinaryDigest: rt.Digest, BinarySize: rt.Size, Hello: &hello, OS: "linux", Arch: runtime.GOARCH}
	if job.Export != nil {
		say("exporting the image " + job.Export.Repository + ":" + job.Export.Tag)
		image, err := b.exportImage(ctx, job.Export, rt)
		if err != nil {
			// The runtime is built and stored: a failed export leaves the
			// build done and says why.
			msg := redact(err.Error())
			if len(msg) > maxErrorBytes {
				msg = msg[:maxErrorBytes] + "..."
			}
			say("exporting the image failed: " + msg)
			result.ExportError = msg
		} else {
			say("exported " + image)
			result.Image = image
		}
	}
	return result
}

// exportImage pushes the image of the built binary with the credentials
// the builder holds.
func (b *builder) exportImage(ctx context.Context, x *worker.BuildExport, rt *container.NativeRuntime) (string, error) {
	if b.export == nil {
		return "", errors.New("this builder exports no images")
	}
	e := *b.export
	if b.registryAuth != "" {
		auth, err := os.ReadFile(b.registryAuth)
		if err != nil {
			return "", errors.New("the builder's registry credentials are unreadable")
		}
		e.Auth = auth
	}
	return e.Push(ctx, x.Repository, x.Tag, rt.Path, rt.Labels)
}

// download fetches an input and checks it against the size and digest the
// server named, reading at most one byte more than the size.
func (b *builder) download(ctx context.Context, in worker.BuildInput, limit int64) ([]byte, error) {
	if in.Size < 0 || in.Size > limit {
		return nil, fmt.Errorf("the input is %d bytes, the limit is %d", in.Size, limit)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, in.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("download: %w", unwrapURL(err))
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download: %w", unwrapURL(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download: object storage answered %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, in.Size+1))
	if err != nil {
		return nil, fmt.Errorf("download: %w", unwrapURL(err))
	}
	if int64(len(raw)) != in.Size {
		return nil, fmt.Errorf("the download is not the %d bytes the server named", in.Size)
	}
	sum := sha256.Sum256(raw)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != in.Digest {
		return nil, fmt.Errorf("the download's digest is %s, the server named %s", got, in.Digest)
	}
	return raw, nil
}

// upload PUTs the file at path to the presigned URL, streaming it with its
// length.
func (b *builder) upload(ctx context.Context, target, path string, size int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, f)
	if err != nil {
		return unwrapURL(err)
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := b.client.Do(req)
	if err != nil {
		return unwrapURL(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("object storage answered %s", resp.Status)
	}
	return nil
}

// unwrapURL drops the URL a net/url error carries: a presigned URL's query
// is a credential.
func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

var (
	// userinfo matches the credentials of a URL, scheme://user:pass@host.
	userinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/\s@]+@`)
	// signature matches the query parameters of a presigned URL that carry
	// the signature.
	signature = regexp.MustCompile(`(?i)(X-Amz-(?:Signature|Credential|Security-Token)|Signature|AWSAccessKeyId)=[^&\s"']+`)
)

// redact removes credentials from a line the builder logs or reports:
// userinfo in URLs and the signature of presigned ones.
func redact(s string) string {
	s = userinfo.ReplaceAllString(s, "${1}***@")
	return signature.ReplaceAllString(s, "${1}=***")
}

// redactor is redact, which also removes the job's presigned URLs whole,
// keeping their host and path.
func redactor(job worker.BuildJob) func(string) string {
	var pairs []string
	add := func(raw string) {
		u, err := url.Parse(raw)
		if err != nil || raw == "" {
			return
		}
		pairs = append(pairs, raw, u.Scheme+"://"+u.Host+u.EscapedPath())
	}
	add(job.Manifest.URL)
	for _, s := range job.Sources {
		add(s.URL)
	}
	add(job.BinaryURL)
	r := strings.NewReplacer(pairs...)
	return func(s string) string { return redact(r.Replace(s)) }
}
