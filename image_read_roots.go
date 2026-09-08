package claudeacp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var errManagedImageRoot = errors.New("image path has no disjoint managed read root")

// managedImageRoots reserves every possible native preparation domain before
// launch. The host keeps those directory identities and their placement stable;
// retained os.Root handles confine reads independently of later symlink changes.
type managedImageRoots struct {
	mu          sync.RWMutex
	domainPaths []string
	handoff     string
	initialized bool
	frozen      bool
	closed      bool
	domains     [][]os.FileInfo
	roots       map[string]managedImageRoot
}

type managedImageRoot struct {
	handle   *os.Root
	resolved string
}

// managedImageAuthority freezes ambient read-root discovery before either
// preparation or a native launch, including helpers and unpublished sessions.
type managedImageAuthority struct {
	HostAuthority
	images *managedImageRoots
}

func (a *managedImageAuthority) PrepareNativeTree(ctx context.Context, path string) error {
	a.images.freeze()

	return a.HostAuthority.PrepareNativeTree(ctx, path)
}

func (a *managedImageAuthority) StartNative(ctx context.Context, request NativeRequest) (NativeProcess, error) {
	a.images.freeze()

	return a.HostAuthority.StartNative(ctx, request)
}

func (r *managedImageRoots) initialize() {
	if r.initialized {
		return
	}

	r.initialized = true

	domains := make([][]os.FileInfo, 0, len(r.domainPaths))
	for _, path := range r.domainPaths {
		if path == "" {
			continue
		}

		_, lineage, err := imageDirectoryLineage(path)
		if err != nil {
			return
		}

		domains = append(domains, lineage)
	}

	r.domains = domains
	r.pin(r.handoff)
}

func (r *managedImageRoots) freeze() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return
	}

	r.initialize()
	r.frozen = true
}

func (r *managedImageRoots) prepare(paths ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.frozen || r.closed {
		return
	}

	r.initialize()

	for _, path := range paths {
		r.pin(path)
	}
}

func (r *managedImageRoots) pin(path string) {
	if path == "" || r.domains == nil {
		return
	}

	if _, exists := r.roots[path]; exists {
		return
	}

	resolved, lineage, err := imageDirectoryLineage(path)
	if err != nil || !lineage[0].IsDir() {
		return
	}

	for _, domain := range r.domains {
		if imageLineageContains(lineage, domain[0]) || imageLineageContains(domain, lineage[0]) {
			return
		}
	}

	handle, err := os.OpenRoot(resolved)
	if err != nil {
		return
	}

	info, err := handle.Stat(".")
	if err != nil || !os.SameFile(info, lineage[0]) {
		_ = handle.Close()

		return
	}

	if r.roots == nil {
		r.roots = make(map[string]managedImageRoot)
	}

	r.roots[path] = managedImageRoot{handle: handle, resolved: resolved}
}

func imageDirectoryLineage(path string) (string, []os.FileInfo, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, err
	}

	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return "", nil, err
	}

	var lineage []os.FileInfo

	for path := absolute; ; path = filepath.Dir(path) {
		info, err := os.Stat(path)
		if err != nil {
			return "", nil, err
		}

		lineage = append(lineage, info)
		if filepath.Dir(path) == path {
			return absolute, lineage, nil
		}
	}
}

func imageLineageContains(lineage []os.FileInfo, directory os.FileInfo) bool {
	for _, ancestor := range lineage {
		if os.SameFile(ancestor, directory) {
			return true
		}
	}

	return false
}

func managedImageRelativeName(path, original, resolved string) (string, bool) {
	for _, root := range []string{original, resolved} {
		name, err := filepath.Rel(root, path)
		if err == nil && name != parentDirSegment && !strings.HasPrefix(name, parentDirSegment+string(filepath.Separator)) {
			return name, true
		}
	}

	return "", false
}

func (r *managedImageRoots) open(path string, allowed []string) (openedHandoffFile, error) {
	r.mu.RLock()

	file, err := r.openLocked(path, allowed)
	if err != nil {
		r.mu.RUnlock()

		return nil, err
	}

	return &managedImageFile{openedHandoffFile: file, release: r.mu.RUnlock}, nil
}

func (r *managedImageRoots) openLocked(path string, allowed []string) (openedHandoffFile, error) {
	for _, requested := range allowed {
		if root, ok := r.roots[requested]; ok {
			if name, within := managedImageRelativeName(path, requested, root.resolved); within {
				return openHandoffFile(root.handle, name)
			}
		}

		for original, parent := range r.roots {
			rootName, within := managedImageRelativeName(requested, original, parent.resolved)
			if !within {
				continue
			}

			name, within := managedImageRelativeName(path, requested, requested)
			if !within {
				continue
			}

			root, err := parent.handle.OpenRoot(rootName)
			if err != nil {
				return nil, err
			}

			file, err := openHandoffFile(root, name)
			_ = root.Close()

			return file, err
		}
	}

	return nil, errManagedImageRoot
}

type managedImageFile struct {
	openedHandoffFile
	release func()
	once    sync.Once
}

func (f *managedImageFile) Close() error {
	err := f.openedHandoffFile.Close()
	f.once.Do(f.release)

	return err
}

func (r *managedImageRoots) close() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closed = true
	for path, root := range r.roots {
		_ = root.handle.Close()

		delete(r.roots, path)
	}
}
