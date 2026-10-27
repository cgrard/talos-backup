// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package encryption_test

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
	"github.com/stretchr/testify/require"

	"github.com/siderolabs/talos-backup/pkg/encryption"
)

type keyPair struct {
	identity  age.Identity
	recipient string
}

func x25519(t *testing.T) keyPair {
	t.Helper()

	id, err := age.GenerateX25519Identity()
	require.NoError(t, err)

	return keyPair{identity: id, recipient: id.Recipient().String()}
}

func hybrid(t *testing.T) keyPair {
	t.Helper()

	id, err := age.GenerateHybridIdentity()
	require.NoError(t, err)

	return keyPair{identity: id, recipient: id.Recipient().String()}
}

func TestEncryptFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys []keyPair
	}{
		{name: "multiple x25519 recipients", keys: []keyPair{x25519(t), x25519(t)}},
		{name: "post-quantum recipient", keys: []keyPair{hybrid(t)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "snap")
			content := []byte("etcd snapshot")

			require.NoError(t, os.WriteFile(src, content, 0o600))

			recipients := make([]string, 0, len(tc.keys))
			for _, k := range tc.keys {
				recipients = append(recipients, k.recipient)
			}

			encrypted, err := encryption.EncryptFile(src, recipients)
			require.NoError(t, err)
			require.Equal(t, src+".age", encrypted)

			// every recipient must be able to decrypt on its own
			for _, k := range tc.keys {
				f, err := os.Open(encrypted)
				require.NoError(t, err)

				r, err := age.Decrypt(f, k.identity)
				require.NoError(t, err)

				out, err := io.ReadAll(r)
				require.NoError(t, err)
				require.Equal(t, content, out)
				require.NoError(t, f.Close())
			}
		})
	}
}

func TestEncryptFileErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		keys    []string
		missing bool
	}{
		{name: "invalid key", keys: []string{"age1invalid"}},
		{name: "no keys"},
		// age refuses to mix them, the file would not be quantum resistant
		{name: "post-quantum and x25519 keys", keys: []string{hybrid(t).recipient, x25519(t).recipient}},
		{name: "missing file", keys: []string{x25519(t).recipient}, missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "snap")

			if !tc.missing {
				require.NoError(t, os.WriteFile(src, []byte("etcd snapshot"), 0o600))
			}

			_, err := encryption.EncryptFile(src, tc.keys)
			require.Error(t, err)
			require.NoFileExists(t, src+".age")
		})
	}
}
