package drivers

import (
	"errors"

	"github.com/lxc/incus/v7/internal/server/storage/quota"
	"github.com/lxc/incus/v7/shared/logger"
	"github.com/lxc/incus/v7/shared/revert"
	"github.com/lxc/incus/v7/shared/units"
)

// setupInitialQuota sets up the project quota for a new volume and applies an initial limit from config.
// Returns a revert function that can be used to undo this function if a subsequent step fails.
func (d *bcachefs) setupInitialQuota(vol Volume) (revert.Hook, error) {
	if vol.IsVMBlock() {
		return nil, nil
	}

	volPath := vol.MountPath()

	// Get the volume ID for the new volume, which is used to set the project quota.
	volID, err := d.getVolID(vol.volType, vol.name)
	if err != nil {
		return nil, err
	}

	reverter := revert.New()
	defer reverter.Fail()

	// Define a function to revert the quota being setup.
	revertFunc := func() { _ = d.deleteQuota(volPath, volID) }
	reverter.Add(revertFunc)

	// Initialize the volume's project using the volume ID and set the quota.
	sizeBytes, err := units.ParseByteSizeString(vol.ConfigSize())
	if err != nil {
		return nil, err
	}

	// Block volumes are size limited by their disk file, only use the project for usage tracking.
	if IsContentBlock(vol.contentType) {
		sizeBytes = 0
	}

	err = d.setQuota(volPath, volID, sizeBytes)
	if err != nil {
		return nil, err
	}

	reverter.Success()
	return revertFunc, nil
}

// deleteQuota removes the project quota for a volID from a path.
func (d *bcachefs) deleteQuota(path string, volID int64) error {
	if volID == volIDQuotaSkip {
		// Disabled on purpose, just ignore.
		return nil
	}

	if volID == 0 {
		return errors.New("Missing volume ID")
	}

	ok, err := quota.Supported(path)
	if err != nil || !ok {
		// Skipping quota as underlying filesystem doesn't support project quotas.
		return nil
	}

	// Clear the project's size limit. Unlike the btrfs and dir drivers the project ID is left on
	// the volume's files: on a bcachefs snapshot subvolume resetting the project would force
	// copy-on-write of metadata shared with other subvolumes for no benefit as it is about to be
	// removed anyway.
	return quota.SetProjectQuota(path, d.quotaProjectID(volID), 0)
}

// quotaProjectID generates a project quota ID from a volume ID.
func (d *bcachefs) quotaProjectID(volID int64) uint32 {
	if volID == volIDQuotaSkip {
		// Disabled on purpose, just ignore.
		return 0
	}

	return uint32(volID + 10000)
}

// setQuota sets the project quota on the path. The volID generates a quota project ID.
//
// A freshly created volume inherits project 0 from its parent directory and gets a project assigned
// here. A volume whose project belongs to another volume is a COW copy (a bcachefs snapshot
// subvolume) sharing its inodes with the volume it was snapshotted from, so it is left alone: moving
// it to its own project would transfer the source volume's accounted usage to it
// (`bch2_set_projid` calls `bch2_fs_quota_transfer`), and the kernel skips accounting for writes to
// snapshot subvolumes anyway.
func (d *bcachefs) setQuota(path string, volID int64, sizeBytes int64) error {
	if volID == volIDQuotaSkip {
		// Disabled on purpose, just ignore.
		return nil
	}

	if volID == 0 {
		return errors.New("Missing volume ID")
	}

	ok, err := quota.Supported(path)
	if err != nil || !ok {
		if sizeBytes > 0 {
			// Skipping quota as underlying filesystem doesn't support project quotas.
			d.logger.Warn("The backing filesystem doesn't support quotas, skipping set quota", logger.Ctx{"path": path, "size": sizeBytes, "volID": volID})
		}

		return nil
	}

	projectID := d.quotaProjectID(volID)
	currentProjectID, err := quota.GetProject(path)
	if err != nil {
		return err
	}

	if currentProjectID != projectID {
		if currentProjectID != 0 {
			// The volume shares another volume's project, leave it alone.
			d.logger.Debug("Skipping bcachefs project quota on volume sharing another volume's project", logger.Ctx{"path": path, "projectID": projectID, "sharedProjectID": currentProjectID, "volID": volID})

			return nil
		}

		err = quota.SetProject(path, projectID)
		if err != nil {
			return err
		}
	}

	// Set the project quota size.
	return quota.SetProjectQuota(path, projectID, sizeBytes)
}
