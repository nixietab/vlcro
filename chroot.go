package main

import (
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// mount tracker

type MountTracker struct {
	mounts []string
	mu     sync.Mutex
}

func (mt *MountTracker) Add(path string) {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	mt.mounts = append(mt.mounts, path)
}

func (mt *MountTracker) GetAll() []string {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	result := make([]string, len(mt.mounts))
	copy(result, mt.mounts)
	sort.SliceStable(result, func(i, j int) bool {
		return len(result[i]) > len(result[j])
	})
	return result
}

func (mt *MountTracker) Reset() {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	mt.mounts = nil
}

// mount helpers

func findActiveMountCount(path string) int {
	if path == "" {
		return 0
	}
	cmd := exec.Command("findmnt", "-R", "-n", "-o", "TARGET", path)
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return 0
	}
	return len(lines)
}

func hasActiveMounts(path string) bool {
	return findActiveMountCount(path) > 0
}

func retryMount(source, target, fstype string, flags uintptr, data string, maxRetries int) error {
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(retryDelay)
		}
		err := syscall.Mount(source, target, fstype, flags, data)
		if err == nil {
			debugf("Mounted %s -> %s", source, target)
			return nil
		}
		lastErr = err
		debugf("Mount attempt %d failed for %s: %v", attempt+1, target, err)
	}
	return fmt.Errorf("mount %s failed after %d attempts: %w", target, maxRetries, lastErr)
}

func retryUnmount(target string, flags int, maxRetries int) error {
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(retryDelay)
		}
		err := syscall.Unmount(target, flags)
		if err == nil || err == syscall.EINVAL || err == syscall.ENOENT {
			return nil
		}
		lastErr = err
		debugf("Unmount attempt %d failed for %s: %v", attempt+1, target, err)
	}
	return fmt.Errorf("unmount %s failed after %d attempts: %w", target, maxRetries, lastErr)
}

// chroot setup

func setupChroot(tmpDir, uuid string, needDev bool, tracker *MountTracker) error {
	base := filepath.Join(tmpDir, uuid)
	rootfs := filepath.Join(base, "rootfs")

	dirs := []string{
		rootfs,
		filepath.Join(base, "run"),
		filepath.Join(base, "var", "cache", "zypp"),
		filepath.Join(base, "var", "lib", "ca-certificates"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	resolvPath := "/etc/resolv.conf"
	if linkTarget, err := os.Readlink(resolvPath); err == nil {
		if !filepath.IsAbs(linkTarget) {
			linkTarget = filepath.Join("/etc", linkTarget)
		}
		target := filepath.Join(base, linkTarget)
		os.MkdirAll(filepath.Dir(target), 0755)
		if data, err := os.ReadFile(linkTarget); err == nil {
			os.WriteFile(target, data, 0644)
		}
	} else if data, err := os.ReadFile(resolvPath); err == nil {
		target := filepath.Join(base, resolvPath)
		os.MkdirAll(filepath.Dir(target), 0755)
		os.WriteFile(target, data, 0644)
	}

	if err := retryMount("/", rootfs, "", syscall.MS_BIND|syscall.MS_RDONLY, "", maxRetries); err != nil {
		return fmt.Errorf("mount rootfs: %w", err)
	}
	tracker.Add(rootfs)

	if needDev {
		devPath := filepath.Join(rootfs, "dev")
		if err := retryMount("/dev", devPath, "", syscall.MS_BIND, "", maxRetries); err != nil {
			return fmt.Errorf("mount /dev bind: %w", err)
		}
		tracker.Add(devPath)

		tmpPath := filepath.Join(rootfs, "tmp")
		if err := retryMount("tmpfs", tmpPath, "tmpfs", 0, "", maxRetries); err != nil {
			return fmt.Errorf("mount tmpfs: %w", err)
		}
		tracker.Add(tmpPath)
	}

	runPath := filepath.Join(rootfs, "run")
	if err := retryMount(filepath.Join(base, "run"), runPath, "", syscall.MS_BIND, "", maxRetries); err != nil {
		return fmt.Errorf("mount run: %w", err)
	}
	tracker.Add(runPath)

	varPath := filepath.Join(rootfs, "var")
	if err := retryMount(filepath.Join(base, "var"), varPath, "", syscall.MS_BIND, "", maxRetries); err != nil {
		return fmt.Errorf("mount var: %w", err)
	}
	tracker.Add(varPath)

	cachePath := filepath.Join(rootfs, "var", "cache", "zypp")
	if err := retryMount("/var/cache/zypp", cachePath, "", syscall.MS_BIND, "", maxRetries); err != nil {
		debugf("Warning: failed to mount cache (non-critical): %v", err)
	} else {
		tracker.Add(cachePath)
	}

	certPath := filepath.Join(rootfs, "var", "lib", "ca-certificates")
	if err := retryMount("/var/lib/ca-certificates", certPath, "", syscall.MS_BIND|syscall.MS_RDONLY, "", maxRetries); err != nil {
		debugf("Warning: failed to mount ca-certs (non-critical): %v", err)
	} else {
		tracker.Add(certPath)
	}

	return nil
}

// unmount

func unmountAll(tmpDir string, tracker *MountTracker) error {
	if tmpDir == "" {
		return nil
	}

	mounts := tracker.GetAll()
	var errs []string

	for _, mountPath := range mounts {
		cmd := exec.Command("findmnt", "-n", "-o", "TARGET", mountPath)
		if err := cmd.Run(); err != nil {
			continue
		}

		if err := retryUnmount(mountPath, syscall.MNT_DETACH, maxRetries); err != nil {
			debugf("Failed to unmount %s: %v", mountPath, err)
			if err := syscall.Unmount(mountPath, syscall.MNT_DETACH|syscall.MNT_FORCE); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", mountPath, err))
			}
		}
	}

	if len(errs) > 0 {
		warningf("Some unmounts failed: %s", strings.Join(errs, "; "))
	}
	return nil
}

// cleanup

func vlcroCleanup(tmpDir string, tracker *MountTracker) {
	releaseZyppLock()

	if tmpDir == "" {
		return
	}

	if _, err := os.Stat(tmpDir); os.IsNotExist(err) {
		return
	}

	absPath, err := filepath.Abs(tmpDir)
	if err != nil || !strings.HasPrefix(absPath, "/tmp/vlcro_") {
		errorf("Refusing to cleanup unsafe temp directory: %s", tmpDir)
		return
	}

	infof("%s", colorText(colorInfo, "Cleaning up temp mounts..."))
	unmountAll(tmpDir, tracker)

	time.Sleep(500 * time.Millisecond)

	for attempt := 0; attempt < 5; attempt++ {
		if !hasActiveMounts(tmpDir) {
			break
		}
		warningf("Active mounts still detected under %s, retrying unmount...", tmpDir)
		unmountAll(tmpDir, tracker)
		time.Sleep(500 * time.Millisecond)
	}

	if hasActiveMounts(tmpDir) {
		warningf("Could not unmount all filesystems under %s. Skipping temp directory removal.", tmpDir)
		return
	}

	infof("%s", colorText(colorInfo, "Cleaning up temp directory..."))
	for attempt := 0; attempt < 3; attempt++ {
		if err := os.RemoveAll(tmpDir); err == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}

	warningf("Could not fully remove temp directory %s (will clean up at next boot)", tmpDir)
}

// uuid

func randomUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
