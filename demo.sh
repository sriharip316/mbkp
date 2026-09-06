#!/bin/bash

set -eou pipefail

# Database flavor, selected via --mariadb|--mysql (default: --mariadb).
FLAVOR=""

ROOT_PASSWORD=$(pwgen -nc1 16)

function usage {
    echo "Usage: $0 [--mariadb|--mysql]"
    echo ""
    echo "  --mariadb  Run the demo against MariaDB 10.11 (default)"
    echo "  --mysql    Run the demo against Percona Server for MySQL 8.4 LTS"
}

# set_flavor configures the image, container name and binaries for the chosen
# database flavor. Everything downstream is flavor-agnostic and only ever uses
# these variables.
function set_flavor {
    case "$1" in
        mariadb)
            CONTAINER=mariadb
            IMAGE="docker.io/library/mariadb:10.11"
            ROOT_PASS_ENV=MARIADB_ROOT_PASSWORD
            ENTRYPOINT_SCRIPT="docker-entrypoint.sh"
            CLIENT_BIN=mariadb
            SERVER_BIN=mariadbd
            SERVER_ARGS=(--log-bin=binlog)
            BKP_DIR=/root/bkp
            SETUP_XTRABACKUP=0
            ;;
        mysql)
            # Percona Server 9.7 is Percona's latest LTS, but Percona XtraBackup
            # 9.7 is not GA yet and mbkp requires xtrabackup for MySQL-family
            # servers, so the demo uses the 8.4 LTS line matched by
            # percona-xtrabackup-84.
            CONTAINER=mysql
            IMAGE="docker.io/percona/percona-server:8.4"
            ROOT_PASS_ENV=MYSQL_ROOT_PASSWORD
            ENTRYPOINT_SCRIPT="/docker-entrypoint.sh"
            CLIENT_BIN=mysql
            SERVER_BIN=mysqld
            # MySQL-family servers need GTID mode enabled: mbkp PITR is
            # GTID-based, and without gtid_mode=ON the binlogs carry no GTIDs
            # at all.
            SERVER_ARGS=(--log-bin=binlog --server-id=1 --gtid-mode=ON --enforce-gtid-consistency=ON)
            # The Percona image execs as the unprivileged mysql user, so the
            # backup directory must live somewhere that user can write.
            BKP_DIR=/tmp/bkp
            SETUP_XTRABACKUP=1
            ;;
    esac
}

function parse_args {
    FLAVOR="mariadb"
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --mariadb) FLAVOR=mariadb ;;
            --mysql) FLAVOR=mysql ;;
            -h|--help) usage; exit 0 ;;
            *) echo "Unknown argument: $1" >&2; usage >&2; exit 1 ;;
        esac
        shift
    done
    set_flavor "$FLAVOR"
}

function write_section {
    local section_name="$1"
    local section_length=120
    local name_length=${#section_name}
    local pad_length=$(((section_length - name_length)/2))
    printf "\033[0;34m%*s" "$pad_length" | tr ' ' '-'
    echo -n " $section_name "
    printf "%*s\033[0m" "$pad_length" | tr ' ' '-'
    echo
}

function create_pod {
    write_section "Creating pod"
    if [[ $(podman ps -q --filter "name=${CONTAINER}" | wc -l ) -eq 0 ]]; then
        # Commands are exec'd as the image's default user (root for MariaDB,
        # the unprivileged "mysql" user for Percona Server), so every path used
        # here (backup dir, installed binary) must be writable/readable by that
        # user. The Percona entrypoint also refuses to run its init server as
        # root, so the container must NOT be forced to --user root.
        podman run --detach --name "${CONTAINER}" --hostname "${CONTAINER}" \
            --env "${ROOT_PASS_ENV}=${ROOT_PASSWORD}" \
            --entrypoint sleep \
            "${IMAGE}" infinity
        sleep 1
        # podman exec "${CONTAINER}" apt-get update
        # podman exec "${CONTAINER}" apt-get install -y lz4
        if [[ ${SETUP_XTRABACKUP} -eq 1 ]]; then
            install_xtrabackup
        fi
    fi
}

# install_xtrabackup installs percona-xtrabackup-84 inside the running Percona
# Server container, mirroring installXtrabackup in cmd/mbkp/e2e_test.go: the
# official images are UBI9-based, ship neither xtrabackup nor libev, and UBI 9
# does not carry libev in its subscription repositories, so the libev RPM is
# fetched directly from the Rocky Linux 9 BaseOS.
function install_xtrabackup {
    write_section "Installing percona-xtrabackup-84"
    local libev_url="https://dl.rockylinux.org/pub/rocky/9/BaseOS/x86_64/os/Packages/l/libev-4.33-6.el9.x86_64.rpm"
    podman exec --user root "${CONTAINER}" curl -sSL -o /tmp/libev.rpm "${libev_url}"
    podman exec --user root "${CONTAINER}" rpm -i /tmp/libev.rpm || true
    podman exec --user root "${CONTAINER}" percona-release enable-only tools release
    # perl-English is required by xtrabackup; perl-Sys-Hostname silences a
    # "Can't locate Sys/Hostname.pm" error xtrabackup prints on every run with
    # the minimal UBI perl.
    podman exec --user root "${CONTAINER}" microdnf install -y percona-xtrabackup-84 perl-English perl-Sys-Hostname
}

function db_exec {
    # The password is passed via the MYSQL_PWD environment variable, never as
    # a command line argument.
    podman exec -e "MYSQL_PWD=${ROOT_PASSWORD}" "${CONTAINER}" "${CLIENT_BIN}" -uroot "$@"
}

function wait_for_db {
    local attempt
    for attempt in $(seq 1 60); do
        # Probe over TCP: the entrypoint's temporary init server is
        # skip-networking, so this only succeeds once the real server is up.
        if db_exec -h127.0.0.1 -e "SELECT 1" >/dev/null 2>&1; then
            return 0
        fi
        echo "Waiting for ${SERVER_BIN} to become ready... (${attempt}/60)"
        sleep 2
    done
    echo "Database server failed to become ready" >&2
    exit 1
}

function start_server {
    write_section "Starting ${SERVER_BIN}"
    if db_exec -h127.0.0.1 -e "SELECT 1" >/dev/null 2>&1; then
        echo "Server already running"
        return 0
    fi
    podman exec -t "${CONTAINER}" nohup "${ENTRYPOINT_SCRIPT}" "${SERVER_BIN}" "${SERVER_ARGS[@]}" &
    wait_for_db
}

function cleanup_data_directory {
    write_section "Cleaning up data directory"
    podman kill "${CONTAINER}"
    podman start "${CONTAINER}"
    sleep 1
    podman exec "${CONTAINER}" bash -c 'rm -rf /var/lib/mysql/* /var/lib/mysql/.*' || true
    if [[ $(podman exec "${CONTAINER}" find /var/lib/mysql/ | wc -l) -gt 1 ]]; then
        echo "Failed to clean up data directory"
        exit 1
    fi
}

function set_permissions {
    write_section "Setting permissions"
    podman exec "${CONTAINER}" bash -c 'chown -R mysql:mysql /var/lib/mysql'
}

function install_mbkp {
    write_section "Installing mbkp"
    make build
    # podman cp writes into the container as root, so the binary can be placed
    # directly into the root-owned /usr/local/bin regardless of the exec user.
    podman cp bin/mbkp "${CONTAINER}:/usr/local/bin/mbkp"
}

function create_schema {
    write_section "Creating schema"
    db_exec -e "CREATE DATABASE IF NOT EXISTS d1"
    db_exec -e "CREATE TABLE IF NOT EXISTS d1.t1 (n INT PRIMARY KEY AUTO_INCREMENT, d DATETIME)"
}

function insert_data {
    write_section "Inserting data"
    db_exec -e "INSERT INTO d1.t1 (d) VALUES (SYSDATE())"
    db_exec -e "SELECT COUNT(*) FROM d1.t1"
}

function get_count {
    db_exec -ABNe "SELECT COUNT(*) FROM d1.t1"
}

function delete_data {
    write_section "Deleting data"
    db_exec -e "DELETE FROM d1.t1"
}

function full_backup {
    write_section "Performing full backup"
    podman exec "${CONTAINER}" mbkp --backup-dir="${BKP_DIR}" backup full
}

function incremental_backup {
    write_section "Performing incremental backup"
    podman exec "${CONTAINER}" mbkp --backup-dir="${BKP_DIR}" backup incremental
}

function binlog_backup {
    write_section "Performing binlog backup"
    podman exec "${CONTAINER}" mbkp --backup-dir="${BKP_DIR}" backup binlog
}

function list_backups {
    write_section "Listing backups"
    podman exec "${CONTAINER}" mbkp --backup-dir="${BKP_DIR}" list --output json
    podman exec "${CONTAINER}" mbkp --backup-dir="${BKP_DIR}" list
}

function purge_backups {
    write_section "Purging backups"
    podman exec "${CONTAINER}" mbkp --backup-dir="${BKP_DIR}" purge --retention "$1"
}

function test_all_backups {
    write_section "Performing all backups"
    insert_data
    full_backup
    insert_data
    incremental_backup
    insert_data
    binlog_backup
    list_backups
}

function test_purge {
    write_section "Purging backups (retention=1d)"
    purge_backups 1d
    list_backups

    test_all_backups

    write_section "Purging backups (retention=1s)"
    purge_backups 1s
    list_backups

    write_section "Deleting full backups (externally)"
    podman exec "${CONTAINER}" find "${BKP_DIR}/" -name "full*" -delete
    list_backups
    purge_backups 1d
    list_backups
}

function test_restore__full_backup {
    write_section "Testing restore full backup"
    purge_backups 1s
    insert_data
    local count
    count=$(get_count)
    full_backup
    delete_data
    cleanup_data_directory
    podman exec "${CONTAINER}" mbkp --backup-dir="${BKP_DIR}" restore --datadir=/var/lib/mysql
    set_permissions
    start_server
    local new_count
    new_count=$(get_count)
    if [[ "$new_count" -ne "$count" ]]; then
        echo "Restore failed: count mismatch ($count vs $new_count)"
        exit 1
    elif [[ "$new_count" -eq "$count" ]]; then
        echo "Restore succeeded: count matches (count=$count)"
    fi
}

function test_restore__incremental_backup {
    write_section "Testing restore incremental backup"
    purge_backups 1s
    insert_data
    full_backup
    insert_data
    local count
    count=$(get_count)
    incremental_backup
    local inc_backup
    inc_backup=$(podman exec "${CONTAINER}" mbkp --backup-dir="${BKP_DIR}" list | grep inc | awk '{print $1}')
    delete_data
    cleanup_data_directory
    podman exec "${CONTAINER}" mbkp --backup-dir="${BKP_DIR}" restore --backup-id="${inc_backup}" --datadir=/var/lib/mysql
    set_permissions
    start_server
    local new_count
    new_count=$(get_count)
    if [ "$new_count" -ne "$count" ]; then
        echo "Restore failed: count mismatch ($count vs $new_count)"
        exit 1
    elif [[ "$new_count" -eq "$count" ]]; then
        echo "Restore succeeded: count matches (count=$count)"
    fi
}

function test_pitr {
    write_section "Testing PITR"
    insert_data
    full_backup
    insert_data
    incremental_backup
    insert_data
    local count
    count=$(get_count)
    sleep 1
    local restore_time
    restore_time=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    sleep 1
    insert_data
    binlog_backup
    delete_data
    cleanup_data_directory
    podman exec "${CONTAINER}" mbkp --backup-dir="${BKP_DIR}" pitr --datadir=/var/lib/mysql --target-time="${restore_time}"
    set_permissions
    start_server
    local new_count
    new_count=$(get_count)
    if [ "$new_count" -ne "$count" ]; then
        echo "PITR failed: count mismatch ($count vs $new_count)"
        exit 1
    elif [[ "$new_count" -eq "$count" ]]; then
        echo "PITR succeeded: count matches (count=$count)"
    fi
}

function main {
    parse_args "$@"
    create_pod
    start_server
    install_mbkp
    create_schema

    test_all_backups
    test_purge
    test_restore__full_backup
    test_restore__incremental_backup
    test_pitr
}

main "$@"
