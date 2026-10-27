// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

//go:build integration

package dockertest_test

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"io"
	"os"
	"regexp"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/klauspost/compress/zstd"
	dockertest "github.com/ory/dockertest/v3"
	dc "github.com/ory/dockertest/v3/docker"
	"github.com/siderolabs/talos/cmd/talosctl/cmd/mgmt/gen"
	"github.com/siderolabs/talos/cmd/talosctl/pkg/talos/helpers"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	talosconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/gendata"
	"github.com/siderolabs/talos/pkg/machinery/resources/v1alpha1"
	"github.com/stretchr/testify/suite"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/siderolabs/talos-backup/cmd/talos-backup/service"
	pkgconfig "github.com/siderolabs/talos-backup/pkg/config"
)

type integrationTestSuite struct {
	suite.Suite

	ctx       context.Context //nolint:containedctx
	ctxCancel context.CancelFunc

	s3Resource    *dockertest.Resource
	talosResource *dockertest.Resource
	pool          *dockertest.Pool

	talosConfig *talosconfig.Config
	talosClient *talosclient.Client
	s3Client    *s3.Client

	serviceConfig pkgconfig.ServiceConfig
}

func TestIntegrationTestSuite(t *testing.T) {
	suite.Run(t, new(integrationTestSuite))
}

func (suite *integrationTestSuite) SetupTest() {
	suite.ctx, suite.ctxCancel = context.WithTimeout(context.Background(), 3*time.Minute)

	var err error

	suite.pool, err = dockertest.NewPool("")
	suite.Require().Nil(err)

	err = suite.pool.Client.Ping()
	suite.Require().Nil(err)

	suite.Require().Nil(suite.startS3(suite.ctx, suite.pool))

	suite.Require().Nil(suite.startTalosControlPlane(suite.ctx, suite.pool))

	suite.Require().Nil(os.Setenv("AWS_REGION", "us-east-1"))
	suite.Require().Nil(os.Setenv(awsAccessKeyIDEnvVar, s3AccessKey))

	suite.Require().Nil(os.Setenv(awsSecretAccessKeyEnvVar, s3SecretKey))
}

func (suite *integrationTestSuite) TearDownTest() {
	suite.T().Log("tear down")

	suite.ctxCancel()

	suite.Require().Nil(cleanup(suite.pool, suite.s3Resource, suite.talosResource))

	suite.Require().Nil(os.Unsetenv(awsAccessKeyIDEnvVar))

	suite.Require().Nil(os.Unsetenv(awsSecretAccessKeyEnvVar))
}

const (
	s3AccessKey              = "talosbackup"
	s3SecretKey              = "talosbackup"
	awsAccessKeyIDEnvVar     = "AWS_ACCESS_KEY_ID"
	awsSecretAccessKeyEnvVar = "AWS_SECRET_ACCESS_KEY"
)

func applyMachineConfig(cfgBytes []byte) func(ctx context.Context, c *talosclient.Client) error {
	return func(ctx context.Context, c *talosclient.Client) error {
		resp, err := c.ApplyConfiguration(ctx, &machineapi.ApplyConfigurationRequest{
			Data:           cfgBytes,
			Mode:           machineapi.ApplyConfigurationRequest_AUTO,
			DryRun:         false,
			TryModeTimeout: durationpb.New(constants.ConfigTryTimeout),
		})
		if err != nil {
			return fmt.Errorf("error applying new configuration: %w", err)
		}

		helpers.PrintApplyResults(resp)

		return nil
	}
}

func (suite *integrationTestSuite) startTalosControlPlane(ctx context.Context, pool *dockertest.Pool) error {
	suite.serviceConfig.ClusterName = "talos-test-cluster"

	options := &dockertest.RunOptions{
		Repository: "ghcr.io/siderolabs/talos",
		Cmd:        []string{"server", "/data"},
		PortBindings: map[dc.Port][]dc.PortBinding{
			"50000/tcp": {{HostPort: "50000"}},
			"6443/tcp":  {{HostPort: "6443"}},
		},
		Tag: gendata.VersionTag,
		Env: []string{
			"PLATFORM=container",
		},
		Privileged: true,
		Name:       "talos-test-container-4",
		Hostname:   "talos-test",
		SecurityOpt: []string{
			"seccomp=unconfined",
		},
	}

	hcOpt := func(config *dc.HostConfig) {
		config.ReadonlyRootfs = true
		config.AutoRemove = true
		config.Mounts = []dc.HostMount{
			{
				Type:   "tmpfs",
				Target: "/run",
			},
			{
				Type:   "tmpfs",
				Target: "/system",
			},
			{
				Type:   "tmpfs",
				Target: "/tmp",
			},
			{
				Type:   "volume",
				Target: "/system/state",
			},
			{
				Type:   "volume",
				Target: "/var",
			},
			{
				Type:   "volume",
				Target: "/etc/cni",
			},
			{
				Type:   "volume",
				Target: "/etc/kubernetes",
			},
			{
				Type:   "volume",
				Target: "/usr/libexec/kubernetes",
			},
			{
				Type:   "volume",
				Target: "/usr/etc/udev",
			},
			{
				Type:   "volume",
				Target: "/opt",
			},
		}
	}

	var err error

	suite.talosResource, err = pool.RunWithOptions(options, hcOpt)
	if err != nil {
		return fmt.Errorf("Could not start resource: %w", err)
	}

	// NetworkSettings.IPAddress is deprecated and empty on recent Docker versions
	endpoint := suite.talosResource.Container.NetworkSettings.Networks["bridge"].IPAddress
	endpointURL := "https://" + endpoint

	bundle, err := gen.GenerateConfigBundle(
		nil,
		suite.serviceConfig.ClusterName,
		endpointURL,
		constants.DefaultKubernetesVersion,
		nil,
		nil,
		nil,
	)
	if err != nil {
		return err
	}

	suite.talosConfig = bundle.TalosCfg

	suite.talosClient, err = talosclient.New(ctx, talosclient.WithConfig(suite.talosConfig), talosclient.WithEndpoints(endpoint))
	if err != nil {
		return err
	}

	cfgBytes, err := bundle.ControlPlane().Bytes()
	if err != nil {
		return err
	}

	err = retry(pool, func() error {
		return withMaintenanceClient(ctx, endpoint, applyMachineConfig(cfgBytes))
	})
	if err != nil {
		return err
	}

	err = retry(pool, func() error {
		return withConfigClient(ctx, endpoint, bundle.TalosCfg, func(ctx context.Context, c *talosclient.Client) error {
			return c.Bootstrap(ctx, &machineapi.BootstrapRequest{
				RecoverEtcd:          false,
				RecoverSkipHashCheck: false,
			})
		})
	})
	if err != nil {
		return err
	}

	err = retry(pool, func() error {
		return withConfigClient(ctx, endpoint, bundle.TalosCfg, func(ctx context.Context, c *talosclient.Client) error {
			etcdServiceResource, serviceErr := safe.ReaderGet[*v1alpha1.Service](ctx, c.COSI, v1alpha1.NewService("etcd").Metadata())
			if serviceErr != nil {
				return serviceErr
			}

			if etcdServiceResource.TypedSpec().Running {
				return nil
			}

			return fmt.Errorf("etcd didn't start")
		})
	})
	if err != nil {
		return err
	}

	return nil
}

func withMaintenanceClient(ctx context.Context, endpoint string, action func(context.Context, *talosclient.Client) error) error {
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
	}

	c, err := talosclient.New(ctx, talosclient.WithTLSConfig(tlsConfig), talosclient.WithEndpoints(endpoint))
	if err != nil {
		return err
	}

	//nolint:errcheck
	defer c.Close()

	return action(ctx, c)
}

func withConfigClient(ctx context.Context, endpoint string, cfg *talosconfig.Config, action func(context.Context, *talosclient.Client) error) error {
	c, err := talosclient.New(ctx, talosclient.WithConfig(cfg), talosclient.WithEndpoints(endpoint))
	if err != nil {
		return err
	}

	//nolint:errcheck
	defer c.Close()

	return action(ctx, c)
}

func (suite *integrationTestSuite) startS3(ctx context.Context, pool *dockertest.Pool) error {
	s3APIPort := "7070"

	options := &dockertest.RunOptions{
		Repository:   "versity/versitygw",
		Cmd:          []string{"--access", s3AccessKey, "--secret", s3SecretKey, "posix", "/tmp"},
		Tag:          "v1.10.0",
		ExposedPorts: []string{s3APIPort + "/tcp"},
	}

	var err error

	suite.s3Resource, err = pool.RunWithOptions(options)
	if err != nil {
		fmt.Printf("failed to run")
		return err
	}

	endpoint := "http://" + suite.s3Resource.GetHostPort(s3APIPort+"/tcp")

	suite.s3Client = s3.New(s3.Options{
		BaseEndpoint: aws.String(endpoint),
		Region:       "us-east-1",
		UsePathStyle: true,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: s3AccessKey, SecretAccessKey: s3SecretKey}, nil
		}),
	})

	bucketName := "integration-test-bucket"

	err = retry(pool, func() error {
		_, createErr := suite.s3Client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucketName)})

		return createErr
	})
	if err != nil {
		return err
	}

	suite.serviceConfig.CustomS3Endpoint = endpoint
	suite.serviceConfig.Bucket = bucketName
	suite.serviceConfig.S3Prefix = "testdata/snapshots"
	suite.serviceConfig.UsePathStyle = true
	suite.serviceConfig.EnableCompression = true

	return nil
}

func retry(pool *dockertest.Pool, f func() error) error {
	var lastErr error

	captureLastError := func() error {
		lastErr = f()

		return lastErr
	}

	if err := pool.Retry(captureLastError); err != nil {
		return fmt.Errorf("Could not complete action: %w; last error: %w", err, lastErr)
	}

	return nil
}

func cleanup(pool *dockertest.Pool, resources ...*dockertest.Resource) error {
	for _, resource := range resources {
		if resource != nil {
			if err := pool.Purge(resource); err != nil {
				return fmt.Errorf("Could not purge resource: %w", err)
			}
		}
	}

	return nil
}

func (suite *integrationTestSuite) TestBackupEncryptedSnapshot() {
	x25519a, err := age.GenerateX25519Identity()
	suite.Require().NoError(err)

	x25519b, err := age.GenerateX25519Identity()
	suite.Require().NoError(err)

	hybrid, err := age.GenerateHybridIdentity()
	suite.Require().NoError(err)

	// multiple classic recipients, each must be able to restore
	suite.serviceConfig.AgeRecipientPublicKey = []string{x25519a.Recipient().String(), x25519b.Recipient().String()}
	suite.Require().NoError(service.BackupSnapshot(suite.ctx, &suite.serviceConfig, suite.talosConfig, suite.talosClient))
	suite.requireRestorableBackups(x25519a, x25519b)

	// post-quantum recipient
	suite.serviceConfig.AgeRecipientPublicKey = []string{hybrid.Recipient().String()}
	suite.Require().NoError(service.BackupSnapshot(suite.ctx, &suite.serviceConfig, suite.talosConfig, suite.talosClient))
	suite.requireRestorableBackups(hybrid)
}

// requireRestorableBackups downloads every backup, restores it with each identity,
// checks it is a valid etcd snapshot and deletes it.
func (suite *integrationTestSuite) requireRestorableBackups(identities ...age.Identity) {
	objects, err := suite.s3Client.ListObjectsV2(suite.ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(suite.serviceConfig.Bucket),
	})
	suite.Require().NoError(err)
	suite.Require().Len(objects.Contents, 1)

	key := objects.Contents[0].Key
	suite.Require().Regexp(regexp.MustCompile(`^testdata/snapshots/talos-test-cluster-\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ\.snap\.zst\.age$`), aws.ToString(key))

	for _, identity := range identities {
		obj, err := suite.s3Client.GetObject(suite.ctx, &s3.GetObjectInput{Bucket: aws.String(suite.serviceConfig.Bucket), Key: key})
		suite.Require().NoError(err)

		decrypted, err := age.Decrypt(obj.Body, identity)
		suite.Require().NoError(err)

		decoder, err := zstd.NewReader(decrypted)
		suite.Require().NoError(err)

		snapshot, err := io.ReadAll(decoder)
		suite.Require().NoError(err)

		decoder.Close()
		suite.Require().NoError(obj.Body.Close())

		// etcd snapshots end with the sha256 of the bbolt database, as checked by etcdutl snapshot restore
		suite.Require().Equal(sha256.Size, len(snapshot)%512)

		sum := sha256.Sum256(snapshot[:len(snapshot)-sha256.Size])
		suite.Require().Equal(sum[:], snapshot[len(snapshot)-sha256.Size:])
	}

	_, err = suite.s3Client.DeleteObject(suite.ctx, &s3.DeleteObjectInput{Bucket: aws.String(suite.serviceConfig.Bucket), Key: key})
	suite.Require().NoError(err)
}
