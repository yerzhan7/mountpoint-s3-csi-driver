package controller

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Placeholders supported in the [ParamPrefixPattern] StorageClass parameter.
const (
	placeholderPVName       = "${.PV.name}"
	placeholderPVCName      = "${.PVC.name}"
	placeholderPVCNamespace = "${.PVC.namespace}"
)

// placeholderRegexp matches the `${...}` placeholders of a prefix pattern.
var placeholderRegexp = regexp.MustCompile(`\$\{[^{}]*\}`)

// volumeMetadata is the identity of the volume being provisioned. It's used to resolve
// the placeholders of a prefix pattern.
type volumeMetadata struct {
	pvName       string
	pvcName      string
	pvcNamespace string
}

// resolvePrefixPattern resolves given `pattern` into an S3 key prefix by substituting its
// placeholders with `metadata`, and normalizes the result into a prefix Mountpoint accepts,
// i.e., a non-empty prefix without a leading "/" and with a trailing "/".
func resolvePrefixPattern(pattern string, metadata volumeMetadata) (string, error) {
	var errs []error

	resolved := placeholderRegexp.ReplaceAllStringFunc(pattern, func(placeholder string) string {
		value, err := metadata.resolvePlaceholder(placeholder)
		if err != nil {
			errs = append(errs, err)
			return ""
		}
		return value
	})
	if err := errors.Join(errs...); err != nil {
		return "", err
	}

	// `placeholderRegexp` only matches terminated placeholders, catch the rest here
	// instead of silently mounting a prefix with a literal "${" in it.
	if strings.Contains(resolved, "${") {
		return "", fmt.Errorf("unterminated placeholder in %q", resolved)
	}

	// Mountpoint expects prefixes to not start with a "/" and to end with a "/".
	// Be lenient here and normalize the resolved prefix instead of rejecting it.
	prefix := strings.TrimPrefix(resolved, "/")
	if prefix == "" {
		return "", errors.New("resolved to an empty prefix")
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	// Mountpoint also rejects prefixes with empty or relative path components.
	for _, component := range strings.Split(strings.TrimSuffix(prefix, "/"), "/") {
		switch component {
		case "":
			return "", fmt.Errorf("resolved prefix %q has an empty path component", prefix)
		case ".", "..":
			return "", fmt.Errorf("resolved prefix %q has a relative path component %q", prefix, component)
		}
	}

	return prefix, nil
}

// resolvePlaceholder returns the value of given `placeholder` from the volume's metadata.
func (m volumeMetadata) resolvePlaceholder(placeholder string) (string, error) {
	var name, value string

	switch placeholder {
	case placeholderPVName:
		name, value = "PersistentVolume name", m.pvName
	case placeholderPVCName:
		name, value = "PersistentVolumeClaim name", m.pvcName
	case placeholderPVCNamespace:
		name, value = "PersistentVolumeClaim namespace", m.pvcNamespace
	default:
		return "", fmt.Errorf("unknown placeholder %q, supported placeholders are %q, %q, and %q",
			placeholder, placeholderPVName, placeholderPVCName, placeholderPVCNamespace)
	}

	if value == "" {
		return "", fmt.Errorf("%s is unknown for placeholder %q, ensure the external-provisioner is run with `--extra-create-metadata`",
			name, placeholder)
	}

	// These are Kubernetes object names and are therefore already validated by the API server, but
	// the PersistentVolumeClaim ones might be chosen by unprivileged users - validate them here as well
	// to ensure they can't escape the prefix they're given.
	if errs := validation.IsDNS1123Subdomain(value); len(errs) > 0 {
		return "", fmt.Errorf("%s %q is not a valid object name: %s", name, value, strings.Join(errs, ", "))
	}

	return value, nil
}
