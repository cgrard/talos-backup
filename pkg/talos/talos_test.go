// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package talos_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"regexp"
	"testing"

	"github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/siderolabs/talos-backup/pkg/talos"
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

func TestTakeEtcdSnapshot(t *testing.T) {
	t.Chdir(t.TempDir())

	path, err := talos.TakeEtcdSnapshot(t.Context(), fakeSnapshotter{data: etcdSnapshot()}, "cluster")
	require.NoError(t, err)
	require.Regexp(t, regexp.MustCompile(`^cluster-\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ\.snap$`), path)

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, etcdSnapshot(), content)

	assertOnlyFiles(t, path)
}

func TestTakeEtcdSnapshotErrors(t *testing.T) {
	for _, tc := range []struct {
		name        string
		wantErr     string
		snapshotter fakeSnapshotter
	}{
		{name: "api error", snapshotter: fakeSnapshotter{err: errors.New("boom")}, wantErr: "error taking snapshot"},
		{name: "truncated snapshot", snapshotter: fakeSnapshotter{data: etcdSnapshot()[:1000]}, wantErr: "sha256 checksum not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())

			_, err := talos.TakeEtcdSnapshot(t.Context(), tc.snapshotter, "cluster")
			require.ErrorContains(t, err, tc.wantErr)

			// no snapshot nor .part file left behind
			assertOnlyFiles(t)
		})
	}
}

func assertOnlyFiles(t *testing.T, names ...string) {
	t.Helper()

	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	found := make([]string, 0, len(entries))
	for _, e := range entries {
		found = append(found, e.Name())
	}

	require.ElementsMatch(t, append([]string{}, names...), found)
}
