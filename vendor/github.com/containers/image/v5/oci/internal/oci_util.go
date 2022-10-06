package internal

import (
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// annotation spex from https://github.com/opencontainers/image-spec/blob/master/annotations.md#pre-defined-annotation-keys
const (
	separator = `(?:[-._:@+]|--)`
	alphanum  = `(?:[A-Za-z0-9]+)`
	component = `(?:` + alphanum + `(?:` + separator + alphanum + `)*)`
)

var refRegexp = regexp.MustCompile(`^` + component + `(?:/` + component + `)*$`)
var windowsRefRegexp = regexp.MustCompile(`^([a-zA-Z]:\\.+?):(.*)$`)

// ValidateImageName returns nil if the image name is empty or matches the open-containers image name specs.
// In any other case an error is returned.
func ValidateImageName(image string) error {
	if len(image) == 0 {
		return nil
	}

	var err error
	if !refRegexp.MatchString(image) {
		err = errors.Errorf("Invalid image %s", image)
	}
	return err
}

// splitPathAndImage tries to split the provided OCI reference into the OCI path and image.
// Neither path nor image parts are validated at this stage.
func splitPathAndImage(reference string) (string, string) {
	if runtime.GOOS == "windows" {
		return splitPathAndImageWindows(reference)
	}
	return splitPathAndImageNonWindows(reference)
}

func splitPathAndImageWindows(reference string) (string, string) {
	groups := windowsRefRegexp.FindStringSubmatch(reference)
	// nil group means no match
	if groups == nil {
		return reference, ""
	}

	// we expect three elements. First one full match, second the capture group for the path and
	// the third the capture group for the image
	if len(groups) != 3 {
		return reference, ""
	}
	return groups[1], groups[2]
}

func splitPathAndImageNonWindows(reference string) (string, string) {
	sep := strings.SplitN(reference, ":", 2)
	path := sep[0]

	var image string
	if len(sep) == 2 {
		image = sep[1]
	}
	return path, image
}

// ValidateOCIPath takes the OCI path and validates it.
func ValidateOCIPath(path string) error {
	if runtime.GOOS == "windows" {
		// On Windows we must allow for a ':' as part of the path
		if strings.Count(path, ":") > 1 {
			return errors.Errorf("Invalid OCI reference: path %s contains more than one colon", path)
		}
	} else {
		if strings.Contains(path, ":") {
			return errors.Errorf("Invalid OCI reference: path %s contains a colon", path)
		}
	}
	return nil
}

// ValidateScope validates a policy configuration scope for an OCI transport.
func ValidateScope(scope string) error {
	var err error
	if runtime.GOOS == "windows" {
		err = validateScopeWindows(scope)
	} else {
		err = validateScopeNonWindows(scope)
	}
	if err != nil {
		return err
	}

	cleaned := filepath.Clean(scope)
	if cleaned != scope {
		return errors.Errorf(`Invalid scope %s: Uses non-canonical path format, perhaps try with path %s`, scope, cleaned)
	}

	return nil
}

func validateScopeWindows(scope string) error {
	matched, _ := regexp.Match(`^[a-zA-Z]:\\`, []byte(scope))
	if !matched {
		return errors.Errorf("Invalid scope '%s'. Must be an absolute path", scope)
	}

	return nil
}

func validateScopeNonWindows(scope string) error {
	if !strings.HasPrefix(scope, "/") {
		return errors.Errorf("Invalid scope %s: must be an absolute path", scope)
	}

	// Refuse also "/", otherwise "/" and "" would have the same semantics,
	// and "" could be unexpectedly shadowed by the "/" entry.
	if scope == "/" {
		return errors.New(`Invalid scope "/": Use the generic default scope ""`)
	}

	return nil
}

// parseOCIReferenceName parses the image from the oci reference.
func parseOCIReferenceName(image string) (img string, index int, err error) {
	index = -1
	if strings.HasPrefix(image, "@") {
		idx, err := strconv.Atoi(image[1:])
		if err != nil {
			return "", index, fmt.Errorf("Invalid source index @%s: not an integer: %w", image[1:], err)
		}
		if idx < 0 {
			return "", index, fmt.Errorf("Invalid source index @%d: must not be negative", idx)
		}
		index = idx
	} else {
		img = image
	}
	return img, index, nil
}

// ParseReferenceIntoElements splits the oci reference into location, image name and source index if exists
func ParseReferenceIntoElements(reference string) (string, string, int, error) {
	dir, image := splitPathAndImage(reference)
	image, index, err := parseOCIReferenceName(image)
	if err != nil {
		return "", "", -1, err
	}
	return dir, image, index, nil
}

// NameFromAnnotations returns a reference string to be used as an image name,
// or an empty string.  The annotations map may be nil.
func NameFromAnnotations(annotations map[string]string) string {
	if annotations == nil {
		return ""
	}
	// buildkit/containerd are using a custom annotation see
	// containers/podman/issues/12560.
	if annotations["io.containerd.image.name"] != "" {
		return annotations["io.containerd.image.name"]
	}
	return annotations[imgspecv1.AnnotationRefName]
}
