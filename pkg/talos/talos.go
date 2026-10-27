// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package talos provides functions for connecting to and
// taking snapshots from a given talos cluster
package talos

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
)

// TakeEtcdSnapshot will take an etcd snapshot given a talos client
// and save/validate it locally.
func TakeEtcdSnapshot(ctx context.Context, tc *talosclient.Client, clusterName string) (string, error) {
	timeStamp := time.Now()

	dbPath := fmt.Sprintf("%s-%s.snap", clusterName, timeStamp.Format(time.RFC3339))
	partPath := dbPath + ".part"

	defer os.RemoveAll(partPath) //nolint:errcheck

	dest, err := os.OpenFile(partPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("error creating temp file: %w", err)
	}

	defer dest.Close() //nolint:errcheck

	r, err := tc.EtcdSnapshot(ctx, &machine.EtcdSnapshotRequest{})
	if err != nil {
		return "", fmt.Errorf("error taking snapshot: %w", err)
	}

	defer r.Close() //nolint:errcheck

	size, err := io.Copy(dest, r)
	if err != nil {
		return "", fmt.Errorf("error reading: %w", err)
	}

	if err = dest.Sync(); err != nil {
		return "", fmt.Errorf("error fsyncing: %w", err)
	}

	// this check is from https://github.com/etcd-io/etcd/blob/client/v3.5.0-alpha.0/client/v3/snapshot/v3_snapshot.go#L46
	if (size % 512) != sha256.Size {
		return "", fmt.Errorf("sha256 checksum not found (size %d)", size)
	}

	if err = os.Rename(partPath, dbPath); err != nil {
		return "", fmt.Errorf("sha256 checksum not found (size %d)", size)
	}

	log.Printf("etcd snapshot for cluster %q saved to %q (%d bytes)\n", clusterName, dbPath, size)

	return dbPath, nil
}
