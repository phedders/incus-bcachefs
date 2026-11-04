(storage-bcachefs)=
# bcachefs - `bcachefs`

{abbr}`bcachefs (B-tree file system)` is a local file system based on the {abbr}`COW (copy-on-write)` principle.
COW means that data is stored to new blocks after it has been modified instead of overwriting the existing blocks, which makes snapshots cheap and reduces the risk of data corruption.

In addition to basic file system features, bcachefs offers checksumming, compression, encryption, replication, pooling and other features.

To use bcachefs, make sure that your kernel supports the bcachefs file system and that the `bcachefs` tool (from `bcachefs-tools`) is installed on your machine.

## Terminology

Like Btrfs, a bcachefs file system can be divided into *subvolumes*, which are independent trees of the main tree of the file system, each with their own file and directory hierarchy.
A *bcachefs snapshot* is a subvolume that captures a specific state of another subvolume.
Snapshots share their data with the subvolume they were taken from and only consume additional space for the blocks that are modified afterwards.
bcachefs snapshots are writable by default; the snapshots that Incus creates are read-only.

## `bcachefs` driver in Incus

The `bcachefs` driver in Incus uses a subvolume per instance, image and snapshot.
Whenever it needs an instant copy of a volume (for example, when creating an instance from a cached image, copying an instance or a custom volume, taking a snapshot or restoring a snapshot), it takes a bcachefs snapshot instead of copying the data.

bcachefs doesn't natively support storing block devices.
Therefore, when using bcachefs for VMs, Incus creates a big file on disk to store the VM.
Because bcachefs is copy-on-write, a snapshot of such a volume keeps the blocks that the VM has overwritten since the snapshot was taken, so VMs that rewrite their disk use more space than the size of their disk.

Unlike Btrfs, bcachefs has no send/receive mechanism and no optimized backup format.
Incus therefore uses the generic paths on bcachefs pools: `rsync`, or a raw block copy for block volumes, when transferring instances and custom volumes between pools, and the tar-based format when backing up instances and custom volumes.

Subvolumes that you create yourself inside an instance's root file system (or inside a custom volume) are part of the volume: a snapshot of the volume recurses into them.
Because bcachefs can't create a subvolume inside a read-only subvolume, the parts of such a snapshot that contain nested subvolumes stay writable on disk; Incus still mounts snapshots read-only, and subvolumes without subvolumes of their own are created read-only.
Because the generic transfer methods don't carry subvolume boundaries, such subvolumes become regular directories when a volume is copied to another pool or restored from a backup.

(storage-bcachefs-source)=
### Storage pool source

By default, the `bcachefs` driver creates a loop-based pool: Incus creates a file of the configured `size` and formats it with bcachefs.

Instead of a loop file, you can put the pool on existing bcachefs storage.
The following values are supported for the pool's `source`:

- A not yet existing path on a bcachefs file system: Incus creates a subvolume at that path.
- An existing, empty subvolume: the subvolume is used as the root of the pool.
- An existing, empty directory on a bcachefs file system: Incus replaces the directory with a subvolume.
- The mount point of an existing bcachefs file system: the file system is used as the root of the pool.
  This also works when the host's root file system is bcachefs, as long as you point Incus at a subvolume of it.
- A block device: the device is formatted with bcachefs and used as the root of the pool.

The source can't be a path that already contains subvolumes, and paths that don't reside on a bcachefs file system are refused.

```{note}
A storage pool owns its source: deleting the pool deletes the pool's root directory and everything below it.
Point `source` at a dedicated subvolume, directory or file system, never at a location that holds other data.
For the same reason, Incus refuses to create a pool on the root of the file system that the daemon itself runs on.
```

### Cached image volumes

To make instance creation near instantaneous, Incus keeps a subvolume per image and creates the root volume of new instances as a bcachefs snapshot of it.

A bcachefs snapshot shares the project quota of the subvolume it was taken from, so such a COW copy can't have its own quota limit.
If you need the `size` limit of instances to be enforced, set {config:option}`storage_bcachefs-common:bcachefs.optimized_images` to `false`: the driver then unpacks the image into each new instance volume instead of creating it as a copy of a cached image.

(storage-bcachefs-quotas)=
### Quotas

bcachefs supports storage quotas via project quotas, and the `bcachefs` driver enables them whenever it formats a file system (including the loop-based case), by passing `--prjquota` to `mkfs.bcachefs`.
Setting `size` on a storage volume applies a project quota limit to the volume.

Quotas are charged to the subvolume that owns the project, which for a storage volume is the volume itself.
Because snapshots share their project with the subvolume they were taken from, snapshots don't have their own usage or limits.

Quota enforcement is done through the file system's quota accounting, which doesn't apply to processes that hold the `CAP_SYS_RESOURCE` capability.
Processes with that capability (for example, root inside a privileged container) can therefore exceed the limits.

If you create a pool on an existing bcachefs file system that wasn't formatted with project quotas, Incus can't enable them afterwards.
Limits that you set on such a pool are stored but not enforced, Incus can't report the usage of a volume (it shows `0B`) and it logs a warning when setting a limit.

## Configuration options

The following configuration options are available for storage pools that use the `bcachefs` driver and for storage volumes in these pools.

(storage-bcachefs-pool-config)=
### Storage pool configuration

% Include content from [config_options.txt](../config_options.txt)
```{include} ../config_options.txt
    :start-after: <!-- config group storage_bcachefs-common start -->
    :end-before: <!-- config group storage_bcachefs-common end -->
```

{{volume_configuration}}

### Storage volume configuration

% Include content from [config_options.txt](../config_options.txt)
```{include} ../config_options.txt
    :start-after: <!-- config group storage_volume_bcachefs-common start -->
    :end-before: <!-- config group storage_volume_bcachefs-common end -->
```

[^*]: {{snapshot_pattern_detail}}
