package base

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/diff"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/leases"
	"github.com/containerd/containerd/remotes/docker"
	"github.com/containerd/containerd/rootfs"
	cdsnapshot "github.com/containerd/containerd/snapshots"
	"github.com/docker/docker/pkg/idtools"
	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/cache/blobs"
	"github.com/moby/buildkit/cache/metadata"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/executor"
	"github.com/moby/buildkit/exporter"
	imageexporter "github.com/moby/buildkit/exporter/containerimage"
	localexporter "github.com/moby/buildkit/exporter/local"
	ociexporter "github.com/moby/buildkit/exporter/oci"
	tarexporter "github.com/moby/buildkit/exporter/tar"
	"github.com/moby/buildkit/frontend"
	gw "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/snapshot/imagerefchecker"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/llbsolver/ops"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/source"
	"github.com/moby/buildkit/source/containerimage"
	"github.com/moby/buildkit/source/git"
	"github.com/moby/buildkit/source/http"
	"github.com/moby/buildkit/source/local"
	"github.com/moby/buildkit/util/contentutil"
	"github.com/moby/buildkit/util/leaseutil"
	"github.com/moby/buildkit/util/progress"
	"github.com/moby/buildkit/util/resolver"
	"github.com/moby/buildkit/worker"
	digest "github.com/opencontainers/go-digest"
	ociidentity "github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	bolt "go.etcd.io/bbolt"
	"golang.org/x/sync/errgroup"
)

const labelCreatedAt = "buildkit/createdat"

// TODO: this file should be removed. containerd defines ContainerdWorker, oci defines OCIWorker. There is no base worker.

// WorkerOpt is specific to a worker.
// See also CommonOpt.
type WorkerOpt struct {
	ID                 string
	Labels             map[string]string
	Platforms          []specs.Platform
	GCPolicy           []client.PruneInfo
	MetadataStore      *metadata.Store
	Executor           executor.Executor
	Snapshotter        snapshot.Snapshotter
	ContentStore       content.Store
	Applier            diff.Applier
	Differ             diff.Comparer
	ImageStore         images.Store // optional
	ResolveOptionsFunc resolver.ResolveOptionsFunc
	IdentityMapping    *idtools.IdentityMapping
	LeaseManager       leases.Manager
}

// Worker is a local worker instance with dedicated snapshotter, cache, and so on.
// TODO: s/Worker/OpWorker/g ?
type Worker struct {
	WorkerOpt
	CacheManager  cache.Manager
	SourceManager *source.Manager
	imageWriter   *imageexporter.ImageWriter
	ImageSource   source.Source
}

// NewWorker instantiates a local worker
func NewWorker(opt WorkerOpt) (*Worker, error) {
	imageRefChecker := imagerefchecker.New(imagerefchecker.Opt{
		ImageStore:   opt.ImageStore,
		Snapshotter:  opt.Snapshotter,
		ContentStore: opt.ContentStore,
	})

	cm, err := cache.NewManager(cache.ManagerOpt{
		Snapshotter:     opt.Snapshotter,
		MetadataStore:   opt.MetadataStore,
		PruneRefChecker: imageRefChecker,
	})
	if err != nil {
		return nil, err
	}

	sm, err := source.NewManager()
	if err != nil {
		return nil, err
	}

	is, err := containerimage.NewSource(containerimage.SourceOpt{
		Snapshotter:   opt.Snapshotter,
		ContentStore:  opt.ContentStore,
		Applier:       opt.Applier,
		ImageStore:    opt.ImageStore,
		CacheAccessor: cm,
		ResolverOpt:   opt.ResolveOptionsFunc,
		LeaseManager:  opt.LeaseManager,
	})
	if err != nil {
		return nil, err
	}

	sm.Register(is)

	if err := git.Supported(); err == nil {
		gs, err := git.NewSource(git.Opt{
			CacheAccessor: cm,
			MetadataStore: opt.MetadataStore,
		})
		if err != nil {
			return nil, err
		}
		sm.Register(gs)
	} else {
		logrus.Warnf("git source cannot be enabled: %v", err)
	}

	hs, err := http.NewSource(http.Opt{
		CacheAccessor: cm,
		MetadataStore: opt.MetadataStore,
	})
	if err != nil {
		return nil, err
	}

	sm.Register(hs)

	ss, err := local.NewSource(local.Opt{
		CacheAccessor: cm,
		MetadataStore: opt.MetadataStore,
	})
	if err != nil {
		return nil, err
	}
	sm.Register(ss)

	iw, err := imageexporter.NewImageWriter(imageexporter.WriterOpt{
		Snapshotter:  opt.Snapshotter,
		ContentStore: opt.ContentStore,
		Applier:      opt.Applier,
		Differ:       opt.Differ,
	})
	if err != nil {
		return nil, err
	}

	return &Worker{
		WorkerOpt:     opt,
		CacheManager:  cm,
		SourceManager: sm,
		imageWriter:   iw,
		ImageSource:   is,
	}, nil
}

func (w *Worker) ContentStore() content.Store {
	return w.WorkerOpt.ContentStore
}

func (w *Worker) ID() string {
	return w.WorkerOpt.ID
}

func (w *Worker) Labels() map[string]string {
	return w.WorkerOpt.Labels
}

func (w *Worker) Platforms() []specs.Platform {
	return w.WorkerOpt.Platforms
}

func (w *Worker) GCPolicy() []client.PruneInfo {
	return w.WorkerOpt.GCPolicy
}

func (w *Worker) LoadRef(id string, hidden bool) (cache.ImmutableRef, error) {
	var opts []cache.RefOption
	if hidden {
		opts = append(opts, cache.NoUpdateLastUsed)
	}
	return w.CacheManager.Get(context.TODO(), id, opts...)
}

func (w *Worker) ResolveOp(v solver.Vertex, s frontend.FrontendLLBBridge, sm *session.Manager) (solver.Op, error) {
	if baseOp, ok := v.Sys().(*pb.Op); ok {
		switch op := baseOp.Op.(type) {
		case *pb.Op_Source:
			return ops.NewSourceOp(v, op, baseOp.Platform, w.SourceManager, sm, w)
		case *pb.Op_Exec:
			return ops.NewExecOp(v, op, baseOp.Platform, w.CacheManager, sm, w.MetadataStore, w.Executor, w)
		case *pb.Op_File:
			return ops.NewFileOp(v, op, w.CacheManager, w.MetadataStore, w)
		case *pb.Op_Build:
			return ops.NewBuildOp(v, op, s, w)
		default:
			return nil, errors.Errorf("no support for %T", op)
		}
	}
	return nil, errors.Errorf("could not resolve %v", v)
}

func (w *Worker) PruneCacheMounts(ctx context.Context, ids []string) error {
	mu := ops.CacheMountsLocker()
	mu.Lock()
	defer mu.Unlock()

	for _, id := range ids {
		id = "cache-dir:" + id
		sis, err := w.MetadataStore.Search(id)
		if err != nil {
			return err
		}
		for _, si := range sis {
			for _, k := range si.Indexes() {
				if k == id || strings.HasPrefix(k, id+":") {
					if siCached := w.CacheManager.Metadata(si.ID()); siCached != nil {
						si = siCached
					}
					if err := cache.CachePolicyDefault(si); err != nil {
						return err
					}
					si.Queue(func(b *bolt.Bucket) error {
						return si.SetValue(b, k, nil)
					})
					if err := si.Commit(); err != nil {
						return err
					}
					// if ref is unused try to clean it up right away by releasing it
					if mref, err := w.CacheManager.GetMutable(ctx, si.ID()); err == nil {
						go mref.Release(context.TODO())
					}
					break
				}
			}
		}
	}

	ops.ClearActiveCacheMounts()
	return nil
}

func (w *Worker) ResolveImageConfig(ctx context.Context, ref string, opt gw.ResolveImageConfigOpt, sm *session.Manager) (digest.Digest, []byte, error) {
	// ImageSource is typically source/containerimage
	resolveImageConfig, ok := w.ImageSource.(resolveImageConfig)
	if !ok {
		return "", nil, errors.Errorf("worker %q does not implement ResolveImageConfig", w.ID())
	}
	return resolveImageConfig.ResolveImageConfig(ctx, ref, opt, sm)
}

type resolveImageConfig interface {
	ResolveImageConfig(ctx context.Context, ref string, opt gw.ResolveImageConfigOpt, sm *session.Manager) (digest.Digest, []byte, error)
}

func (w *Worker) Exec(ctx context.Context, meta executor.Meta, rootFS cache.ImmutableRef, stdin io.ReadCloser, stdout, stderr io.WriteCloser) error {
	active, err := w.CacheManager.New(ctx, rootFS)
	if err != nil {
		return err
	}
	defer active.Release(context.TODO())
	return w.Executor.Exec(ctx, meta, active, nil, stdin, stdout, stderr)
}

func (w *Worker) DiskUsage(ctx context.Context, opt client.DiskUsageInfo) ([]*client.UsageInfo, error) {
	return w.CacheManager.DiskUsage(ctx, opt)
}

func (w *Worker) Prune(ctx context.Context, ch chan client.UsageInfo, opt ...client.PruneInfo) error {
	return w.CacheManager.Prune(ctx, ch, opt...)
}

func (w *Worker) Exporter(name string, sm *session.Manager) (exporter.Exporter, error) {
	switch name {
	case client.ExporterImage:
		return imageexporter.New(imageexporter.Opt{
			Images:         w.ImageStore,
			SessionManager: sm,
			ImageWriter:    w.imageWriter,
			ResolverOpt:    w.ResolveOptionsFunc,
			LeaseManager:   w.LeaseManager,
		})
	case client.ExporterLocal:
		return localexporter.New(localexporter.Opt{
			SessionManager: sm,
		})
	case client.ExporterTar:
		return tarexporter.New(tarexporter.Opt{
			SessionManager: sm,
		})
	case client.ExporterOCI:
		return ociexporter.New(ociexporter.Opt{
			SessionManager: sm,
			ImageWriter:    w.imageWriter,
			Variant:        ociexporter.VariantOCI,
			LeaseManager:   w.LeaseManager,
		})
	case client.ExporterDocker:
		return ociexporter.New(ociexporter.Opt{
			SessionManager: sm,
			ImageWriter:    w.imageWriter,
			Variant:        ociexporter.VariantDocker,
			LeaseManager:   w.LeaseManager,
		})
	default:
		return nil, errors.Errorf("exporter %q could not be found", name)
	}
}

func (w *Worker) GetRemote(ctx context.Context, ref cache.ImmutableRef, createIfNeeded bool) (*solver.Remote, error) {
	diffPairs, err := blobs.GetDiffPairs(ctx, w.ContentStore(), w.Snapshotter, w.Differ, ref, createIfNeeded)
	if err != nil {
		return nil, errors.Wrap(err, "failed calculating diff pairs for exported snapshot")
	}
	if len(diffPairs) == 0 {
		return nil, nil
	}

	createdTimes := getCreatedTimes(ref)
	if len(createdTimes) != len(diffPairs) {
		return nil, errors.Errorf("invalid createdtimes/diffpairs")
	}

	descs := make([]ocispec.Descriptor, len(diffPairs))

	for i, dp := range diffPairs {
		info, err := w.ContentStore().Info(ctx, dp.Blobsum)
		if err != nil {
			return nil, err
		}

		tm, err := createdTimes[i].MarshalText()
		if err != nil {
			return nil, err
		}

		descs[i] = ocispec.Descriptor{
			Digest:    dp.Blobsum,
			Size:      info.Size,
			MediaType: images.MediaTypeDockerSchema2LayerGzip,
			Annotations: map[string]string{
				"containerd.io/uncompressed": dp.DiffID.String(),
				labelCreatedAt:               string(tm),
			},
		}
	}

	return &solver.Remote{
		Descriptors: descs,
		Provider:    w.ContentStore(),
	}, nil
}

func getCreatedTimes(ref cache.ImmutableRef) (out []time.Time) {
	parent := ref.Parent()
	if parent != nil {
		defer parent.Release(context.TODO())
		out = getCreatedTimes(parent)
	}
	return append(out, cache.GetCreatedAt(ref.Metadata()))
}

func (w *Worker) FromRemote(ctx context.Context, remote *solver.Remote) (cache.ImmutableRef, error) {
	ctx, done, err := leaseutil.WithLease(ctx, w.LeaseManager)
	if err != nil {
		return nil, err
	}
	defer done(ctx)

	eg, gctx := errgroup.WithContext(ctx)
	for _, desc := range remote.Descriptors {
		func(desc ocispec.Descriptor) {
			eg.Go(func() error {
				done := oneOffProgress(ctx, fmt.Sprintf("pulling %s", desc.Digest))
				if err := contentutil.Copy(gctx, w.ContentStore(), remote.Provider, desc); err != nil {
					return done(err)
				}
				if ref, ok := desc.Annotations["containerd.io/distribution.source.ref"]; ok {
					hf, err := docker.AppendDistributionSourceLabel(w.ContentStore(), ref)
					if err != nil {
						return done(err)
					}
					_, err = hf(ctx, desc)
					return done(err)
				}
				return done(nil)
			})
		}(desc)
	}

	if err := eg.Wait(); err != nil {
		return nil, err
	}

	cd, release := snapshot.NewContainerdSnapshotter(w.Snapshotter)
	defer release()

	unpackProgressDone := oneOffProgress(ctx, "unpacking")
	chainIDs, refs, err := w.unpack(ctx, w.CacheManager, remote.Descriptors, cd)
	if err != nil {
		return nil, unpackProgressDone(err)
	}
	defer func() {
		for _, ref := range refs {
			ref.Release(context.TODO())
		}
	}()
	unpackProgressDone(nil)

	for i, chainID := range chainIDs {
		tm := time.Now()
		if tmstr, ok := remote.Descriptors[i].Annotations[labelCreatedAt]; ok {
			if err := (&tm).UnmarshalText([]byte(tmstr)); err != nil {
				return nil, err
			}
		}
		descr := fmt.Sprintf("imported %s", remote.Descriptors[i].Digest)
		if v, ok := remote.Descriptors[i].Annotations["buildkit/description"]; ok {
			descr = v
		}
		ref, err := w.CacheManager.Get(ctx, chainID, cache.WithDescription(descr), cache.WithCreationTime(tm))
		if err != nil {
			return nil, err
		}
		if i == len(remote.Descriptors)-1 {
			return ref, nil
		}
		ref.Release(context.TODO())
	}
	return nil, errors.Errorf("unreachable")
}

func (w *Worker) unpack(ctx context.Context, cm cache.Manager, descs []ocispec.Descriptor, s cdsnapshot.Snapshotter) (ids []string, refs []cache.ImmutableRef, err error) {
	defer func() {
		if err != nil {
			for _, r := range refs {
				r.Release(context.TODO())
			}
		}
	}()

	layers, err := getLayers(ctx, descs)
	if err != nil {
		return nil, nil, err
	}

	var chain []digest.Digest
	for _, layer := range layers {
		newChain := append(chain, layer.Diff.Digest)

		chainID := ociidentity.ChainID(newChain)
		ref, err := cm.Get(ctx, string(chainID))
		if err == nil {
			refs = append(refs, ref)
		} else {
			if _, err := rootfs.ApplyLayer(ctx, layer, chain, s, w.Applier); err != nil {
				return nil, nil, err
			}
		}
		chain = newChain

		if err := w.Snapshotter.SetBlob(ctx, string(chainID), layer.Diff.Digest, layer.Blob.Digest); err != nil {
			return nil, nil, err
		}
	}

	ids = make([]string, len(chain))
	for i := range chain {
		ids[i] = string(ociidentity.ChainID(chain[:i+1]))
	}

	return ids, refs, nil
}

// Labels returns default labels
// utility function. could be moved to the constructor logic?
func Labels(executor, snapshotter string) map[string]string {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	labels := map[string]string{
		worker.LabelExecutor:    executor,
		worker.LabelSnapshotter: snapshotter,
		worker.LabelHostname:    hostname,
	}
	return labels
}

// ID reads the worker id from the `workerid` file.
// If not exist, it creates a random one,
func ID(root string) (string, error) {
	f := filepath.Join(root, "workerid")
	b, err := ioutil.ReadFile(f)
	if err != nil {
		if os.IsNotExist(err) {
			id := identity.NewID()
			err := ioutil.WriteFile(f, []byte(id), 0400)
			return id, err
		} else {
			return "", err
		}
	}
	return string(b), nil
}

func getLayers(ctx context.Context, descs []ocispec.Descriptor) ([]rootfs.Layer, error) {
	layers := make([]rootfs.Layer, len(descs))
	for i, desc := range descs {
		diffIDStr := desc.Annotations["containerd.io/uncompressed"]
		if diffIDStr == "" {
			return nil, errors.Errorf("%s missing uncompressed digest", desc.Digest)
		}
		diffID, err := digest.Parse(diffIDStr)
		if err != nil {
			return nil, err
		}
		layers[i].Diff = ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageLayer,
			Digest:    diffID,
		}
		layers[i].Blob = ocispec.Descriptor{
			MediaType: desc.MediaType,
			Digest:    desc.Digest,
			Size:      desc.Size,
		}
	}
	return layers, nil
}

func oneOffProgress(ctx context.Context, id string) func(err error) error {
	pw, _, _ := progress.FromContext(ctx)
	now := time.Now()
	st := progress.Status{
		Started: &now,
	}
	pw.Write(id, st)
	return func(err error) error {
		// TODO: set error on status
		now := time.Now()
		st.Completed = &now
		pw.Write(id, st)
		pw.Close()
		return err
	}
}
