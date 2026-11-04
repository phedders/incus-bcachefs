bcachefs_setup() {
    # shellcheck disable=2039,3043
    local INCUS_DIR

    INCUS_DIR=$1

    echo "==> Setting up bcachefs backend in ${INCUS_DIR}"
}

bcachefs_configure() {
    # shellcheck disable=2039,3043
    local INCUS_DIR

    INCUS_DIR=$1

    # Use a larger pool than other backends to leave room for the snapshots and
    # copies the test suite creates.
    incus storage create "incustest-$(basename "${INCUS_DIR}")" bcachefs size=4GiB
    incus profile device add default root disk path="/" pool="incustest-$(basename "${INCUS_DIR}")"

    echo "==> Configuring bcachefs backend in ${INCUS_DIR}"
}

bcachefs_teardown() {
    # shellcheck disable=2039,3043
    local INCUS_DIR

    INCUS_DIR=$1

    echo "==> Tearing down bcachefs backend in ${INCUS_DIR}"
}
