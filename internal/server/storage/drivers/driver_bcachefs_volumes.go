package drivers

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/lxc/incus/v7/internal/instancewriter"
	"github.com/lxc/incus/v7/internal/migration"
	"github.com/lxc/incus/v7/internal/server/backup"
	localMigration "github.com/lxc/incus/v7/internal/server/migration"
	"github.com/lxc/incus/v7/internal/server/operations"
	"github.com/lxc/incus/v7/internal/server/storage/quota"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/logger"
	"github.com/lxc/incus/v7/shared/revert"
	"github.com/lxc/incus/v7/shared/subprocess"
	"github.com/lxc/incus/v7/shared/units"
	"github.com/lxc/incus/v7/shared/util"
)

// CreateVolume creates an empty volume and can optionally fill it by executing the supplied filler function.
func (d *bcachefs) CreateVolume(vol Volume, filler *VolumeFiller, op *operations.Operation) error {
	volPath := vol.MountPath()

	// Setup revert.
	reverter := revert.New()
	defer reverter.Fail()

	// Create the volume subvolume itself.
	_, err := subprocess.RunCommand("bcachefs", "subvolume", "create", volPath)
	if err != nil {
		return err
	}

	reverter.Add(func() {
		_ = d.deleteSubvolume(volPath, false)
		_ = os.Remove(volPath)
	})

	// Set up the project quota for the new volume (usage tracking only for block volumes).
	revertQuota, err := d.setupInitialQuota(vol)
	if err != nil {
		return err
	}

	if revertQuota != nil {
		reverter.Add(revertQuota)
	}

	// Create sparse loopback file if volume is block.
	rootBlockPath := ""
	if IsContentBlock(vol.contentType) {
		// We expect the filler to copy the VM image into this path.
		rootBlockPath, err = d.GetVolumeDiskPath(vol)
		if err != nil {
			return err
		}
	}

	err = genericRunFiller(d, vol, rootBlockPath, filler, false)
	if err != nil {
		return err
	}

	// If we are creating a block volume, resize it to the requested size or the default.
	// We expect the filler function to have converted the qcow2 image to raw into the rootBlockPath.
	if IsContentBlock(vol.contentType) {
		// Convert to bytes.
		sizeBytes, err := units.ParseByteSizeString(vol.ConfigSize())
		if err != nil {
			return err
		}

		// During initial volume fill we allow growing of image volumes because the snapshot
		// hasn't been taken yet, so no volume types are passed as unsupported.
		_, err = ensureVolumeBlockFile(vol, rootBlockPath, sizeBytes, false)
		if err != nil && !errors.Is(err, ErrCannotBeShrunk) {
			return err
		}

		// Move the GPT alt header to end of disk if needed and if filler specified.
		if vol.IsVMBlock() && filler != nil && filler.Fill != nil {
			err = d.moveGPTAltHeader(rootBlockPath)
			if err != nil {
				return err
			}
		}
	}

	// Tweak any permissions that need tweaking after filling.
	err = vol.EnsureMountPath(true)
	if err != nil {
		return err
	}

	reverter.Success()
	return nil
}

// CreateVolumeFromCopy provides same-pool volume copying functionality.
func (d *bcachefs) CreateVolumeFromCopy(vol Volume, srcVol Volume, copySnapshots bool, allowInconsistent bool, op *operations.Operation) error {
	reverter := revert.New()
	defer reverter.Fail()

	target := vol.MountPath()

	// Recursively copy the main volume as a writable snapshot.
	cleanup, err := d.snapshotSubvolume(srcVol.MountPath(), target, true, false)
	if err != nil {
		return err
	}

	if cleanup != nil {
		reverter.Add(cleanup)
	}

	// Resize volume to the size specified. Only uses volume "size" property and does not use pool/defaults
	// to give the caller more control over the size being used.
	err = d.SetVolumeQuota(vol, vol.config["size"], false, op)
	if err != nil {
		return err
	}

	// Fixup permissions after snapshot created.
	err = vol.EnsureMountPath(false)
	if err != nil {
		return err
	}

	var snapshots []string

	// Get snapshot list if copying snapshots.
	if copySnapshots && !srcVol.IsSnapshot() {
		// Get the list of snapshots.
		snapshots, err = d.VolumeSnapshots(srcVol, op)
		if err != nil {
			return err
		}
	}

	// Copy any snapshots needed.
	if len(snapshots) > 0 {
		// Create the parent directory.
		err = CreateParentSnapshotDirIfMissing(d.name, vol.volType, vol.name)
		if err != nil {
			return err
		}

		// Copy the snapshots.
		for _, snapName := range snapshots {
			srcSnapshot := GetVolumeMountPath(d.name, srcVol.volType, GetSnapshotVolumeName(srcVol.name, snapName))
			dstSnapshot := GetVolumeMountPath(d.name, vol.volType, GetSnapshotVolumeName(vol.name, snapName))

			// Snapshots are created read-only.
			cleanup, err := d.snapshotSubvolume(srcSnapshot, dstSnapshot, true, true)
			if err != nil {
				return err
			}

			if cleanup != nil {
				reverter.Add(cleanup)
			}
		}
	}

	reverter.Success()
	return nil
}

// readonlySnapshot creates a temporary read-only snapshot of a volume in the pool, so a consistent
// view of the volume can be sent while it is being written to. Returns the snapshot's path and a
// cleanup function.
func (d *bcachefs) readonlySnapshot(vol Volume) (string, revert.Hook, error) {
	reverter := revert.New()
	defer reverter.Fail()

	sourcePath := vol.MountPath()
	poolPath := GetPoolMountPath(d.name)
	tmpDir, err := os.MkdirTemp(poolPath, "readonly.")
	if err != nil {
		return "", nil, err
	}

	reverter.Add(func() {
		_ = os.RemoveAll(tmpDir)
	})

	// Prevent the snapshot from being descended into by the sender.
	err = os.Chmod(tmpDir, 0o100)
	if err != nil {
		return "", nil, err
	}

	mountPath := filepath.Join(tmpDir, vol.name)

	cleanup, err := d.snapshotSubvolume(sourcePath, mountPath, true, true)
	if err != nil {
		return "", nil, err
	}

	if cleanup != nil {
		reverter.Add(cleanup)
	}

	d.logger.Debug("Created read-only volume snapshot", logger.Ctx{"sourcePath": sourcePath, "path": mountPath})

	cleanup = reverter.Clone().Fail
	reverter.Success()
	return mountPath, cleanup, nil
}

// MigrateVolume streams the volume (and any snapshots) for a migration.
// This driver has no send/receive stream, so the generic rsync sender is used.
func (d *bcachefs) MigrateVolume(vol Volume, conn io.ReadWriteCloser, volSrcArgs *localMigration.VolumeSourceArgs, op *operations.Operation) error {
	if volSrcArgs.MigrationType.FSType != migration.MigrationFSType_RSYNC && volSrcArgs.MigrationType.FSType != migration.MigrationFSType_BLOCK_AND_RSYNC {
		return ErrNotSupported
	}

	// Take a read-only snapshot of a filesystem volume so the sender sees a consistent view.
	if vol.contentType == ContentTypeFS && !vol.IsSnapshot() {
		snapshotPath, cleanup, err := d.readonlySnapshot(vol)
		if err != nil {
			return err
		}

		defer cleanup()

		// Send from the snapshot rather than the live volume.
		vol.mountCustomPath = snapshotPath
	}

	return genericVFSMigrateVolume(d, d.state, vol, conn, volSrcArgs, op)
}

// CreateVolumeFromMigration creates a volume being sent via a migration.
func (d *bcachefs) CreateVolumeFromMigration(vol Volume, conn io.ReadWriteCloser, volTargetArgs localMigration.VolumeTargetArgs, preFiller *VolumeFiller, op *operations.Operation) error {
	return genericVFSCreateVolumeFromMigration(d, d.setupInitialQuota, vol, conn, volTargetArgs, preFiller, op)
}

// RefreshVolume provides same-pool volume and specific snapshots syncing functionality.
func (d *bcachefs) RefreshVolume(vol Volume, srcVol Volume, srcSnapshots []Volume, allowInconsistent bool, op *operations.Operation) error {
	return genericVFSCopyVolume(d, d.setupInitialQuota, vol, srcVol, srcSnapshots, true, allowInconsistent, op)
}

// BackupVolume copies a volume (and optionally its snapshots) to the backup writer.
// This driver has no optimized backup format, so the generic tarball is always used.
func (d *bcachefs) BackupVolume(vol Volume, writer instancewriter.InstanceWriter, basePrefix string, optimized bool, snapshots []string, op *operations.Operation) error {
	// Take a read-only snapshot of a filesystem volume so the tarball is consistent.
	if vol.contentType == ContentTypeFS {
		snapshotPath, cleanup, err := d.readonlySnapshot(vol)
		if err != nil {
			return err
		}

		defer cleanup()

		// Read from the snapshot rather than the live volume.
		vol.mountCustomPath = snapshotPath
	}

	return genericVFSBackupVolume(d, vol, writer, basePrefix, snapshots, op)
}

// CreateVolumeFromBackup restores a backup tarball onto the storage device.
func (d *bcachefs) CreateVolumeFromBackup(vol Volume, srcBackup backup.Info, srcData io.ReadSeeker, basePrefix string, op *operations.Operation) (VolumePostHook, revert.Hook, error) {
	// Unpack with quotas disabled: the volume IDs of instance volumes don't exist in the database yet.
	postHook, revertHook, err := genericVFSBackupUnpack(d.withoutGetVolID(), d.state.OS, vol, srcBackup.Snapshots, srcData, basePrefix, op)
	if err != nil {
		return nil, nil, err
	}

	// Instance volumes only get their database record once the config has been restored, so the
	// project has to be set up afterwards.
	if postHook != nil {
		postHookWrapper := func(vol Volume) error {
			err := postHook(vol)
			if err != nil {
				return err
			}

			reverter := revert.New()
			defer reverter.Fail()

			revertQuota, err := d.setupInitialQuota(vol)
			if err != nil {
				return err
			}

			reverter.Add(revertQuota)

			reverter.Success()
			return nil
		}

		return postHookWrapper, revertHook, nil
	}

	// Custom volumes have their database record before the restore, so the project can be set up now.
	reverter := revert.New()
	defer reverter.Fail()

	if revertHook != nil {
		reverter.Add(revertHook)
	}

	revertQuota, err := d.setupInitialQuota(vol)
	if err != nil {
		return nil, nil, err
	}

	if revertQuota != nil {
		reverter.Add(revertQuota)
	}

	cleanup := reverter.Clone().Fail
	reverter.Success()

	return nil, cleanup, nil
}

// DeleteVolume deletes a volume of the storage device. If any snapshots of the volume remain then
// this function will return an error.
func (d *bcachefs) DeleteVolume(vol Volume, op *operations.Operation) error {
	// Check that we don't have snapshots.
	snapshots, err := d.VolumeSnapshots(vol, op)
	if err != nil {
		return err
	}

	if len(snapshots) > 0 {
		return errors.New("Cannot remove a volume that has snapshots")
	}

	// If the volume doesn't exist, then nothing more to do.
	volPath := vol.MountPath()
	if !util.PathExists(volPath) {
		return nil
	}

	// Get the volume ID for the volume, which is used to remove the project quota.
	volID, err := d.getVolID(vol.volType, vol.name)
	if err != nil {
		return err
	}

	// Remove the project quota.
	err = d.deleteQuota(volPath, volID)
	if err != nil {
		return err
	}

	// Delete the volume (and any nested subvolumes).
	err = d.deleteSubvolume(volPath, true)
	if err != nil {
		return err
	}

	// Although the volume snapshot directory should already be removed, lets remove it here
	// to just in case the top-level directory is left.
	err = deleteParentSnapshotDirIfEmpty(d.name, vol.volType, vol.name)
	if err != nil {
		return err
	}

	return nil
}

// HasVolume indicates whether a specific volume exists on the storage pool.
func (d *bcachefs) HasVolume(vol Volume) (bool, error) {
	return genericVFSHasVolume(vol)
}

// ValidateVolume validates the supplied volume config.
func (d *bcachefs) ValidateVolume(vol Volume, removeUnknownKeys bool) error {
	// gendoc:generate(entity=storage_volume_bcachefs, group=common, key=initial.gid)
	//
	// ---
	//  type: int
	//  condition: custom volume with content type `filesystem`
	//  default: same as `volume.initial.gid` or `0`
	//  shortdesc: GID of the volume owner in the instance

	// gendoc:generate(entity=storage_volume_bcachefs, group=common, key=initial.mode)
	//
	// ---
	//  type: int
	//  condition: custom volume with content type `filesystem`
	//  default: same as `volume.initial.mode` or `711`
	//  shortdesc: Mode of the volume in the instance

	// gendoc:generate(entity=storage_volume_bcachefs, group=common, key=initial.uid)
	//
	// ---
	//  type: int
	//  condition: custom volume with content type `filesystem`
	//  default: same as `volume.initial.uid` or `0`
	//  shortdesc: UID of the volume owner in the instance

	// gendoc:generate(entity=storage_volume_bcachefs, group=common, key=security.shared)
	//
	// ---
	//  type: bool
	//  condition: custom block volume
	//  default: same as `volume.security.shared` or `false`
	//  shortdesc: Enable sharing the volume across multiple instances

	// gendoc:generate(entity=storage_volume_bcachefs, group=common, key=security.shifted)
	//
	// ---
	//  type: bool
	//  condition: custom volume
	//  default: same as `volume.security.shifted` or `false`
	//  shortdesc: {{enable_ID_shifting}}

	// gendoc:generate(entity=storage_volume_bcachefs, group=common, key=security.unmapped)
	//
	// ---
	//  type: bool
	//  condition: custom volume
	//  default: same as `volume.security.unmapped` or `false`
	//  shortdesc: Disable ID mapping for the volume

	// gendoc:generate(entity=storage_volume_bcachefs, group=common, key=size)
	//
	// ---
	//  type: string
	//  condition: appropriate driver
	//  default: same as `volume.size`
	//  shortdesc: Size/quota of the storage volume

	// gendoc:generate(entity=storage_volume_bcachefs, group=common, key=snapshots.expiry)
	// {{snapshot_expiry_detail}}
	// ---
	//  type: string
	//  condition: custom volume
	//  default: same as `volume.snapshot.expiry`
	//  shortdesc: {{snapshot_expiry_format}}

	// gendoc:generate(entity=storage_volume_bcachefs, group=common, key=snapshots.expiry.manual)
	// {{snapshot_expiry_detail}}
	// ---
	//  type: string
	//  condition: custom volume
	//  default: same as `volume.snapshot.expiry.manual`
	//  shortdesc: {{snapshot_expiry_format}}

	// gendoc:generate(entity=storage_volume_bcachefs, group=common, key=snapshots.pattern)
	//
	// ---
	//  type: string
	//  condition: custom volume
	//  default: same as `volume.snapshot.pattern` or `snap%d`
	//  shortdesc: {{snapshot_pattern_format}} [^*]

	// gendoc:generate(entity=storage_volume_bcachefs, group=common, key=snapshots.schedule)
	//
	// ---
	//  type: string
	//  condition: custom volume
	//  default: same as `volume.snapshot.schedule`
	//  shortdesc: {{snapshot_schedule_format}}

	return d.validateVolume(vol, d.commonVolumeRules(), removeUnknownKeys)
}

// UpdateVolume applies config changes to the volume.
func (d *bcachefs) UpdateVolume(vol Volume, changedConfig map[string]string) error {
	newSize, sizeChanged := changedConfig["size"]
	if sizeChanged {
		err := d.SetVolumeQuota(vol, newSize, false, nil)
		if err != nil {
			return err
		}
	}

	return d.updateVolume(vol, changedConfig)
}

// GetVolumeUsage returns the disk space used by the volume.
func (d *bcachefs) GetVolumeUsage(vol Volume) (int64, error) {
	// Snapshot usage isn't supported as bcachefs project quotas are only accounted to the inodes
	// of a master subvolume, not to snapshot subvolumes.
	if vol.IsSnapshot() {
		return -1, ErrNotSupported
	}

	volPath := vol.MountPath()

	ok, err := quota.Supported(volPath)
	if err != nil || !ok {
		return -1, ErrNotSupported
	}

	// Get the volume ID for the volume to access quota.
	volID, err := d.getVolID(vol.volType, vol.name)
	if err != nil {
		return -1, err
	}

	// Only volumes that own their project are accounted. A COW copy shares the project of the volume
	// it was snapshotted from and the kernel doesn't account writes to it.
	currentProjectID, err := quota.GetProject(volPath)
	if err != nil {
		return -1, err
	}

	if currentProjectID != d.quotaProjectID(volID) {
		return -1, ErrNotSupported
	}

	// Get project quota used.
	size, err := quota.GetProjectUsage(volPath, currentProjectID)
	if err != nil {
		return -1, err
	}

	return size, nil
}

// SetVolumeQuota applies a size limit on volume.
// Block volume backing files are resized and filesystem volumes get a project quota limit.
func (d *bcachefs) SetVolumeQuota(vol Volume, size string, allowUnsafeResize bool, op *operations.Operation) error {
	// Convert to bytes.
	sizeBytes, err := units.ParseByteSizeString(size)
	if err != nil {
		return err
	}

	// For VM block files, resize the file if needed.
	if vol.contentType == ContentTypeBlock {
		// Do nothing if size isn't specified.
		if sizeBytes <= 0 {
			return nil
		}

		rootBlockPath, err := d.GetVolumeDiskPath(vol)
		if err != nil {
			return err
		}

		resized, err := ensureVolumeBlockFile(vol, rootBlockPath, sizeBytes, allowUnsafeResize, VolumeTypeImage)
		if err != nil {
			return err
		}

		// Move the GPT alt header to end of disk if needed and resize has taken place (not needed in
		// unsafe resize mode as it is expected the caller will do all necessary post resize actions
		// themselves).
		if vol.IsVMBlock() && resized && !allowUnsafeResize {
			err = d.moveGPTAltHeader(rootBlockPath)
			if err != nil {
				return err
			}
		}

		return nil
	}

	// For filesystem volumes, set the project quota.
	volPath := vol.MountPath()

	// Get the volume ID for the volume to access quota.
	volID, err := d.getVolID(vol.volType, vol.name)
	if err != nil {
		return err
	}

	// Custom handling for filesystem volume associated with a VM.
	if sizeBytes > 0 && vol.volType == VolumeTypeVM && util.PathExists(filepath.Join(volPath, genericVolumeDiskFile)) {
		// Get the size of the VM image.
		blockSize, err := BlockDiskSizeBytes(filepath.Join(volPath, genericVolumeDiskFile))
		if err != nil {
			return err
		}

		// Add that to the requested filesystem size (to ignore it from the quota).
		sizeBytes += blockSize
		d.logger.Debug("Accounting for VM image file size", logger.Ctx{"sizeBytes": sizeBytes})
	}

	return d.setQuota(volPath, volID, sizeBytes)
}

// GetVolumeDiskPath returns the location and file format of a disk volume.
func (d *bcachefs) GetVolumeDiskPath(vol Volume) (string, error) {
	return genericVFSGetVolumeDiskPath(vol)
}

// ListVolumes returns a list of volumes in storage pool.
func (d *bcachefs) ListVolumes() ([]Volume, error) {
	return genericVFSListVolumes(d)
}

// MountVolume simulates mounting a volume.
func (d *bcachefs) MountVolume(vol Volume, op *operations.Operation) error {
	unlock, err := vol.MountLock()
	if err != nil {
		return err
	}

	defer unlock()

	// Don't attempt to modify the permission of an existing custom volume root.
	// A user inside the instance may have modified this and we don't want to reset it on restart.
	if !util.PathExists(vol.MountPath()) || vol.volType != VolumeTypeCustom {
		err := vol.EnsureMountPath(false)
		if err != nil {
			return err
		}
	}

	vol.MountRefCountIncrement() // From here on it is up to caller to call UnmountVolume() when done.
	return nil
}

// UnmountVolume simulates unmounting a volume.
// As driver doesn't have volumes to unmount it returns false indicating the volume was already unmounted.
func (d *bcachefs) UnmountVolume(vol Volume, keepBlockDev bool, op *operations.Operation) (bool, error) {
	unlock, err := vol.MountLock()
	if err != nil {
		return false, err
	}

	defer unlock()

	refCount := vol.MountRefCountDecrement()
	if refCount > 0 {
		d.logger.Debug("Skipping unmount as in use", logger.Ctx{"volName": vol.name, "refCount": refCount})
		return false, ErrInUse
	}

	return false, nil
}

// RenameVolume renames a volume and its snapshots.
func (d *bcachefs) RenameVolume(vol Volume, newVolName string, op *operations.Operation) error {
	return genericVFSRenameVolume(d, vol, newVolName, op)
}

// VolumeSnapshots returns a list of snapshots for the volume (in no particular order).
func (d *bcachefs) VolumeSnapshots(vol Volume, op *operations.Operation) ([]string, error) {
	return genericVFSVolumeSnapshots(d, vol, op)
}

// CreateVolumeSnapshot creates a snapshot of a volume.
func (d *bcachefs) CreateVolumeSnapshot(snapVol Volume, op *operations.Operation) error {
	parentName, _, _ := api.GetParentAndSnapshotName(snapVol.name)
	srcPath := GetVolumeMountPath(d.name, snapVol.volType, parentName)
	snapPath := snapVol.MountPath()

	// Create the parent directory.
	err := CreateParentSnapshotDirIfMissing(d.name, snapVol.volType, parentName)
	if err != nil {
		return err
	}

	reverter := revert.New()
	defer reverter.Fail()

	// Snapshots are created read-only.
	cleanup, err := d.snapshotSubvolume(srcPath, snapPath, true, true)
	if err != nil {
		return err
	}

	if cleanup != nil {
		reverter.Add(cleanup)
	}

	reverter.Success()
	return nil
}

// DeleteVolumeSnapshot removes a snapshot from the storage device.
func (d *bcachefs) DeleteVolumeSnapshot(snapVol Volume, op *operations.Operation) error {
	snapPath := snapVol.MountPath()

	// Delete the snapshot (and any nested subvolumes it contains).
	err := d.deleteSubvolume(snapPath, true)
	if err != nil {
		return err
	}

	// Remove the parent snapshot directory if this is the last snapshot being removed.
	parentName, _, _ := api.GetParentAndSnapshotName(snapVol.name)
	err = deleteParentSnapshotDirIfEmpty(d.name, snapVol.volType, parentName)
	if err != nil {
		return err
	}

	return nil
}

// MountVolumeSnapshot sets up a read-only mount on top of the snapshot to avoid accidental modifications.
func (d *bcachefs) MountVolumeSnapshot(snapVol Volume, op *operations.Operation) error {
	unlock, err := snapVol.MountLock()
	if err != nil {
		return err
	}

	defer unlock()

	snapPath := snapVol.MountPath()

	// Don't attempt to modify the permission of an existing custom volume root.
	// A user inside the instance may have modified this and we don't want to reset it on restart.
	if !util.PathExists(snapPath) || snapVol.volType != VolumeTypeCustom {
		err := snapVol.EnsureMountPath(false)
		if err != nil {
			return err
		}
	}

	_, err = mountReadOnly(snapPath, snapPath)
	if err != nil {
		return err
	}

	snapVol.MountRefCountIncrement() // From here on it is up to caller to call UnmountVolumeSnapshot() when done.
	return nil
}

// UnmountVolumeSnapshot removes the read-only mount placed on top of a snapshot.
func (d *bcachefs) UnmountVolumeSnapshot(snapVol Volume, op *operations.Operation) (bool, error) {
	unlock, err := snapVol.MountLock()
	if err != nil {
		return false, err
	}

	defer unlock()

	refCount := snapVol.MountRefCountDecrement()
	if refCount > 0 {
		d.logger.Debug("Skipping unmount as in use", logger.Ctx{"volName": snapVol.name, "refCount": refCount})
		return false, ErrInUse
	}

	snapPath := snapVol.MountPath()
	return forceUnmount(snapPath)
}

// RestoreVolume restores a volume from a snapshot.
func (d *bcachefs) RestoreVolume(vol Volume, snapshotName string, op *operations.Operation) error {
	reverter := revert.New()
	defer reverter.Fail()

	srcVol := NewVolume(d, d.name, vol.volType, vol.contentType, GetSnapshotVolumeName(vol.name, snapshotName), vol.config, vol.poolConfig)

	target := vol.MountPath()

	// Create a backup so we can revert.
	backupSubvolume := fmt.Sprintf("%s%s", target, tmpVolSuffix)
	err := os.Rename(target, backupSubvolume)
	if err != nil {
		return fmt.Errorf("Failed to rename %q to %q: %w", target, backupSubvolume, err)
	}

	reverter.Add(func() { _ = os.Rename(backupSubvolume, target) })

	// Restore the snapshot as a new writable volume.
	cleanup, err := d.snapshotSubvolume(srcVol.MountPath(), target, true, false)
	if err != nil {
		return err
	}

	if cleanup != nil {
		reverter.Add(cleanup)
	}

	reverter.Success()

	// Remove the backup subvolume.
	return d.deleteSubvolume(backupSubvolume, true)
}

// RenameVolumeSnapshot renames a volume snapshot.
func (d *bcachefs) RenameVolumeSnapshot(snapVol Volume, newSnapshotName string, op *operations.Operation) error {
	return genericVFSRenameVolumeSnapshot(d, snapVol, newSnapshotName, op)
}

// snapshotSubvolume creates a snapshot of the specified path at the dest supplied. If readonly is true
// the snapshot is created read-only. If recursion is true and subvolumes are found below the path then
// they are created at the relative location in dest.
func (d *bcachefs) snapshotSubvolume(path string, dest string, recursion bool, readonly bool) (revert.Hook, error) {
	reverter := revert.New()
	defer reverter.Fail()

	// Get the subvolumes list first, it determines which snapshots can be read-only.
	var subSubVols []string

	if recursion {
		var err error

		subSubVols, err = d.getSubvolumes(path)
		if err != nil {
			return nil, err
		}

		sort.Strings(subSubVols)
	}

	// Single subvolume snapshot creation.
	snapshot := func(path string, dest string, readonly bool) error {
		args := []string{"subvolume", "snapshot"}
		if readonly {
			args = append(args, "--read-only")
		}

		args = append(args, path, dest)

		_, err := subprocess.RunCommand("bcachefs", args...)
		if err != nil {
			return err
		}

		reverter.Add(func() {
			// Don't delete recursive since there already is a revert hook
			// for each subvolume that got created.
			_ = d.deleteSubvolume(dest, false)
		})

		return nil
	}

	// bcachefs can't create a subvolume inside a read-only one, so a snapshot that has to contain
	// nested subvolumes can only be writable.
	rootReadonly := readonly
	if rootReadonly && subvolumeHasChildren(subSubVols, "") {
		d.logger.Warn("Creating writable snapshot as bcachefs can't create subvolumes in a read-only snapshot", logger.Ctx{"path": path, "dest": dest})
		rootReadonly = false
	}

	// First snapshot the root.
	err := snapshot(path, dest, rootReadonly)
	if err != nil {
		return nil, err
	}

	// Now snapshot all subvolumes of the root.
	for _, subSubVol := range subSubVols {
		subSubVolSnapPath := filepath.Join(dest, subSubVol)

		// Clear the target for the subvol to use.
		_ = os.Remove(subSubVolSnapPath)

		// Subvolumes with nested subvolumes have to stay writable too.
		subSubVolReadonly := readonly && !subvolumeHasChildren(subSubVols, subSubVol)

		err := snapshot(filepath.Join(path, subSubVol), subSubVolSnapPath, subSubVolReadonly)
		if err != nil {
			return nil, err
		}
	}

	cleanup := reverter.Clone().Fail
	reverter.Success()
	return cleanup, nil
}

// commonVolumeRules returns validation rules which are common for pool and volume.
func (d *bcachefs) commonVolumeRules() map[string]func(value string) error {
	return map[string]func(value string) error{}
}

// ActivateTask allows running a function while the volume is active (but not mounted).
func (d *bcachefs) ActivateTask(vol Volume, task func(devPath string, op *operations.Operation) error, op *operations.Operation) error {
	// Prevent concurrent mounting actions.
	unlock, err := vol.MountLock()
	if err != nil {
		return err
	}

	defer unlock()

	volDevPath, err := d.GetVolumeDiskPath(vol)
	if err != nil {
		return err
	}

	// Run the task.
	return task(volDevPath, op)
}
