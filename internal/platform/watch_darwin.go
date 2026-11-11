//go:build darwin

package platform

/*
#cgo LDFLAGS: -framework CoreServices
#include <CoreServices/CoreServices.h>
#include <stdint.h>

extern void viffFSEvent(uintptr_t handle, char *path, uint32_t flags);

static void viffCallback(ConstFSEventStreamRef stream, void *info, size_t count, void *paths,
                         const FSEventStreamEventFlags flags[], const FSEventStreamEventId ids[]) {
	char **list = (char **)paths;
	for (size_t i = 0; i < count; i++) {
		viffFSEvent((uintptr_t)info, list[i], flags[i]);
	}
}

static FSEventStreamRef viffStartWatch(const char **paths, int n, uintptr_t handle, double latency) {
	CFMutableArrayRef array = CFArrayCreateMutable(NULL, n, &kCFTypeArrayCallBacks);
	for (int i = 0; i < n; i++) {
		CFStringRef p = CFStringCreateWithCString(NULL, paths[i], kCFStringEncodingUTF8);
		CFArrayAppendValue(array, p);
		CFRelease(p);
	}
	FSEventStreamContext context = {0, (void *)handle, NULL, NULL, NULL};
	FSEventStreamRef stream = FSEventStreamCreate(NULL, viffCallback, &context, array,
		kFSEventStreamEventIdSinceNow, latency,
		kFSEventStreamCreateFlagFileEvents | kFSEventStreamCreateFlagNoDefer | kFSEventStreamCreateFlagWatchRoot);
	CFRelease(array);
	if (stream == NULL) {
		return NULL;
	}
	dispatch_queue_t queue = dispatch_queue_create("dev.viff.fsevents", DISPATCH_QUEUE_SERIAL);
	FSEventStreamSetDispatchQueue(stream, queue);
	if (!FSEventStreamStart(stream)) {
		FSEventStreamInvalidate(stream);
		FSEventStreamRelease(stream);
		return NULL;
	}
	return stream;
}

static void viffStopWatch(FSEventStreamRef stream) {
	FSEventStreamStop(stream);
	FSEventStreamInvalidate(stream);
	FSEventStreamRelease(stream);
}
*/
import "C"

import (
	"errors"
	"path/filepath"
	"runtime/cgo"
	"strings"
	"sync"
	"time"
	"unsafe"

	"viff/internal/domain"
)

type fsWatch struct {
	repo     domain.Repo
	debounce *debouncer
	stream   C.FSEventStreamRef
	handle   cgo.Handle
	once     sync.Once
	backup   *pollWatcher
}

//export viffFSEvent
func viffFSEvent(handle C.uintptr_t, path *C.char, flags C.uint32_t) {
	w, ok := cgo.Handle(handle).Value().(*fsWatch)
	if !ok {
		return
	}
	const lost = C.kFSEventStreamEventFlagMustScanSubDirs | C.kFSEventStreamEventFlagUserDropped |
		C.kFSEventStreamEventFlagKernelDropped | C.kFSEventStreamEventFlagEventIdsWrapped |
		C.kFSEventStreamEventFlagRootChanged
	if flags&lost != 0 {
		w.debounce.event(true, true)
		return
	}
	p := C.GoString(path)
	if relevant, meta := classify(w.repo, p); relevant {
		w.debounce.event(meta, false)
	}
}

// Watch reports changes in the working tree and the Git directories of repo.
// FSEvents delivers per-file events without one descriptor per file, so
// large trees cost nothing while idle. A slow fingerprint check of the Git
// directory backs it up in case events are dropped.
func Watch(repo domain.Repo, changed func(domain.RepoChange), _ WatchOptions) (Watcher, error) {
	w := &fsWatch{repo: repo, debounce: newDebouncer(250*time.Millisecond, changed)}
	roots := []string{repo.Root}
	for _, d := range []string{repo.MetaDir, repo.CommonDir} {
		if d != "" && !strings.HasPrefix(filepath.Clean(d)+"/", filepath.Clean(repo.Root)+"/") {
			roots = append(roots, d)
		}
	}
	cpaths := make([]*C.char, len(roots))
	for i, r := range roots {
		// FSEvents reports real paths; resolve symlinks so classify matches.
		if real, err := filepath.EvalSymlinks(r); err == nil {
			r = real
		}
		cpaths[i] = C.CString(r)
	}
	defer func() {
		for _, p := range cpaths {
			C.free(unsafe.Pointer(p))
		}
	}()
	if real, err := filepath.EvalSymlinks(repo.MetaDir); err == nil {
		w.repo.MetaDir = real
	}
	if real, err := filepath.EvalSymlinks(repo.CommonDir); err == nil {
		w.repo.CommonDir = real
	}
	w.handle = cgo.NewHandle(w)
	w.stream = C.viffStartWatch((**C.char)(unsafe.Pointer(&cpaths[0])), C.int(len(cpaths)), C.uintptr_t(w.handle), 0.15)
	if w.stream == nil {
		w.handle.Delete()
		return startPolling(repo, 2*time.Second, 3, changed), errors.New("fsevents unavailable; polling")
	}
	w.backup = startPolling(repo, 30*time.Second, 0, changed)
	return w, nil
}

func (w *fsWatch) Close() {
	w.once.Do(func() {
		w.debounce.close()
		C.viffStopWatch(w.stream)
		w.handle.Delete()
		w.backup.Close()
	})
}
