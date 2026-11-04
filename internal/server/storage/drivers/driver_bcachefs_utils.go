package drivers

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/lxc/incus/v7/shared/subprocess"
	"github.com/lxc/incus/v7/shared/util"
)

// parseBcachefsVersion extracts the version number from the output of `bcachefs version`.
// Unlike btrfs-progs the output has no prefix, it is just the bare version (e.g. "1.39.6").
func parseBcachefsVersion(out string) string {
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		fields := strings.Fields(line)

		return strings.TrimPrefix(fields[len(fields)-1], "v")
	}

	return ""
}

// withoutGetVolID returns a copy of this struct but with a volIDFunc which will cause quotas to be skipped.
func (d *bcachefs) withoutGetVolID() Driver {
	newDriver := &bcachefs{}
	getVolID := func(volType VolumeType, volName string) (int64, error) { return volIDQuotaSkip, nil }
	newDriver.init(d.state, d.name, d.config, d.logger, getVolID, d.commonRules)
	_ = newDriver.load()

	return newDriver
}

// getMountOptions returns the mount options to use when mounting the pool.
func (d *bcachefs) getMountOptions() string {
	return d.config["bcachefs.mount_options"]
}

// optimizedImages returns whether images are cached as volumes and instances created from them as
// COW copies. Defaults to true. A bcachefs COW copy is a snapshot subvolume that shares its project
// quota with the volume it was copied from, so instances created this way don't get a size limit of
// their own (see "bcachefs.optimized_images").
func (d *bcachefs) optimizedImages() bool {
	return !util.IsFalse(d.config["bcachefs.optimized_images"])
}

// getMkfsOptions returns the options to pass to mkfs.bcachefs when creating the pool.
// Project quotas must be enabled at format time as they cannot be turned on later, and
// they are required for volume size limits. Any user supplied create options are appended.
func (d *bcachefs) getMkfsOptions() *mkfsOptions {
	extraArgs := "--prjquota"
	if d.config["bcachefs.create_options"] != "" {
		extraArgs = fmt.Sprintf("%s %s", extraArgs, d.config["bcachefs.create_options"])
	}

	return &mkfsOptions{
		Label:     d.name,
		ExtraArgs: extraArgs,
	}
}

// pathSubvol returns the ID of the bcachefs subvolume that the given path belongs to.
func (d *bcachefs) pathSubvol(path string) (uint64, error) {
	stat := unix.Statx_t{}

	err := unix.Statx(unix.AT_FDCWD, path, 0, unix.STATX_SUBVOL, &stat)
	if err != nil {
		return 0, fmt.Errorf("Failed getting subvolume of %q: %w", path, err)
	}

	if stat.Mask&unix.STATX_SUBVOL == 0 {
		return 0, fmt.Errorf("Path %q is not on a bcachefs filesystem", path)
	}

	return stat.Subvol, nil
}

// isSubvolume returns true if the given path is the root of a bcachefs subvolume.
// Mount points of other filesystems count too as their subvolume IDs can collide with the parent's.
func (d *bcachefs) isSubvolume(path string) bool {
	subvol, err := d.pathSubvol(path)
	if err != nil {
		return false
	}

	pathStat := unix.Stat_t{}
	err = unix.Stat(path, &pathStat)
	if err != nil {
		return false
	}

	parentStat := unix.Stat_t{}
	err = unix.Stat(filepath.Dir(path), &parentStat)
	if err != nil {
		return false
	}

	// A different device means the path is the root of its own filesystem.
	if pathStat.Dev != parentStat.Dev {
		return true
	}

	parentSubvol, err := d.pathSubvol(filepath.Dir(path))
	if err != nil {
		return false
	}

	return subvol != parentSubvol
}

// isRootOfRootFilesystem returns true if the given path is the root of the daemon's filesystem.
// Deleting a pool wipes its source, so such a path must never be used as one.
func isRootOfRootFilesystem(path string) bool {
	pathStat := unix.Stat_t{}
	err := unix.Stat(path, &pathStat)
	if err != nil {
		return false
	}

	rootStat := unix.Stat_t{}
	err = unix.Stat("/", &rootStat)
	if err != nil {
		return false
	}

	return pathStat.Dev == rootStat.Dev && pathStat.Ino == rootStat.Ino
}

// getSubvolumes returns the paths of any subvolumes inside the given path, relative to it.
// The tree is walked rather than parsing `bcachefs subvolume list` (its paths are relative).
func (d *bcachefs) getSubvolumes(path string) ([]string, error) {
	var result []string

	root := strings.TrimRight(path, "/")

	err := filepath.Walk(root, func(fpath string, entry fs.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Ignore the base path itself and any non-directories.
		if fpath == root || !entry.IsDir() {
			return nil
		}

		if d.isSubvolume(fpath) {
			result = append(result, strings.TrimPrefix(fpath, root+"/"))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// subvolumeHasChildren returns whether any of the supplied (relative) subvolume paths lies below
// the supplied path. An empty path refers to the root of the tree being walked.
func subvolumeHasChildren(subVols []string, relPath string) bool {
	if relPath == "" {
		return len(subVols) > 0
	}

	prefix := relPath + "/"

	for _, subVol := range subVols {
		if strings.HasPrefix(subVol, prefix) {
			return true
		}
	}

	return false
}

// hasSubvolumes returns true if the given path contains any subvolumes.
func (d *bcachefs) hasSubvolumes(path string) (bool, error) {
	subVols, err := d.getSubvolumes(path)
	if err != nil {
		return false, err
	}

	return len(subVols) > 0, nil
}

// deleteSubvolume deletes the subvolume at the given path. If recursion is true then any nested
// subvolumes are deleted first, as a subvolume cannot be removed while it still contains others.
func (d *bcachefs) deleteSubvolume(path string, recursion bool) error {
	destroy := func(p string) error {
		// Temporarily change ownership and mode to help with nesting.
		_ = os.Chmod(p, 0o700)
		_ = os.Chown(p, 0, 0)

		_, err := subprocess.RunCommand("bcachefs", "subvolume", "delete", p)
		return err
	}

	if recursion {
		subVols, err := d.getSubvolumes(path)
		if err != nil {
			return err
		}

		// Delete the deepest subvolumes first.
		sort.Sort(sort.Reverse(sort.StringSlice(subVols)))

		for _, subVol := range subVols {
			subVolPath := filepath.Join(path, subVol)

			err = destroy(subVolPath)
			if err != nil {
				return fmt.Errorf("Failed deleting bcachefs subvolume %q: %w", subVolPath, err)
			}
		}
	}

	err := destroy(path)
	if err != nil {
		return fmt.Errorf("Failed deleting bcachefs subvolume %q: %w", path, err)
	}

	return nil
}
