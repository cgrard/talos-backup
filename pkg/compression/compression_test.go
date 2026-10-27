// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package compression_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"

	"github.com/siderolabs/talos-backup/pkg/compression"
)

func TestCompressFile(t *testing.T) {
	src := filepath.Join(t.TempDir(), "snap")
	content := bytes.Repeat([]byte("etcd snapshot "), 1024)

	require.NoError(t, os.WriteFile(src, content, 0o600))

	compressed, err := compression.CompressFile(src)
	require.NoError(t, err)
	require.Equal(t, src+".zst", compressed)

	f, err := os.Open(compressed)
	require.NoError(t, err)

	t.Cleanup(func() { f.Close() }) //nolint:errcheck

	decoder, err := zstd.NewReader(f)
	require.NoError(t, err)

	defer decoder.Close()

	var out bytes.Buffer

	_, err = out.ReadFrom(decoder)
	require.NoError(t, err)
	require.Equal(t, content, out.Bytes())
}

func TestCompressFileMissing(t *testing.T) {
	src := filepath.Join(t.TempDir(), "missing")

	_, err := compression.CompressFile(src)
	require.Error(t, err)
	require.NoFileExists(t, src+".zst")
}
