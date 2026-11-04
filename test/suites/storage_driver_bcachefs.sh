test_storage_driver_bcachefs() {
    # shellcheck disable=2039,3043
    local INCUS_STORAGE_DIR incus_backend

    incus_backend=$(storage_backend "$INCUS_DIR")
    if [ "$incus_backend" != "bcachefs" ]; then
        return
    fi

    INCUS_STORAGE_DIR=$(mktemp -d -p "${TEST_DIR}" XXXXXXXXX)
    chmod +x "${INCUS_STORAGE_DIR}"
    spawn_incus "${INCUS_STORAGE_DIR}" false

    (
        set -e
        # shellcheck disable=2030
        INCUS_DIR="${INCUS_STORAGE_DIR}"

        pool1="incustest-$(basename "${INCUS_DIR}")-pool1"
        pool2="incustest-$(basename "${INCUS_DIR}")-pool2"
        pool1Path="${INCUS_DIR}/storage-pools/${pool1}"
        pool2Path="${INCUS_DIR}/storage-pools/${pool2}"

        incus storage create "${pool1}" bcachefs

        # Disabling bcachefs.optimized_images makes the driver unpack images into each instance
        # volume, which allows instance size limits to be enforced.
        incus storage create "${pool2}" bcachefs bcachefs.optimized_images=false
        incus storage get "${pool2}" bcachefs.optimized_images | grep -q "^false$"

        # A pool can't use the root of the file system the daemon runs on, as deleting the pool
        # would wipe it.
        ! incus storage create "incustest-$(basename "${INCUS_DIR}")-pool-root" bcachefs source=/ || false

        # Set default storage pool for image import.
        incus profile device add default root disk path="/" pool="${pool1}"

        # Import image into default storage pool.
        ensure_import_testimage

        # A pool with cached images keeps an image subvolume once an instance has been launched.
        incus launch testimage c1
        [ -n "$(find "${pool1Path}/images" -mindepth 1 -maxdepth 1 -print -quit 2>/dev/null)" ]

        # Snapshot without any nested subvolumes: it is read-only.
        incus snapshot create c1 snap0
        ! touch "${pool1Path}/containers-snapshots/c1/snap0/rootfs/w.txt" || false

        # Create some nested subvolumes in the instance rootfs and populate them.
        rootfs="${pool1Path}/containers/c1/rootfs"
        owner="$(stat -c %u:%g "${rootfs}")"
        bcachefs subvolume create "${rootfs}/a"
        chown "${owner}" "${rootfs}/a"
        bcachefs subvolume create "${rootfs}/a/b"
        chown "${owner}" "${rootfs}/a/b"
        incus exec c1 -- touch /a/a1.txt
        incus exec c1 -- touch /a/b/b1.txt

        # Snapshot again now that nested subvolumes exist: they are snapshotted too, and any
        # subvolume without subvolumes of its own is read-only.
        incus snapshot create c1 snap1
        test -f "${pool1Path}/containers-snapshots/c1/snap1/rootfs/a/b/b1.txt"
        ! touch "${pool1Path}/containers-snapshots/c1/snap1/rootfs/a/b/w.txt" || false

        # bcachefs can't create a subvolume inside a read-only one, so the parts of the snapshot
        # that contain nested subvolumes stay writable on disk (Incus mounts snapshots read-only).
        touch "${pool1Path}/containers-snapshots/c1/snap1/rootfs/w.txt"
        rm -f "${pool1Path}/containers-snapshots/c1/snap1/rootfs/w.txt"

        # Add some more files to the nested subvolumes.
        incus exec c1 -- touch /a/a2.txt
        incus exec c1 -- touch /a/b/b2.txt
        incus snapshot create c1 snap2

        # Copy the instance to the pool without cached images (uses the migration subsystem).
        incus copy c1 c2 -s "${pool2}"
        incus start c2
        incus exec c2 -- stat /a/a2.txt
        incus exec c2 -- stat /a/b/b2.txt

        # Copies are read-write, including the nested directories.
        incus exec c2 -- touch /a/w.txt
        incus exec c2 -- touch /a/b/w.txt
        incus delete -f c2

        # Copy the snapshot as a new instance on the other pool.
        incus copy c1/snap1 c2 -s "${pool2}"
        incus start c2
        incus exec c2 -- stat /a/a1.txt
        incus exec c2 -- stat /a/b/b1.txt
        ! incus exec c2 -- stat /a/a2.txt || false
        ! incus exec c2 -- stat /a/b/b2.txt || false
        incus delete -f c2

        # Delete the nested subvolumes from the host and restore snap1: the nested subvolumes
        # and their contents come back.
        bcachefs subvolume delete "${pool1Path}/containers/c1/rootfs/a/b"
        bcachefs subvolume delete "${pool1Path}/containers/c1/rootfs/a"
        incus snapshot restore c1 snap1
        incus exec c1 -- stat /a/a1.txt
        incus exec c1 -- stat /a/b/b1.txt
        ! incus exec c1 -- stat /a/a2.txt || false
        ! incus exec c1 -- stat /a/b/b2.txt || false

        # Copy the instance within the pool: an instant COW copy which keeps the nested
        # subvolumes, and is read-write.
        incus copy c1 c3
        incus start c3
        incus exec c3 -- stat /a/a1.txt
        incus exec c3 -- stat /a/b/b1.txt
        incus exec c3 -- touch /a/w.txt
        incus exec c3 -- touch /a/b/w.txt
        incus delete -f c3

        # Volume usage is accounted through project quotas.
        incus storage volume create "${pool1}" vol-quota size=64MiB
        echo "hello" | incus storage volume file push - "${pool1}" vol-quota/data
        incus storage volume info "${pool1}" vol-quota | grep -qE "^Usage: [1-9]"

        # The volume can be attached to a running instance and written to.
        incus exec c1 -- mkdir -p /mnt/quota
        incus config device add c1 quota disk pool="${pool1}" source=vol-quota path=/mnt/quota
        incus exec c1 -- touch /mnt/quota/from-container
        [ -f "${pool1Path}/custom/default_vol-quota/from-container" ]
        incus config device remove c1 quota

        # Unprivileged writers (no CAP_SYS_RESOURCE in the initial user namespace) can't
        # exceed the volume's size limit. The user namespace keeps the writer unprivileged
        # from the filesystem's point of view while still allowing it to reach the volume.
        quotaPath="${pool1Path}/custom/default_vol-quota"
        chmod 777 "${quotaPath}"
        unshare -Ur dd if=/dev/zero "of=${quotaPath}/small" bs=1k count=1
        ! unshare -Ur dd if=/dev/zero "of=${quotaPath}/big" bs=1M count=192 || false
        incus storage volume delete "${pool1}" vol-quota

        # Custom volume snapshots are read-only and are copied along with the volume.
        incus storage volume create "${pool1}" vol1
        echo "hello" | incus storage volume file push - "${pool1}" vol1/data
        incus storage volume snapshot create "${pool1}" vol1 snap0
        incus storage volume snapshot show "${pool1}" vol1 snap0
        ! touch "${pool1Path}/custom-snapshots/default_vol1/snap0/w.txt" || false
        incus storage volume copy "${pool1}"/vol1 "${pool2}"/vol1-copy
        incus storage volume snapshot show "${pool2}" vol1-copy snap0
        echo "hello" | incus storage volume file pull "${pool2}" vol1-copy/data - | grep -q "hello"
        incus storage volume delete "${pool1}" vol1
        incus storage volume delete "${pool2}" vol1-copy

        # Block volumes are stored as files and can be snapshotted and copied.
        incus storage volume create "${pool1}" vol-block --type=block size=32MiB
        incus storage volume snapshot create "${pool1}" vol-block snap0
        incus storage volume copy "${pool1}"/vol-block "${pool2}"/vol-block-copy
        incus storage volume snapshot show "${pool2}" vol-block-copy snap0

        # The copied block volume keeps its 32MiB backing file.
        blockFileSize="$(stat -c %s "${pool2Path}/custom/default_vol-block-copy/root.img")"
        if [ "${blockFileSize}" != "33554432" ]; then
                echo "Unexpected copied block volume size: ${blockFileSize}"
                false
        fi

        incus storage volume delete "${pool1}" vol-block
        incus storage volume delete "${pool2}" vol-block-copy

        # ISO volumes are stored as files too.
        truncate -s 24MiB "${INCUS_DIR}/vol.iso"
        incus storage volume import "${pool1}" "${INCUS_DIR}/vol.iso" vol-iso
        incus storage volume show "${pool1}" vol-iso | grep -q "content_type: iso"
        incus storage volume copy "${pool1}"/vol-iso "${pool2}"/vol-iso-copy
        incus storage volume show "${pool2}" vol-iso-copy | grep -q "content_type: iso"
        incus storage volume delete "${pool1}" vol-iso
        incus storage volume delete "${pool2}" vol-iso-copy
        rm -f "${INCUS_DIR}/vol.iso"

        # There is no optimized backup format, so requesting it falls back to the generic format.
        incus export c1 "${INCUS_DIR}/c1.tar.gz" --optimized-storage
        tar -xOf "${INCUS_DIR}/c1.tar.gz" backup/index.yaml | grep -q "optimized: false"

        # Nested subvolumes are flattened when exported, so the files come back as plain dirs.
        incus delete -f c1
        incus import "${INCUS_DIR}/c1.tar.gz"
        incus start c1
        incus exec c1 -- stat /a/a1.txt
        incus exec c1 -- stat /a/b/b1.txt
        incus delete -f c1
        rm -f "${INCUS_DIR}/c1.tar.gz"

        incus profile device remove default root
        incus storage delete "${pool1}"
        incus storage delete "${pool2}"

        # Functional test: verify that bcachefs.create_options are applied when creating the
        # pool's filesystem.
        incus storage create "incustest-$(basename "${INCUS_DIR}")-createopts" bcachefs bcachefs.create_options="--block_size=4k"
        bcachefs show-super "${INCUS_DIR}/disks/incustest-$(basename "${INCUS_DIR}")-createopts.img" | grep -q "^  block_size:.*4"
        incus storage delete "incustest-$(basename "${INCUS_DIR}")-createopts"

        # Invalid create options are refused by mkfs.bcachefs.
        ! incus storage create "incustest-$(basename "${INCUS_DIR}")-badopts" bcachefs bcachefs.create_options="--invalid-option" || false

        # Test creating a storage pool from an existing bcachefs file system.
        truncate -s 512M testpool.img
        mkfs.bcachefs --prjquota -q -f testpool.img
        basepath="$(pwd)/mnt"
        mkdir -p "${basepath}"
        mount testpool.img "${basepath}"

        # An existing file system can be used as the source, using its mount point.
        incus storage create "incustest-$(basename "${INCUS_DIR}")-pool-mount" bcachefs source="${basepath}"
        incus storage volume create "incustest-$(basename "${INCUS_DIR}")-pool-mount" vol1
        incus storage volume create "incustest-$(basename "${INCUS_DIR}")-pool-mount" vol2
        incus storage volume delete "incustest-$(basename "${INCUS_DIR}")-pool-mount" vol1
        incus storage volume delete "incustest-$(basename "${INCUS_DIR}")-pool-mount" vol2
        incus storage delete "incustest-$(basename "${INCUS_DIR}")-pool-mount"

        bcachefs subvolume create "${basepath}/foo"
        bcachefs subvolume create "${basepath}/foo/bar"

        # This should fail as the source itself has subvolumes.
        ! incus storage create "incustest-$(basename "${INCUS_DIR}")-pool-subvol" bcachefs source="${basepath}/foo" || false

        # This should work as the provided subvolume is empty.
        bcachefs subvolume delete "${basepath}/foo/bar"
        incus storage create "incustest-$(basename "${INCUS_DIR}")-pool-subvol" bcachefs source="${basepath}/foo"
        incus storage volume create "incustest-$(basename "${INCUS_DIR}")-pool-subvol" vol1
        incus storage volume delete "incustest-$(basename "${INCUS_DIR}")-pool-subvol" vol1
        incus storage delete "incustest-$(basename "${INCUS_DIR}")-pool-subvol"

        sleep 1

        umount "${basepath}"
        rmdir "${basepath}"
        rm -f testpool.img
    )

    # shellcheck disable=SC2031
    kill_incus "${INCUS_STORAGE_DIR}"
}
