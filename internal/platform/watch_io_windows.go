package platform

import (
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const windowsWatchBuffer = 64 << 10

var errWindowsWatchUnsupported = errors.New("native Windows repository watching is unavailable")

type windowsDir struct {
	path string
	real string
	id   [3]uint64 // FILE_ID_INFO: volume serial number and 128-bit file ID
	file windows.Handle
}

func windowsExtendedPath(path string) string {
	if strings.HasPrefix(path, `\\?\`) {
		return path
	}
	return `\\?\` + filepath.Clean(path)
}

func windowsFixedPath(path string) bool {
	dosPath := strings.TrimPrefix(path, `\\?\`)
	volume := filepath.VolumeName(dosPath)
	if !filepath.IsAbs(dosPath) || len(volume) != 2 || volume[1] != ':' {
		return false
	}
	root, err := windows.UTF16PtrFromString(volume + `\`)
	return err == nil && windows.GetDriveType(root) == windows.DRIVE_FIXED
}

func openWindowsDir(path string, access, flags uint32) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(windowsExtendedPath(path))
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(name, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|flags, 0)
}

func windowsFileID(file windows.Handle) ([3]uint64, error) {
	var id [3]uint64
	err := windows.GetFileInformationByHandleEx(file, windows.FileIdInfo, (*byte)(unsafe.Pointer(&id)), uint32(unsafe.Sizeof(id)))
	return id, err
}

func resolveWindowsDir(path string) (dir windowsDir, err error) {
	file, err := openWindowsDir(path, windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES, windows.FILE_FLAG_OVERLAPPED)
	if err != nil {
		return dir, fmt.Errorf("open watch directory: %w", err)
	}
	defer func() {
		if err != nil {
			windows.CloseHandle(file)
		}
	}()
	var fs [32]uint16
	if err := windows.GetVolumeInformationByHandle(file, nil, 0, nil, nil, nil, &fs[0], uint32(len(fs))); err != nil {
		return dir, fmt.Errorf("read watch volume: %w", err)
	}
	if name := windows.UTF16ToString(fs[:]); name != "NTFS" && name != "ReFS" {
		return dir, errWindowsWatchUnsupported
	}
	// Use the volume GUID rather than a drive letter. Junctions, mount points,
	// SUBST drives and case variants must agree on which roots already overlap.
	name := make([]uint16, 32768)
	const volumeNameGUID = 1
	n, err := windows.GetFinalPathNameByHandle(file, &name[0], uint32(len(name)), volumeNameGUID)
	if err != nil || n >= uint32(len(name)) {
		// UNC targets (including junctions to a remote drive) have no GUID.
		return dir, errWindowsWatchUnsupported
	}
	dir = windowsDir{path: path, real: filepath.Clean(windows.UTF16ToString(name[:n])), file: file}
	dir.id, err = windowsFileID(file)
	if err != nil {
		return dir, fmt.Errorf("read watch directory identity: %w", err)
	}
	return dir, nil
}

func (d windowsDir) stillPresent() bool {
	file, err := openWindowsDir(d.path, windows.FILE_READ_ATTRIBUTES, 0)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(file)
	id, err := windowsFileID(file)
	return err == nil && id == d.id
}

// A single overlapped request owns a pinned buffer and OVERLAPPED until its
// completion. The watcher goroutine alone opens, arms and retires requests.
type windowsWatchRead struct {
	event   windows.Handle
	dir     windowsDir
	op      windows.Overlapped
	buf     [windowsWatchBuffer]byte
	pin     runtime.Pinner
	pending bool
}

func newWindowsWatchRead(dir windowsDir) (*windowsWatchRead, error) {
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return nil, fmt.Errorf("create directory notification event: %w", err)
	}
	r := &windowsWatchRead{event: event, dir: dir}
	r.pin.Pin(&r.op)
	r.pin.Pin(&r.buf[0])
	return r, nil
}

func (r *windowsWatchRead) arm() error {
	r.op = windows.Overlapped{HEvent: r.event}
	if err := windows.ResetEvent(r.event); err != nil {
		return fmt.Errorf("reset directory notification event: %w", err)
	}
	const mask = windows.FILE_NOTIFY_CHANGE_FILE_NAME | windows.FILE_NOTIFY_CHANGE_DIR_NAME |
		windows.FILE_NOTIFY_CHANGE_ATTRIBUTES | windows.FILE_NOTIFY_CHANGE_SIZE |
		windows.FILE_NOTIFY_CHANGE_LAST_WRITE | windows.FILE_NOTIFY_CHANGE_SECURITY
	err := windows.ReadDirectoryChanges(r.dir.file, &r.buf[0], uint32(len(r.buf)), true, mask, nil, &r.op, 0)
	if err != nil && !errors.Is(err, windows.ERROR_IO_PENDING) {
		return fmt.Errorf("read directory notifications: %w", err)
	}
	r.pending = true
	return nil
}

func (r *windowsWatchRead) complete(visit func(string)) (lost bool, err error) {
	var n uint32
	err = windows.GetOverlappedResult(r.dir.file, &r.op, &n, false)
	if !errors.Is(err, windows.ERROR_IO_INCOMPLETE) {
		r.pending = false
	}
	if errors.Is(err, windows.ERROR_NOTIFY_ENUM_DIR) || err == nil && (n == 0 || n > uint32(len(r.buf))) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("complete directory notification: %w", err)
	}
	return !visitWindowsChanges(r.buf[:n], visit), nil
}

func (r *windowsWatchRead) close() {
	if r.pending {
		// CancelIoEx requests cancellation. The completion can still be
		// successful; in either case wait before unpinning or closing.
		_ = windows.CancelIoEx(r.dir.file, &r.op)
		var n uint32
		_ = windows.GetOverlappedResult(r.dir.file, &r.op, &n, true)
	}
	r.pin.Unpin()
	windows.CloseHandle(r.event)
}

// visitWindowsChanges walks a single FILE_NOTIFY_INFORMATION buffer.
// Both names of a rename matter, and a malformed/truncated record is a loss.
func visitWindowsChanges(buf []byte, visit func(string)) bool {
	const header = 12
	for len(buf) >= header {
		offset := binary.LittleEndian.Uint32(buf)
		action := binary.LittleEndian.Uint32(buf[4:])
		size := binary.LittleEndian.Uint32(buf[8:])
		if action < windows.FILE_ACTION_ADDED || action > windows.FILE_ACTION_RENAMED_NEW_NAME ||
			size == 0 || size%2 != 0 || size > uint32(len(buf)-header) {
			return false
		}
		name := make([]uint16, size/2)
		for i := range name {
			name[i] = binary.LittleEndian.Uint16(buf[header+2*i:])
			if name[i] == 0 {
				return false
			}
		}
		path := string(utf16.Decode(name))
		if !filepath.IsLocal(path) || filepath.Clean(path) != path {
			return false
		}
		visit(path)
		if offset == 0 {
			return true
		}
		if offset%4 != 0 || offset < header+size || offset > uint32(len(buf)) {
			return false
		}
		buf = buf[offset:]
	}
	return false
}
