package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// setupRootFS switches the calling process (already in a new mount
// namespace) into rootfs and mounts the pseudo-filesystems a Linux program
// expects to find: /proc, /dev and /tmp.
func setupRootFS(rootfs string) error {
	// Mount events normally propagate between namespaces that share a
	// mount (systemd makes / "shared"). Make everything private first so
	// nothing we do here leaks back to the host.
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make / private: %w", err)
	}

	// pivot_root requires the new root to be a mount point. Bind-mounting a
	// directory onto itself turns it into one.
	if err := syscall.Mount(rootfs, rootfs, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("bind rootfs: %w", err)
	}

	// pivot_root makes rootfs the new "/" and moves the old root to
	// put_old. Unlike chroot, the old root can then be unmounted entirely,
	// so the container has no path back to the host filesystem.
	putOld := filepath.Join(rootfs, ".oldroot")
	if err := os.MkdirAll(putOld, 0o700); err != nil {
		return err
	}
	if err := syscall.PivotRoot(rootfs, putOld); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}

	// /proc lists processes. Because we are in a new PID namespace, a
	// fresh procfs only shows the container's own processes.
	if err := os.MkdirAll("/proc", 0o555); err != nil {
		return err
	}
	if err := syscall.Mount("proc", "/proc", "proc", syscall.MS_NOSUID|syscall.MS_NOEXEC|syscall.MS_NODEV, ""); err != nil {
		return fmt.Errorf("mount /proc: %w", err)
	}
	if err := setupDev(); err != nil {
		return err
	}
	if err := os.MkdirAll("/tmp", 0o1777); err != nil {
		return err
	}
	if err := syscall.Mount("tmpfs", "/tmp", "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, "mode=1777"); err != nil {
		return fmt.Errorf("mount /tmp: %w", err)
	}

	// Detach the host's filesystem. MNT_DETACH unmounts lazily, which
	// works even though the old root still has mounts under it.
	if err := syscall.Unmount("/.oldroot", syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount old root: %w", err)
	}
	return os.Remove("/.oldroot")
}

// setupDev mounts a fresh, minimal /dev instead of exposing the host's
// devices (disks, GPUs and so on).
func setupDev() error {
	if err := os.MkdirAll("/dev", 0o755); err != nil {
		return err
	}
	if err := syscall.Mount("tmpfs", "/dev", "tmpfs", syscall.MS_NOSUID|syscall.MS_STRICTATIME, "mode=755,size=65536k"); err != nil {
		return fmt.Errorf("mount /dev: %w", err)
	}
	devices := []struct {
		name         string
		major, minor uint32
	}{
		{"null", 1, 3}, {"zero", 1, 5}, {"full", 1, 7},
		{"random", 1, 8}, {"urandom", 1, 9}, {"tty", 5, 0},
	}
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	for _, d := range devices {
		dev := int(d.major<<8 | d.minor)
		if err := syscall.Mknod("/dev/"+d.name, syscall.S_IFCHR|0o666, dev); err != nil {
			return fmt.Errorf("mknod /dev/%s: %w", d.name, err)
		}
	}
	links := map[string]string{
		"/dev/fd": "/proc/self/fd", "/dev/stdin": "/proc/self/fd/0",
		"/dev/stdout": "/proc/self/fd/1", "/dev/stderr": "/proc/self/fd/2",
	}
	for link, target := range links {
		if err := os.Symlink(target, link); err != nil {
			return err
		}
	}
	return nil
}

// loopbackUp sets the IFF_UP flag on "lo". A new network namespace starts
// with only a loopback interface, and it starts down.
func loopbackUp() error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)

	// struct ifreq: a 16-byte interface name followed by a union, of which
	// we use the 16-bit flags field.
	var ifr struct {
		name  [syscall.IFNAMSIZ]byte
		flags uint16
		_     [22]byte
	}
	copy(ifr.name[:], "lo")
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.SIOCGIFFLAGS, uintptr(unsafe.Pointer(&ifr))); e != 0 {
		return e
	}
	ifr.flags |= syscall.IFF_UP
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.SIOCSIFFLAGS, uintptr(unsafe.Pointer(&ifr))); e != 0 {
		return e
	}
	return nil
}
