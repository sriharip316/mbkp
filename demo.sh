#!/bin/bash

set -eou pipefail

ROOT_PASSWORD=$(pwgen -nc1 16)

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
    if [[ $(podman ps -q --filter name=mariadb | wc -l ) -eq 0 ]]; then
        podman run --detach --name mariadb --hostname mariadb \
            --env MARIADB_ROOT_PASSWORD=${ROOT_PASSWORD} \
            --entrypoint sleep \
            docker.io/library/mariadb:10.11 infinity
        sleep 1
        # podman exec mariadb apt-get update
        # podman exec mariadb apt-get install -y lz4
    fi
}

function start_mariadb {
    write_section "Starting MariaDB"
    if [[ $(podman exec mariadb pgrep -cx mariadbd) -eq 0 ]]; then
        podman exec -t mariadb nohup docker-entrypoint.sh --log-bin=binlog &
        sleep 5
    fi
}

function cleanup_data_directory {
    write_section "Cleaning up data directory"
    podman kill mariadb
    podman start mariadb
    sleep 1
    podman exec mariadb bash -c 'rm -rf /var/lib/mysql/* /var/lib/mysql/.*' || true
    if [[ $(podman exec mariadb find /var/lib/mysql/ | wc -l) -gt 1 ]]; then
        echo "Failed to clean up data directory"
        exit 1
    fi
}

function set_permissions {
    write_section "Setting permissions"
    podman exec mariadb bash -c 'chown -R mysql:mysql /var/lib/mysql'
}

function install_mbkp {
    write_section "Installing mbkp"
    make build
    podman cp bin/mbkp mariadb:/root/
    podman exec mariadb mv /root/mbkp /usr/bin/
}

function create_schema {
    write_section "Creating schema"
    podman exec mariadb mariadb -uroot -p${ROOT_PASSWORD} -e "CREATE DATABASE IF NOT EXISTS d1"
    podman exec mariadb mariadb -uroot -p${ROOT_PASSWORD} -e "CREATE TABLE IF NOT EXISTS d1.t1 (n INT PRIMARY KEY AUTO_INCREMENT, d DATETIME)"
}

function insert_data {
    write_section "Inserting data"
    podman exec mariadb mariadb -uroot -p${ROOT_PASSWORD} -e "INSERT INTO d1.t1 (d) VALUES (SYSDATE())"
    podman exec mariadb mariadb -uroot -p${ROOT_PASSWORD} -e "SELECT COUNT(*) FROM d1.t1"
}

function get_count {
    podman exec mariadb mariadb -uroot -p${ROOT_PASSWORD} -ABNe "SELECT COUNT(*) FROM d1.t1"
}

function delete_data {
    write_section "Deleting data"
    podman exec mariadb mariadb -uroot -p${ROOT_PASSWORD} -e "DELETE FROM d1.t1"
}

function full_backup {
    write_section "Performing full backup"
    podman exec mariadb mbkp --backup-dir=/root/bkp backup full
}

function incremental_backup {
    write_section "Performing incremental backup"
    podman exec mariadb mbkp --backup-dir=/root/bkp backup incremental
}

function binlog_backup {
    write_section "Performing binlog backup"
    podman exec mariadb mbkp --backup-dir=/root/bkp backup binlog
}

function list_backups {
    write_section "Listing backups"
    podman exec mariadb mbkp --backup-dir=/root/bkp list --output json
    podman exec mariadb mbkp --backup-dir=/root/bkp list
}

function purge_backups {
    write_section "Purging backups"
    podman exec mariadb mbkp --backup-dir=/root/bkp purge --retention $1
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
    podman exec mariadb find /root/bkp/ -name "full*" -delete
    list_backups
    purge_backups 1d
    list_backups
}

function test_restore__full_backup {
    write_section "Testing restore full backup"
    purge_backups 1s
    insert_data
    local count=$(get_count)
    full_backup
    delete_data
    cleanup_data_directory
    podman exec mariadb mbkp --backup-dir=/root/bkp restore --datadir=/var/lib/mysql
    set_permissions
    start_mariadb
    local new_count=$(get_count)
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
    local count=$(get_count)
    incremental_backup
    local inc_backup=$(podman exec mariadb mbkp --backup-dir=/root/bkp list | grep inc | awk '{print $1}')
    delete_data
    cleanup_data_directory
    podman exec mariadb mbkp --backup-dir=/root/bkp restore --backup-id=$inc_backup --datadir=/var/lib/mysql
    set_permissions
    start_mariadb
    local new_count=$(get_count)
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
    local count=$(get_count)
    sleep 1
    local restore_time=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    sleep 1
    insert_data
    binlog_backup
    delete_data
    cleanup_data_directory
    podman exec mariadb mbkp --backup-dir=/root/bkp pitr --datadir=/var/lib/mysql --target-time=$restore_time
    set_permissions
    start_mariadb
    local new_count=$(get_count)
    if [ "$new_count" -ne "$count" ]; then
        echo "PITR failed: count mismatch ($count vs $new_count)"
        exit 1
    elif [[ "$new_count" -eq "$count" ]]; then
        echo "PITR succeeded: count matches (count=$count)"
    fi
}

function main {
    create_pod
    start_mariadb
    install_mbkp
    create_schema

    test_all_backups
    test_purge
    test_restore__full_backup
    test_restore__incremental_backup
    test_pitr
}

main
