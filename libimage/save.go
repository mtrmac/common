package libimage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	dirTransport "github.com/containers/image/v5/directory"
	dockerArchiveTransport "github.com/containers/image/v5/docker/archive"
	"github.com/containers/image/v5/docker/reference"
	"github.com/containers/image/v5/manifest"
	"github.com/containers/image/v5/oci/archive"
	ociTransport "github.com/containers/image/v5/oci/layout"
	"github.com/containers/image/v5/types"
	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
)

// SaveOptions allow for customizing saving images.
type SaveOptions struct {
	CopyOptions

	// AdditionalTags for the saved image.  Incompatible when saving
	// multiple images.
	AdditionalTags []string
}

// Save saves one or more images indicated by `names` in the specified `format`
// to `path`.  Supported formats are oci-archive, docker-archive, oci-dir and
// docker-dir.  The latter two adhere to the dir transport in the corresponding
// oci or docker v2s2 format.  Please note that only docker-archive supports
// saving more than one images.  Other formats will yield an error attempting
// to save more than one.
func (r *Runtime) Save(ctx context.Context, names []string, format, path string, options *SaveOptions) error {
	logrus.Debugf("Saving one more images (%s) to %q", names, path)

	if options == nil {
		options = &SaveOptions{}
	}

	// First some sanity checks to simplify subsequent code.
	switch len(names) {
	case 0:
		return errors.New("no image specified for saving images")
	case 1:
		// All formats support saving 1.
	default:
		if format != "docker-archive" && format != "oci-archive" {
			return fmt.Errorf("unsupported format %q for saving multiple images (only docker-archive and oci-archive)", format)
		}
		if len(options.AdditionalTags) > 0 {
			return fmt.Errorf("cannot save multiple images with multiple tags")
		}
	}

	// Dispatch the save operations.
	switch format {
	case "oci-dir", "docker-dir":
		if len(names) > 1 {
			return fmt.Errorf("%q does not support saving multiple images (%v)", format, names)
		}
		return r.saveSingleImage(ctx, names[0], format, path, options)
	case "docker-archive":
		options.ManifestMIMEType = manifest.DockerV2Schema2MediaType
		return r.saveArchive(ctx, names, format, path, options)
	case "oci-archive":
		options.ManifestMIMEType = ociv1.MediaTypeImageManifest
		return r.saveArchive(ctx, names, format, path, options)
	}

	return fmt.Errorf("unsupported format %q for saving images", format)
}

// saveSingleImage saves the specified image name to the specified path.
// Supported formats are "oci-dir" and "docker-dir".
func (r *Runtime) saveSingleImage(ctx context.Context, name, format, path string, options *SaveOptions) error {
	image, imageName, err := r.LookupImage(name, nil)
	if err != nil {
		return err
	}

	if r.eventChannel != nil {
		defer r.writeEvent(&Event{ID: image.ID(), Name: path, Time: time.Now(), Type: EventTypeImageSave})
	}

	// Unless the image was referenced by ID, use the resolved name as a
	// tag.
	var tag string
	if !strings.HasPrefix(image.ID(), imageName) {
		tag = imageName
	}

	srcRef, err := image.StorageReference()
	if err != nil {
		return err
	}

	// Prepare the destination reference.
	var destRef types.ImageReference
	switch format {
	case "oci-dir":
		destRef, err = ociTransport.NewReference(path, tag)
		options.ManifestMIMEType = ociv1.MediaTypeImageManifest

	case "docker-dir":
		destRef, err = dirTransport.NewReference(path)
		options.ManifestMIMEType = manifest.DockerV2Schema2MediaType

	default:
		return fmt.Errorf("unsupported format %q for saving images", format)
	}

	if err != nil {
		return err
	}

	c, err := r.newCopier(&options.CopyOptions)
	if err != nil {
		return err
	}
	defer c.close()

	_, err = c.copy(ctx, srcRef, destRef)
	return err
}

type localImage struct {
	image     *Image
	tags      []reference.NamedTagged
	destNames []string
}

// saveArchive saves the specified images indicated by names to the path.
// It loads all images from the local containers storage and assembles the meta
// data needed to properly save images.  Since multiple names could refer to
// the *same* image, we need to dance a bit and store additional "names".
// Those can then be used as additional tags when copying.
func (r *Runtime) saveArchive(ctx context.Context, names []string, format, path string, options *SaveOptions) (finalErr error) {
	additionalTags := []reference.NamedTagged{}
	for _, tag := range options.AdditionalTags {
		named, err := NormalizeName(tag)
		if err == nil {
			tagged, withTag := named.(reference.NamedTagged)
			if !withTag {
				return fmt.Errorf("invalid additional tag %q: normalized to untagged %q", tag, named.String())
			}
			additionalTags = append(additionalTags, tagged)
		}
	}

	orderedIDs := []string{}                    // to preserve the relative order
	localImages := make(map[string]*localImage) // to assemble tags
	visitedNames := make(map[string]bool)       // filters duplicate names
	for _, name := range names {
		// Look up local images.
		image, imageName, err := r.LookupImage(name, nil)
		if err != nil {
			return err
		}
		// Make sure to filter duplicates purely based on the resolved
		// name.
		if _, exists := visitedNames[imageName]; exists {
			continue
		}
		visitedNames[imageName] = true
		// Extract and assemble the data.
		local, exists := localImages[image.ID()]
		if !exists {
			local = &localImage{image: image}
			local.tags = additionalTags
			orderedIDs = append(orderedIDs, image.ID())
		}
		// Add the tag if the locally resolved name is properly tagged
		// (which it should unless we looked it up by ID).
		named, err := reference.ParseNamed(imageName)
		if err == nil {
			tagged, withTag := named.(reference.NamedTagged)
			if withTag {
				local.tags = append(local.tags, tagged)
			}
			local.destNames = append(local.destNames, tagged.String())
		}
		localImages[image.ID()] = local
		if r.eventChannel != nil {
			defer r.writeEvent(&Event{ID: image.ID(), Name: path, Time: time.Now(), Type: EventTypeImageSave})
		}
	}

	switch format {
	case "docker-archive":
		if err := r.saveDockerArchive(ctx, path, orderedIDs, localImages, options); err != nil {
			return err
		}

	case "oci-archive":
		if err := r.saveOCIArchive(ctx, path, orderedIDs, localImages, options); err != nil {
			return err
		}

	default:
		return errors.Errorf("internal error: cannot save multiple images to format %q", format)
	}

	return nil
}

func (r *Runtime) saveDockerArchive(ctx context.Context, path string, orderedIDs []string, localImages map[string]*localImage, options *SaveOptions) (finalErr error) {
	writer, err := dockerArchiveTransport.NewWriter(r.systemContextCopy(), path)
	if err != nil {
		return err
	}
	defer func() {
		err := writer.Close()
		if err == nil {
			return
		}
		if finalErr == nil {
			finalErr = err
			return
		}
		finalErr = errors.Wrap(finalErr, err.Error())
	}()

	for _, id := range orderedIDs {
		local, exists := localImages[id]
		if !exists {
			return fmt.Errorf("internal error: saveDockerArchive: ID %s not found in local map", id)
		}

		copyOpts := options.CopyOptions
		copyOpts.dockerArchiveAdditionalTags = local.tags

		c, err := r.newCopier(&copyOpts)
		if err != nil {
			return err
		}
		defer c.close()

		destRef, err := writer.NewReference(nil)
		if err != nil {
			return err
		}

		srcRef, err := local.image.StorageReference()
		if err != nil {
			return err
		}

		if _, err := c.copy(ctx, srcRef, destRef); err != nil {
			return err
		}
	}
	return finalErr
}

func (r *Runtime) saveOCIArchive(ctx context.Context, path string, orderedIDs []string, localImages map[string]*localImage, options *SaveOptions) (finalErr error) {
	writer, err := archive.NewWriter(ctx, r.systemContextCopy(), path)
	if err != nil {
		return err
	}
	defer func() {
		err := writer.Close()
		if err == nil {
			return
		}
		if finalErr == nil {
			finalErr = err
		}
		finalErr = errors.Wrap(finalErr, err.Error())
	}()

	for _, id := range orderedIDs {
		local, exists := localImages[id]
		if !exists {
			return errors.Errorf("internal error: saveOCIArchive: ID %s not found in local map", id)
		}

		copyOpts := options.CopyOptions

		c, err := r.newCopier(&copyOpts)
		if err != nil {
			return err
		}
		defer c.close()

		for _, destName := range local.destNames {
			destRef, err := writer.NewReference(destName)
			if err != nil {
				return err
			}

			srcRef, err := local.image.StorageReference()
			if err != nil {
				return err
			}

			if _, err := c.copy(ctx, srcRef, destRef); err != nil {
				return err
			}
		}
	}
	return finalErr
}
