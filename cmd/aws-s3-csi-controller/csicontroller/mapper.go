package csicontroller

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

const (
	ACKBucketAPIVersion = "s3.services.k8s.aws/v1alpha1"
	ACKBucketKind       = "Bucket"
)

// ParameterMapper handles the conversion of StorageClass parameters to an unstructured object (for use with dynamic or controller-runtime clients)
type ParameterMapper struct{}

// MapToBucketObject converts yaml string of Bucket Custom Resource template to an unstructured.Unstructured object.
func (m *ParameterMapper) MapToBucketObject(bucketDefYAML string) (*unstructured.Unstructured, error) {
	// Parse the YAML into an unstructured object
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(bucketDefYAML), &obj); err != nil {
		return nil, fmt.Errorf("failed to unmarshal bucket definition: %w", err)
	}

	// Validate and set API version and kind if not set
	// TODO: Add more validations, maybe add ability for customers to configure namespace allowlist
	if obj.GetAPIVersion() == "" {
		obj.SetAPIVersion(ACKBucketAPIVersion)
	} else if obj.GetAPIVersion() != ACKBucketAPIVersion {
		return nil, fmt.Errorf("invalid apiVersion: %s, expected: %s", obj.GetAPIVersion(), ACKBucketAPIVersion)
	}

	if obj.GetKind() == "" {
		obj.SetKind(ACKBucketKind)
	} else if obj.GetKind() != ACKBucketKind {
		return nil, fmt.Errorf("invalid kind: %s, expected: %s", obj.GetKind(), ACKBucketKind)
	}

	return obj, nil
}
