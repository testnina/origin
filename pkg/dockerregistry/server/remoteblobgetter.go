package server

import (
	"net/http"

	"github.com/docker/distribution"
	"github.com/docker/distribution/context"
	"github.com/docker/distribution/digest"

	"k8s.io/kubernetes/pkg/api/errors"

	imageapi "github.com/openshift/origin/pkg/image/api"
	"github.com/openshift/origin/pkg/image/importer"
)

// BlobGetterService combines the operations to access and read blobs.
type BlobGetterService interface {
	distribution.BlobStatter
	distribution.BlobProvider
	distribution.BlobServer
}

// remoteBlobGetterService implements BlobGetterService and allows to serve blobs from remote
// repositories.
type remoteBlobGetterService struct {
	repo                       *repository
	digestToStore              map[string]distribution.BlobStore
	pullFromInsecureRegistries bool
}

var _ BlobGetterService = &remoteBlobGetterService{}

// Stat provides metadata about a blob identified by the digest. If the
// blob is unknown to the describer, ErrBlobUnknown will be returned.
func (rbs *remoteBlobGetterService) Stat(ctx context.Context, dgst digest.Digest) (distribution.Descriptor, error) {
	// look up the potential remote repositories that this blob could be part of (at this time,
	// we don't know which image in the image stream surfaced the content).
	is, err := rbs.repo.getImageStream()
	if err != nil {
		if errors.IsNotFound(err) || errors.IsForbidden(err) {
			return distribution.Descriptor{}, distribution.ErrBlobUnknown
		}
		context.GetLogger(ctx).Errorf("Error retrieving image stream for blob: %v", err)
		return distribution.Descriptor{}, err
	}

	rbs.pullFromInsecureRegistries = false

	if insecure, ok := is.Annotations[imageapi.InsecureRepositoryAnnotation]; ok {
		rbs.pullFromInsecureRegistries = insecure == "true"
	}

	var localRegistry string
	if local, err := imageapi.ParseDockerImageReference(is.Status.DockerImageRepository); err == nil {
		// TODO: normalize further?
		localRegistry = local.Registry
	}

	retriever := rbs.repo.importContext()
	cached := rbs.repo.cachedLayers.RepositoriesForDigest(dgst)

	// look at the first level of tagged repositories first
	search := rbs.identifyCandidateRepositories(is, localRegistry, true)
	if desc, err := rbs.findCandidateRepository(ctx, search, cached, dgst, retriever); err == nil {
		return desc, nil
	}

	// look at all other repositories tagged by the server
	secondary := rbs.identifyCandidateRepositories(is, localRegistry, false)
	for k := range search {
		delete(secondary, k)
	}
	if desc, err := rbs.findCandidateRepository(ctx, secondary, cached, dgst, retriever); err == nil {
		return desc, nil
	}

	return distribution.Descriptor{}, distribution.ErrBlobUnknown
}

func (rbs *remoteBlobGetterService) Open(ctx context.Context, dgst digest.Digest) (distribution.ReadSeekCloser, error) {
	store, ok := rbs.digestToStore[dgst.String()]
	if ok {
		return store.Open(ctx, dgst)
	}

	desc, err := rbs.Stat(ctx, dgst)
	if err != nil {
		context.GetLogger(ctx).Errorf("Open: failed to stat blob %q in remote repositories: %v", dgst.String(), err)
		return nil, err
	}

	store, ok = rbs.digestToStore[desc.Digest.String()]
	if !ok {
		return nil, distribution.ErrBlobUnknown
	}

	return store.Open(ctx, desc.Digest)
}

func (rbs *remoteBlobGetterService) ServeBlob(ctx context.Context, w http.ResponseWriter, req *http.Request, dgst digest.Digest) error {
	store, ok := rbs.digestToStore[dgst.String()]
	if ok {
		return store.ServeBlob(ctx, w, req, dgst)
	}

	desc, err := rbs.Stat(ctx, dgst)
	if err != nil {
		context.GetLogger(ctx).Errorf("ServeBlob: failed to stat blob %q in remote repositories: %v", dgst.String(), err)
		return err
	}

	store, ok = rbs.digestToStore[desc.Digest.String()]
	if !ok {
		return distribution.ErrBlobUnknown
	}

	return store.ServeBlob(ctx, w, req, desc.Digest)
}

// proxyStat attempts to locate the digest in the provided remote repository or returns an error. If the digest is found,
// rbs.digestToStore saves the store.
func (rbs *remoteBlobGetterService) proxyStat(ctx context.Context, retriever importer.RepositoryRetriever, ref imageapi.DockerImageReference, dgst digest.Digest) (distribution.Descriptor, error) {
	context.GetLogger(ctx).Infof("Trying to stat %q from %q", dgst, ref.Exact())

	ctx = WithRemoteBlobGetter(ctx, rbs)

	repo, err := retriever.Repository(ctx, ref.RegistryURL(), ref.RepositoryName(), rbs.pullFromInsecureRegistries)
	if err != nil {
		context.GetLogger(ctx).Errorf("error getting remote repository for image %q: %v", ref.Exact(), err)
		return distribution.Descriptor{}, err
	}

	bs := repo.Blobs(ctx)

	desc, err := bs.Stat(ctx, dgst)
	if err != nil {
		if err != distribution.ErrBlobUnknown {
			context.GetLogger(ctx).Errorf("error getting remoteBlobGetterService for image %q: %v", ref.Exact(), err)
		}
		return distribution.Descriptor{}, err
	}

	rbs.digestToStore[dgst.String()] = bs

	return desc, nil
}

// Get attempts to fetch the requested blob by digest using a remote proxy store if necessary.
func (rbs *remoteBlobGetterService) Get(ctx context.Context, dgst digest.Digest) ([]byte, error) {
	store, ok := rbs.digestToStore[dgst.String()]
	if ok {
		return store.Get(ctx, dgst)
	}

	desc, err := rbs.Stat(ctx, dgst)
	if err != nil {
		context.GetLogger(ctx).Errorf("Get: failed to stat blob %q in remote repositories: %v", dgst.String(), err)
		return nil, err
	}

	store, ok = rbs.digestToStore[desc.Digest.String()]
	if !ok {
		return nil, distribution.ErrBlobUnknown
	}

	return store.Get(ctx, desc.Digest)
}

// findCandidateRepository looks in search for a particular blob, referring to previously cached items
func (rbs *remoteBlobGetterService) findCandidateRepository(ctx context.Context, search map[string]*imageapi.DockerImageReference, cachedLayers []string, dgst digest.Digest, retriever importer.RepositoryRetriever) (distribution.Descriptor, error) {
	// no possible remote locations to search, exit early
	if len(search) == 0 {
		return distribution.Descriptor{}, distribution.ErrBlobUnknown
	}

	// see if any of the previously located repositories containing this digest are in this
	// image stream
	for _, repo := range cachedLayers {
		ref, ok := search[repo]
		if !ok {
			continue
		}
		desc, err := rbs.proxyStat(ctx, retriever, *ref, dgst)
		if err != nil {
			delete(search, repo)
			continue
		}
		context.GetLogger(ctx).Infof("Found digest location from cache %q in %q", dgst, repo)
		return desc, nil
	}

	// search the remaining registries for this digest
	for repo, ref := range search {
		desc, err := rbs.proxyStat(ctx, retriever, *ref, dgst)
		if err != nil {
			continue
		}
		rbs.repo.cachedLayers.RememberDigest(dgst, rbs.repo.blobrepositorycachettl, repo)
		context.GetLogger(ctx).Infof("Found digest location by search %q in %q", dgst, repo)
		return desc, nil
	}

	return distribution.Descriptor{}, distribution.ErrBlobUnknown
}

// identifyCandidateRepositories returns a map of remote repositories referenced by this image stream.
func (rbs *remoteBlobGetterService) identifyCandidateRepositories(is *imageapi.ImageStream, localRegistry string, primary bool) map[string]*imageapi.DockerImageReference {
	// identify the canonical location of referenced registries to search
	search := make(map[string]*imageapi.DockerImageReference)
	for _, tagEvent := range is.Status.Tags {
		var candidates []imageapi.TagEvent
		if primary {
			if len(tagEvent.Items) == 0 {
				continue
			}
			candidates = tagEvent.Items[:1]
		} else {
			if len(tagEvent.Items) <= 1 {
				continue
			}
			candidates = tagEvent.Items[1:]
		}
		for _, event := range candidates {
			ref, err := imageapi.ParseDockerImageReference(event.DockerImageReference)
			if err != nil {
				continue
			}
			// skip anything that matches the innate registry
			// TODO: there may be a better way to make this determination
			if len(localRegistry) != 0 && localRegistry == ref.Registry {
				continue
			}
			ref = ref.DockerClientDefaults()
			search[ref.AsRepository().Exact()] = &ref
		}
	}
	return search
}
