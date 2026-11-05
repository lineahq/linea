package pg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lineahq/linea/internal/debugger/artifact"
)

// testEnv is a disposable PostgreSQL and S3-compatible server, named by
// environment variables (scripts/debugger/test-store.sh starts both). Tests
// that need it skip when it is not set.
type testEnv struct {
	databaseURL string
	s3          S3Config
}

func environment(t *testing.T) testEnv {
	t.Helper()
	get := func(name string) string { return os.Getenv("LINEA_TEST_" + name) }
	env := testEnv{databaseURL: get("DATABASE_URL"), s3: S3Config{
		Endpoint: get("S3_ENDPOINT"), Bucket: get("S3_BUCKET"), AccessKey: get("S3_ACCESS_KEY"), SecretKey: get("S3_SECRET_KEY"),
		Region: get("S3_REGION"), UseTLS: get("S3_TLS") == "1", UsePathStyle: true,
	}}
	if env.databaseURL == "" || env.s3.Endpoint == "" || env.s3.Bucket == "" || env.s3.AccessKey == "" || env.s3.SecretKey == "" {
		t.Skip("set LINEA_TEST_DATABASE_URL and LINEA_TEST_S3_{ENDPOINT,BUCKET,ACCESS_KEY,SECRET_KEY} (scripts/debugger/test-store.sh)")
	}
	return env
}

func randomName(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// fixture is one isolated installation: a fresh schema and a fresh object
// prefix.
type fixture struct {
	url    string
	s3     S3Config
	schema string
}

// newFixture creates a schema and an object prefix and removes them, and
// every object under the prefix, when the test ends.
func newFixture(t *testing.T) fixture {
	t.Helper()
	env := environment(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, env.databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := randomName("t_")
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(env.databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	f := fixture{url: u.String(), s3: env.s3, schema: schema}
	f.s3.Prefix = randomName("test/")
	ensureBucket(t, env.s3)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close(ctx)
		objects, err := newS3(ctx, f.s3)
		if err != nil {
			return
		}
		var keys []string
		_ = objects.List(ctx, f.s3.Prefix+"/", func(o ObjectInfo) error { keys = append(keys, o.Key); return nil })
		for _, k := range keys {
			_ = objects.Delete(ctx, k)
		}
	})
	return f
}

// ensureBucket creates the test bucket when the server does not have it yet.
func ensureBucket(t *testing.T, c S3Config) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := newS3(ctx, c); err == nil {
		return
	}
	client, err := newS3Client(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(c.Bucket)}); err != nil {
		t.Fatalf("test bucket %q is missing and cannot be created: %v", c.Bucket, err)
	}
}

func (f fixture) config(limits artifact.Limits) Config {
	return Config{DatabaseURL: f.url, S3: f.s3, Limits: limits}
}

func (f fixture) open(t *testing.T, limits artifact.Limits) *Store {
	t.Helper()
	s, err := Open(context.Background(), f.config(limits))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func openPool(t *testing.T, f fixture) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), f.url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
