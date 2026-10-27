// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package s3_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/require"

	buconfig "github.com/siderolabs/talos-backup/pkg/config"
	"github.com/siderolabs/talos-backup/pkg/s3"
)

func TestCreateClientRegion(t *testing.T) {
	// ignore any AWS configuration of the machine running the tests
	t.Setenv("AWS_CONFIG_FILE", os.DevNull)
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")

	client, err := s3.CreateClientWithCustomEndpoint(t.Context(), &buconfig.ServiceConfig{CustomS3Endpoint: "http://localhost:9000"})
	require.NoError(t, err)
	require.Equal(t, "us-east-1", client.Options().Region)

	_, err = s3.CreateClientWithCustomEndpoint(t.Context(), &buconfig.ServiceConfig{})
	require.ErrorContains(t, err, "AWS_REGION is required")
}

func TestPushSnapshotCustomEndpoint(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Chdir(t.TempDir())

	require.NoError(t, os.WriteFile("snap.db", []byte("snapshot"), 0o600))

	var req *http.Request

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		req = r
	}))
	t.Cleanup(srv.Close)

	client, err := s3.CreateClientWithCustomEndpoint(t.Context(), &buconfig.ServiceConfig{
		CustomS3Endpoint: srv.URL,
		Region:           "us-east-1",
		UsePathStyle:     true,
	})
	require.NoError(t, err)

	require.NoError(t, s3.PushSnapshot(t.Context(), buconfig.S3Info{Bucket: "bucket"}, client, "prefix", "snap.db"))

	require.Equal(t, "/bucket/prefix/snap.db", req.URL.Path)

	// S3-compatible providers often reject the SDK default flexible checksums
	for name := range req.Header {
		require.False(t, strings.HasPrefix(strings.ToLower(name), "x-amz-checksum-"), "unexpected checksum header %s", name)
	}

	require.NotContains(t, req.Header.Get("X-Amz-Content-Sha256"), "TRAILER")
}

func TestPushSnapshotPrefix(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Chdir(t.TempDir())

	require.NoError(t, os.WriteFile("snap.db", []byte("snapshot"), 0o600))

	var got string

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
	}))
	t.Cleanup(srv.Close)

	client, err := s3.CreateClientWithCustomEndpoint(t.Context(), &buconfig.ServiceConfig{CustomS3Endpoint: srv.URL, UsePathStyle: true})
	require.NoError(t, err)

	for prefix, want := range map[string]string{
		"":      "/bucket/snap.db",
		"a/b":   "/bucket/a/b/snap.db",
		"/a/b/": "/bucket/a/b/snap.db",
		"a//b":  "/bucket/a/b/snap.db",
	} {
		require.NoError(t, s3.PushSnapshot(t.Context(), buconfig.S3Info{Bucket: "bucket"}, client, prefix, "snap.db"))
		require.Equal(t, want, got, "prefix %q", prefix)
	}
}

func TestPushSnapshotError(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Chdir(t.TempDir())

	require.NoError(t, os.WriteFile("snap.db", []byte("snapshot"), 0o600))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	client, err := s3.CreateClientWithCustomEndpoint(t.Context(), &buconfig.ServiceConfig{
		CustomS3Endpoint: srv.URL,
		Region:           "us-east-1",
		UsePathStyle:     true,
	})
	require.NoError(t, err)

	require.ErrorContains(t, s3.PushSnapshot(t.Context(), buconfig.S3Info{Bucket: "bucket"}, client, "prefix", "snap.db"), "StatusCode: 403")
	require.Error(t, s3.PushSnapshot(t.Context(), buconfig.S3Info{Bucket: "bucket"}, client, "prefix", "missing.db"))
}

func TestCreateClientAWS(t *testing.T) {
	// ignore any AWS configuration of the machine running the tests
	t.Setenv("AWS_CONFIG_FILE", os.DevNull)
	t.Setenv("AWS_REQUEST_CHECKSUM_CALCULATION", "")

	client, err := s3.CreateClientWithCustomEndpoint(t.Context(), &buconfig.ServiceConfig{
		Region:       "eu-west-1",
		UsePathStyle: true,
	})
	require.NoError(t, err)

	// custom endpoint settings must not leak to AWS S3
	opts := client.Options()
	require.Nil(t, opts.BaseEndpoint)
	require.False(t, opts.UsePathStyle)
	require.Equal(t, "eu-west-1", opts.Region)
	require.NotEqual(t, aws.RequestChecksumCalculationWhenRequired, opts.RequestChecksumCalculation)
}
