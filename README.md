# talos-backup

talos-backup is a dead simple backup tool for Talos Linux-based Kubernetes clusters.
It runs as a Kubernetes CronJob in the cluster it backs up: it takes an etcd snapshot through the Talos API, optionally compresses it, encrypts it with [age](https://github.com/FiloSottile/age) and uploads it to AWS S3 or any S3-compatible storage.

## Installation

### 1. Allow Talos API access from Kubernetes

The pod talks to the Talos API with the `os:etcd:backup` role, which must be allowed in the machine config of the control plane nodes.

On Talos v1.14 and later, add this document to the machine config:

```yaml
apiVersion: v1alpha1
kind: KubeTalosAPIAccessConfig
allowedRoles:
  - os:etcd:backup
allowedKubernetesNamespaces:
  - default
```

On older versions, use the (now deprecated) `machine.features` section instead:

```yaml
machine:
  features:
    kubernetesTalosAPIAccess:
      enabled: true
      allowedRoles:
        - os:etcd:backup
      allowedKubernetesNamespaces:
        - default
```

`allowedKubernetesNamespaces` must contain the namespace the CronJob runs in.

### 2. Generate an encryption key

Backups are encrypted with age public keys, only the matching private keys can decrypt them.
[Install age](https://github.com/FiloSottile/age#installation) and generate a key pair:

```bash
age-keygen -o talos-backup.key       # classic X25519 key
age-keygen -pq -o talos-backup.key   # or a post-quantum key (age v1.3+)
```

The public key (`age1...`) goes into the CronJob configuration.
Keep the private key somewhere safe **outside of the cluster**: without it, backups cannot be restored.

Several public keys can be given, comma separated, and each of them can decrypt the backups on its own.
Post-quantum and classic keys cannot be mixed: age refuses it, as the backup would not be quantum resistant.

### 3. Deploy the CronJob

[`cronjob.sample.yaml`](cronjob.sample.yaml) contains:

* a Talos `ServiceAccount` (`talos.dev/v1alpha1`): Talos creates a Kubernetes secret with the same name, holding a talosconfig limited to the `os:etcd:backup` role,
* a CronJob running talos-backup every 10 minutes, with that secret mounted on `/var/run/secrets/talos.dev`, where talos-backup looks for its talosconfig.

The CronJob forbids concurrent runs and stops a backup after 10 minutes (`activeDeadlineSeconds`), so a stuck upload cannot pile up jobs: adjust it along with the schedule.

Snapshots are written to the working directory (`/tmp`, an `emptyDir`) before upload, so it needs room for up to three times the etcd database size (snapshot, compressed and encrypted copies).

Update the S3 settings and the age public key, then apply it:

```bash
kubectl apply -f cronjob.sample.yaml
```

To check the setup, trigger a backup right away:

```bash
kubectl create job --from=cronjob/talos-backup talos-backup-manual
kubectl logs -f job/talos-backup-manual
```

Images are published at `ghcr.io/siderolabs/talos-backup`.

## Configuration

talos-backup is configured with environment variables.

| Variable | Default | Description |
| --- | --- | --- |
| `BUCKET` | required | Bucket to upload the backups to. |
| `AGE_RECIPIENT_PUBLIC_KEY` | required unless encryption is disabled | Comma separated age public keys. |
| `AGE_X25519_PUBLIC_KEY` | | Deprecated alias of `AGE_RECIPIENT_PUBLIC_KEY`, both are merged. |
| `DISABLE_ENCRYPTION` | `false` | Set to `true` to upload unencrypted snapshots. |
| `ENABLE_COMPRESSION` | `false` | Set to `true` to compress snapshots with zstd before encryption. |
| `AWS_REGION` | `us-east-1` with `CUSTOM_S3_ENDPOINT` | Bucket region, required with AWS S3. |
| `CUSTOM_S3_ENDPOINT` | AWS S3 | URL of an S3-compatible API, e.g. `https://s3.example.com`. |
| `USE_PATH_STYLE` | `true` | With `CUSTOM_S3_ENDPOINT` only, set to `false` for virtual-hosted-style addressing. |
| `CLUSTER_NAME` | talosconfig context name | Used in the backup file name. |
| `S3_PREFIX` | cluster name | Prefix (folder) of the backups in the bucket. |

The configuration is checked before any snapshot is taken: a missing bucket, an invalid age key or a missing AWS region makes the job fail right away.

### Backup names

Backups are stored as:

```text
<S3_PREFIX>/<CLUSTER_NAME>-<UTC timestamp>.snap[.zst][.age]
```

For example `prod/prod-2026-10-27T09:59:37Z.snap.zst.age` for a compressed and encrypted backup.

### S3 credentials

Credentials are resolved by the [AWS SDK for Go v2](https://docs.aws.amazon.com/sdkref/latest/guide/standardized-credentials.html) default chain: `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` environment variables, shared config and credentials files (`AWS_PROFILE`, assumed roles, `credential_process`), web identity tokens (IRSA, EKS Pod Identity) and the EC2 instance metadata service.

Store static keys in a Kubernetes secret rather than in the CronJob itself:

```bash
kubectl create secret generic talos-backup-s3 \
  --from-literal=AWS_ACCESS_KEY_ID=... \
  --from-literal=AWS_SECRET_ACCESS_KEY=...
```

### S3-compatible providers

Set `CUSTOM_S3_ENDPOINT` to use another provider than AWS S3.
Path-style addressing (`https://endpoint/bucket/key`) is used by default as it works with most providers, set `USE_PATH_STYLE` to `false` for providers requiring virtual-hosted-style addressing (`https://bucket.endpoint/key`).

With a custom endpoint, request checksums are only sent when the S3 API requires them, as many S3-compatible providers reject the checksums the AWS SDK adds by default.

Some provider specific values:

| Provider | `CUSTOM_S3_ENDPOINT` | `AWS_REGION` |
| --- | --- | --- |
| Backblaze B2 | `https://s3.<region>.backblazeb2.com` | `<region>`, e.g. `us-west-004` |
| Cloudflare R2 | `https://<account id>.r2.cloudflarestorage.com` | `auto` |
| Self-hosted (Garage, Ceph RGW, ...) | URL of the S3 API | region configured on the server |

### Retention

talos-backup never deletes backups: set a lifecycle policy on the bucket to expire old ones.

Backblaze B2:

* In the list of buckets, click on "Lifecycle Settings" next to the bucket storing the Talos backups.
* Select "Use custom lifecycle rules".
* Leave the "File Path" box empty.
* In "Days Till Hide", put the number of days of your desired retention minus 1, then 1 in "Days Till Delete".
* Click on "Update Bucket".

## Restore

Download a backup, for example with the AWS CLI (add `--endpoint-url` for an S3-compatible provider):

```bash
aws s3 cp s3://talos-backups/prod/prod-2026-10-27T09:59:37Z.snap.zst.age .
```

Decrypt it with the private key, then decompress it if compression was enabled:

```bash
age -d -i talos-backup.key -o db.snapshot.zst prod-2026-10-27T09:59:37Z.snap.zst.age
zstd -d db.snapshot.zst -o db.snapshot
```

`db.snapshot` is a regular etcd snapshot, restore it by following the Talos [disaster recovery guide](https://docs.siderolabs.com/talos/latest/build-and-extend-talos/cluster-operations-and-maintenance/disaster-recovery):

```bash
talosctl -n <control plane IP> bootstrap --recover-from=./db.snapshot
```

## Development

Build the binary:

```bash
make talos-backup
```

Build and push a container image:

```bash
make REGISTRY=registry.example.com USERNAME=myusername PUSH=true TAG=latest image-talos-backup
```

Run the linters and unit tests:

```bash
make lint unit-tests
```

The integration test starts a Talos node and an S3 server ([versitygw](https://github.com/versity/versitygw)) in Docker, takes real backups and restores them.
It needs a Docker daemon able to run privileged containers, and ports `50000` and `6443` to be free:

```bash
make integration
```
