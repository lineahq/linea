package pg

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestS3CopyIsConditional checks the conditional copy at the protocol level,
// as S3 documents it, independent of any one server: the copy names its
// source escaped, sends the ETag as x-amz-copy-source-if-match only when one
// is given, and maps a failed precondition, also one reported in a 200
// response, and a missing source to their errors.
func TestS3CopyIsConditional(t *testing.T) {
	const (
		changed  = `<?xml version="1.0" encoding="UTF-8"?><Error><Code>PreconditionFailed</Code><Message>At least one of the pre-conditions you specified did not hold</Message></Error>`
		missing  = `<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>`
		copied   = `<?xml version="1.0" encoding="UTF-8"?><CopyObjectResult><ETag>"abc"</ETag><LastModified>2026-10-31T00:00:00.000Z</LastModified></CopyObjectResult>`
		srcKey   = "staging/a b/bin"
		wantPath = "/bucket/objects/sha"
		wantSrc  = "bucket/staging/a%20b/bin"
	)
	for _, c := range []struct {
		name    string
		ifMatch string
		status  int
		body    string
		want    error
	}{
		{"copied", `"abc"`, http.StatusOK, copied, nil},
		{"unconditional", "", http.StatusOK, copied, nil},
		{"source changed", `"abc"`, http.StatusPreconditionFailed, changed, ErrObjectChanged},
		{"source changed, reported in a 200", `"abc"`, http.StatusOK, changed, ErrObjectChanged},
		{"source missing", `"abc"`, http.StatusNotFound, missing, ErrObjectNotFound},
	} {
		t.Run(c.name, func(t *testing.T) {
			var calls int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPut || r.URL.Path != wantPath {
					t.Errorf("request = %s %s; want PUT %s", r.Method, r.URL.Path, wantPath)
				}
				if got := r.Header.Get("X-Amz-Copy-Source"); got != wantSrc {
					t.Errorf("copy source = %q; want %q", got, wantSrc)
				}
				got, sent := r.Header["X-Amz-Copy-Source-If-Match"]
				if c.ifMatch == "" && sent || c.ifMatch != "" && (len(got) != 1 || got[0] != c.ifMatch) {
					t.Errorf("if-match = %q (sent %v); want %q", got, sent, c.ifMatch)
				}
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			client, err := newS3Client(S3Config{Endpoint: srv.URL, Bucket: "bucket", AccessKey: "a", SecretKey: "s", UsePathStyle: true})
			if err != nil {
				t.Fatal(err)
			}
			m := &s3Store{client: client, bucket: "bucket"}
			err = m.Copy(context.Background(), srcKey, "objects/sha", c.ifMatch)
			if c.want == nil && err != nil || c.want != nil && !errors.Is(err, c.want) {
				t.Errorf("copy = %v; want %v", err, c.want)
			}
			if calls == 0 {
				t.Error("no request reached the server")
			}
		})
	}
}
