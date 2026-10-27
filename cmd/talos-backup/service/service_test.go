// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package service_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"
	"github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/siderolabs/talos-backup/cmd/talos-backup/service"
	"github.com/siderolabs/talos-backup/pkg/config"
)

type fakeSnapshotter struct {
	err  error
	data []byte
}

func (f fakeSnapshotter) EtcdSnapshot(context.Context, *machine.EtcdSnapshotRequest, ...grpc.CallOption) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.data)), f.err
}

// etcdSnapshot returns data shaped like an etcd snapshot: a db padded to 512 bytes followed by its sha256.
func etcdSnapshot() []byte {
	db := bytes.Repeat([]byte{1}, 1024)
	sum := sha256.Sum256(db)

	return append(db, sum[:]...)
}

// fakeS3 records objects uploaded with path-style PutObject requests.
type fakeS3 struct {
	objects map[string][]byte
	status  int
	mu      sync.Mutex
}

func newFakeS3(t *testing.T, status int) (*fakeS3, string) {
	t.Helper()

	f := &fakeS3{objects: map[string][]byte{}, status: status}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)

			return
		}

		f.mu.Lock()
		defer f.mu.Unlock()

		if f.status != http.StatusOK {
			w.WriteHeader(f.status)

			return
		}

		f.objects[r.URL.Path] = body
	}))
	t.Cleanup(srv.Close)

	return f, srv.URL
}

func setup(t *testing.T) {
	t.Helper()

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_CONFIG_FILE", os.DevNull)
	t.Chdir(t.TempDir())
}

func requireEmptyDir(t *testing.T) {
	t.Helper()

	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	require.Empty(t, entries, "temporary files left behind")
}

func TestBackupSnapshot(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)

	for _, tc := range []struct {
		name      string
		extension string
		compress  bool
		encrypt   bool
	}{
		{name: "plain", extension: ".snap"},
		{name: "compressed", extension: ".snap.zst", compress: true},
		{name: "encrypted", extension: ".snap.age", encrypt: true},
		{name: "compressed and encrypted", extension: ".snap.zst.age", compress: true, encrypt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup(t)

			s3, endpoint := newFakeS3(t, http.StatusOK)

			svcConf := &config.ServiceConfig{
				CustomS3Endpoint:      endpoint,
				Bucket:                "bucket",
				Region:                "us-east-1",
				UsePathStyle:          true,
				EnableCompression:     tc.compress,
				DisableEncryption:     !tc.encrypt,
				AgeRecipientPublicKey: []string{identity.Recipient().String()},
			}

			// cluster name and prefix fall back to the talosconfig context
			err := service.BackupSnapshot(t.Context(), svcConf, &talosconfig.Config{Context: "ctx-cluster"}, fakeSnapshotter{data: etcdSnapshot()})
			require.NoError(t, err)

			require.Len(t, s3.objects, 1)

			for key, body := range s3.objects {
				require.Regexp(t,
					regexp.MustCompile(`^/bucket/ctx-cluster/ctx-cluster-\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ`+regexp.QuoteMeta(tc.extension)+`$`),
					key)

				// the uploaded object must restore to the original snapshot
				require.Equal(t, etcdSnapshot(), restore(t, body, identity, tc.compress, tc.encrypt))
			}

			requireEmptyDir(t)
		})
	}
}

func TestBackupSnapshotNames(t *testing.T) {
	setup(t)

	s3, endpoint := newFakeS3(t, http.StatusOK)

	svcConf := &config.ServiceConfig{
		CustomS3Endpoint:  endpoint,
		Bucket:            "bucket",
		Region:            "us-east-1",
		UsePathStyle:      true,
		DisableEncryption: true,
		ClusterName:       "prod",
		S3Prefix:          "backups/etcd",
	}

	require.NoError(t, service.BackupSnapshot(t.Context(), svcConf, &talosconfig.Config{Context: "ignored"}, fakeSnapshotter{data: etcdSnapshot()}))

	require.Len(t, s3.objects, 1)

	for key := range s3.objects {
		require.True(t, strings.HasPrefix(key, "/bucket/backups/etcd/prod-"), key)
	}
}

func TestBackupSnapshotErrors(t *testing.T) {
	for _, tc := range []struct {
		snapshotter fakeSnapshotter
		name        string
		wantErr     string
		keys        []string
		s3Status    int
	}{
		{
			name:        "talos error",
			snapshotter: fakeSnapshotter{err: errors.New("boom")},
			s3Status:    http.StatusOK,
			wantErr:     "failed to take etcd snapshot",
		},
		{
			name:        "invalid age key",
			snapshotter: fakeSnapshotter{data: etcdSnapshot()},
			keys:        []string{"age1invalid"},
			s3Status:    http.StatusOK,
			wantErr:     "failed to encrypt etcd snapshot",
		},
		{
			name:        "upload error",
			snapshotter: fakeSnapshotter{data: etcdSnapshot()},
			s3Status:    http.StatusForbidden,
			wantErr:     "failed to push encrypted snapshot",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup(t)

			identity, err := age.GenerateX25519Identity()
			require.NoError(t, err)

			if tc.keys == nil {
				tc.keys = []string{identity.Recipient().String()}
			}

			_, endpoint := newFakeS3(t, tc.s3Status)

			svcConf := &config.ServiceConfig{
				CustomS3Endpoint:      endpoint,
				Bucket:                "bucket",
				Region:                "us-east-1",
				UsePathStyle:          true,
				EnableCompression:     true,
				ClusterName:           "cluster",
				AgeRecipientPublicKey: tc.keys,
			}

			err = service.BackupSnapshot(t.Context(), svcConf, &talosconfig.Config{}, tc.snapshotter)
			require.ErrorContains(t, err, tc.wantErr)

			requireEmptyDir(t)
		})
	}
}

func restore(t *testing.T, body []byte, identity age.Identity, compressed, encrypted bool) []byte {
	t.Helper()

	var r io.Reader = bytes.NewReader(body)

	if encrypted {
		var err error

		r, err = age.Decrypt(r, identity)
		require.NoError(t, err)
	}

	if compressed {
		decoder, err := zstd.NewReader(r)
		require.NoError(t, err)

		defer decoder.Close()

		r = decoder
	}

	out, err := io.ReadAll(r)
	require.NoError(t, err)

	return out
}
