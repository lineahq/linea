package pg

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

// status wraps err as the SDK does for an HTTP response with that status.
func status(code int, err error) error {
	return &smithy.OperationError{ServiceID: "S3", OperationName: "HeadBucket", Err: &awshttp.ResponseError{
		ResponseError: &smithyhttp.ResponseError{Response: &smithyhttp.Response{Response: &http.Response{StatusCode: code}}, Err: err},
	}}
}

func TestBucketMissing(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"typed not found", status(404, &s3types.NotFound{}), true},
		{"typed no such bucket", &smithy.OperationError{Err: &s3types.NoSuchBucket{}}, true},
		{"generic NotFound code", &smithy.GenericAPIError{Code: "NotFound"}, true},
		{"generic NoSuchBucket code", &smithy.GenericAPIError{Code: "NoSuchBucket"}, true},
		{"generic 404 code", &smithy.GenericAPIError{Code: "404"}, true},
		{"bare 404 from a compatible server", status(404, errors.New("not found")), true},
		{"access denied", status(403, &smithy.GenericAPIError{Code: "Forbidden"}), false},
		{"access denied code", &smithy.GenericAPIError{Code: "AccessDenied"}, false},
		{"server error", status(500, errors.New("boom")), false},
		{"network", errors.New("dial tcp: connection refused"), false},
		{"canceled", context.Canceled, false},
	} {
		if got := bucketMissing(tc.err); got != tc.want {
			t.Errorf("%s: bucketMissing = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestBucketExists(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"owned by you", &s3types.BucketAlreadyOwnedByYou{}, true},
		{"already exists", &smithy.OperationError{Err: &s3types.BucketAlreadyExists{}}, true},
		{"generic owned code", &smithy.GenericAPIError{Code: "BucketAlreadyOwnedByYou"}, true},
		{"generic exists code", &smithy.GenericAPIError{Code: "BucketAlreadyExists"}, true},
		{"access denied", &smithy.GenericAPIError{Code: "AccessDenied"}, false},
		{"network", errors.New("connection refused"), false},
	} {
		if got := bucketExists(tc.err); got != tc.want {
			t.Errorf("%s: bucketExists = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// fakeBucketAPI answers HeadBucket from a queue and records CreateBucket.
type fakeBucketAPI struct {
	heads   []error
	create  error
	heaped  int
	created []*s3.CreateBucketInput
}

func (f *fakeBucketAPI) HeadBucket(context.Context, *s3.HeadBucketInput, ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	err := f.heads[min(f.heaped, len(f.heads)-1)]
	f.heaped++
	return &s3.HeadBucketOutput{}, err
}

func (f *fakeBucketAPI) CreateBucket(_ context.Context, in *s3.CreateBucketInput, _ ...func(*s3.Options)) (*s3.CreateBucketOutput, error) {
	f.created = append(f.created, in)
	return &s3.CreateBucketOutput{}, f.create
}

func TestEnsureBucketExists(t *testing.T) {
	missing := status(404, &s3types.NotFound{})
	denied := status(403, &smithy.GenericAPIError{Code: "Forbidden"})
	for _, tc := range []struct {
		name        string
		api         fakeBucketAPI
		create      bool
		region      string
		wantErr     string
		wantCreates int
		wantHeads   int
		wantLocale  string
	}{
		{name: "present", api: fakeBucketAPI{heads: []error{nil}}, create: true, region: "us-east-1", wantHeads: 1},
		{name: "present without create", api: fakeBucketAPI{heads: []error{nil}}, region: "us-east-1", wantHeads: 1},
		{name: "missing is created", api: fakeBucketAPI{heads: []error{missing}}, create: true, region: "us-east-1", wantCreates: 1, wantHeads: 1},
		{name: "missing in another region names it", api: fakeBucketAPI{heads: []error{missing}}, create: true, region: "eu-west-1", wantCreates: 1, wantHeads: 1, wantLocale: "eu-west-1"},
		{name: "missing without create fails as before", api: fakeBucketAPI{heads: []error{missing}}, region: "us-east-1", wantErr: `object storage bucket "linea"`, wantHeads: 1},
		{name: "denied is never created", api: fakeBucketAPI{heads: []error{denied}}, create: true, region: "us-east-1", wantErr: "object storage bucket", wantHeads: 1},
		{name: "network error is never created", api: fakeBucketAPI{heads: []error{errors.New("refused")}}, create: true, region: "us-east-1", wantErr: "refused", wantHeads: 1},
		{name: "concurrent start created it first", api: fakeBucketAPI{heads: []error{missing, nil}, create: &s3types.BucketAlreadyOwnedByYou{}}, create: true, region: "us-east-1", wantCreates: 1, wantHeads: 2},
		{name: "exists elsewhere and unreachable", api: fakeBucketAPI{heads: []error{missing, denied}, create: &s3types.BucketAlreadyExists{}}, create: true, region: "us-east-1", wantErr: "object storage bucket", wantCreates: 1, wantHeads: 2},
		{name: "create refused", api: fakeBucketAPI{heads: []error{missing}, create: &smithy.GenericAPIError{Code: "AccessDenied", Message: "no"}}, create: true, region: "us-east-1", wantErr: "create object storage bucket", wantCreates: 1, wantHeads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ensureBucketExists(context.Background(), &tc.api, "linea", tc.region, tc.create)
			if tc.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr) || !errors.Is(err, artifact.ErrStorage)) {
				t.Fatalf("error = %v, want %q wrapping ErrStorage", err, tc.wantErr)
			}
			if len(tc.api.created) != tc.wantCreates || tc.api.heaped != tc.wantHeads {
				t.Fatalf("creates %d, heads %d; want %d, %d", len(tc.api.created), tc.api.heaped, tc.wantCreates, tc.wantHeads)
			}
			if tc.wantCreates == 0 {
				return
			}
			in := tc.api.created[0]
			if *in.Bucket != "linea" {
				t.Errorf("created bucket %q", *in.Bucket)
			}
			if tc.wantLocale == "" && in.CreateBucketConfiguration != nil {
				t.Errorf("the default region sent a location constraint %+v", in.CreateBucketConfiguration)
			}
			if tc.wantLocale != "" && (in.CreateBucketConfiguration == nil || string(in.CreateBucketConfiguration.LocationConstraint) != tc.wantLocale) {
				t.Errorf("location constraint = %+v, want %s", in.CreateBucketConfiguration, tc.wantLocale)
			}
		})
	}
}
