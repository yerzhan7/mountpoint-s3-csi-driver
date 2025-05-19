# Dynamic Provisioning with AWS Mountpoint S3 CSI Driver

This guide explains how to dynamically provision S3 buckets using the AWS Mountpoint S3 CSI Driver. The driver enables you to provision and manage S3 buckets directly from Kubernetes using standard Persistent Volume Claims (PVCs).

Dynamic Provisioning can be configured to either create new S3 buckets or re-use existing one.

## AWS Controllers for Kubernetes (ACK) S3 Controller Requirement

The AWS Mountpoint S3 CSI Driver relies on the AWS Controllers for Kubernetes (ACK) S3 Controller for dynamic bucket provisioning if you want to create new buckets. **Important information to know:**

1. **User Installation Requirement:**
   - **You must install and configure the AWS ACK S3 Controller yourself if you want to create new buckets during dynamic provisioning.** The S3 CSI Driver does not include or install this component automatically.
   - If your cluster does not have the ACK S3 Controller installed, dynamic provisioning will fail to create buckets when you use `createBucket: true` in StorageClass parameter. It will work fine when you only want to re-use existing bucket `createBucket: false`.
   - Please follow the [ACK S3 Controller installation instructions](https://aws-controllers-k8s.github.io/community/docs/user-docs/install/) to set up the controller properly.

2. **Multi-Region and Multi-Account Support:**
   - ACK supports installing multiple instances of controllers for different namespaces to work with different regions, AWS accounts, or IAM credentials.

## Storage Class Parameters

The driver supports the following parameters in the StorageClass definition:

### Core Parameters

| Parameter | Type | Description | Default |
|-----------|------|-------------|---------|
| createBucket | boolean | Whether CSI driver should create Bucket CRD and create new bucket or use existing bucket | true |
| uniqueBucketPerVolume | boolean | Whether CSI driver should create/use one bucket per PVC or share a single bucket across all PVCs for this StorageClass | true |
| bucketNamePrefix | string | Prefix to use for bucket names | "" |
| enablePVCPathIsolation | boolean | Whether to add PVC name as prefix in mountpoint for isolation (only works when uniqueBucketPerVolume=false) | false |
| bucketDefinition | yaml string | Complete ACK S3 Bucket CRD definition including apiVersion, kind, metadata and spec | {} |

**TODO: Add `authenticationSource: pod` parameter? OR ask customer to specify this and other `volumeAttributes` via PVC labels/annotations?**

**Important Notes:**
- When `uniqueBucketPerVolume=false`, all PVCs will share the same bucket
- If a bucket name is specified in `bucketDefinition.spec.name`, it cannot be used with `uniqueBucketPerVolume=true`
- You cannot specify bucket name (via metadata.name and spec.name) at the same time as using bucketNamePrefix
- Use `enablePVCPathIsolation=true` with shared bucket to automatically isolate PVC data in the same bucket by using `--prefix {pv_namespace}/{pv_name}/` mountpoint option
- The `bucketDefinition` parameter must include the valid ACK Bucket CRD structure including apiVersion and kind
- `metadata.name` should match `spec.name` to avoid creating duplicate buckets if CreateVolume is invoked twice

## Configuration Modes

### 1. Default Mode (One Bucket Per PVC)

Creates a new bucket for each PVC with an auto-generated name:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: s3-bucket-basic
provisioner: s3.csi.aws.com
parameters:
  createBucket: "true"
  uniqueBucketPerVolume: "true"
  bucketDefinition: |
    apiVersion: s3.services.k8s.aws/v1alpha1
    kind: Bucket
    spec:
      versioning:
        status: Enabled
      encryption:
        rules:
          - applyServerSideEncryptionByDefault:
              sseAlgorithm: AES256
      notification:
        topicConfigurations:
        - id: "Publish new objects to SNS"
          topicARN: "$NOTIFICATION_TOPIC_ARN"
          events:
          - "s3:ObjectCreated:Put"
```

### 2. Shared Bucket Mode with Namespace Specification

Share a single bucket across multiple PVCs and specify which ACK controller to use via namespace:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: s3-shared-bucket
provisioner: s3.csi.aws.com
parameters:
  createBucket: "true"
  uniqueBucketPerVolume: "false"
  enablePVCPathIsolation: "true"
  bucketDefinition: |
    apiVersion: s3.services.k8s.aws/v1alpha1
    kind: Bucket
    metadata:
      name: shared-data-bucket
      namespace: prod-aws-account
      annotations:
        services.k8s.aws/region: us-west-2
    spec:
      name: shared-data-bucket
      versioning:
        status: Enabled
```

### 3. Custom Bucket Name with Prefix

Create buckets with custom prefix:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: s3-bucket-with-prefix
provisioner: s3.csi.aws.com
parameters:
  createBucket: "true"
  uniqueBucketPerVolume: "true"
  bucketNamePrefix: "myapp"
  bucketDefinition: |
    apiVersion: s3.services.k8s.aws/v1alpha1
    kind: Bucket
    # Note: Do not specify metadata.name or spec.name when using bucketNamePrefix
    spec:
      versioning:
        status: Enabled
```

### 4. Use Existing Bucket in Different AWS Account

Use an existing bucket in a different AWS account managed by a specific ACK controller:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: s3-bucket-cross-account
provisioner: s3.csi.aws.com
parameters:
  createBucket: "false"
  uniqueBucketPerVolume: "false"
  enablePVCPathIsolation: "true"
  bucketDefinition: |
    apiVersion: s3.services.k8s.aws/v1alpha1
    kind: Bucket
    metadata:
      name: my-existing-bucket
      namespace: secondary-aws-account
    spec:
      name: my-existing-bucket
```

## Bucket Properties

The `bucketDefinition` parameter requires the complete ACK S3 Controller's Bucket CRD structure: https://pkg.go.dev/github.com/aws-controllers-k8s/s3-controller@v1.0.29/apis/v1alpha1#Bucket

### Required Fields

| Field | Description |
|-------|-------------|
| apiVersion | Must be "s3.services.k8s.aws/v1alpha1" |
| kind | Must be "Bucket" |

#### Metadata Fields

| Field | Type | Description |
|-------|------|-------------|
| name | string | Name of the bucket CRD (should match spec.name) |
| namespace | string | Namespace for the bucket CRD (determines which ACK controller is used) |

#### Spec Fields

| Property | Type | Description |
|----------|------|-------------|
| name | string | Bucket name (required when uniqueBucketPerVolume=false) |

See full spec fields in https://pkg.go.dev/github.com/aws-controllers-k8s/s3-controller@v1.0.29/apis/v1alpha1#Bucket

## Mount Options

Mount options can be specified in the StorageClass:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: s3-bucket-cross-account
provisioner: s3.csi.aws.com
mountOptions:
  - allow-delete
  - region us-east-1
  - debug-crt
```

### PVC example

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: s3-dp-pvc-1
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: s3-storage-class
  resources:
    requests:
      storage: 1200Gi # Ignored, required
```


### StatefulSets example

```yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: s3-ss
spec:
  serviceName: s3-ss
  replicas: 2
  selector:
    matchLabels:
      app: s3-ss
  template:
    metadata:
      labels:
        app: s3-ss
    spec:
      containers:
      - name: s3-ss
        image: ubuntu
        command: ["/bin/sh"]
        # Write 1 object to S3 and sleep
        args: ["-c", "trap 'exit' TERM; echo 'Hello from the container!' >> /data/s3-ss-1-$NODE_NAME-$(date -u +'%Y-%m-%dT%H:%M:%S.%3N').txt; while true; do sleep 1; done"]
        env:
        - name: NODE_NAME
          valueFrom:
            fieldRef:
              fieldPath: spec.nodeName
        volumeMounts:
        - name: persistent-storage
          mountPath: /data
  volumeClaimTemplates:
  - metadata:
      name: persistent-storage
    spec:
      accessModes: ["ReadWriteMany"]
      storageClassName: s3-storage-class
      resources:
        requests:
          storage: 1Gi # Not used
```