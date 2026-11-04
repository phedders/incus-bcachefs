package drivers

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/lxc/incus/v7/internal/linux"
	deviceConfig "github.com/lxc/incus/v7/internal/server/device/config"
	"github.com/lxc/incus/v7/internal/server/operations"
	internalUtil "github.com/lxc/incus/v7/internal/util"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/logger"
	"github.com/lxc/incus/v7/shared/revert"
	"github.com/lxc/incus/v7/shared/subprocess"
	"github.com/lxc/incus/v7/shared/units"
	"github.com/lxc/incus/v7/shared/util"
	"github.com/lxc/incus/v7/shared/validate"
)

var (
	bcachefsVersion string
	bcachefsLoaded  bool
)

type bcachefs struct {
	common
}

// load is used to run one-time action per-driver rather than per-pool.
func (d *bcachefs) load() error {
	// Done if previously loaded.
	if bcachefsLoaded {
		return nil
	}

	// Validate the required binaries.
	for _, tool := range []string{"bcachefs"} {
		_, err := exec.LookPath(tool)
		if err != nil {
			return fmt.Errorf("Required tool %q is missing", tool)
		}
	}

	// Detect and record the version.
	if bcachefsVersion == "" {
		out, err := subprocess.RunCommand("bcachefs", "version")
		if err != nil {
			return err
		}

		bcachefsVersion = parseBcachefsVersion(out)
		if bcachefsVersion == "" {
			return errors.New("The 'bcachefs' tool isn't working properly")
		}
	}

	bcachefsLoaded = true
	return nil
}

// Info returns info about the driver and its environment.
func (d *bcachefs) Info() Info {
	return Info{
		Name:                         "bcachefs",
		Version:                      bcachefsVersion,
		DefaultVMBlockFilesystemSize: deviceConfig.DefaultVMBlockFilesystemSize,
		OptimizedImages:              d.optimizedImages(),
		OptimizedBackups:             false,
		OptimizedBackupHeader:        false,
		PreservesInodes:              !d.state.OS.RunningInUserNS,
		Remote:                       d.isRemote(),
		NearLiveMigration:            true,
		VolumeTypes:                  []VolumeType{VolumeTypeCustom, VolumeTypeImage, VolumeTypeContainer, VolumeTypeVM},
		VolumeMultiNode:              d.isRemote(),
		BlockBacking:                 false,
		RunningCopyFreeze:            false,
		DirectIO:                     true,
		IOUring:                      true,
		MountedRoot:                  true,
		Buckets:                      false,
	}
}

// FillConfig populates the storage pool's configuration file with the default values.
func (d *bcachefs) FillConfig() error {
	loopPath := loopFilePath(d.name)
	if d.config["source"] == "" || d.config["source"] == loopPath {
		// Pick a default size of the loop file if not specified.
		if d.config["size"] == "" {
			defaultSize, err := loopFileSizeDefault()
			if err != nil {
				return err
			}

			d.config["size"] = fmt.Sprintf("%dGiB", defaultSize)
		}
	} else {
		// Unset size property since it's irrelevant.
		d.config["size"] = ""
	}

	return nil
}

// Create is called during pool creation and is effectively using an empty driver struct.
// WARNING: The Create() function cannot rely on any of the struct attributes being set.
func (d *bcachefs) Create() error {
	// Store the provided source as we are likely to be mangling it.
	d.config["volatile.initial_source"] = d.config["source"]

	reverter := revert.New()
	defer reverter.Fail()

	err := d.FillConfig()
	if err != nil {
		return err
	}

	loopPath := loopFilePath(d.name)
	if d.config["source"] == "" || d.config["source"] == loopPath {
		// Check for IncusOS.
		if d.state.OS.IncusOS != nil {
			return errors.New("Loop backed pools aren't supported on IncusOS")
		}

		// Create a loop based pool.
		d.config["source"] = loopPath

		// Create the loop file itself.
		size, err := units.ParseByteSizeString(d.config["size"])
		if err != nil {
			return err
		}

		err = ensureSparseFile(d.config["source"], size)
		if err != nil {
			return fmt.Errorf("Failed to create the sparse file: %w", err)
		}

		reverter.Add(func() { _ = os.Remove(d.config["source"]) })

		// Format the file.
		_, err = makeFSType(d.config["source"], "bcachefs", d.getMkfsOptions())
		if err != nil {
			return fmt.Errorf("Failed to format sparse file: %w", err)
		}
	} else if linux.IsBlockdevPath(d.config["source"]) {
		// Wipe if requested.
		if util.IsTrue(d.config["source.wipe"]) {
			err := wipeBlockHeaders(d.config["source"])
			if err != nil {
				return fmt.Errorf("Failed to wipe headers from disk %q: %w", d.config["source"], err)
			}

			d.config["source.wipe"] = ""
		}

		// Format the block device.
		_, err := makeFSType(d.config["source"], "bcachefs", d.getMkfsOptions())
		if err != nil {
			return fmt.Errorf("Failed to format block device: %w", err)
		}

		// Record the UUID as the source.
		devUUID, err := fsUUID(d.config["source"])
		if err != nil {
			return err
		}

		// Confirm that the symlink is appearing (give it 10s).
		// In case of timeout it falls back to using the volume's path
		// instead of its UUID.
		if tryExists(fmt.Sprintf("/dev/disk/by-uuid/%s", devUUID)) {
			// Override the config to use the UUID.
			d.config["source"] = devUUID
		}
	} else if d.config["source"] != "" {
		hostPath := d.config["source"]
		cleanSource := filepath.Clean(hostPath)
		daemonDir := internalUtil.VarPath()

		// Restrict source paths under the daemon directory to the pool's own mount path.
		// Anything else gets bind-mounted over the daemon directory layout on pool mount,
		// aliasing paths like the image cache into the pool.
		if cleanSource == daemonDir || strings.HasPrefix(cleanSource, daemonDir+"/") {
			if cleanSource != GetPoolMountPath(d.name) {
				return fmt.Errorf("Only allowed source path under %q is %q", internalUtil.VarPath(), GetPoolMountPath(d.name))
			}
		}

		// Refuse the root of the filesystem the daemon runs on: a pool must own its source,
		// which gets wiped when the pool is deleted.
		if isRootOfRootFilesystem(hostPath) {
			return fmt.Errorf("Provided path %q is the root of the daemon's filesystem", hostPath)
		}

		if d.isSubvolume(hostPath) {
			// Existing bcachefs subvolume.
			hasSubvolumes, err := d.hasSubvolumes(hostPath)
			if err != nil {
				return fmt.Errorf("Could not determine if existing bcachefs subvolume is empty: %w", err)
			}

			// Check that the provided subvolume is empty.
			if hasSubvolumes {
				return errors.New("Requested bcachefs subvolume exists but is not empty")
			}
		} else {
			// New bcachefs subvolume on existing bcachefs filesystem.
			pathExists := util.PathExists(hostPath)
			if pathExists {
				hostPathFS, _ := linux.DetectFilesystem(hostPath)
				if hostPathFS != "bcachefs" {
					return fmt.Errorf("Provided path does not reside on a bcachefs filesystem (detected %s)", hostPathFS)
				}

				if cleanSource == GetPoolMountPath(d.name) {
					storagePoolDirFS, _ := linux.DetectFilesystem(internalUtil.VarPath("storage-pools"))
					if storagePoolDirFS != "bcachefs" {
						return fmt.Errorf("Provided path does not reside on a bcachefs filesystem (detected %s)", storagePoolDirFS)
					}
				}

				// bcachefs can't promote a directory in place, so replace the empty directory
				// by a subvolume (required for COW copies and snapshots).
				hostPathInfo, err := os.Lstat(cleanSource)
				if err != nil {
					return fmt.Errorf("Failed to stat %q: %w", cleanSource, err)
				}

				if !hostPathInfo.IsDir() {
					return fmt.Errorf("Provided path %q isn't a directory", cleanSource)
				}

				isEmpty, err := internalUtil.PathIsEmpty(cleanSource)
				if err != nil {
					return fmt.Errorf("Failed to check whether %q is empty: %w", cleanSource, err)
				}

				if !isEmpty {
					return fmt.Errorf("Provided path %q exists and isn't an empty directory (use a non-existing path, or an existing empty directory or subvolume, on a bcachefs filesystem)", cleanSource)
				}

				// Delete the current directory to replace it by a subvolume.
				err = os.Remove(cleanSource)
				if err != nil {
					return fmt.Errorf("Failed to remove %q: %w", cleanSource, err)
				}
			}

			// Create the subvolume.
			_, err := subprocess.RunCommand("bcachefs", "subvolume", "create", hostPath)
			if err != nil {
				return err
			}
		}
	} else {
		return errors.New(`Invalid "source" property`)
	}

	reverter.Success()
	return nil
}

// Delete removes the storage pool from the storage device.
func (d *bcachefs) Delete(op *operations.Operation) error {
	// Deleting a pool wipes its source, so never proceed if it is the daemon's own root.
	if isRootOfRootFilesystem(d.config["source"]) {
		return fmt.Errorf("Refusing to delete storage pool %q with source %q", d.name, d.config["source"])
	}

	// If the user completely destroyed it, call it done.
	if !util.PathExists(GetPoolMountPath(d.name)) {
		return nil
	}

	// Delete potential intermediate bcachefs subvolumes.
	for _, volType := range d.Info().VolumeTypes {
		for _, dir := range BaseDirectories[volType].Paths {
			path := filepath.Join(GetPoolMountPath(d.name), dir)
			if !util.PathExists(path) {
				continue
			}

			if !d.isSubvolume(path) {
				continue
			}

			err := d.deleteSubvolume(path, true)
			if err != nil {
				return fmt.Errorf("Failed deleting bcachefs subvolume %q: %w", path, err)
			}
		}
	}

	// On delete, wipe everything in the directory.
	mountPath := GetPoolMountPath(d.name)
	err := wipeDirectory(mountPath)
	if err != nil {
		return fmt.Errorf("Failed removing mount path %q: %w", mountPath, err)
	}

	// Unmount the path.
	_, err = d.Unmount()
	if err != nil {
		return err
	}

	// If the pool path is a subvolume itself, delete it.
	if d.isSubvolume(mountPath) {
		err := d.deleteSubvolume(mountPath, false)
		if err != nil {
			return err
		}

		// And re-create as an empty directory to make the backend happy.
		err = os.Mkdir(mountPath, 0o700)
		if err != nil {
			return fmt.Errorf("Failed creating directory %q: %w", mountPath, err)
		}
	}

	// Delete any loop file we may have used.
	loopPath := loopFilePath(d.name)
	err = os.Remove(loopPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("Failed removing loop file %q: %w", loopPath, err)
	}

	return nil
}

// Validate checks that all provide keys are supported and that no conflicting or missing configuration is present.
func (d *bcachefs) Validate(config map[string]string) error {
	// gendoc:generate(entity=storage_bcachefs, group=common, key=source)
	//
	// ---
	//  type: string
	//  scope: local
	//  default: -
	//  shortdesc: Path to an existing block device, loop file, directory or bcachefs file system to use for the pool

	// gendoc:generate(entity=storage_bcachefs, group=common, key=source.wipe)
	//
	// ---
	//  type: bool
	//  scope: local
	//  default: `false`
	//  shortdesc: Wipe the block device specified in `source` prior to creating the storage pool

	rules := map[string]func(value string) error{
		// gendoc:generate(entity=storage_bcachefs, group=common, key=size)
		//
		// ---
		//  type: string
		//  scope: local
		//  default: auto (20% of free disk space, >= 5 GiB and <= 30 GiB)
		//  shortdesc: Size of the storage pool when creating loop-based pools (in bytes, suffixes supported, can be increased to grow storage pool)
		"size": validate.Optional(validate.IsSize),

		// gendoc:generate(entity=storage_bcachefs, group=common, key=bcachefs.mount_options)
		//
		// ---
		//  type: string
		//  scope: global
		//  default: -
		//  shortdesc: Mount options for block devices
		"bcachefs.mount_options": validate.IsAny,

		// gendoc:generate(entity=storage_bcachefs, group=common, key=bcachefs.create_options)
		//
		// ---
		//  type: string
		//  scope: global
		//  default: -
		//  shortdesc: Additional options to pass to `mkfs.bcachefs` when creating the pool
		"bcachefs.create_options": validate.IsAny,

		// gendoc:generate(entity=storage_bcachefs, group=common, key=bcachefs.optimized_images)
		// A bcachefs COW copy of a volume shares its project quota, so setting this to `false`
		// unpacks images into each new instance volume instead, which is slower but lets instance
		// `size` limits be enforced. An instance whose volume config differs from the pool's
		// defaults (for example one that sets `size`) is never created from a cached image.
		// ---
		//  type: bool
		//  scope: global
		//  default: `true`
		//  shortdesc: Cache images as volumes and create instances from them using COW copies
		"bcachefs.optimized_images": validate.Optional(validate.IsBool),
	}

	return d.validatePool(config, rules, d.commonVolumeRules())
}

// Update applies any driver changes required from a configuration change.
func (d *bcachefs) Update(changedConfig map[string]string) error {
	// We only care about bcachefs.mount_options.
	val, ok := changedConfig["bcachefs.mount_options"]
	if ok {
		// Custom mount options don't work inside containers.
		if d.state.OS.RunningInUserNS {
			return nil
		}

		// Trigger a re-mount.
		d.config["bcachefs.mount_options"] = val
		mntFlags, mntOptions := linux.ResolveMountOptions(strings.Split(d.getMountOptions(), ","))
		mntFlags |= unix.MS_REMOUNT

		err := TryMount("", GetPoolMountPath(d.name), "none", mntFlags, mntOptions)
		if err != nil {
			return err
		}
	}

	size, ok := changedConfig["size"]
	if ok {
		// Figure out loop path.
		loopPath := loopFilePath(d.name)

		if d.config["source"] != loopPath {
			return errors.New("Cannot resize non-loopback pools")
		}

		// Resize loop file.
		f, err := os.OpenFile(loopPath, os.O_RDWR, 0o600)
		if err != nil {
			return err
		}

		defer logger.WarnOnError(f.Close, "Failed to close file")

		sizeBytes, _ := units.ParseByteSizeString(size)

		err = f.Truncate(sizeBytes)
		if err != nil {
			return err
		}

		loopDevPath, err := loopDeviceSetup(loopPath)
		if err != nil {
			return err
		}

		defer logger.WarnOnError(func() error { return loopDeviceAutoDetach(loopDevPath) }, "Failed to detach loop device")

		err = loopDeviceSetCapacity(loopDevPath)
		if err != nil {
			return err
		}

		_, err = subprocess.RunCommand("bcachefs", "device", "resize", loopDevPath)
		if err != nil {
			return err
		}
	}

	return nil
}

// Mount mounts the storage pool.
func (d *bcachefs) Mount() (bool, error) {
	// Check if already mounted.
	if linux.IsMountPoint(GetPoolMountPath(d.name)) {
		return false, nil
	}

	var err error

	// Setup mount options.
	loopPath := loopFilePath(d.name)
	mntSrc := ""
	mntDst := GetPoolMountPath(d.name)
	mntFilesystem := "bcachefs"
	if d.config["source"] == loopPath {
		mntSrc, err = loopDeviceSetup(d.config["source"])
		if err != nil {
			return false, err
		}

		defer logger.WarnOnError(func() error { return loopDeviceAutoDetach(mntSrc) }, "Failed to detach loop device")
	} else if filepath.IsAbs(d.config["source"]) {
		// Bring up an existing device or path.
		mntSrc = d.config["source"]

		if !linux.IsBlockdevPath(mntSrc) {
			mntFilesystem = "none"

			mntSrcFS, _ := linux.DetectFilesystem(mntSrc)
			if mntSrcFS != "bcachefs" {
				return false, fmt.Errorf("Source path %q isn't bcachefs (detected %s)", mntSrc, mntSrcFS)
			}
		}
	} else {
		// Mount using UUID.
		mntSrc = fmt.Sprintf("/dev/disk/by-uuid/%s", d.config["source"])
	}

	// Get the custom mount flags/options.
	mntFlags, mntOptions := linux.ResolveMountOptions(strings.Split(d.getMountOptions(), ","))

	// Handle bind-mounts first.
	if mntFilesystem == "none" {
		// Setup the bind-mount itself.
		err := TryMount(mntSrc, mntDst, mntFilesystem, unix.MS_BIND, "")
		if err != nil {
			return false, err
		}

		// Custom mount options don't work inside containers.
		if d.state.OS.RunningInUserNS {
			return true, nil
		}

		// Now apply the custom options.
		mntFlags |= unix.MS_REMOUNT
		err = TryMount("", mntDst, mntFilesystem, mntFlags, mntOptions)
		if err != nil {
			return false, err
		}

		return true, nil
	}

	// Handle traditional mounts.
	err = TryMount(mntSrc, mntDst, mntFilesystem, mntFlags, mntOptions)
	if err != nil {
		return false, err
	}

	return true, nil
}

// Unmount unmounts the storage pool.
func (d *bcachefs) Unmount() (bool, error) {
	// Unmount the pool.
	ourUnmount, err := forceUnmount(GetPoolMountPath(d.name))
	if err != nil {
		return false, err
	}

	return ourUnmount, nil
}

// GetResources returns the pool resource usage information.
func (d *bcachefs) GetResources() (*api.ResourcesStoragePool, error) {
	return genericVFSGetResources(d)
}
