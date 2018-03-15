package image

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"syscall"
	"time"

	types2 "github.com/containernetworking/cni/pkg/types"
	cp "github.com/containers/image/copy"
	"github.com/containers/image/docker/reference"
	is "github.com/containers/image/storage"
	"github.com/containers/image/transports/alltransports"
	"github.com/containers/image/types"
	"github.com/containers/storage"
	"github.com/containers/storage/pkg/reexec"
	"github.com/opencontainers/go-digest"
	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/projectatomic/libpod/pkg/inspect"
	"github.com/projectatomic/libpod/pkg/util"
)

// Image is the primary struct for dealing with images
// It is still very much a work in progress
type Image struct {
	inspect.ImageData
	InputName string
	Local     bool
	//runtime   *libpod.Runtime
	image        *storage.Image
	imageruntime *Runtime
}

// Runtime contains the store
type Runtime struct {
	store storage.Store
}

// NewImageRuntime creates an Image Runtime including the store given
// store options
func NewImageRuntime(options storage.StoreOptions) (*Runtime, error) {
	if reexec.Init() {
		return nil, errors.Errorf("unable to reexec")
	}
	store, err := setStore(options)
	if err != nil {
		return nil, err
	}

	return &Runtime{
		store: store,
	}, nil
}

func setStore(options storage.StoreOptions) (storage.Store, error) {
	store, err := storage.GetStore(options)
	if err != nil {
		return nil, err
	}
	is.Transport.SetStore(store)
	return store, nil
}

// newFromStorage creates a new image object from a storage.Image
func (ir *Runtime) newFromStorage(img *storage.Image) *Image {
	image := Image{
		InputName:    img.ID,
		Local:        true,
		imageruntime: ir,
		image:        img,
	}
	return &image
}

// NewFromLocal creates a new image object that is intended
// to only deal with local images already in the store (or
// its aliases)
func (ir *Runtime) NewFromLocal(name string) (*Image, error) {

	image := Image{
		InputName:    name,
		Local:        true,
		imageruntime: ir,
	}
	localImage, err := image.getLocalImage()
	if err != nil {
		return nil, err
	}
	image.image = localImage
	return &image, nil
}

// New creates a new image object where the image could be local
// or remote
func (ir *Runtime) New(name, signaturePolicyPath, authfile string, writer io.Writer, dockeroptions *DockerRegistryOptions, signingoptions SigningOptions) (*Image, error) {
	// We don't know if the image is local or not ... check local first
	newImage := Image{
		InputName:    name,
		Local:        false,
		imageruntime: ir,
	}
	localImage, err := newImage.getLocalImage()
	if err == nil {
		newImage.Local = true
		newImage.image = localImage
		return &newImage, nil
	}

	// The image is not local

	imageName, err := newImage.pullImage(writer, authfile, signaturePolicyPath, signingoptions, dockeroptions)
	if err != nil {
		return &newImage, errors.Errorf("unable to pull %s", name)
	}

	newImage.InputName = imageName
	img, err := newImage.getLocalImage()
	newImage.image = img
	return &newImage, nil
}

// Shutdown closes down the storage and require a bool arg as to
// whether it should do so forcibly.
func (ir *Runtime) Shutdown(force bool) error {
	_, err := ir.store.Shutdown(force)
	return err
}

func (i *Image) reloadImage() error {
	newImage, err := i.imageruntime.getImage(i.ID())
	if err != nil {
		return errors.Wrapf(err, "unable to reload image")
	}
	i.image = newImage.image
	return nil
}

// getLocalImage resolves an unknown input describing an image and
// returns a storage.Image or an error. It is used by NewFromLocal.
func (i *Image) getLocalImage() (*storage.Image, error) {
	imageError := fmt.Sprintf("unable to find '%s' in local storage\n", i.InputName)
	if i.InputName == "" {
		return nil, errors.Errorf("input name is blank")
	}
	var taggedName string
	img, err := i.imageruntime.getImage(i.InputName)
	if err == nil {
		return img.image, err
	}

	// container-storage wasn't able to find it in its current form
	// check if the input name has a tag, and if not, run it through
	// again
	decomposedImage, err := decompose(i.InputName)
	if err != nil {
		return nil, err
	}
	// the inputname isn't tagged, so we assume latest and try again
	if !decomposedImage.isTagged {
		taggedName = fmt.Sprintf("%s:latest", i.InputName)
		img, err = i.imageruntime.getImage(taggedName)
		if err == nil {
			return img.image, nil
		}
	}
	hasReg, err := i.hasRegistry()
	if err != nil {
		return nil, errors.Wrapf(err, imageError)
	}

	// if the input name has a registry in it, the image isnt here
	if hasReg {
		return nil, errors.Errorf("%s", imageError)
	}

	// grab all the local images
	images, err := i.imageruntime.GetImages()
	if err != nil {
		return nil, err
	}

	// check the repotags of all images for a match
	repoImage, err := findImageInRepotags(decomposedImage, images)
	if err == nil {
		return repoImage, nil
	}

	return nil, errors.Errorf("%s", imageError)
}

// hasRegistry returns a bool/err response if the image has a registry in its
// name
func (i *Image) hasRegistry() (bool, error) {
	imgRef, err := reference.Parse(i.InputName)
	if err != nil {
		return false, err
	}
	registry := reference.Domain(imgRef.(reference.Named))
	if registry != "" {
		return true, nil
	}
	return false, nil
}

// ID returns the image ID as a string
func (i *Image) ID() string {
	return i.image.ID
}

// Digest returns the image's Manifest
func (i *Image) Digest() digest.Digest {
	return i.image.Digest
}

// Names returns a string array of names associated with the image
func (i *Image) Names() []string {
	return i.image.Names
}

// Created returns the time the image was created
func (i *Image) Created() time.Time {
	return i.image.Created
}

// TopLayer returns the top layer id as a string
func (i *Image) TopLayer() string {
	return i.image.TopLayer
}

// Remove an image; container removal for the image must be done
// outside the context of images
func (i *Image) Remove(force bool) error {
	_, err := i.imageruntime.store.DeleteImage(i.ID(), true)
	return err
}

func annotations(manifest []byte, manifestType string) map[string]string {
	annotations := make(map[string]string)
	switch manifestType {
	case ociv1.MediaTypeImageManifest:
		var m ociv1.Manifest
		if err := json.Unmarshal(manifest, &m); err == nil {
			for k, v := range m.Annotations {
				annotations[k] = v
			}
		}
	}
	return annotations
}

// Decompose an Image
func (i *Image) Decompose() error {
	return types2.NotImplementedError
}

// TODO: Rework this method to not require an assembly of the fq name with transport
/*
// GetManifest tries to GET an images manifest, returns nil on success and err on failure
func (i *Image) GetManifest() error {
	pullRef, err := alltransports.ParseImageName(i.assembleFqNameTransport())
	if err != nil {
		return errors.Errorf("unable to parse '%s'", i.Names()[0])
	}
	imageSource, err := pullRef.NewImageSource(nil)
	if err != nil {
		return errors.Wrapf(err, "unable to create new image source")
	}
	_, _, err = imageSource.GetManifest(nil)
	if err == nil {
		return nil
	}
	return err
}
*/

// getImage retrieves an image matching the given name or hash from system
// storage
// If no matching image can be found, an error is returned
func (ir *Runtime) getImage(image string) (*Image, error) {
	var img *storage.Image
	ref, err := is.Transport.ParseStoreReference(ir.store, image)
	if err == nil {
		img, err = is.Transport.GetStoreImage(ir.store, ref)
	}
	if err != nil {
		img2, err2 := ir.store.Image(image)
		if err2 != nil {
			if ref == nil {
				return nil, errors.Wrapf(err, "error parsing reference to image %q", image)
			}
			return nil, errors.Wrapf(err, "unable to locate image %q", image)
		}
		img = img2
	}
	newImage := ir.newFromStorage(img)
	return newImage, nil
}

// GetImages retrieves all images present in storage
func (ir *Runtime) GetImages() ([]*Image, error) {
	var newImages []*Image
	images, err := ir.store.Images()
	if err != nil {
		return nil, err
	}
	for _, i := range images {
		newImages = append(newImages, ir.newFromStorage(&i))
	}
	return newImages, nil
}

// getImageDigest creates an image object and uses the hex value of the digest as the image ID
// for parsing the store reference
func getImageDigest(src types.ImageReference, ctx *types.SystemContext) (string, error) {
	newImg, err := src.NewImage(ctx)
	if err != nil {
		return "", err
	}
	defer newImg.Close()
	digest := newImg.ConfigInfo().Digest
	if err = digest.Validate(); err != nil {
		return "", errors.Wrapf(err, "error getting config info")
	}
	return "@" + digest.Hex(), nil
}

// TagImage adds a tag to the given image
func (i *Image) TagImage(tag string) error {
	tags := i.Names()
	if util.StringInSlice(tag, tags) {
		return nil
	}
	tags = append(tags, tag)
	i.reloadImage()
	return i.imageruntime.store.SetNames(i.ID(), tags)
}

// PushImage pushes the given image to a location described by the given path
func (i *Image) PushImage(destination, manifestMIMEType, authFile, signaturePolicyPath string, writer io.Writer, forceCompress bool, signingOptions SigningOptions, dockerRegistryOptions *DockerRegistryOptions) error {
	// PushImage pushes the src image to the destination
	//func PushImage(source, destination string, options CopyOptions) error {
	if destination == "" {
		return errors.Wrapf(syscall.EINVAL, "destination image name must be specified")
	}

	// Get the destination Image Reference
	dest, err := alltransports.ParseImageName(destination)
	if err != nil {
		if hasTransport(destination) {
			return errors.Wrapf(err, "error getting destination imageReference for %q", destination)
		}
		// Try adding the images default transport
		destination2 := DefaultTransport + destination
		dest, err = alltransports.ParseImageName(destination2)
		if err != nil {
			return err
		}
	}

	sc := GetSystemContext(signaturePolicyPath, authFile, forceCompress)

	policyContext, err := getPolicyContext(sc)
	if err != nil {
		return err
	}
	defer policyContext.Destroy()

	// Look up the source image, expecting it to be in local storage
	src, err := is.Transport.ParseStoreReference(i.imageruntime.store, i.ID())
	if err != nil {
		return errors.Wrapf(err, "error getting source imageReference for %q", i.InputName)
	}

	copyOptions := getCopyOptions(writer, signaturePolicyPath, nil, dockerRegistryOptions, signingOptions, authFile, manifestMIMEType, forceCompress)

	// Copy the image to the remote destination
	err = cp.Image(policyContext, dest, src, copyOptions)
	if err != nil {
		return errors.Wrapf(err, "Error copying image to the remote destination")
	}
	return nil
}

// MatchesID returns a bool based on if the input id
// matches the image's id
func (i *Image) MatchesID(id string) bool {
	return strings.HasPrefix(i.ID(), id)
}

// toStorageReference returns a *storageReference from an Image
func (i *Image) toStorageReference() (types.ImageReference, error) {
	return is.Transport.ParseStoreReference(i.imageruntime.store, i.ID())
}

// toImageRef returns an Image Reference type from an image
func (i *Image) toImageRef() (types.Image, error) {
	ref, err := is.Transport.ParseStoreReference(i.imageruntime.store, "@"+i.ID())
	if err != nil {
		return nil, errors.Wrapf(err, "error parsing reference to image %q", i.ID())
	}
	imgRef, err := ref.NewImage(nil)
	if err != nil {
		return nil, errors.Wrapf(err, "error reading image %q", i.ID())
	}
	return imgRef, nil
}

// sizer knows its size.
type sizer interface {
	Size() (int64, error)
}

//Size returns the size of the image
func (i *Image) Size() (*uint64, error) {
	storeRef, err := is.Transport.ParseStoreReference(i.imageruntime.store, i.ID())
	if err != nil {
		return nil, err
	}
	systemContext := &types.SystemContext{}
	img, err := storeRef.NewImageSource(systemContext)
	if err != nil {
		return nil, err
	}
	if s, ok := img.(sizer); ok {
		if sum, err := s.Size(); err == nil {
			usum := uint64(sum)
			return &usum, nil
		}
	}
	return nil, errors.Errorf("unable to determine size")

}